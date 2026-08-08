package render

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// htmlOrigin serves one HTML document from a loopback listener on an
// unguessable path.
//
// Navigating to a real http:// origin rather than a data: URL matters for three
// reasons: data: URLs are capped at 2 MiB by Chromium's kMaxURLChars (and
// percent-escaping inflates HTML well past that), a data: URL is an opaque
// origin so every relative subresource URL in the document breaks, and
// Page.setDocumentContent leaves the document URL as about:blank.
type htmlOrigin struct {
	URL string

	srv  *http.Server
	ln   net.Listener
	hits atomic.Int64
}

func serveHTML(ctx context.Context, body []byte) (*htmlOrigin, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, internalf(err, "generate nonce")
	}
	path := "/" + hex.EncodeToString(nonce)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, internalf(err, "listen on loopback")
	}

	o := &htmlOrigin{
		URL: "http://" + ln.Addr().String() + path,
		ln:  ln,
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		// Set the charset explicitly. Without it Chromium sniffs the encoding,
		// and any non-ASCII text in a document with no <meta charset> renders as
		// mojibake — a classic wkhtmltopdf-migration bug report.
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		o.hits.Add(1)
		_, _ = w.Write(body)
	})

	o.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = o.srv.Serve(ln) }()

	return o, nil
}

// Hits reports how many times the document was fetched. Zero hits at print time
// means Chromium never loaded our HTML — a far better diagnostic than an
// inexplicably empty PDF.
func (o *htmlOrigin) Hits() int64 { return o.hits.Load() }

// Close tears the listener down immediately, in-flight requests included.
//
// Deliberately not called right after the first hit: a document can re-fetch its
// own URL via <iframe src=""> or a script-triggered reload, and a 404 there
// would silently produce a blank page.
func (o *htmlOrigin) Close() {
	_ = o.srv.Close()
}
