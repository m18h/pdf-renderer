package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/m18h/pdf-renderer/internal/browser"
	"github.com/m18h/pdf-renderer/internal/httpapi"
	"github.com/m18h/pdf-renderer/internal/render"
)

// version is set at build time via -ldflags.
var version = "dev"

type config struct {
	Port           string
	ExecPath       string
	NoSandbox      bool
	MaxConcurrent  int
	AcquireTimeout time.Duration
	MaxRenders     int
	MaxAge         time.Duration
	RenderTimeout  time.Duration
	ReadyTimeout   time.Duration
	MaxBodyBytes   int64
	LogFormat      string
}

func main() {
	// The image has no curl or wget, so the binary probes itself for HEALTHCHECK.
	healthcheck := flag.Bool("healthcheck", false, "probe the local /readyz endpoint and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version) //nolint:forbidigo // deliberate stdout for --version
		return
	}

	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}

	if *healthcheck {
		os.Exit(runHealthcheck(cfg.Port))
	}

	if err := run(cfg); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func loadConfig(lookup func(string) (string, bool)) (config, error) {
	c := config{
		Port:           "8080",
		ExecPath:       "/headless-shell/headless-shell",
		MaxConcurrent:  0, // 0 → NumCPU, resolved by the pool
		AcquireTimeout: 5 * time.Second,
		MaxRenders:     500,
		MaxAge:         30 * time.Minute,
		RenderTimeout:  httpapi.DefaultRenderTimeout,
		ReadyTimeout:   render.DefaultReadyTimeout,
		MaxBodyBytes:   httpapi.DefaultMaxBodyBytes,
		LogFormat:      "json",
	}

	if v, ok := lookup("PORT"); ok && v != "" {
		// Fail loudly rather than silently falling back.
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return c, fmt.Errorf("PORT must be a port number between 1 and 65535, got %q", v)
		}
		c.Port = v
	}
	if v, ok := lookup("PDFRENDER_EXEC_PATH"); ok && v != "" {
		c.ExecPath = v
	}
	if v, ok := lookup("PDFRENDER_NO_SANDBOX"); ok {
		c.NoSandbox = v == "1" || v == "true"
	}
	if v, ok := lookup("PDFRENDER_LOG_FORMAT"); ok && v != "" {
		if v != "json" && v != "text" {
			return c, fmt.Errorf("PDFRENDER_LOG_FORMAT must be \"json\" or \"text\", got %q", v)
		}
		c.LogFormat = v
	}

	ints := []struct {
		env string
		dst *int
	}{
		{"PDFRENDER_MAX_CONCURRENT", &c.MaxConcurrent},
		{"PDFRENDER_MAX_RENDERS", &c.MaxRenders},
	}
	for _, f := range ints {
		if v, ok := lookup(f.env); ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return c, fmt.Errorf("%s must be a non-negative integer, got %q", f.env, v)
			}
			*f.dst = n
		}
	}

	durations := []struct {
		env string
		dst *time.Duration
	}{
		{"PDFRENDER_ACQUIRE_TIMEOUT", &c.AcquireTimeout},
		{"PDFRENDER_MAX_AGE", &c.MaxAge},
		{"PDFRENDER_RENDER_TIMEOUT", &c.RenderTimeout},
		{"PDFRENDER_READY_TIMEOUT", &c.ReadyTimeout},
	}
	for _, f := range durations {
		if v, ok := lookup(f.env); ok && v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return c, fmt.Errorf("%s must be a non-negative duration such as \"30s\", got %q", f.env, v)
			}
			*f.dst = d
		}
	}

	if v, ok := lookup("PDFRENDER_MAX_BODY_BYTES"); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("PDFRENDER_MAX_BODY_BYTES must be a positive integer, got %q", v)
		}
		c.MaxBodyBytes = n
	}

	return c, nil
}

func newLogger(format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func run(cfg config) error {
	logger := newLogger(cfg.LogFormat, os.Stdout)
	slog.SetDefault(logger)
	logger.Info("starting", "version", version, "port", cfg.Port)

	if cfg.NoSandbox {
		logger.Warn("Chromium sandbox is DISABLED (PDFRENDER_NO_SANDBOX). " +
			"This service renders untrusted HTML; prefer running with " +
			"--security-opt seccomp=deploy/chrome-seccomp.json and the sandbox on.")
	}

	pool, err := browser.New(browser.Config{
		ExecPath:       cfg.ExecPath,
		NoSandbox:      cfg.NoSandbox,
		MaxConcurrent:  cfg.MaxConcurrent,
		AcquireTimeout: cfg.AcquireTimeout,
		MaxRenders:     cfg.MaxRenders,
		MaxAge:         cfg.MaxAge,
		Logger:         logger,
	})
	if err != nil {
		return fmt.Errorf("start browser pool: %w", err)
	}
	defer pool.Close()

	router, err := httpapi.NewRouter(httpapi.Config{
		Renderer:      &render.Renderer{ReadyTimeout: cfg.ReadyTimeout},
		Pool:          pool,
		Logger:        logger,
		MaxBodyBytes:  cfg.MaxBodyBytes,
		RenderTimeout: cfg.RenderTimeout,
	})
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// Generous: a large document can legitimately take a while to render.
		WriteTimeout: cfg.RenderTimeout + 30*time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-serveErr:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	// The browser pool is closed by the deferred Close, after Shutdown has
	// drained in-flight renders — those renders still need their tabs.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.RenderTimeout+15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// runHealthcheck is the HEALTHCHECK entrypoint: probe /readyz and exit 0 or 1.
func runHealthcheck(port string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://" + net.JoinHostPort("127.0.0.1", port) + "/readyz"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
