// Package httpapi exposes the render service over HTTP.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/m18h/pdf-renderer/internal/browser"
	"github.com/m18h/pdf-renderer/internal/render"
)

// DefaultMaxBodyBytes is the request size limit. gin never installs a
// MaxBytesReader of its own (MaxMultipartMemory is multipart-only), so we own
// this and the 413 that comes with it.
const DefaultMaxBodyBytes int64 = 10 << 20 // 10 MiB

// DefaultRenderTimeout bounds a single render end to end.
const DefaultRenderTimeout = 60 * time.Second

// Renderer produces a PDF on a browser tab. The concrete implementation is
// render.Renderer; the interface exists so handlers can be tested without a
// browser.
type Renderer interface {
	Render(tabCtx context.Context, n *render.Normalized) (*render.Result, error)
}

// Pool hands out browser tabs.
type Pool interface {
	Do(ctx context.Context, fn func(tabCtx context.Context) error) error
	Healthy() bool
}

// ProbeFunc verifies a tab is responsive. Defaults to render.Probe; overridden
// in tests so readiness can be checked without a browser.
type ProbeFunc func(tabCtx context.Context) error

// Config configures the HTTP layer.
type Config struct {
	Renderer      Renderer
	Pool          Pool
	Logger        *slog.Logger
	MaxBodyBytes  int64
	RenderTimeout time.Duration
	// Probe verifies a tab responds. Nil means render.Probe.
	Probe ProbeFunc
}

func (c *Config) withDefaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Probe == nil {
		c.Probe = render.Probe
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.RenderTimeout <= 0 {
		c.RenderTimeout = DefaultRenderTimeout
	}
}

type server struct {
	cfg Config
}

// NewRouter builds the gin engine.
func NewRouter(cfg Config) (*gin.Engine, error) {
	cfg.withDefaults()
	s := &server{cfg: cfg}

	r := gin.New()
	r.Use(requestLogger(cfg.Logger), gin.Recovery())

	// This service never reads ClientIP, so trust no forwarding headers.
	if err := r.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "PDF Renderer"})
	})
	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)
	r.POST("/api/render_html", s.renderHTML)

	return r, nil
}

func requestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logger.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"duration", time.Since(start).String(),
		)
	}
}

// healthz is liveness only: the process is up and serving.
func (s *server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz exercises the browser rather than trusting bookkeeping, so an
// orchestrator restarts us if self-healing has failed.
func (s *server) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	err := s.cfg.Pool.Do(ctx, s.cfg.Probe)
	if err != nil {
		s.cfg.Logger.Warn("readiness probe failed", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

type renderResponse struct {
	Data     []byte   `json:"data"`
	Warnings []string `json:"warnings,omitempty"`
}

func (s *server) renderHTML(c *gin.Context) {
	// Own the body limit so oversize payloads become a clean 413.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxBodyBytes)

	var req render.Request
	// ShouldBindJSON, not BindJSON: MustBindWith aborts and writes a status
	// itself, which would make our own response a second write.
	if err := c.ShouldBindJSON(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "request body exceeds " + strconv.FormatInt(s.cfg.MaxBodyBytes, 10) + " bytes",
				"code":  render.KindTooLarge.String(),
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": render.KindInvalid.String()})
		return
	}

	normalized, err := render.Normalize(&req)
	if err != nil {
		s.writeError(c, err)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), s.cfg.RenderTimeout)
	defer cancel()

	var result *render.Result
	err = s.cfg.Pool.Do(ctx, func(tabCtx context.Context) error {
		var rerr error
		result, rerr = s.cfg.Renderer.Render(tabCtx, normalized)
		return rerr
	})
	if err != nil {
		s.writeError(c, err)
		return
	}

	if wantsRawPDF(c.GetHeader("Accept")) {
		c.Header("Content-Length", strconv.Itoa(len(result.PDF)))
		if len(result.Warnings) > 0 {
			c.Header("X-Render-Warnings", strconv.Itoa(len(result.Warnings)))
		}
		c.Data(http.StatusOK, MIMEPDF, result.PDF)
		return
	}

	// Legacy shape. Go marshals []byte as a base64 string, which is what this
	// endpoint has always actually returned.
	c.JSON(http.StatusOK, renderResponse{Data: result.PDF, Warnings: result.Warnings})
}

// writeError maps a failure onto a status code, and keeps internal error text
// out of the response body.
func (s *server) writeError(c *gin.Context, err error) {
	status, kind := classify(err)

	if kind == render.KindInternal {
		s.cfg.Logger.Error("render failed", "error", err)
		c.JSON(status, gin.H{"error": "internal error", "code": kind.String()})
		return
	}

	if kind == render.KindUnavailable {
		c.Header("Retry-After", "5")
	}
	s.cfg.Logger.Warn("request rejected", "code", kind.String(), "error", err)
	c.JSON(status, gin.H{"error": err.Error(), "code": kind.String()})
}

func classify(err error) (int, render.Kind) {
	switch {
	case errors.Is(err, browser.ErrBusy), errors.Is(err, browser.ErrUnavailable), errors.Is(err, browser.ErrClosed):
		return http.StatusServiceUnavailable, render.KindUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, render.KindTimeout
	}

	var re *render.Error
	if errors.As(err, &re) {
		switch re.Kind {
		case render.KindInvalid:
			return http.StatusBadRequest, render.KindInvalid
		case render.KindTooLarge:
			return http.StatusRequestEntityTooLarge, render.KindTooLarge
		case render.KindTimeout:
			return http.StatusGatewayTimeout, render.KindTimeout
		case render.KindUnavailable:
			return http.StatusServiceUnavailable, render.KindUnavailable
		case render.KindInternal:
			return http.StatusInternalServerError, render.KindInternal
		}
	}
	return http.StatusInternalServerError, render.KindInternal
}
