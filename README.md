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

1. PageSize 
```

```
2. Orientation
```

```

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
