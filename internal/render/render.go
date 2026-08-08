package render

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// DefaultReadyTimeout bounds the wait for fonts and images. It is deliberately
// shorter than the overall render deadline so one unreachable external asset
// costs a warning rather than the whole request.
const DefaultReadyTimeout = 15 * time.Second

// maxWarnings caps the advisory list so a page with thousands of broken images
// cannot balloon the response.
const maxWarnings = 20

// Result is a rendered document.
type Result struct {
	PDF      []byte
	Warnings []string
}

// readyExpr resolves once the document has loaded, its webfonts are ready, and
// every image has either loaded or failed.
//
// Images resolve on 'error' as well as 'load' on purpose: wkhtmltopdf defaulted
// to --load-error-handling ignore, so callers depend on a broken image producing
// a PDF rather than a failure.
const readyExpr = `(async () => {
  if (document.readyState !== 'complete') {
    await new Promise(r => window.addEventListener('load', r, {once: true}));
  }
  try { await document.fonts.ready; } catch (e) {}
  await Promise.all(Array.from(document.images).map(img => img.complete ? null :
    new Promise(r => {
      img.addEventListener('load',  r, {once: true});
      img.addEventListener('error', r, {once: true});
    })));
  return true;
})()`

// warnings accumulates advisory notes from CDP events. The listener runs on
// chromedp's event goroutine, so it needs its own lock.
type warnings struct {
	mu   sync.Mutex
	list []string
}

func (w *warnings) add(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.list) >= maxWarnings {
		return
	}
	w.list = append(w.list, s)
}

func (w *warnings) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.list...)
}

// Renderer turns a normalized request into a PDF on a supplied tab.
type Renderer struct {
	ReadyTimeout time.Duration
}

// NewRenderer returns a Renderer with default timings.
func NewRenderer() *Renderer {
	return &Renderer{ReadyTimeout: DefaultReadyTimeout}
}

// Render performs the full choreography on tabCtx: serve the HTML from a
// loopback origin, apply emulation, navigate, wait for assets, print.
func (r *Renderer) Render(tabCtx context.Context, n *Normalized) (*Result, error) {
	origin, err := serveHTML(tabCtx, n.HTML)
	if err != nil {
		return nil, err
	}
	defer origin.Close()

	w := &warnings{list: append([]string(nil), n.Warnings...)}
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *network.EventLoadingFailed:
			w.add("subresource failed to load: " + e.ErrorText)
		case *runtime.EventExceptionThrown:
			if e.ExceptionDetails != nil {
				w.add("script error: " + e.ExceptionDetails.Text)
			}
		}
	})

	var actions []chromedp.Action

	// Emulation must precede Navigate. Applied afterwards, devicePixelRatio is
	// still 1 during initial layout, so Chromium picks the 1x candidate from
	// every srcset/image-set() and the dpi mapping does nothing.
	if n.EmulateMetrics {
		actions = append(actions, emulation.SetDeviceMetricsOverride(
			n.ViewportW, n.ViewportH, n.DeviceScaleFactor, false))
	}
	// wkhtmltopdf rendered with screen media by default; Chromium's printToPDF
	// uses print. Set this explicitly so the default stays screen.
	actions = append(actions, emulation.SetEmulatedMedia().WithMedia(n.MediaType))

	var pdf []byte
	actions = append(actions,
		r.navigate(origin.URL, w),
		r.waitReady(w),
		printToPDF(n.printParams(), &pdf),
	)

	if err := chromedp.Run(tabCtx, actions...); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &Error{Kind: KindTimeout, Msg: "render timed out", Err: err}
		}
		if errors.Is(err, context.Canceled) {
			return nil, &Error{Kind: KindTimeout, Msg: "render cancelled", Err: err}
		}
		return nil, internalf(err, "render")
	}

	// Zero hits means Chromium never fetched our document, which yields a blank
	// PDF. Saying so beats letting the caller wonder.
	if origin.Hits() == 0 {
		return nil, internalf(nil, "browser never fetched the document")
	}
	if len(pdf) == 0 {
		return nil, internalf(nil, "browser returned an empty PDF")
	}

	return &Result{PDF: pdf, Warnings: w.snapshot()}, nil
}

// navigate loads the document, bounded by the ready budget.
//
// chromedp.Navigate waits for the frame's load event, which never fires while a
// subresource hangs — so without its own deadline a single unresponsive external
// image would stall the render for the whole request timeout, not just the ready
// budget. On expiry the document is already parsed and laid out, so printing what
// we have is both possible and the behaviour callers had under wkhtmltopdf, whose
// default was --load-error-handling ignore.
func (r *Renderer) navigate(url string, w *warnings) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		sub, cancel := context.WithTimeout(ctx, r.budget())
		defer cancel()

		err := chromedp.Navigate(url).Do(sub)
		if err == nil {
			return nil
		}
		if ctx.Err() == nil && sub.Err() != nil {
			w.add("page did not finish loading within the ready timeout; rendered it as-is")
			return nil
		}
		return err
	})
}

func (r *Renderer) budget() time.Duration {
	if r.ReadyTimeout > 0 {
		return r.ReadyTimeout
	}
	return DefaultReadyTimeout
}

// waitReady blocks until the document's assets settle, or the ready budget runs
// out — in which case it prints what it has and records a warning.
//
// The timeout lives on a child context so its expiry does not poison tabCtx, and
// the parent's own error is checked to tell "one slow image ran out its budget"
// (proceed) from "the whole request was cancelled" (abort).
func (r *Renderer) waitReady(w *warnings) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		sub, cancel := context.WithTimeout(ctx, r.budget())
		defer cancel()

		var ok bool
		err := chromedp.Evaluate(readyExpr, &ok, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		}).Do(sub)
		if err == nil {
			return nil
		}
		if ctx.Err() == nil && sub.Err() != nil {
			w.add("timed out waiting for fonts and images; rendered the page as-is")
			return nil
		}
		return err
	})
}

// Probe evaluates a trivial expression on a tab. A readiness check uses this to
// confirm the browser actually responds, rather than trusting the pool's
// bookkeeping about whether it should.
func Probe(tabCtx context.Context) error {
	var got int
	if err := chromedp.Run(tabCtx, chromedp.Evaluate("1+1", &got)); err != nil {
		return internalf(err, "browser probe")
	}
	if got != 2 {
		return internalf(nil, "browser probe returned %d, want 2", got)
	}
	return nil
}

// printToPDF wraps Page.printToPDF, which returns three values and therefore
// cannot be used as a bare chromedp.Action.
func printToPDF(p *page.PrintToPDFParams, out *[]byte) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		data, _, err := p.Do(ctx)
		if err != nil {
			return err
		}
		*out = data
		return nil
	})
}
