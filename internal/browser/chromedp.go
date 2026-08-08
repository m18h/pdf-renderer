package browser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/chromedp/chromedp"
)

// chromeHandle is the production Handle: one Chromium process, addressed through
// a root chromedp context.
type chromeHandle struct {
	allocCtx      context.Context
	cancelAlloc   context.CancelFunc
	browserCtx    context.Context
	cancelBrowser context.CancelFunc

	logger *slog.Logger
	// isolate requests a fresh BrowserContext per tab. Cleared if the browser
	// turns out not to support it; see NewTab.
	isolate  atomic.Bool
	warnOnce sync.Once
}

func launchChromium(cfg Config) (Handle, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)

	if cfg.ExecPath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ExecPath))
	}
	opts = append(opts,
		// chromedp's defaults include "site-per-process" in disable-features,
		// which turns OFF Chromium site isolation. This service renders untrusted
		// HTML, so that boundary matters; re-enable it by overriding the flag
		// without site-per-process. chromedp.Flag writes into a map, so this
		// later value wins.
		chromedp.Flag("disable-features", "Translate,BlinkGenPropertyTrees"),
		// Software rasterisation, matching what upstream's headless-shell run.sh
		// passes. Deterministic across hosts and needs no GPU.
		chromedp.Flag("use-gl", "angle"),
		chromedp.Flag("use-angle", "swiftshader"),
		chromedp.Flag("hide-scrollbars", true),
	)
	if cfg.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}

	// Parent must be Background: the browser has to outlive any single request,
	// and it must survive the first SIGTERM so in-flight renders can drain.
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(func(f string, a ...any) { cfg.Logger.Debug(fmt.Sprintf(f, a...)) }),
		chromedp.WithErrorf(func(f string, a ...any) { cfg.Logger.Warn(fmt.Sprintf(f, a...)) }),
	)

	// Run with no actions is what actually starts the process.
	if err := chromedp.Run(browserCtx); err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, fmt.Errorf("launch chromium: %w%s", err, sandboxHint(err))
	}

	h := &chromeHandle{
		allocCtx:      allocCtx,
		cancelAlloc:   cancelAlloc,
		browserCtx:    browserCtx,
		cancelBrowser: cancelBrowser,
		logger:        cfg.Logger,
	}
	h.isolate.Store(true)
	return h, nil
}

// sandboxHint turns Chromium's opaque sandbox failure into an actionable
// message. Docker's default seccomp profile denies clone(CLONE_NEWUSER), so the
// namespace sandbox cannot start in a stock `docker run` — which is exactly why
// upstream's own image passes --no-sandbox.
func sandboxHint(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"sandbox", "namespace", "clone"} {
		if strings.Contains(msg, needle) {
			return "\nhint: Chromium's sandbox could not start. Either run the container with" +
				" --security-opt seccomp=deploy/chrome-seccomp.json (preferred), or" +
				" --cap-add SYS_ADMIN, or set PDFRENDER_NO_SANDBOX=1 to disable the" +
				" sandbox (weakest — this service renders untrusted HTML)."
		}
	}
	return ""
}

// NewTab allocates a fresh tab on the running browser.
//
// chromedp.NewContext inherits the Allocator and Browser from the parent but
// deliberately drops the Target, which is what makes this a new tab rather than
// a whole new browser process.
// WithNewBrowserContext gives each request an incognito-like profile, so
// cookies, localStorage and the HTTP cache cannot leak between callers. The
// production base image (chromedp/headless-shell) supports it, but a full
// Chrome/Edge in new-headless mode rejects Target.createTarget against a fresh
// browser context with "no browser is open". Rather than fail on a developer
// machine, degrade to a shared context and say so once.
func (h *chromeHandle) NewTab() (context.Context, context.CancelFunc, error) {
	if h.isolate.Load() {
		tabCtx, cancel, err := h.newTab(chromedp.WithNewBrowserContext())
		if err == nil {
			return tabCtx, cancel, nil
		}
		if h.Dead() {
			return nil, nil, fmt.Errorf("new isolated tab: %w", err)
		}
		h.isolate.Store(false)
		h.warnOnce.Do(func() {
			h.logger.Warn("per-request browser contexts unavailable; "+
				"falling back to a shared context, so cookies, localStorage and the "+
				"HTTP cache are NOT isolated between requests",
				"error", err)
		})
	}

	tabCtx, cancel, err := h.newTab()
	if err != nil {
		return nil, nil, fmt.Errorf("new tab: %w", err)
	}
	return tabCtx, cancel, nil
}

func (h *chromeHandle) newTab(opts ...chromedp.ContextOption) (context.Context, context.CancelFunc, error) {
	tabCtx, cancel := chromedp.NewContext(h.browserCtx, opts...)
	if err := chromedp.Run(tabCtx); err != nil {
		cancel()
		return nil, nil, err
	}
	return tabCtx, cancel, nil
}

// Isolated reports whether tabs currently get their own BrowserContext.
func (h *chromeHandle) Isolated() bool { return h.isolate.Load() }

func (h *chromeHandle) Dead() bool {
	return h.browserCtx.Err() != nil || h.allocCtx.Err() != nil
}

func (h *chromeHandle) Close() {
	h.cancelBrowser()
	h.cancelAlloc()
}
