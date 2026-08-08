//go:build browser

package browser

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

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

func realPool(t *testing.T, cfg Config) *Pool {
	t.Helper()
	cfg.ExecPath = execPath(t)
	cfg.NoSandbox = os.Getenv("PDFRENDER_NO_SANDBOX") == "1"
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 2
	}
	if cfg.AcquireTimeout == 0 {
		cfg.AcquireTimeout = 30 * time.Second
	}
	cfg.Logger = quietLogger()

	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func evalOK(t *testing.T, p *Pool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.Do(ctx, func(tabCtx context.Context) error {
		var got int
		if err := chromedp.Run(tabCtx, chromedp.Evaluate("1+1", &got)); err != nil {
			return err
		}
		if got != 2 {
			t.Errorf("1+1 = %d, wanted 2", got)
		}
		return nil
	})
}

func TestRealBrowserRoundTrip(t *testing.T) {
	p := realPool(t, Config{})
	if err := evalOK(t, p); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !p.Healthy() {
		t.Error("Healthy() = false after a successful render")
	}
}

// A crashed renderer must not take the browser with it: the next request should
// simply get a new tab.
func TestRendererCrashRecovery(t *testing.T) {
	p := realPool(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Navigating to chrome://crash kills that renderer process. The error is
	// expected; what matters is what happens next.
	_ = p.Do(ctx, func(tabCtx context.Context) error {
		return chromedp.Run(tabCtx, chromedp.Navigate("chrome://crash"))
	})

	if err := evalOK(t, p); err != nil {
		t.Errorf("after a renderer crash, Do() error = %v, want nil", err)
	}
}

// The direct test for the poisoning hazard: killing the browser itself must be
// survivable, because chromedp cancels the shared context when the connection
// drops and every later request would otherwise fail forever.
func TestBrowserCrashRecovery(t *testing.T) {
	p := realPool(t, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.Do(ctx, func(tabCtx context.Context) error {
		return chromedp.Run(tabCtx, chromedp.ActionFunc(func(c context.Context) error {
			return cdpbrowser.Crash().Do(c)
		}))
	})

	// Self-healing may take one failed attempt; it must not take forever.
	var lastErr error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if lastErr = evalOK(t, p); lastErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("pool never recovered from Browser.crash: %v", lastErr)
	}
	if !p.Healthy() {
		t.Error("Healthy() = false after recovery")
	}
}

// Catches a missing cancelTab(): every tab is a renderer process, so leaking
// them accumulates until the container OOMs.
func TestNoProcessLeak(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process counting is implemented for linux and darwin only")
	}
	p := realPool(t, Config{MaxConcurrent: 2})

	if err := evalOK(t, p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	before := countBrowserProcs(t)

	for i := range 25 {
		if err := evalOK(t, p); err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
	}
	// Renderer teardown is asynchronous; give it a moment to settle.
	time.Sleep(5 * time.Second)
	after := countBrowserProcs(t)

	// A small delta is normal process churn; 25 leaked renderers would not be.
	if after > before+8 {
		t.Errorf("browser process count went %d -> %d over 25 renders; tabs are leaking", before, after)
	}
	t.Logf("browser processes: before=%d after=%d", before, after)
}

func countBrowserProcs(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "comm").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "headless-shell") || strings.Contains(l, "chromium") ||
			strings.Contains(l, "chrome") || strings.Contains(l, "msedge") {
			n++
		}
	}
	return n
}

// Recycling against a real browser: the old process must actually go away and
// the pool must keep serving throughout.
func TestRecycleWithRealBrowser(t *testing.T) {
	p := realPool(t, Config{MaxConcurrent: 1, MaxRenders: 2})

	for i := range 6 {
		if err := evalOK(t, p); err != nil {
			t.Fatalf("render %d after recycling: %v", i, err)
		}
	}
	if !p.Healthy() {
		t.Error("Healthy() = false after recycling")
	}
}

func TestConcurrentRealRenders(t *testing.T) {
	p := realPool(t, Config{MaxConcurrent: 3})

	errs := make(chan error, 9)
	for range 9 {
		go func() { errs <- evalOK(t, p) }()
	}
	for i := range 9 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent render %d: %v", i, err)
		}
	}
}
