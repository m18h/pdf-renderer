//go:build browser

package render

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/m18h/pdf-renderer/internal/browser"
)

// browserPool aliases the concrete pool so test signatures stay short.
type browserPool = browser.Pool

// awaitPromise makes chromedp.Evaluate resolve a returned promise.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

var _ = chromedp.Run // keep the import used across build configurations

func decodePNG(data []byte) (image.Image, error) {
	return png.Decode(bytes.NewReader(data))
}

// writePNG emits a tiny opaque PNG, enough to satisfy an <img> fetch.
func writePNG(w http.ResponseWriter) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for x := range 2 {
		for y := range 2 {
			img.Set(x, y, color.RGBA{R: 0, G: 0, B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// execPath finds a browser binary, preferring PDFRENDER_EXEC_PATH. Without one
// the browser-tagged tests skip rather than fail, so a laptop with no Chromium
// can still run the default suite.
func execPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PDFRENDER_EXEC_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Fatalf("PDFRENDER_EXEC_PATH=%q does not exist", p)
	}
	for _, cand := range []string{
		"/headless-shell/headless-shell",
		"/usr/bin/chromium-headless-shell",
		"/usr/bin/chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	t.Skip("no browser found; set PDFRENDER_EXEC_PATH")
	return ""
}

// testPool starts a real browser for the duration of the test binary.
func testPool(t *testing.T) *browser.Pool {
	t.Helper()
	p, err := browser.New(browser.Config{
		ExecPath:       execPath(t),
		NoSandbox:      os.Getenv("PDFRENDER_NO_SANDBOX") == "1",
		MaxConcurrent:  2,
		AcquireTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start browser: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// renderFixture renders a file from testdata with the given request options.
func renderFixture(t *testing.T, p *browser.Pool, req *Request) *Result {
	t.Helper()
	res, err := tryRenderFixture(t, p, req)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return res
}

func tryRenderFixture(t *testing.T, p *browser.Pool, req *Request) (*Result, error) {
	t.Helper()
	n, err := Normalize(req)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	r := &Renderer{ReadyTimeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var res *Result
	err = p.Do(ctx, func(tabCtx context.Context) error {
		var rerr error
		res, rerr = r.Render(tabCtx, n)
		return rerr
	})
	return res, err
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// dumpPDF writes the bytes somewhere inspectable. Debugging "the parser said no"
// without the artifact is miserable.
func dumpPDF(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Logf("could not dump PDF: %v", err)
		return ""
	}
	t.Logf("PDF written to %s (%d bytes)", path, len(data))
	return path
}
