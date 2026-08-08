package render

import (
	"github.com/chromedp/cdproto/page"
)

// printParams builds the Page.printToPDF request.
//
// The complete protocol 1.3 parameter set is: landscape, displayHeaderFooter,
// printBackground, scale, paperWidth/Height, margin{Top,Bottom,Left,Right},
// pageRanges, header/footerTemplate, preferCSSPageSize, transferMode,
// generateTaggedPDF, generateDocumentOutline. There is nothing else, and in
// particular there is no DPI parameter — Chromium hardcodes print DPI to 72
// (kPointsPerInch) in pdf_print_utils.cc.
func (n *Normalized) printParams() *page.PrintToPDFParams {
	return page.PrintToPDF().
		WithLandscape(n.Landscape).
		// Chromium defaults printBackground to false, which silently drops every
		// background-color and background-image. wkhtmltopdf painted them, so
		// defaulting this to true is what keeps existing output recognisable.
		WithPrintBackground(n.PrintBackground).
		WithScale(n.Scale).
		WithPaperWidth(n.PaperWidth).
		WithPaperHeight(n.PaperHeight).
		WithMarginTop(n.MarginTop).
		WithMarginBottom(n.MarginBottom).
		WithMarginLeft(n.MarginLeft).
		WithMarginRight(n.MarginRight).
		WithPageRanges(n.PageRanges).
		// Our resolved paper size wins over any @page rule in the document.
		WithPreferCSSPageSize(false).
		WithDisplayHeaderFooter(false).
		WithTransferMode(page.PrintToPDFTransferModeReturnAsBase64)
}
