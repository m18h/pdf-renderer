package main

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/SebastiaanKlippert/go-wkhtmltopdf"
	"github.com/gin-gonic/gin"
)

// request object
type htmlRequest struct {
	HTMLBody     string `json:"htmlBody" validate:"required"`
	PageSize     string `json:"pageSize"`
	PageWidth    uint   `json:"pageWidth"`
	PageHeight   uint   `json:"pageHeight"`
	Orientation  string `json:"orientation"`
	DPI          uint   `json:"dpi"`
	MarginTop    uint   `json:"marginTop"`
	MarginBottom uint   `json:"marginBottom"`
	MarginLeft   uint   `json:"marginLeft"`
	MarginRight  uint   `json:"marginRight"`
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
		data := &htmlRequest{}

		err := c.BindJSON(&data)
		if err != nil {
			log.Fatal(err)
			c.JSON(
				http.StatusBadRequest,
				gin.H{
					"error": err.Error(),
				},
			)
			return
		}

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
	port, exists := os.LookupEnv("PORT")
	if exists {
		r.Run(":" + port)
	} else {
		r.Run(":7900")
	}
}

func renderHTML(data *htmlRequest) ([]byte, error) {
	pdfg := wkhtmltopdf.NewPDFPreparer()
	pdfg.AddPage(wkhtmltopdf.NewPageReader(strings.NewReader(data.HTMLBody)))
	if data.DPI != 0 {
		pdfg.Dpi.Set(data.DPI)
	}
	if data.PageWidth != 0 && data.PageHeight != 0 {
		pdfg.PageWidth.Set(data.PageWidth)
		pdfg.PageHeight.Set(data.PageHeight)
	} else {
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
