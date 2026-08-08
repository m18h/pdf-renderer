package httpapi

import "testing"

func TestWantsRawPDF(t *testing.T) {
	tests := []struct {
		accept string
		want   bool
	}{
		// Explicit request for PDF.
		{"application/pdf", true},
		{"Application/PDF", true},
		{" application/pdf ", true},
		{"application/pdf;q=1.0", true},
		{"text/html, application/pdf", true},
		{"application/json, application/pdf;q=0.9", true},

		// Everything else must keep the legacy JSON shape. The */* cases are the
		// ones that matter: curl sends Accept: */* by default and browsers send a
		// list ending in */*, so treating those as "wants PDF" would silently
		// break every existing caller.
		{"", false},
		{"*/*", false},
		{"application/*", false},
		{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", false},
		{"application/json", false},
		{"application/pdfx", false},
		{"xapplication/pdf", false},
	}
	for _, tt := range tests {
		if got := wantsRawPDF(tt.accept); got != tt.want {
			t.Errorf("wantsRawPDF(%q) = %v, want %v", tt.accept, got, tt.want)
		}
	}
}
