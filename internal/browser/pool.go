// Package browser supervises a long-lived headless Chromium and hands out one
// tab per request.
package browser

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrClosed is returned once Close has been called.
	ErrClosed = errors.New("browser pool is closed")
	// ErrBusy means every slot was in use for longer than AcquireTimeout.
	ErrBusy = errors.New("browser pool is busy")
	// ErrUnavailable means no usable browser could be obtained, even after a
	// relaunch.
	ErrUnavailable = errors.New("no usable browser")
)

// Handle is the part of a running browser the pool depends on. The real
// implementation wraps chromedp (see chromedp.go); tests supply a fake, which is
// what lets the whole supervisor be exercised without Chromium.
type Handle interface {
	// NewTab returns a context scoped to a fresh tab and a cancel that closes it.
	NewTab() (context.Context, context.CancelFunc, error)
	// Dead reports whether the browser connection has gone away.
	Dead() bool
	// Close terminates the browser process.
	Close()
}

// Config configures a Pool.
type Config struct {
	// ExecPath is the browser binary. Empty means let chromedp autodetect.
	ExecPath string
	// NoSandbox disables Chromium's sandbox. Weakens isolation for untrusted
	// HTML; only set it where the namespace sandbox cannot start.
	NoSandbox bool

	// MaxConcurrent caps simultaneous tabs. Each tab is an OS process, so size
	// this from measured RSS rather than goroutine count.
	MaxConcurrent int
	// AcquireTimeout bounds the wait for a free slot.
	AcquireTimeout time.Duration
	// MaxRenders recycles the browser after this many renders. 0 disables.
	MaxRenders int
	// MaxAge recycles the browser after this long. 0 disables.
	MaxAge time.Duration

	Logger *slog.Logger

	// launch is a test seam. Nil means launch real Chromium.
	launch func(Config) (Handle, error)
	// now is a test seam for age-based recycling.
	now func() time.Time
}

func (c *Config) withDefaults() {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = max(1, runtime.NumCPU())
	}
	if c.AcquireTimeout <= 0 {
		c.AcquireTimeout = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.launch == nil {
		c.launch = launchChromium
	}
	if c.now == nil {
		c.now = time.Now
	}
}

// generation is one browser process plus its bookkeeping. A crash retires the
// generation rather than the Pool, so the Pool can heal by starting a new one.
type generation struct {
	seq     uint64
	handle  Handle
	started time.Time

	renders  atomic.Int64
	inflight sync.WaitGroup
	dead     atomic.Bool
	retiring atomic.Bool
}

// Pool owns one browser at a time and serialises replacement of it.
type Pool struct {
	cfg Config
	sem chan struct{}

	// mu guards gen. Replacement holds it for the whole relaunch so that
	// concurrent callers wait for the new browser instead of all stampeding into
	// their own relaunch.
	mu  sync.RWMutex
	gen *generation
	seq atomic.Uint64

	closed atomic.Bool
}

// New starts a Pool and launches its first browser.
func New(cfg Config) (*Pool, error) {
	cfg.withDefaults()
	p := &Pool{
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
	g, err := p.start()
	if err != nil {
		return nil, err
	}
	p.gen = g
	return p, nil
}

func (p *Pool) start() (*generation, error) {
	h, err := p.cfg.launch(p.cfg)
	if err != nil {
		return nil, err
	}
	g := &generation{
		seq:     p.seq.Add(1),
		handle:  h,
		started: p.cfg.now(),
	}
	p.cfg.Logger.Info("browser started", "generation", g.seq)
	return g, nil
}

func (p *Pool) current() *generation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.gen
}

// Do runs fn against a fresh tab on the current browser.
//
// If the browser has died, Do retires that generation, launches a replacement
// and retries once. Without this, a single browser crash would poison the
// shared context and fail every subsequent request until the process restarted.
func (p *Pool) Do(ctx context.Context, fn func(tabCtx context.Context) error) error {
	if p.closed.Load() {
		return ErrClosed
	}

	timer := time.NewTimer(p.cfg.AcquireTimeout)
	defer timer.Stop()
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrBusy
	}
	defer func() { <-p.sem }()

	var lastErr error
	for attempt := range 2 {
		if p.closed.Load() {
			return ErrClosed
		}
		g := p.current()
		if g == nil {
			lastErr = ErrUnavailable
			continue
		}
		if g.dead.Load() || g.handle.Dead() {
			p.replace(g, "browser dead before use")
			lastErr = ErrUnavailable
			continue
		}

		g.inflight.Add(1)
		tabCtx, cancelTab, err := g.handle.NewTab()
		if err != nil {
			g.inflight.Done()
			g.dead.Store(true)
			p.replace(g, "tab allocation failed: "+err.Error())
			lastErr = err
			continue
		}

		// The tab context descends from the browser, not from ctx, so the
		// caller's deadline does not reach it on its own. Without this bridge a
		// client disconnect or an expired request timeout would leave the render
		// running to completion, holding its semaphore slot and its process.
		stopBridge := context.AfterFunc(ctx, cancelTab)

		// contextcheck cannot see the AfterFunc bridge above; tabCtx must descend
		// from the browser (that is what makes it a tab), and ctx cancellation
		// reaches it through cancelTab rather than through parentage.
		err = fn(tabCtx) //nolint:contextcheck

		stopBridge()
		// Unconditional: a leaked tab is a leaked renderer process.
		cancelTab()
		p.afterRender(g)
		g.inflight.Done()

		// Distinguish "this document failed" from "the browser is gone". Only the
		// latter is worth retrying, and only if the caller is still waiting.
		if err != nil && g.handle.Dead() {
			g.dead.Store(true)
			p.replace(g, "browser died during render")
			lastErr = err
			if attempt == 0 && ctx.Err() == nil {
				continue
			}
		}
		return err
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return lastErr
}

// replace retires g and installs a fresh generation.
//
// The seq check makes this idempotent under concurrency: whoever gets the lock
// first does the relaunch, and everyone else observes a newer generation and
// returns. That is what keeps 50 simultaneous failures to one relaunch.
func (p *Pool) replace(g *generation, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed.Load() {
		return
	}
	if p.gen == nil || p.gen.seq != g.seq {
		return // already replaced by another caller
	}
	if !g.retiring.CompareAndSwap(false, true) {
		return
	}

	p.cfg.Logger.Warn("replacing browser", "generation", g.seq, "reason", reason)
	retire(g)

	newGen, err := p.start()
	if err != nil {
		// Leave gen nil; the next Do reports ErrUnavailable rather than spinning.
		p.gen = nil
		p.cfg.Logger.Error("browser relaunch failed", "error", err)
		return
	}
	p.gen = newGen
}

// retire closes a generation's browser once its in-flight renders finish, off
// the caller's critical path.
func retire(g *generation) {
	go func() {
		g.inflight.Wait()
		g.handle.Close()
	}()
}

// afterRender counts the render and recycles the browser when it gets old or
// busy. Chromium leaks slowly under sustained load, so proactive recycling is
// cheaper than waiting for an OOM.
func (p *Pool) afterRender(g *generation) {
	n := g.renders.Add(1)

	overCount := p.cfg.MaxRenders > 0 && n >= int64(p.cfg.MaxRenders)
	overAge := p.cfg.MaxAge > 0 && p.cfg.now().Sub(g.started) >= p.cfg.MaxAge
	if !overCount && !overAge {
		return
	}

	reason := "render count reached"
	if overAge {
		reason = "max age reached"
	}
	p.replace(g, reason)
}

// Healthy reports whether a live browser is currently installed. It is a cheap
// liveness check; Probe actually exercises the browser.
func (p *Pool) Healthy() bool {
	if p.closed.Load() {
		return false
	}
	g := p.current()
	return g != nil && !g.dead.Load() && !g.handle.Dead()
}

// Probe opens a tab and runs fn against it, so a readiness check can verify the
// browser really responds rather than trusting bookkeeping.
func (p *Pool) Probe(ctx context.Context, fn func(tabCtx context.Context) error) error {
	return p.Do(ctx, fn)
}

// Close stops the pool and terminates the browser.
func (p *Pool) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return // idempotent
	}
	p.mu.Lock()
	g := p.gen
	p.gen = nil
	p.mu.Unlock()

	if g != nil {
		g.inflight.Wait()
		g.handle.Close()
	}
	p.cfg.Logger.Info("browser pool closed")
}
