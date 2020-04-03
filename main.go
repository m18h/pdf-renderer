package main

import (
	"bytes"
	"log"
	"net/http"
	"strings"

	"github.com/SebastiaanKlippert/go-wkhtmltopdf"
	"github.com/gin-gonic/gin"
)

// request object
type renderRequest struct {
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
		data := &renderRequest{}
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

	// listen and serve on 0.0.0.0:80
	r.Run(":80")
}

func renderHTML(data *renderRequest) ([]byte, error) {
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

	// convert html to json string
	jsonBytes, err := pdfg.ToJSON()
	if err != nil {
		log.Fatal(err)
	}

	// use wkhtmltopdf tp create pdf
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
