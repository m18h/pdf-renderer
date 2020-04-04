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
pageSize | `A4`
orientation | `Portrait`
marginTop | -
marginBottom | `10mm`
marginLeft | `10mm`
marginRight | -

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
