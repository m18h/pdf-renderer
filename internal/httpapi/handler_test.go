package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/m18h/pdf-renderer/internal/browser"
	"github.com/m18h/pdf-renderer/internal/render"
)

func init() { gin.SetMode(gin.TestMode) }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubRenderer returns canned output, so handlers are testable without Chromium.
type stubRenderer struct {
	pdf      []byte
	warnings []string
	err      error
	panicMsg string
	calls    int
}

func (s *stubRenderer) Render(_ context.Context, _ *render.Normalized) (*render.Result, error) {
	s.calls++
	if s.panicMsg != "" {
		panic(s.panicMsg)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &render.Result{PDF: s.pdf, Warnings: s.warnings}, nil
}

// stubPool runs fn inline with no browser at all.
type stubPool struct {
	err     error
	healthy bool
}

func (p *stubPool) Do(ctx context.Context, fn func(context.Context) error) error {
	if p.err != nil {
		return p.err
	}
	return fn(ctx)
}

func (p *stubPool) Healthy() bool { return p.healthy }

func newTestServer(t *testing.T, r Renderer, p Pool) *gin.Engine {
	t.Helper()
	if p == nil {
		p = &stubPool{healthy: true}
	}
	router, err := NewRouter(Config{
		Renderer: r,
		Pool:     p,
		Logger:   quietLogger(),
		Probe:    func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	return router
}

func post(t *testing.T, router *gin.Engine, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/render_html", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRenderHTMLRejectsBadInput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"malformed json", `{`, http.StatusBadRequest, "invalid_request"},
		{"missing htmlBody", `{"pageSize":"A4"}`, http.StatusBadRequest, "invalid_request"},
		{"empty htmlBody", `{"htmlBody":""}`, http.StatusBadRequest, "invalid_request"},
		{"unknown pageSize", `{"htmlBody":"x","pageSize":"A4-ish"}`, http.StatusBadRequest, "invalid_request"},
		{"bad orientation", `{"htmlBody":"x","orientation":"sideways"}`, http.StatusBadRequest, "invalid_request"},
		{"dpi out of range", `{"htmlBody":"x","dpi":9000}`, http.StatusBadRequest, "invalid_request"},
		{"lone pageWidth", `{"htmlBody":"x","pageWidth":210}`, http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sr := &stubRenderer{pdf: []byte("%PDF-1.4")}
			rec := post(t, newTestServer(t, sr, nil), tt.body, nil)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if got["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", got["code"], tt.wantCode)
			}
			if sr.calls != 0 {
				t.Errorf("renderer was called %d times, want 0 for a rejected request", sr.calls)
			}
		})
	}
}

// gin never installs a MaxBytesReader of its own, so this proves our middleware.
func TestRenderHTMLOversizeBodyReturns413(t *testing.T) {
	router, err := NewRouter(Config{
		Renderer:     &stubRenderer{pdf: []byte("%PDF-1.4")},
		Pool:         &stubPool{healthy: true},
		Logger:       quietLogger(),
		MaxBodyBytes: 1024,
		Probe:        func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	big := `{"htmlBody":"` + strings.Repeat("a", 4096) + `"}`
	rec := post(t, router, big, nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != "payload_too_large" {
		t.Errorf("code = %v, want %q", got["code"], "payload_too_large")
	}
}

func TestRenderHTMLErrorStatusMapping(t *testing.T) {
	tests := []struct {
		name        string
		renderErr   error
		poolErr     error
		wantStatus  int
		wantCode    string
		wantRetryAf bool
	}{
		{
			name:       "render timeout",
			renderErr:  &render.Error{Kind: render.KindTimeout, Msg: "render timed out"},
			wantStatus: http.StatusGatewayTimeout, wantCode: "render_timeout",
		},
		{
			name:       "deadline exceeded",
			renderErr:  context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout, wantCode: "render_timeout",
		},
		{
			name:       "browser unavailable",
			poolErr:    browser.ErrUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantCode: "browser_unavailable", wantRetryAf: true,
		},
		{
			name:       "pool busy",
			poolErr:    browser.ErrBusy,
			wantStatus: http.StatusServiceUnavailable, wantCode: "browser_unavailable", wantRetryAf: true,
		},
		{
			name:       "pool closed",
			poolErr:    browser.ErrClosed,
			wantStatus: http.StatusServiceUnavailable, wantCode: "browser_unavailable", wantRetryAf: true,
		},
		{
			name:       "internal",
			renderErr:  &render.Error{Kind: render.KindInternal, Msg: "boom"},
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
		{
			name:       "unclassified error",
			renderErr:  errors.New("something odd"),
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newTestServer(t,
				&stubRenderer{err: tt.renderErr},
				&stubPool{err: tt.poolErr, healthy: true})
			rec := post(t, router, `{"htmlBody":"<p>x</p>"}`, nil)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if got["code"] != tt.wantCode {
				t.Errorf("code = %v, want %q", got["code"], tt.wantCode)
			}
			if gotRA := rec.Header().Get("Retry-After") != ""; gotRA != tt.wantRetryAf {
				t.Errorf("Retry-After present = %v, want %v", gotRA, tt.wantRetryAf)
			}
		})
	}
}

// Internal error text must never reach the client.
func TestInternalErrorDoesNotLeakDetail(t *testing.T) {
	const secret = "postgres://user:hunter2@db.internal:5432"
	router := newTestServer(t,
		&stubRenderer{err: &render.Error{Kind: render.KindInternal, Msg: secret}}, nil)

	rec := post(t, router, `{"htmlBody":"x"}`, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), secret) {
		t.Errorf("response leaked internal detail: %s", rec.Body)
	}
}

func TestRenderHTMLLegacyJSONResponse(t *testing.T) {
	pdf := []byte("%PDF-1.4\nfake\n%%EOF")
	router := newTestServer(t, &stubRenderer{pdf: pdf}, nil)

	rec := post(t, router, `{"htmlBody":"<p>x</p>"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got struct {
		Data     string   `json:"data"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Data)
	if err != nil {
		t.Fatalf("data is not standard base64: %v", err)
	}
	if !bytes.Equal(decoded, pdf) {
		t.Errorf("decoded data = %q, want %q", decoded, pdf)
	}
}

// The frozen wire format. Go marshals []byte as base64, which is what this
// endpoint has always returned regardless of what the old README claimed.
func TestRenderHTMLLegacyResponseIsByteIdentical(t *testing.T) {
	router := newTestServer(t, &stubRenderer{pdf: []byte("%PDF-1.4")}, nil)
	rec := post(t, router, `{"htmlBody":"<p>x</p>"}`, nil)

	const want = `{"data":"JVBERi0xLjQ="}`
	if got := rec.Body.String(); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestRenderHTMLWarningsIncludedWhenPresent(t *testing.T) {
	router := newTestServer(t, &stubRenderer{
		pdf:      []byte("%PDF-1.4"),
		warnings: []string{"subresource failed to load: net::ERR_CONNECTION_REFUSED"},
	}, nil)
	rec := post(t, router, `{"htmlBody":"x"}`, nil)

	var got struct {
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", got.Warnings)
	}
}

func TestRenderHTMLAcceptPDFReturnsRawBytes(t *testing.T) {
	pdf := []byte("%PDF-1.4\nfake\n%%EOF")
	router := newTestServer(t, &stubRenderer{pdf: pdf}, nil)

	rec := post(t, router, `{"htmlBody":"x"}`, map[string]string{"Accept": MIMEPDF})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, MIMEPDF) {
		t.Errorf("Content-Type = %q, want %q", ct, MIMEPDF)
	}
	if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(pdf)) {
		t.Errorf("Content-Length = %q, want %q", cl, strconv.Itoa(len(pdf)))
	}
	if !bytes.Equal(rec.Body.Bytes(), pdf) {
		t.Errorf("body = %q, want the raw PDF bytes", rec.Body.Bytes())
	}
}

// The regression guard for existing clients: curl's default Accept must not
// silently switch them to a binary response.
func TestRenderHTMLWildcardAcceptStaysJSON(t *testing.T) {
	router := newTestServer(t, &stubRenderer{pdf: []byte("%PDF-1.4")}, nil)

	for _, accept := range []string{"*/*", "application/*", "text/html,*/*;q=0.8"} {
		rec := post(t, router, `{"htmlBody":"x"}`, map[string]string{"Accept": accept})
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Accept %q: Content-Type = %q, want application/json", accept, ct)
		}
	}
}

func TestPanicInRendererBecomes500(t *testing.T) {
	router := newTestServer(t, &stubRenderer{panicMsg: "unexpected nil"}, nil)
	rec := post(t, router, `{"htmlBody":"x"}`, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestHealthzIsLivenessOnly(t *testing.T) {
	// Unhealthy browser, but liveness must still report ok.
	router := newTestServer(t, &stubRenderer{}, &stubPool{err: browser.ErrUnavailable})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzReflectsBrowserState(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		router := newTestServer(t, &stubRenderer{}, &stubPool{healthy: true})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		router := newTestServer(t, &stubRenderer{}, &stubPool{err: browser.ErrUnavailable})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})

	// A probe that fails must fail readiness even when bookkeeping says healthy.
	t.Run("probe failure", func(t *testing.T) {
		router, err := NewRouter(Config{
			Renderer: &stubRenderer{},
			Pool:     &stubPool{healthy: true},
			Logger:   quietLogger(),
			Probe:    func(context.Context) error { return errors.New("browser wedged") },
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})
}

func TestRootEndpoint(t *testing.T) {
	router := newTestServer(t, &stubRenderer{}, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["message"] != "PDF Renderer" {
		t.Errorf("message = %q, want %q", got["message"], "PDF Renderer")
	}
}
