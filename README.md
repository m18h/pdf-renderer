# PDF Renderer

A micro-service for generating PDF documents

## How To Use

### Request Object
```json
{
    "HtmlBody": <string>,
    "DPI": <int>,
    "PageSize": <string>,
    "Orientation": <string>,
    "MarginTop": <int>,
    "MarginBottom": <int>,
    "MarginLeft": <int>,
    "MarginRight": <int>
}
```

#### Request Options

Value | Default 
--- | ---
HtmlBody | -
DPI | `96`
PageSize | `A4`
Orientation | `Portrait`
MarginTop | -
MarginBottom | `10mm`
MarginLeft | `10mm`
MarginRight | -

### Response Object
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
