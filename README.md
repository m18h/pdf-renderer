# PDF Renderer

A micro-service for generating PDF documents

## Endpoints

### Render HTML

`/api/render_html`

#### Request
```json
{
    "htmlBody": <string>,
    "dpi": <int>,
    "pageWidth": <int>,
    "pageHeight": <int>,
    "pageSize": <string>,
    "orientation": <string>,
    "marginTop": <int>,
    "marginBottom": <int>,
    "marginLeft": <int>,
    "marginRight": <int>
}
```

Options

Value | Default 
--- | ---
htmlBody | -
dpi | `96`
pageWidth | `0` _(mm)_
pageHeight | `0` _(mm)_
pageSize | `A4`
orientation | `Portrait`
marginTop | -
marginBottom | `10` _(mm)_
marginLeft | `10` _(mm)_
marginRight | -

> NOTE: When the page width and page height is specified, the size and orientation is ignored.

#### Response
```json
{
    "data": <byte-array>
}
```

## Development

### Requirements

- Go v1.14+

### Install dependencies

```
dep ensure
```
