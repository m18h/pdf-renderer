package main

import (
	"bytes"
	"log"
	"net/http"
	"strings"

	"github.com/SebastiaanKlippert/go-wkhtmltopdf"
	"github.com/gin-gonic/gin"
)

// Request
type RenderRequest struct {
	HTMLBody     string
	DPI          uint
	PageSize     string
	Orientation  string
	MarginTop    uint
	MarginBottom uint
	MarginLeft   uint
	MarginRight  uint
}

func main() {
	r := gin.Default()

	r.GET("/", func(c *gin.Context) {
		c.JSON(
			http.StatusOK,
			gin.H{
				"message": "PDF Renderer",
			},
		)
	})

	r.POST("/api/render_html", func(c *gin.Context) {
		data := &RenderRequest{}
		c.Bind(data)

		pdf, err := renderHTML(data)

		if err == nil {
			c.JSON(
				http.StatusOK,
				gin.H{
					"data": pdf,
				},
			)
		} else {
			c.JSON(
				http.StatusInternalServerError,
				gin.H{
					"error": err,
				},
			)
		}
	})

	r.Run(":80") // listen and serve on 0.0.0.0:80
}

func renderHTML(data *RenderRequest) ([]byte, error) {
	pdfg := wkhtmltopdf.NewPDFPreparer()
	pdfg.AddPage(wkhtmltopdf.NewPageReader(strings.NewReader(data.HTMLBody)))
	if data.DPI != 0 {
		pdfg.Dpi.Set(data.DPI)
	}
	if data.PageSize != "" {
		pdfg.PageSize.Set(data.PageSize)
	} else {
		pdfg.PageSize.Set("A4")
	}
	if data.Orientation != "" {
		pdfg.Orientation.Set(data.Orientation)
	} else {
		pdfg.Orientation.Set("Portrait")
	}
	pdfg.MarginTop.Set(data.MarginTop)
	pdfg.MarginBottom.Set(data.MarginBottom)
	pdfg.MarginLeft.Set(data.MarginLeft)
	pdfg.MarginRight.Set(data.MarginRight)

	// The html string is also saved as base64 string in the JSON file
	jsonBytes, err := pdfg.ToJSON()
	if err != nil {
		log.Fatal(err)
	}

	// The JSON can be saved, uploaded, etc.

	// Server code, create a new PDF generator from JSON, also looks for the wkhtmltopdf executable
	pdfgFromJSON, err := wkhtmltopdf.NewPDFGeneratorFromJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		log.Fatal(err)
	}

	// Create the PDF
	err = pdfgFromJSON.Create()
	if err != nil {
		log.Fatal(err)
	} else {
		log.Printf("PDF Generated :: Size %d bytes", pdfgFromJSON.Buffer().Len())
	}

	return pdfgFromJSON.Buffer().Bytes(), err
}
