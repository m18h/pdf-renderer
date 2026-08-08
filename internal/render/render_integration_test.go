//go:build browser

package render

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"rsc.io/pdf"
)

// assertValidPDF checks the cheapest real signals: the magic bytes and a
// terminating %%EOF, which together catch truncation.
func assertValidPDF(t *testing.T, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		dumpPDF(t, data)
		head := data
		if len(head) > 16 {
			head = head[:16]
		}
		t.Fatalf("not a PDF: first bytes = %q", head)
	}
	tail := data
	if len(tail) > 1024 {
		tail = tail[len(tail)-1024:]
	}
	if !bytes.Contains(tail, []byte("%%EOF")) {
		dumpPDF(t, data)
		t.Fatal("PDF is truncated: no EOF marker in the last 1KB")
	}
}

func openPDF(t *testing.T, data []byte) *pdf.Reader {
	t.Helper()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		dumpPDF(t, data)
		t.Fatalf("parse PDF: %v", err)
	}
	return r
}

// mediaBox returns the page's width and height in points.
func mediaBox(t *testing.T, r *pdf.Reader, page int) (float64, float64) {
	t.Helper()
	mb := r.Page(page).V.Key("MediaBox")
	if mb.Kind() != pdf.Array || mb.Len() != 4 {
		t.Fatalf("MediaBox is not a 4-element array: %v", mb)
	}
	return mb.Index(2).Float64(), mb.Index(3).Float64()
}

func TestRenderProducesValidPDF(t *testing.T) {
	p := testPool(t)
	res := renderFixture(t, p, &Request{HTMLBody: fixture(t, "simple.html")})

	assertValidPDF(t, res.PDF)
	if r := openPDF(t, res.PDF); r.NumPage() != 1 {
		t.Errorf("NumPage() = %d, want 1", r.NumPage())
	}
}

// The assertion that actually validates the whole option mapping. Landscape is
// the important half: Chromium swaps internally, so if we ever swapped too the
// output would be silently portrait.
func TestRenderMediaBoxDimensions(t *testing.T) {
	p := testPool(t)

	tests := []struct {
		size         string
		landscape    bool
		wantW, wantH float64
	}{
		// ISO sizes are not whole points, so they gain Chromium's ceil.
		{"A4", false, 596, 842},
		{"A4", true, 842, 596},
		{"A3", false, 842, 1191},
		{"A5", false, 420, 596},
		// Whole-inch sizes must be exact.
		{"Letter", false, 612, 792},
		{"Letter", true, 792, 612},
		{"Legal", false, 612, 1008},
		{"Tabloid", false, 792, 1224},
	}
	for _, tt := range tests {
		name := tt.size + "-portrait"
		orientation := "Portrait"
		if tt.landscape {
			name = tt.size + "-landscape"
			orientation = "Landscape"
		}
		t.Run(name, func(t *testing.T) {
			res := renderFixture(t, p, &Request{
				HTMLBody: fixture(t, "simple.html"),
				PageSize: tt.size, Orientation: orientation,
			})
			assertValidPDF(t, res.PDF)

			w, h := mediaBox(t, openPDF(t, res.PDF), 1)
			if math.Abs(w-tt.wantW) > 1 || math.Abs(h-tt.wantH) > 1 {
				t.Errorf("MediaBox = %.0f x %.0f pt, want %.0f x %.0f pt", w, h, tt.wantW, tt.wantH)
			}
		})
	}
}

// Selectable, searchable text is the whole reason this service uses Chromium
// rather than a raster-backed engine, so assert it structurally.
//
// Note we do not compare extracted strings: Chromium emits Type0/Identity-H
// fonts whose text operators carry glyph indices, and rsc.io/pdf does not apply
// the ToUnicode CMap needed to map those back to characters. The markers below
// are the actual guarantees — real glyph runs, an embedded font program, and a
// ToUnicode CMap, which together are what make a viewer able to select, copy and
// search the text.
func TestRenderProducesSelectableText(t *testing.T) {
	p := testPool(t)
	res := renderFixture(t, p, &Request{HTMLBody: fixture(t, "simple.html")})

	r := openPDF(t, res.PDF)
	if got := len(r.Page(1).Content().Text); got == 0 {
		dumpPDF(t, res.PDF)
		t.Fatal("no text runs in the PDF: the output is an image, not text")
	}
	if r.Page(1).Resources().Key("Font").Kind() == pdf.Null {
		t.Error("no /Font resource on the page")
	}

	// A ToUnicode CMap is what makes the text searchable and copyable.
	if !bytes.Contains(res.PDF, []byte("/ToUnicode")) {
		dumpPDF(t, res.PDF)
		t.Error("no /ToUnicode CMap: text would not be searchable or copyable")
	}
	// An embedded font program means the glyphs travel with the document.
	if !bytes.Contains(res.PDF, []byte("/FontFile")) {
		dumpPDF(t, res.PDF)
		t.Error("no /FontFile: no font program is embedded")
	}
}

func TestRenderMultiPageAndPageRanges(t *testing.T) {
	p := testPool(t)
	html := fixture(t, "multipage.html")

	all := renderFixture(t, p, &Request{HTMLBody: html})
	if got := openPDF(t, all.PDF).NumPage(); got != 3 {
		t.Errorf("NumPage() = %d, want 3", got)
	}

	one := renderFixture(t, p, &Request{HTMLBody: html, PageRanges: "2"})
	if got := openPDF(t, one.PDF).NumPage(); got != 1 {
		t.Errorf("with pageRanges=\"2\", NumPage() = %d, want 1", got)
	}
}

// printBackground defaults to false in Chromium and silently drops every CSS
// background. This is the single most visible regression if it ever stops being
// set, so verify it at the pixel level rather than by byte size alone.
func TestRenderPrintBackground(t *testing.T) {
	p := testPool(t)
	html := fixture(t, "background.html")

	on := renderFixture(t, p, &Request{HTMLBody: html})
	off := renderFixture(t, p, &Request{HTMLBody: html, PrintBackground: b(false)})

	assertValidPDF(t, on.PDF)
	assertValidPDF(t, off.PDF)

	if bytes.Equal(on.PDF, off.PDF) {
		t.Error("printBackground true and false produced identical output")
	}
	if len(on.PDF) <= len(off.PDF) {
		t.Errorf("PDF with backgrounds (%d bytes) should be larger than without (%d bytes)",
			len(on.PDF), len(off.PDF))
	}

	// Confirm the page really is red, via a screenshot in the same browser.
	assertTopLeftIsRed(t, p, html)
}

func assertTopLeftIsRed(t *testing.T, p *browserPool, html string) {
	t.Helper()
	var shot []byte
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	origin, err := serveHTML(context.Background(), []byte(html))
	if err != nil {
		t.Fatalf("serveHTML: %v", err)
	}
	defer origin.Close()

	err = p.Do(ctx, func(tabCtx context.Context) error {
		return chromedp.Run(tabCtx,
			chromedp.Navigate(origin.URL),
			chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.CaptureScreenshot(&shot),
		)
	})
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}

	img, err := decodePNG(shot)
	if err != nil {
		t.Fatalf("decode screenshot: %v", err)
	}
	r, g, bl, _ := img.At(10, 10).RGBA()
	if r>>8 < 200 || g>>8 > 60 || bl>>8 > 60 {
		t.Errorf("pixel at (10,10) = rgb(%d,%d,%d), want approximately red", r>>8, g>>8, bl>>8)
	}
}

// The test that catches a broken Dockerfile from Go: the headless-shell base
// image ships no fonts at all, and Chromium with empty fontconfig renders tofu.
func TestRenderFontsAreInstalled(t *testing.T) {
	p := testPool(t)
	html := fixture(t, "cjk.html")

	origin, err := serveHTML(context.Background(), []byte(html))
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()

	var cjkWidth float64
	var haveLatin bool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = p.Do(ctx, func(tabCtx context.Context) error {
		return chromedp.Run(tabCtx,
			chromedp.Navigate(origin.URL),
			chromedp.WaitReady("body", chromedp.ByQuery),
			chromedp.Evaluate(`document.fonts.ready.then(() =>
				document.getElementById('cjk').getBoundingClientRect().width)`,
				&cjkWidth, awaitPromise),
			chromedp.Evaluate(`document.fonts.check('32px sans-serif')`, &haveLatin),
		)
	})
	if err != nil {
		t.Fatalf("measure text: %v", err)
	}

	// Two full-width ideographs at 32px measure about 64px with a real CJK font.
	// Tofu or a fallback face measures noticeably differently.
	if cjkWidth < 56 || cjkWidth > 72 {
		t.Errorf("漢字 at 32px measured %.1fpx, want 56-72px; a CJK font is probably missing", cjkWidth)
	}
	if !haveLatin {
		t.Error("document.fonts.check('32px sans-serif') = false; no Latin font available")
	}
}

// One unreachable image must never hang or fail a render — wkhtmltopdf defaulted
// to ignoring load errors and callers depend on that.
func TestRenderSurvivesUnreachableImage(t *testing.T) {
	p := testPool(t)

	start := time.Now()
	res := renderFixture(t, p, &Request{HTMLBody: fixture(t, "slow-image.html")})
	elapsed := time.Since(start)

	assertValidPDF(t, res.PDF)
	if elapsed > 30*time.Second {
		t.Errorf("render took %v; a refused image should fail fast", elapsed)
	}
	if len(res.Warnings) == 0 {
		t.Error("Warnings is empty, want a note about the failed subresource")
	}
}

// A hanging image must be bounded by ReadyTimeout, not by the request deadline.
func TestRenderBoundedByReadyTimeoutOnHangingImage(t *testing.T) {
	p := testPool(t)

	// A handler that never responds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	html := fmt.Sprintf(`<!doctype html><html><body><h1>hang</h1>
		<img src="http://%s/slow.png"></body></html>`, ln.Addr().String())

	n, err := Normalize(&Request{HTMLBody: html})
	if err != nil {
		t.Fatal(err)
	}
	r := &Renderer{ReadyTimeout: 3 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	var res *Result
	err = p.Do(ctx, func(tabCtx context.Context) error {
		var rerr error
		res, rerr = r.Render(tabCtx, n)
		return rerr
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Render() error = %v, want a PDF despite the hanging image", err)
	}
	assertValidPDF(t, res.PDF)
	if elapsed > 25*time.Second {
		t.Errorf("render took %v, want it bounded by the 3s ReadyTimeout", elapsed)
	}
	if len(res.Warnings) == 0 {
		t.Error("Warnings is empty, want a note about the ready timeout")
	}
}

// Pins the compatibility decision: the default must stay screen media, because
// wkhtmltopdf's --print-media-type was opt-in and this service never set it.
func TestRenderMediaTypeDefaultIsScreen(t *testing.T) {
	p := testPool(t)
	html := fixture(t, "media.html")

	screen := renderFixture(t, p, &Request{HTMLBody: html})
	printMedia := renderFixture(t, p, &Request{HTMLBody: html, MediaType: "print"})

	// The fixture's only content is hidden by @media print, so counting glyph
	// runs distinguishes the two without needing to decode Identity-H text.
	screenRuns := countTextRuns(t, screen.PDF)
	printRuns := countTextRuns(t, printMedia.PDF)

	if screenRuns == 0 {
		dumpPDF(t, screen.PDF)
		t.Error("default render has no text; the screen-only element should be visible")
	}
	if printRuns != 0 {
		dumpPDF(t, printMedia.PDF)
		t.Errorf("mediaType=print produced %d text runs, want 0: @media print hides the element", printRuns)
	}
	if screenRuns == printRuns {
		t.Error("screen and print media produced the same output; setEmulatedMedia had no effect")
	}
}

func countTextRuns(t *testing.T, data []byte) int {
	t.Helper()
	r := openPDF(t, data)
	n := 0
	for i := 1; i <= r.NumPage(); i++ {
		n += len(r.Page(i).Content().Text)
	}
	return n
}

// The only test that proves the dpi → deviceScaleFactor mapping does anything:
// at 2x, Chromium must pick the 2x srcset candidate.
func TestRenderDPISelectsHighDensityImage(t *testing.T) {
	p := testPool(t)

	var got1x, got2x atomic.Bool
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/1x.png", func(w http.ResponseWriter, _ *http.Request) {
		got1x.Store(true)
		writePNG(w)
	})
	mux.HandleFunc("/2x.png", func(w http.ResponseWriter, _ *http.Request) {
		got2x.Store(true)
		writePNG(w)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	base := "http://" + ln.Addr().String()
	html := fmt.Sprintf(`<!doctype html><html><body>
		<img src="%s/1x.png" srcset="%s/1x.png 1x, %s/2x.png 2x" width="100">
		</body></html>`, base, base, base)

	t.Run("96dpi picks 1x", func(t *testing.T) {
		got1x.Store(false)
		got2x.Store(false)
		renderFixture(t, p, &Request{HTMLBody: html, DPI: f(96)})
		if !got1x.Load() {
			t.Error("1x.png was not fetched at 96 dpi")
		}
		if got2x.Load() {
			t.Error("2x.png was fetched at 96 dpi, want only 1x")
		}
	})

	t.Run("192dpi picks 2x", func(t *testing.T) {
		got1x.Store(false)
		got2x.Store(false)
		renderFixture(t, p, &Request{HTMLBody: html, DPI: f(192)})
		if !got2x.Load() {
			t.Error("2x.png was not fetched at 192 dpi; the deviceScaleFactor mapping is a no-op")
		}
	})
}
