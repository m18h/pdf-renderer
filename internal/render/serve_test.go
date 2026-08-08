package render

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestServeHTMLServesExactBytes(t *testing.T) {
	body := []byte("<html><body>héllo 漢字</body></html>")
	o, err := serveHTML(context.Background(), body)
	if err != nil {
		t.Fatalf("serveHTML() error = %v", err)
	}
	defer o.Close()

	resp, err := http.Get(o.URL) //nolint:noctx // test-local loopback fetch
	if err != nil {
		t.Fatalf("GET %s: %v", o.URL, err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}

	// The explicit charset is what keeps non-ASCII text from becoming mojibake.
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %q", cl, strconv.Itoa(len(body)))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-store")
	}
}

func TestServeHTMLBindsLoopbackOnly(t *testing.T) {
	o, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("serveHTML() error = %v", err)
	}
	defer o.Close()

	if !strings.HasPrefix(o.URL, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want a http://127.0.0.1: prefix", o.URL)
	}
}

func TestServeHTMLOtherPathsAre404(t *testing.T) {
	o, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("serveHTML() error = %v", err)
	}
	defer o.Close()

	base := o.URL[:strings.LastIndex(o.URL, "/")]
	for _, p := range []string{"/", "/index.html", "/../etc/passwd", "/deadbeef"} {
		resp, err := http.Get(base + p) //nolint:noctx // test-local loopback fetch
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestServeHTMLNonceIsUnguessable(t *testing.T) {
	a, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	pathA := a.URL[strings.LastIndex(a.URL, "/"):]
	pathB := b.URL[strings.LastIndex(b.URL, "/"):]
	if pathA == pathB {
		t.Error("two origins produced the same path; the nonce is not random")
	}
	// 16 random bytes hex-encoded, plus the leading slash.
	if len(pathA) != 33 {
		t.Errorf("path length = %d, want 33 (16 hex-encoded bytes + \"/\")", len(pathA))
	}
}

// A document may re-fetch its own URL, so the origin must survive the first hit
// and count every one.
func TestServeHTMLCountsHitsAndSurvivesRepeats(t *testing.T) {
	o, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	if got := o.Hits(); got != 0 {
		t.Errorf("Hits() before any request = %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		resp, err := http.Get(o.URL) //nolint:noctx // test-local loopback fetch
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := int(o.Hits()); got != i {
			t.Errorf("after %d requests Hits() = %d, want %d", i, got, i)
		}
	}
}

func TestServeHTMLCloseStopsServing(t *testing.T) {
	o, err := serveHTML(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	url := o.URL
	o.Close()

	if resp, err := http.Get(url); err == nil { //nolint:noctx // test-local loopback fetch
		resp.Body.Close()
		t.Error("GET succeeded after Close(), want a connection error")
	}
}
