package httpapi

import (
	"strings"
)

// MIMEPDF is the content type for a raw PDF response.
const MIMEPDF = "application/pdf"

// wantsRawPDF reports whether the caller explicitly asked for application/pdf.
//
// The bar is deliberately "explicitly": the legacy response is
// {"data":"<base64>"} and every existing client depends on it. curl sends
// Accept: */* by default, and browsers send a long list ending in */*, so
// anything short of naming application/pdf must keep returning JSON.
func wantsRawPDF(accept string) bool {
	if accept == "" {
		return false
	}
	for _, part := range strings.Split(accept, ",") {
		// Drop any parameters (";q=0.9").
		mediaType := part
		if i := strings.IndexByte(part, ';'); i >= 0 {
			mediaType = part[:i]
		}
		if strings.EqualFold(strings.TrimSpace(mediaType), MIMEPDF) {
			return true
		}
	}
	return false
}
