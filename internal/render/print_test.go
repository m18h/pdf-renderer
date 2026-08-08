package render

import (
	"math"
	"testing"
)

func closeTo(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %.17g, want %.17g", label, got, want)
	}
}

func exactly(t *testing.T, label string, got, want float64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %.17g, want exactly %.17g", label, got, want)
	}
}

func TestPrintParamsA4Portrait(t *testing.T) {
	n := mustNormalize(t, &Request{HTMLBody: "x", PageSize: "A4"})
	p := n.printParams()

	closeTo(t, "PaperWidth", p.PaperWidth, 8.267716535433071)
	closeTo(t, "PaperHeight", p.PaperHeight, 11.692913385826772)
	exactly(t, "MarginTop", p.MarginTop, 0)
	exactly(t, "MarginBottom", p.MarginBottom, 0)
	exactly(t, "MarginLeft", p.MarginLeft, 0)
	exactly(t, "MarginRight", p.MarginRight, 0)

	if p.Landscape {
		t.Error("Landscape = true, want false")
	}
	if !p.PrintBackground {
		t.Error("PrintBackground = false, want true")
	}
	if p.PreferCSSPageSize {
		t.Error("PreferCSSPageSize = true, want false: our paper size must win")
	}
	if p.DisplayHeaderFooter {
		t.Error("DisplayHeaderFooter = true, want false")
	}
	exactly(t, "Scale", p.Scale, 1.0)
}

// The North American sizes are defined in whole inches, so these must be
// bit-exact — a rounding slip here is the kind a human spots in the output.
func TestPrintParamsExactInchSizes(t *testing.T) {
	tests := []struct {
		size string
		w, h float64
	}{
		{"Letter", 8.5, 11},
		{"Legal", 8.5, 14},
		{"Tabloid", 11, 17},
		{"Ledger", 17, 11},
	}
	for _, tt := range tests {
		t.Run(tt.size, func(t *testing.T) {
			n := mustNormalize(t, &Request{HTMLBody: "x", PageSize: tt.size})
			p := n.printParams()
			exactly(t, "PaperWidth", p.PaperWidth, tt.w)
			exactly(t, "PaperHeight", p.PaperHeight, tt.h)
		})
	}
}

// Chromium converts inches to points and ceils to a whole number, so any float
// noise in our conversion becomes a visible extra point in the PDF MediaBox.
// This asserts the point values Chromium will actually derive.
func TestPrintParamsCeiledPointsMatchExpectedMediaBox(t *testing.T) {
	tests := []struct {
		size         string
		wantW, wantH int
	}{
		// ISO sizes are not whole points, so they legitimately gain the ceil.
		{"A4", 596, 842}, // 595.2756 x 841.8898
		{"A3", 842, 1191},
		{"A5", 420, 596},
		// These are whole inches by definition and must NOT gain a point.
		{"Letter", 612, 792},
		{"Legal", 612, 1008},
		{"Tabloid", 792, 1224},
	}
	for _, tt := range tests {
		t.Run(tt.size, func(t *testing.T) {
			p := mustNormalize(t, &Request{HTMLBody: "x", PageSize: tt.size}).printParams()
			gotW := int(math.Ceil(p.PaperWidth * 72))
			gotH := int(math.Ceil(p.PaperHeight * 72))
			if gotW != tt.wantW || gotH != tt.wantH {
				t.Errorf("ceiled MediaBox = %dx%d pt, want %dx%d pt", gotW, gotH, tt.wantW, tt.wantH)
			}
		})
	}
}

// Chromium's SetOrientation() swaps width and height internally. If we swapped
// here too the two would cancel out and landscape would silently do nothing.
func TestPrintParamsLandscapeDoesNotSwapPaper(t *testing.T) {
	n := mustNormalize(t, &Request{HTMLBody: "x", PageSize: "A4", Orientation: "Landscape"})
	p := n.printParams()

	if !p.Landscape {
		t.Fatal("Landscape = false, want true")
	}
	closeTo(t, "PaperWidth", p.PaperWidth, 8.267716535433071)
	closeTo(t, "PaperHeight", p.PaperHeight, 11.692913385826772)
}

func TestPrintParamsMarginConversion(t *testing.T) {
	n := mustNormalize(t, &Request{
		HTMLBody:  "x",
		MarginTop: f(10), MarginBottom: f(20), MarginLeft: f(5), MarginRight: f(2.5),
	})
	p := n.printParams()
	closeTo(t, "MarginTop", p.MarginTop, 10.0/25.4)
	closeTo(t, "MarginBottom", p.MarginBottom, 20.0/25.4)
	closeTo(t, "MarginLeft", p.MarginLeft, 5.0/25.4)
	closeTo(t, "MarginRight", p.MarginRight, 2.5/25.4)
}

func TestPrintParamsPageRanges(t *testing.T) {
	n := mustNormalize(t, &Request{HTMLBody: "x", PageRanges: " 1-3,5 "})
	if got := n.printParams().PageRanges; got != "1-3,5" {
		t.Errorf("PageRanges = %q, want %q", got, "1-3,5")
	}
}

// The viewport DOES swap for landscape, unlike the paper dimensions. Getting
// exactly one of the two right is the easy mistake.
func TestViewport(t *testing.T) {
	tests := []struct {
		name         string
		req          *Request
		wantW, wantH int64
	}{
		// A4 at the 0mm default: 210/25.4*96 = 793.70 → 794, 297/25.4*96 = 1122.52 → 1123.
		{"A4 portrait", &Request{HTMLBody: "x", PageSize: "A4"}, 794, 1123},
		{"A4 landscape", &Request{HTMLBody: "x", PageSize: "A4", Orientation: "Landscape"}, 1123, 794},
		{"Letter portrait", &Request{HTMLBody: "x", PageSize: "Letter"}, 816, 1056},
		{"Letter landscape", &Request{HTMLBody: "x", PageSize: "Letter", Orientation: "Landscape"}, 1056, 816},
		// 10mm margins all round: (210-20)/25.4*96 = 718.11 → 718.
		{"A4 portrait 10mm margins", &Request{
			HTMLBody: "x", PageSize: "A4",
			MarginTop: f(10), MarginBottom: f(10), MarginLeft: f(10), MarginRight: f(10),
		}, 718, 1047},
		{"A4 landscape 10mm margins", &Request{
			HTMLBody: "x", PageSize: "A4", Orientation: "Landscape",
			MarginTop: f(10), MarginBottom: f(10), MarginLeft: f(10), MarginRight: f(10),
		}, 1047, 718},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := mustNormalize(t, tt.req)
			if n.ViewportW != tt.wantW || n.ViewportH != tt.wantH {
				t.Errorf("viewport = %dx%d, want %dx%d", n.ViewportW, n.ViewportH, tt.wantW, tt.wantH)
			}
		})
	}
}
