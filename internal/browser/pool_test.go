package browser

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeHandle stands in for a browser process. No Chromium involved, so the whole
// supervisor — crash detection, replacement, recycling, concurrency — is
// exercised deterministically.
type fakeHandle struct {
	mu       sync.Mutex
	dead     bool
	closed   bool
	tabErr   error
	openTabs atomic.Int64
	maxTabs  atomic.Int64
}

func (h *fakeHandle) NewTab() (context.Context, context.CancelFunc, error) {
	h.mu.Lock()
	err := h.tabErr
	h.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}

	n := h.openTabs.Add(1)
	for {
		cur := h.maxTabs.Load()
		if n <= cur || h.maxTabs.CompareAndSwap(cur, n) {
			break
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			h.openTabs.Add(-1)
			cancel()
		})
	}, nil
}

func (h *fakeHandle) Dead() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dead
}

func (h *fakeHandle) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}

func (h *fakeHandle) kill() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dead = true
}

func (h *fakeHandle) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// fakeLauncher records every launch so tests can count relaunches.
type fakeLauncher struct {
	mu       sync.Mutex
	handles  []*fakeHandle
	launches atomic.Int64
	err      error
}

func (l *fakeLauncher) launch(Config) (Handle, error) {
	l.launches.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	h := &fakeHandle{}
	l.handles = append(l.handles, h)
	return h, nil
}

func (l *fakeLauncher) last() *fakeHandle {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.handles) == 0 {
		return nil
	}
	return l.handles[len(l.handles)-1]
}

func (l *fakeLauncher) at(i int) *fakeHandle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handles[i]
}

func newTestPool(t *testing.T, cfg Config) (*Pool, *fakeLauncher) {
	t.Helper()
	l := &fakeLauncher{}
	cfg.launch = l.launch
	cfg.Logger = quietLogger()
	if cfg.AcquireTimeout == 0 {
		cfg.AcquireTimeout = time.Second
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(p.Close)
	return p, l
}

func noop(context.Context) error { return nil }

// waitClosed polls for an asynchronous close. retire() hands teardown to a
// goroutine that first drains in-flight renders, so a bare assertion right after
// a replacement races that goroutine.
func waitClosed(t *testing.T, h *fakeHandle, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if h.isClosed() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h.isClosed()
}

func TestConcurrencyCap(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 3, AcquireTimeout: 5 * time.Second})

	var wg sync.WaitGroup
	release := make(chan struct{})
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Do(context.Background(), func(context.Context) error {
				<-release
				return nil
			})
		}()
	}

	// Let the first wave occupy every slot.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := l.last().maxTabs.Load(); got != 3 {
		t.Errorf("max concurrent tabs = %d, want 3", got)
	}
	if got := l.last().openTabs.Load(); got != 0 {
		t.Errorf("open tabs after completion = %d, want 0 (leaked tab = leaked process)", got)
	}
}

func TestAcquireTimeoutReturnsBusy(t *testing.T) {
	p, _ := newTestPool(t, Config{MaxConcurrent: 1, AcquireTimeout: 100 * time.Millisecond})

	held := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Do(context.Background(), func(context.Context) error {
			close(held)
			time.Sleep(500 * time.Millisecond)
			return nil
		})
	}()
	<-held

	start := time.Now()
	err := p.Do(context.Background(), noop)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBusy) {
		t.Errorf("error = %v, want ErrBusy", err)
	}
	if elapsed < 90*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Errorf("waited %v, want roughly the 100ms AcquireTimeout", elapsed)
	}
	<-done
}

func TestDoRespectsCallerCancellation(t *testing.T) {
	p, _ := newTestPool(t, Config{MaxConcurrent: 1, AcquireTimeout: 10 * time.Second})

	held := make(chan struct{})
	go func() {
		_ = p.Do(context.Background(), func(context.Context) error {
			close(held)
			time.Sleep(300 * time.Millisecond)
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Do(ctx, noop); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestRecycleAfterRenderCount(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1, MaxRenders: 5})

	for range 10 {
		if err := p.Do(context.Background(), noop); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
	}

	// 1 initial launch + 2 recycles (after render 5 and render 10).
	if got := l.launches.Load(); got != 3 {
		t.Errorf("launches = %d, want 3 (initial + 2 recycles)", got)
	}
	if !waitClosed(t, l.at(0), 2*time.Second) {
		t.Error("the first generation's browser was never closed")
	}
}

func TestRecycleAfterAge(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, now.Load()) }

	p, l := newTestPool(t, Config{MaxConcurrent: 1, MaxAge: time.Minute, now: clock})

	if err := p.Do(context.Background(), noop); err != nil {
		t.Fatal(err)
	}
	if got := l.launches.Load(); got != 1 {
		t.Fatalf("launches = %d, want 1 before the clock advances", got)
	}

	now.Add(int64(2 * time.Minute))
	if err := p.Do(context.Background(), noop); err != nil {
		t.Fatal(err)
	}
	if got := l.launches.Load(); got != 2 {
		t.Errorf("launches = %d, want 2 after exceeding MaxAge", got)
	}
}

// The poisoning regression: a browser crash must retire that generation only,
// leaving the Pool able to serve the very next request. Getting this wrong
// bricks the service until a restart.
func TestCrashReplacesGenerationAndPoolRecovers(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1})

	renderErr := errors.New("render blew up")
	err := p.Do(context.Background(), func(context.Context) error {
		l.last().kill() // browser dies mid-render
		return renderErr
	})
	if err == nil {
		t.Fatal("Do() error = nil, want the render error surfaced")
	}
	if got := l.launches.Load(); got < 2 {
		t.Fatalf("launches = %d, want at least 2: the dead generation must be replaced", got)
	}

	// The critical assertion: subsequent calls succeed.
	for i := range 3 {
		if err := p.Do(context.Background(), noop); err != nil {
			t.Fatalf("call %d after crash: error = %v, want nil", i+1, err)
		}
	}
	if !p.Healthy() {
		t.Error("Healthy() = false after recovery, want true")
	}
}

func TestDeadBrowserDetectedBeforeUse(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1})
	l.last().kill()

	if err := p.Do(context.Background(), noop); err != nil {
		t.Errorf("Do() error = %v, want nil (retry on a fresh browser should succeed)", err)
	}
	if got := l.launches.Load(); got != 2 {
		t.Errorf("launches = %d, want 2", got)
	}
}

// 50 simultaneous failures must produce one relaunch, not 50.
func TestThunderingHerdCausesOneRelaunch(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 16, AcquireTimeout: 5 * time.Second})
	l.last().kill()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Do(context.Background(), noop)
		}()
	}
	wg.Wait()

	if got := l.launches.Load(); got != 2 {
		t.Errorf("launches = %d, want 2 (initial + exactly one relaunch)", got)
	}
}

// Retiring a generation must not cancel a render that is still running on it.
func TestReplaceWaitsForInflightBeforeClosing(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 4, AcquireTimeout: 5 * time.Second})
	gen0 := l.last()

	inRender := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Do(context.Background(), func(context.Context) error {
			close(inRender)
			<-finish
			return nil
		})
	}()
	<-inRender

	// Force a replacement while that render is still in flight.
	gen0.kill()
	if err := p.Do(context.Background(), noop); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	if gen0.isClosed() {
		t.Error("generation 0 was closed while a render was still in flight")
	}
	close(finish)
	<-done

	// Now it may close, asynchronously.
	if !waitClosed(t, gen0, 2*time.Second) {
		t.Error("generation 0 was never closed after its renders drained")
	}
}

func TestTabAllocationFailureTriggersReplacement(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1})
	h := l.last()
	h.mu.Lock()
	h.tabErr = errors.New("cannot open tab")
	h.mu.Unlock()

	// First attempt fails to open a tab, replacement succeeds, retry works.
	if err := p.Do(context.Background(), noop); err != nil {
		t.Errorf("Do() error = %v, want nil after replacement", err)
	}
	if got := l.launches.Load(); got != 2 {
		t.Errorf("launches = %d, want 2", got)
	}
}

// A failing launcher must stay bounded rather than spin.
func TestLaunchFailureIsBounded(t *testing.T) {
	l := &fakeLauncher{}
	p, err := New(Config{MaxConcurrent: 1, AcquireTimeout: time.Second, Logger: quietLogger(), launch: l.launch})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer p.Close()

	// Break the launcher, then kill the running browser.
	l.last().kill()
	l.mu.Lock()
	l.err = errors.New("no chromium")
	l.mu.Unlock()

	before := l.launches.Load()
	if err := p.Do(context.Background(), noop); err == nil {
		t.Error("Do() error = nil, want a failure when no browser can start")
	}
	// Two attempts at most, so at most two relaunch tries — not a spin loop.
	if got := l.launches.Load() - before; got > 2 {
		t.Errorf("relaunch attempts = %d, want at most 2", got)
	}
	if p.Healthy() {
		t.Error("Healthy() = true with no browser available")
	}
}

func TestCloseIsIdempotentAndBlocksFurtherWork(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1})
	h := l.last()

	p.Close()
	p.Close() // must not panic or double-close

	if !h.isClosed() {
		t.Error("browser was not closed by Close()")
	}
	if err := p.Do(context.Background(), noop); !errors.Is(err, ErrClosed) {
		t.Errorf("Do() after Close() = %v, want ErrClosed", err)
	}
	if p.Healthy() {
		t.Error("Healthy() = true after Close()")
	}
}

func TestRenderErrorOnHealthyBrowserIsNotRetried(t *testing.T) {
	p, l := newTestPool(t, Config{MaxConcurrent: 1})

	sentinel := errors.New("bad html")
	var calls int
	err := p.Do(context.Background(), func(context.Context) error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want the sentinel returned verbatim", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want 1: a document error is not a browser failure", calls)
	}
	if got := l.launches.Load(); got != 1 {
		t.Errorf("launches = %d, want 1: no relaunch for a document error", got)
	}
}

// The tab context descends from the browser, not from the caller, so without an
// explicit bridge a client disconnect or expired deadline would leave the render
// running — holding its semaphore slot and its renderer process.
func TestCallerCancellationCancelsTheTab(t *testing.T) {
	p, _ := newTestPool(t, Config{MaxConcurrent: 1, AcquireTimeout: 5 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	tabDone := make(chan struct{})

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := p.Do(ctx, func(tabCtx context.Context) error {
		close(started)
		select {
		case <-tabCtx.Done(): // the bridge fired
			close(tabDone)
			return tabCtx.Err()
		case <-time.After(3 * time.Second):
			return errors.New("tab context was never cancelled")
		}
	})

	<-started
	select {
	case <-tabDone:
	default:
		t.Fatal("caller cancellation did not reach the tab context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
