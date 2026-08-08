package render

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

func mustNormalize(t *testing.T, r *Request) *Normalized {
	t.Helper()
	n, err := Normalize(r)
	if err != nil {
		t.Fatalf("Normalize() error = %v, want nil", err)
	}
	return n
}

// wantInvalid asserts the error is a *Error with KindInvalid, and that its
// message mentions each of the given substrings.
func wantInvalid(t *testing.T, err error, substrings ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("Normalize() error = nil, want KindInvalid")
	}
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("error type = %T, want *render.Error", err)
	}
	if re.Kind != KindInvalid {
		t.Errorf("Kind = %v, want KindInvalid", re.Kind)
	}
	for _, s := range substrings {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("message %q does not contain %q", err.Error(), s)
		}
	}
}

func TestNormalizeDefaults(t *testing.T) {
	n := mustNormalize(t, &Request{HTMLBody: "x"})

	// A4 portrait.
	if got, want := n.PaperWidth, 210.0/25.4; math.Abs(got-want) > 1e-9 {
		t.Errorf("PaperWidth = %v, want %v", got, want)
	}
	if got, want := n.PaperHeight, 297.0/25.4; math.Abs(got-want) > 1e-9 {
		t.Errorf("PaperHeight = %v, want %v", got, want)
	}
	if n.Landscape {
		t.Error("Landscape = true, want false")
	}
	// Margins default to 0mm, matching the historical behaviour of this service.
	for name, got := range map[string]float64{
		"MarginTop": n.MarginTop, "MarginBottom": n.MarginBottom,
		"MarginLeft": n.MarginLeft, "MarginRight": n.MarginRight,
	} {
		if got != 0 {
			t.Errorf("%s = %v, want 0", name, got)
		}
	}
	if n.Scale != 1.0 {
		t.Errorf("Scale = %v, want 1.0", n.Scale)
	}
	if !n.PrintBackground {
		t.Error("PrintBackground = false, want true (wkhtmltopdf painted backgrounds)")
	}
	// screen, not print: wkhtmltopdf's --print-media-type was opt-in.
	if n.MediaType != MediaScreen {
		t.Errorf("MediaType = %q, want %q", n.MediaType, MediaScreen)
	}
	if n.EmulateMetrics {
		t.Error("EmulateMetrics = true, want false at the default 96 dpi")
	}
	if len(n.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", n.Warnings)
	}
}

func TestNormalizePageSizeLookup(t *testing.T) {
	// Case and surrounding whitespace must not matter.
	for _, in := range []string{"a4", "A4", " A4 ", "\tA4\n"} {
		n := mustNormalize(t, &Request{HTMLBody: "x", PageSize: in})
		if got, want := n.PaperWidth, 210.0/25.4; math.Abs(got-want) > 1e-9 {
			t.Errorf("pageSize %q: PaperWidth = %v, want %v", in, got, want)
		}
	}

	_, err := Normalize(&Request{HTMLBody: "x", PageSize: "A4-ish"})
	// The message must name the bad value and list what is supported.
	wantInvalid(t, err, "A4-ish", "a4", "letter", "tabloid")
}

func TestNormalizeOrientation(t *testing.T) {
	for _, in := range []string{"", "portrait", "Portrait", "PORTRAIT"} {
		if n := mustNormalize(t, &Request{HTMLBody: "x", Orientation: in}); n.Landscape {
			t.Errorf("orientation %q: Landscape = true, want false", in)
		}
	}
	for _, in := range []string{"landscape", "Landscape", "LANDSCAPE", " landscape "} {
		if n := mustNormalize(t, &Request{HTMLBody: "x", Orientation: in}); !n.Landscape {
			t.Errorf("orientation %q: Landscape = false, want true", in)
		}
	}
	_, err := Normalize(&Request{HTMLBody: "x", Orientation: "sideways"})
	wantInvalid(t, err, "sideways")
}

func TestNormalizeExplicitDimensionsRequireBoth(t *testing.T) {
	_, err := Normalize(&Request{HTMLBody: "x", PageWidth: f(210)})
	wantInvalid(t, err, "pageHeight")

	_, err = Normalize(&Request{HTMLBody: "x", PageHeight: f(297)})
	wantInvalid(t, err, "pageWidth")

	n := mustNormalize(t, &Request{HTMLBody: "x", PageWidth: f(100), PageHeight: f(200)})
	if got, want := n.PaperWidth, 100.0/25.4; math.Abs(got-want) > 1e-9 {
		t.Errorf("PaperWidth = %v, want %v", got, want)
	}
}

// Explicit dimensions used to silence orientation entirely. They no longer do.
func TestNormalizeOrientationHonouredWithExplicitDimensions(t *testing.T) {
	n := mustNormalize(t, &Request{
		HTMLBody: "x", PageWidth: f(210), PageHeight: f(297), Orientation: "landscape",
	})
	if !n.Landscape {
		t.Error("Landscape = false, want true: orientation must apply to explicit dimensions too")
	}
}

// The regression test that justifies *float64 over uint: an explicit zero and
// an omitted field must be distinguishable.
func TestNormalizeExplicitZeroMarginIsDistinctFromAbsent(t *testing.T) {
	absent := mustNormalize(t, &Request{HTMLBody: "x"})
	explicit := mustNormalize(t, &Request{HTMLBody: "x", MarginTop: f(0)})
	if absent.MarginTop != 0 || explicit.MarginTop != 0 {
		t.Fatalf("both should be 0: absent=%v explicit=%v", absent.MarginTop, explicit.MarginTop)
	}

	// The distinction is observable once the default is non-zero, so assert the
	// plumbing directly: a supplied pointer is read, not ignored.
	n := mustNormalize(t, &Request{HTMLBody: "x", MarginTop: f(10)})
	if got, want := n.MarginTop, 10.0/25.4; math.Abs(got-want) > 1e-9 {
		t.Errorf("MarginTop = %v, want %v", got, want)
	}
	if deref(nil, defaultMarginMM) != defaultMarginMM {
		t.Error("deref(nil) must yield the default")
	}
	if deref(f(0), 10) != 0 {
		t.Error("deref(&0) must yield 0, not the default")
	}
}

func TestNormalizeRejections(t *testing.T) {
	inf := math.Inf(1)
	tests := []struct {
		name string
		req  *Request
		want []string
	}{
		{"empty htmlBody", &Request{HTMLBody: ""}, []string{"htmlBody"}},
		{"whitespace htmlBody", &Request{HTMLBody: "   "}, []string{"htmlBody"}},
		{"negative margin", &Request{HTMLBody: "x", MarginTop: f(-1)}, []string{"marginTop", "negative"}},
		{"infinite margin", &Request{HTMLBody: "x", MarginLeft: f(inf)}, []string{"marginLeft", "finite"}},
		{"margins exceed width", &Request{HTMLBody: "x", MarginLeft: f(120), MarginRight: f(120)}, []string{"marginLeft+marginRight"}},
		{"margins exceed height", &Request{HTMLBody: "x", MarginTop: f(150), MarginBottom: f(150)}, []string{"marginTop+marginBottom"}},
		{"page too small", &Request{HTMLBody: "x", PageWidth: f(0.5), PageHeight: f(100)}, []string{"pageWidth"}},
		{"page too large", &Request{HTMLBody: "x", PageWidth: f(5001), PageHeight: f(100)}, []string{"pageWidth"}},
		{"dpi zero", &Request{HTMLBody: "x", DPI: f(0)}, []string{"dpi"}},
		{"dpi 71", &Request{HTMLBody: "x", DPI: f(71)}, []string{"dpi"}},
		{"dpi 385", &Request{HTMLBody: "x", DPI: f(385)}, []string{"dpi"}},
		{"scale too small", &Request{HTMLBody: "x", Scale: f(0.09)}, []string{"scale"}},
		{"scale too large", &Request{HTMLBody: "x", Scale: f(2.01)}, []string{"scale"}},
		{"bad mediaType", &Request{HTMLBody: "x", MediaType: "braille"}, []string{"braille"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(tt.req)
			wantInvalid(t, err, tt.want...)
		})
	}
}

func TestNormalizeMediaType(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", MediaScreen},
		{"screen", MediaScreen},
		{"SCREEN", MediaScreen},
		{"print", MediaPrint},
		{" Print ", MediaPrint}, // surrounding whitespace must be tolerated
	}
	for _, tt := range tests {
		n := mustNormalize(t, &Request{HTMLBody: "x", MediaType: tt.in})
		if n.MediaType != tt.want {
			t.Errorf("mediaType %q → %q, want %q", tt.in, n.MediaType, tt.want)
		}
	}
}

func TestNormalizePrintBackgroundOverride(t *testing.T) {
	if n := mustNormalize(t, &Request{HTMLBody: "x", PrintBackground: b(false)}); n.PrintBackground {
		t.Error("PrintBackground = true, want false when explicitly disabled")
	}
	if n := mustNormalize(t, &Request{HTMLBody: "x", PrintBackground: b(true)}); !n.PrintBackground {
		t.Error("PrintBackground = false, want true")
	}
}

func TestDeviceScaleFactor(t *testing.T) {
	tests := []struct {
		dpi        *float64
		wantFactor float64
		wantEmul   bool
		wantWarn   bool
	}{
		{nil, 1.0, false, false},
		{f(96), 1.0, false, false},
		{f(192), 2.0, true, true},
		{f(300), 3.125, true, true},
		{f(384), 4.0, true, true},
		{f(72), 0.75, true, true},
	}
	for _, tt := range tests {
		n := mustNormalize(t, &Request{HTMLBody: "x", DPI: tt.dpi})
		if math.Abs(n.DeviceScaleFactor-tt.wantFactor) > 1e-9 {
			t.Errorf("dpi %v: factor = %v, want %v", tt.dpi, n.DeviceScaleFactor, tt.wantFactor)
		}
		if n.EmulateMetrics != tt.wantEmul {
			t.Errorf("dpi %v: EmulateMetrics = %v, want %v", tt.dpi, n.EmulateMetrics, tt.wantEmul)
		}
		if gotWarn := len(n.Warnings) > 0; gotWarn != tt.wantWarn {
			t.Errorf("dpi %v: warning present = %v, want %v", tt.dpi, gotWarn, tt.wantWarn)
		}
	}
}
