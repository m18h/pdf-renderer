package render

import (
	"math"
	"sort"
	"strings"
)

// Unit constants. Chromium's printToPDF takes every dimension in INCHES; the
// public API speaks millimetres, as wkhtmltopdf did.
const (
	mmPerInch  = 25.4
	cssPxPerIn = 96.0 // the CSS reference pixel, fixed in Chromium's units.h

	// defaultMarginMM matches the historical behaviour of this service: the old
	// code called MarginTop.Set(data.MarginTop) unconditionally on a zero-valued
	// uint, so every margin has always been 0.
	defaultMarginMM = 0.0

	defaultScale = 1.0
	minScale     = 0.1 // Chromium rejects outside [0.1, 2.0]
	maxScale     = 2.0

	defaultDPI = 96.0
	minDPI     = 72.0
	// maxDPI caps deviceScaleFactor at 4.0. Higher factors multiply the raster
	// backing store per tab and are an easy OOM.
	maxDPI = 384.0

	minPageMM = 1.0
	maxPageMM = 5000.0

	// MediaScreen preserves wkhtmltopdf behaviour: it defaulted to screen media
	// (--print-media-type was opt-in and this service never set it), whereas
	// Chromium's printToPDF renders with print media.
	MediaScreen = "screen"
	MediaPrint  = "print"
)

// Request is the JSON body of POST /api/render_html.
//
// Numeric fields are pointers on purpose: a plain uint cannot distinguish an
// omitted field from an explicit 0, which makes non-zero defaults and explicit
// zero margins mutually exclusive. float64 additionally allows sub-millimetre
// values such as Letter's 215.9mm, and integer JSON still unmarshals cleanly,
// so existing payloads keep working.
type Request struct {
	HTMLBody     string   `json:"htmlBody" binding:"required"`
	PageSize     string   `json:"pageSize"`
	PageWidth    *float64 `json:"pageWidth"`  // mm
	PageHeight   *float64 `json:"pageHeight"` // mm
	Orientation  string   `json:"orientation"`
	DPI          *float64 `json:"dpi"`
	MarginTop    *float64 `json:"marginTop"`    // mm
	MarginBottom *float64 `json:"marginBottom"` // mm
	MarginLeft   *float64 `json:"marginLeft"`   // mm
	MarginRight  *float64 `json:"marginRight"`  // mm

	// Added in v2.
	MediaType       string   `json:"mediaType"`       // "screen" (default) | "print"
	PrintBackground *bool    `json:"printBackground"` // default true
	Scale           *float64 `json:"scale"`           // default 1.0
	PageRanges      string   `json:"pageRanges"`      // e.g. "1-3,5"
}

// Normalized is a validated Request resolved into the units Chromium wants.
type Normalized struct {
	HTML []byte

	// Paper dimensions in inches, always portrait-oriented. These are NOT
	// swapped for landscape: Chromium's SetOrientation() does that internally,
	// and swapping here too would cancel it out.
	PaperWidth, PaperHeight float64
	Landscape               bool

	MarginTop, MarginBottom, MarginLeft, MarginRight float64 // inches

	Scale           float64
	PrintBackground bool
	PageRanges      string
	MediaType       string

	// Emulation. Unlike PaperWidth, the viewport DOES swap for landscape.
	ViewportW, ViewportH int64   // CSS px
	DeviceScaleFactor    float64 // dpi / 96
	EmulateMetrics       bool    // false when the factor is 1.0; skip the CDP call

	// Warnings are advisory notes returned to the caller.
	Warnings []string
}

type sizeMM struct{ W, H float64 }

// pageSizes is our own table because there is no named-size parameter in the
// DevTools protocol — Puppeteer's format:"A4" is a client-side lookup. The ISO
// B/C and Qt-specific names are carried over so callers who passed them to
// wkhtmltopdf keep working.
var pageSizes = map[string]sizeMM{
	"a0": {841, 1189}, "a1": {594, 841}, "a2": {420, 594}, "a3": {297, 420},
	"a4": {210, 297}, "a5": {148, 210}, "a6": {105, 148},
	"b4": {250, 353}, "b5": {176, 250},
	"c5e": {163, 229}, "dle": {110, 220}, "comm10e": {105, 241},
	"letter": {215.9, 279.4}, "legal": {215.9, 355.6},
	"tabloid": {279.4, 431.8}, "ledger": {431.8, 279.4},
	"executive": {184.15, 266.7}, "folio": {210, 330},
}

func supportedPageSizes() string {
	names := make([]string, 0, len(pageSizes))
	for n := range pageSizes {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// pointsPerInch is the PDF user-space unit. Chromium converts our inches to
// points and ceils to a whole number (gfx::ToCeiledSize in pdf_print_utils.cc),
// which makes float noise dangerous: 355.6/25.4 evaluates to 14.000000000000002,
// so Legal would come out 1009pt tall instead of 1008.
const pointsPerInch = 72.0

// mmToIn converts millimetres to inches, snapping to an exact point boundary
// when the result is within float-division noise of one. Without the snap, sizes
// that are whole inches by definition (Letter, Legal, Tabloid) pick up a
// spurious extra point in the PDF MediaBox.
func mmToIn(mm float64) float64 {
	in := mm / mmPerInch
	pts := in * pointsPerInch
	if r := math.Round(pts); math.Abs(pts-r) < 1e-9 {
		return r / pointsPerInch
	}
	return in
}

func deref(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Normalize validates a Request and resolves it into Chromium's units.
func Normalize(r *Request) (*Normalized, error) {
	if strings.TrimSpace(r.HTMLBody) == "" {
		return nil, invalidf("htmlBody is required")
	}

	n := &Normalized{
		HTML:            []byte(r.HTMLBody),
		PrintBackground: true,
		PageRanges:      strings.TrimSpace(r.PageRanges),
	}

	if err := resolveOrientation(r, n); err != nil {
		return nil, err
	}
	if err := resolvePaper(r, n); err != nil {
		return nil, err
	}
	if err := resolveMargins(r, n); err != nil {
		return nil, err
	}
	if err := resolveScale(r, n); err != nil {
		return nil, err
	}
	if err := resolveMediaType(r, n); err != nil {
		return nil, err
	}
	if r.PrintBackground != nil {
		n.PrintBackground = *r.PrintBackground
	}
	if err := resolveDPI(r, n); err != nil {
		return nil, err
	}

	n.computeViewport()
	return n, nil
}

func resolveOrientation(r *Request, n *Normalized) error {
	switch strings.ToLower(strings.TrimSpace(r.Orientation)) {
	case "", "portrait":
		n.Landscape = false
	case "landscape":
		n.Landscape = true
	default:
		return invalidf("unsupported orientation %q; want \"Portrait\" or \"Landscape\"", r.Orientation)
	}
	return nil
}

func resolvePaper(r *Request, n *Normalized) error {
	// Explicit dimensions require both. The old code silently ignored a lone
	// pageWidth, which made a typo look like it worked.
	switch {
	case r.PageWidth != nil && r.PageHeight == nil:
		return invalidf("pageWidth requires pageHeight")
	case r.PageHeight != nil && r.PageWidth == nil:
		return invalidf("pageHeight requires pageWidth")
	case r.PageWidth != nil && r.PageHeight != nil:
		w, h := *r.PageWidth, *r.PageHeight
		for name, v := range map[string]float64{"pageWidth": w, "pageHeight": h} {
			if !finite(v) {
				return invalidf("%s must be a finite number", name)
			}
			if v < minPageMM || v > maxPageMM {
				return invalidf("%s must be between %gmm and %gmm, got %gmm", name, minPageMM, maxPageMM, v)
			}
		}
		n.PaperWidth, n.PaperHeight = mmToIn(w), mmToIn(h)
		return nil
	}

	name := strings.ToLower(strings.TrimSpace(r.PageSize))
	if name == "" {
		name = "a4"
	}
	size, ok := pageSizes[name]
	if !ok {
		return invalidf("unsupported pageSize %q; supported: %s", r.PageSize, supportedPageSizes())
	}
	n.PaperWidth, n.PaperHeight = mmToIn(size.W), mmToIn(size.H)
	return nil
}

func resolveMargins(r *Request, n *Normalized) error {
	margins := []struct {
		name string
		val  *float64
		dst  *float64
	}{
		{"marginTop", r.MarginTop, &n.MarginTop},
		{"marginBottom", r.MarginBottom, &n.MarginBottom},
		{"marginLeft", r.MarginLeft, &n.MarginLeft},
		{"marginRight", r.MarginRight, &n.MarginRight},
	}
	for _, m := range margins {
		mm := deref(m.val, defaultMarginMM)
		if !finite(mm) {
			return invalidf("%s must be a finite number", m.name)
		}
		if mm < 0 {
			return invalidf("%s must not be negative, got %gmm", m.name, mm)
		}
		*m.dst = mmToIn(mm)
	}

	// Catch this ourselves: Chromium's own error for it is an opaque
	// "Invalid print parameters".
	if n.MarginLeft+n.MarginRight >= n.PaperWidth {
		return invalidf("marginLeft+marginRight (%.1fmm) must be less than the page width (%.1fmm)",
			(n.MarginLeft+n.MarginRight)*mmPerInch, n.PaperWidth*mmPerInch)
	}
	if n.MarginTop+n.MarginBottom >= n.PaperHeight {
		return invalidf("marginTop+marginBottom (%.1fmm) must be less than the page height (%.1fmm)",
			(n.MarginTop+n.MarginBottom)*mmPerInch, n.PaperHeight*mmPerInch)
	}
	return nil
}

func resolveScale(r *Request, n *Normalized) error {
	s := deref(r.Scale, defaultScale)
	if !finite(s) {
		return invalidf("scale must be a finite number")
	}
	// Reject rather than clamp, so callers find out.
	if s < minScale || s > maxScale {
		return invalidf("scale must be between %g and %g, got %g", minScale, maxScale, s)
	}
	n.Scale = s
	return nil
}

func resolveMediaType(r *Request, n *Normalized) error {
	switch strings.ToLower(strings.TrimSpace(r.MediaType)) {
	case "":
		n.MediaType = MediaScreen
	case MediaScreen:
		n.MediaType = MediaScreen
	case MediaPrint:
		n.MediaType = MediaPrint
	default:
		return invalidf("unsupported mediaType %q; want %q or %q", r.MediaType, MediaScreen, MediaPrint)
	}
	return nil
}

func resolveDPI(r *Request, n *Normalized) error {
	dpi := deref(r.DPI, defaultDPI)
	if !finite(dpi) {
		return invalidf("dpi must be a finite number")
	}
	if dpi < minDPI || dpi > maxDPI {
		return invalidf("dpi must be between %g and %g, got %g", minDPI, maxDPI, dpi)
	}
	n.DeviceScaleFactor = dpi / cssPxPerIn
	n.EmulateMetrics = n.DeviceScaleFactor != 1.0

	if dpi != defaultDPI {
		n.Warnings = append(n.Warnings,
			"dpi affects raster assets only (srcset, image-set(), canvas); "+
				"text and vector output are resolution-independent")
	}
	return nil
}

// computeViewport derives the emulated viewport from the printable area.
//
// Emulation.setDeviceMetricsOverride treats a zero width or height as "disable
// the override", so a deviceScaleFactor cannot be set without also supplying a
// viewport. This is the one place the landscape swap applies.
func (n *Normalized) computeViewport() {
	w, h := n.PaperWidth, n.PaperHeight
	if n.Landscape {
		w, h = h, w
	}
	n.ViewportW = int64(math.Round((w - n.MarginLeft - n.MarginRight) * cssPxPerIn))
	n.ViewportH = int64(math.Round((h - n.MarginTop - n.MarginBottom) * cssPxPerIn))
}
