# PDF Renderer

A micro-service for generating PDF documents from HTML, rendered by headless Chromium.

Output is **vector PDF**: text is real text — selectable, searchable and copyable.

---

## Upgrading from v1 — read this first

v2 replaces the rendering engine. wkhtmltopdf was archived in 2022 and renders through a
2012 WebKit with no security pipeline; upstream's own guidance is *"do not use wkhtmltopdf
with any untrusted HTML"*, which is exactly what this service accepts.

Breaking changes, in the order they are likely to bite:

| Change | v1 | v2 | Action |
|---|---|---|---|
| **Container port** | `80` | **`8080`** | Update port mappings. The container now runs as non-root (UID 10001), which cannot bind a privileged port. `PORT` still overrides. |
| **Sandbox** | n/a | Chromium sandbox **on** | The container needs `--security-opt seccomp=deploy/chrome-seccomp.json`. It refuses to start otherwise rather than silently weakening itself. See [Running it](#running-it). |
| **Shared memory** | n/a | needs more than Docker's 64MB | Pass `--shm-size=1g`. |
| **Fonts** | Alpine's set | Liberation + Noto (incl. CJK and emoji) | Text may be laid out with different metrics. Expect visual differences. |
| **`htmlBody` validation** | silently optional | required | v1 declared `validate:"required"`, which gin never reads (it uses `binding:`), so an empty body rendered a blank PDF. It is now a 400. |
| **`pageWidth` / `pageHeight`** | a lone value was ignored | both or neither | Supplying one without the other is a 400. |
| **`orientation` with explicit dimensions** | ignored | honoured | v1 dropped `orientation` whenever explicit dimensions were given. |
| **Unknown `pageSize`** | silently fell back to A4 | 400 | A typo no longer produces a wrong-sized document. |
| **`dpi`** | resolution of the whole render | raster assets only | See [About `dpi`](#about-dpi). |

**Unchanged on purpose:** the endpoint path, the request field names, the default 0mm
margins, the `{"data": "<base64>"}` response shape, and **screen** media emulation.

> The v1 README documented `{"data": <byte-array>}`. That was never accurate — Go marshals
> `[]byte` as a base64 string, so the wire format has always been `{"data":"JVBERi0..."}`.

### Media type: the one to actually test

wkhtmltopdf rendered with **screen** media by default (`--print-media-type` was opt-in and
this service never set it). Chromium's `printToPDF` uses **print** media.

v2 therefore emulates **screen** media by default, so your existing documents keep
rendering the way they do today. If you want Chromium-native behaviour — `@media print`
rules applying, `@media screen` rules not — send `"mediaType": "print"`.

---

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/render_html` | Render HTML to PDF |
| `GET` | `/healthz` | Liveness — the process is serving |
| `GET` | `/readyz` | Readiness — opens a real tab and evaluates an expression |
| `GET` | `/` | Service name |

### Render HTML

```
POST /api/render_html
Content-Type: application/json
```

```json
{
    "htmlBody": "<h1>Hello</h1>",
    "pageSize": "A4",
    "orientation": "Portrait",
    "pageWidth": 210,
    "pageHeight": 297,
    "dpi": 96,
    "marginTop": 0,
    "marginBottom": 0,
    "marginLeft": 0,
    "marginRight": 0,
    "mediaType": "screen",
    "printBackground": true,
    "scale": 1.0,
    "pageRanges": "1-3,5"
}
```

| Field | Default | Notes |
|---|---|---|
| `htmlBody` | — | **Required.** Raw HTML. |
| `pageSize` | `A4` | Case-insensitive. Ignored when `pageWidth`/`pageHeight` are given. |
| `orientation` | `Portrait` | `Portrait` or `Landscape`. |
| `pageWidth` | — | Millimetres, 1–5000. Requires `pageHeight`. |
| `pageHeight` | — | Millimetres, 1–5000. Requires `pageWidth`. |
| `dpi` | `96` | 72–384. Affects raster assets only — see below. |
| `marginTop` | `0` | Millimetres. |
| `marginBottom` | `0` | Millimetres. |
| `marginLeft` | `0` | Millimetres. |
| `marginRight` | `0` | Millimetres. |
| `mediaType` | `screen` | `screen` (wkhtmltopdf-compatible) or `print`. |
| `printBackground` | `true` | Chromium's own default is `false`, which drops every CSS background. |
| `scale` | `1.0` | 0.1–2.0. Rejected outside that range rather than clamped. |
| `pageRanges` | all | e.g. `"1-3,5"`. |

Supported `pageSize` values: `a0` `a1` `a2` `a3` `a4` `a5` `a6` `b4` `b5` `c5e` `comm10e`
`dle` `executive` `folio` `ledger` `legal` `letter` `tabloid`.

#### Response

Default — unchanged from v1:

```json
{ "data": "<base64-encoded PDF>", "warnings": ["..."] }
```

Send `Accept: application/pdf` for the raw bytes instead:

```bash
curl -X POST localhost:8080/api/render_html \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/pdf' \
  -d '{"htmlBody":"<h1>Hello</h1>"}' \
  -o out.pdf
```

Only an explicit `application/pdf` triggers this. `Accept: */*` — curl's default — still
returns JSON, so existing callers are unaffected.

`warnings` is advisory and does not indicate failure. A failed subresource or a script
error never fails a render, matching wkhtmltopdf's default `--load-error-handling ignore`.

#### Errors

```json
{ "error": "unsupported pageSize \"A4-ish\"; supported: a0, a1, ...", "code": "invalid_request" }
```

| Status | `code` | Cause |
|---|---|---|
| 400 | `invalid_request` | Malformed JSON, missing `htmlBody`, out-of-range option |
| 413 | `payload_too_large` | Body over `PDFRENDER_MAX_BODY_BYTES` |
| 500 | `internal_error` | Unexpected failure. Detail is logged, never returned. |
| 503 | `browser_unavailable` | No usable browser, or all slots busy. Sends `Retry-After`. |
| 504 | `render_timeout` | Render exceeded `PDFRENDER_RENDER_TIMEOUT` |

---

## Running it

```bash
docker run -d --name pdf-renderer \
  -p 8080:8080 \
  --shm-size=1g \
  --security-opt seccomp=deploy/chrome-seccomp.json \
  ghcr.io/m18h/pdf-renderer:latest
```

### The sandbox

Chromium's namespace sandbox needs `clone(CLONE_NEWUSER)`, which Docker's default seccomp
profile denies for unprivileged containers. That is why so many headless-Chrome images
resort to `--no-sandbox`. Since this service renders arbitrary attacker-supplied HTML, the
sandbox is the boundary that matters, so it is **on by default and the service fails to
start without it** rather than quietly degrading.

Three ways to satisfy it, best first:

1. **`--security-opt seccomp=deploy/chrome-seccomp.json`** — keeps the sandbox. The profile
   is derived from Docker's current default with exactly three restrictions lifted
   (`clone` namespace flags, `clone3`, `unshare`); regenerate it with
   `python3 deploy/gen-seccomp.py > deploy/chrome-seccomp.json`.
   Kubernetes: use a `localhostProfile` seccomp profile.
2. **`--cap-add SYS_ADMIN`** — works, but a much broader grant.
3. **`-e PDFRENDER_NO_SANDBOX=1`** — weakest. Logs a warning at startup. Only with a
   hardened container: non-root, read-only rootfs, dropped capabilities, no egress.

> Do not copy the widely-shared `jfrazelle/chrome.json` seccomp profile — it predates
> modern runc and the container will not start.

`--shm-size=1g` matters because Docker's default `/dev/shm` is 64MB and Chromium
exhausting it produces `BUS_ADRERR` crashes rather than graceful degradation.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `PDFRENDER_EXEC_PATH` | `/headless-shell/headless-shell` | Browser binary |
| `PDFRENDER_FONTS_DIR` | `/usr/local/share/fonts` | Extra fonts mounted at runtime. Empty disables the scan. |
| `PDFRENDER_NO_SANDBOX` | unset | `1` disables the Chromium sandbox |
| `PDFRENDER_MAX_CONCURRENT` | CPU count | Simultaneous tabs |
| `PDFRENDER_ACQUIRE_TIMEOUT` | `5s` | Wait for a free slot before 503 |
| `PDFRENDER_MAX_RENDERS` | `500` | Recycle the browser after N renders (0 disables) |
| `PDFRENDER_MAX_AGE` | `30m` | Recycle the browser after this long (0 disables) |
| `PDFRENDER_RENDER_TIMEOUT` | `60s` | End-to-end budget for one render |
| `PDFRENDER_READY_TIMEOUT` | `15s` | Budget for page load, fonts and images |
| `PDFRENDER_MAX_BODY_BYTES` | `10485760` | Request body limit |
| `PDFRENDER_LOG_FORMAT` | `json` | `json` or `text` |

An invalid value is a startup failure, not a silent fallback.

### Adding your own fonts

The image bundles Liberation and Noto, which covers Latin, Cyrillic, Greek, CJK and emoji —
but not brand or licensed faces. Mount a directory of font files and they are picked up at
startup, no rebuild required:

```bash
docker run -d \
  -p 8080:8080 --shm-size=1g \
  --security-opt seccomp=deploy/chrome-seccomp.json \
  -v /path/to/your/fonts:/usr/local/share/fonts:ro \
  ghcr.io/m18h/pdf-renderer:latest
```

The service scans the directory (recursively), registers it with fontconfig and rebuilds
the font cache before Chromium starts. It logs what it found:

```json
{"level":"INFO","msg":"loaded extra fonts","dir":"/usr/local/share/fonts","count":3}
```

Your documents then reference the faces by family name as usual — `font-family: "Your Brand
Sans"`. Use `fc-list` inside the container to see the names fontconfig derived:

```bash
docker exec <container> fc-list : family | sort -u
```

Notes:

- **`/usr/local/share/fonts` is the zero-config path** — fontconfig already scans it. Any
  other location works too, via `PDFRENDER_FONTS_DIR`; the service then generates a
  fontconfig file and points `FONTCONFIG_FILE` at it.
- **Only formats fontconfig can load**: `.ttf`, `.ttc`, `.otf`, `.otc`, `.pfa`, `.pfb`,
  `.pcf`, `.bdf`, `.dfont`. **`.woff` and `.woff2` are ignored** — fontconfig cannot read
  them. Mounting web fonts is a silent no-op, so the service warns when it sees them.
  Convert them to `.ttf`/`.otf` first.
- A missing or empty directory is fine and is not an error, so the same run command works
  whether or not anything is mounted.
- Mounting read-only (`:ro`) is fine; the font cache is written under `$XDG_CACHE_HOME`.
- `@font-face` with a URL your document can reach still works and needs none of this. Use
  mounted fonts when the same faces are needed across many documents.

Verify the whole path end to end — it builds a deliberately CJK-less image and checks that
a mounted CJK font is genuinely embedded in the output:

```bash
./deploy/smoke-fonts.sh
```

### Sizing

Each concurrent tab is a separate renderer **process**, so size `PDFRENDER_MAX_CONCURRENT`
from measured RSS rather than from CPU count, and set a container memory limit. One
pathological document — a huge table, a giant background image — can spike a single
renderer far above your median.

---

## Notes on behaviour

### About `dpi`

**`dpi` cannot affect text sharpness, and no browser engine can make it.** Chromium
hardcodes print DPI to 72 (`kPointsPerInch`) in `pdf_print_utils.cc`; there is no DPI
parameter anywhere in the DevTools protocol. This is not a limitation so much as a
consequence of the output being vector: text is stored as font programs and glyph
positions, and re-rasterises at whatever resolution the viewer or printer runs at.
`--dpi` was meaningful under wkhtmltopdf only because it painted through a Qt `QPrinter`
whose device resolution drove the pixel-to-point conversion.

What `dpi` does here is set `deviceScaleFactor` to `dpi/96`, which raises
`window.devicePixelRatio`. That makes Chromium pick higher-resolution candidates from
`srcset` and `image-set()`, and gives `<canvas>` a larger backing store. If your document
has none of those, `dpi` will have no visible effect, and the response carries a warning
saying so.

### Page dimensions

Chromium converts inches to points and rounds each side up to a whole point, so ISO sizes
gain up to a point: **A4 is emitted as 596 × 842 pt**, not the canonical 595.28 × 841.89.
Sizes that are whole inches by definition (Letter, Legal, Tabloid) are exact. If anything
downstream asserts exact A4 dimensions — imposition, prepress preflight, a strict PDF/A
validator — this is worth knowing.

### Not supported

Headers, footers and page numbers (wkhtmltopdf's `--header-html` / `--footer-html`) are not
implemented yet. The underlying `displayHeaderFooter`, `generateTaggedPDF` and
`generateDocumentOutline` options exist and are straightforward to add on request.

### Security

The service renders arbitrary caller-supplied HTML. Chromium's sandbox and site isolation
are both enabled, but **network-level egress control is the real mitigation** for SSRF: a
document can reference `http://169.254.169.254/` or any internal address. Run it on a
network that cannot reach anything it should not.

---

## Development

Requires **Go 1.26+**.

```bash
go build ./...
go test ./...                  # unit tests, no browser needed
```

Browser-backed tests are behind a build tag so the default suite runs anywhere:

```bash
# Uses PDFRENDER_EXEC_PATH, or autodetects a local Chrome/Chromium/Edge.
go test -tags browser -timeout 15m ./...
```

They assert on real output: MediaBox dimensions per page size and orientation, page counts
and `pageRanges`, that a `/ToUnicode` CMap and an embedded font are present (i.e. the text
really is searchable), that `printBackground` paints, that CJK glyphs measure correctly
(which is what catches a Dockerfile missing fonts), that `mediaType` changes what renders,
that `dpi` actually selects the 2x `srcset` candidate, and that the pool recovers from
`Browser.crash`.

The container smoke test covers what no Go test can — fonts installed, the ENTRYPOINT
override, non-root with a writable `$HOME`, PID-1 reaping, missing shared libraries:

```bash
docker build -t pdf-renderer:smoke .
./deploy/smoke.sh pdf-renderer:smoke
```

Lint:

```bash
golangci-lint run ./...
golangci-lint run --build-tags browser ./...
```

### Smaller image

The default image bundles Noto CJK and colour emoji, and weighs **819MB** on disk (the
`chromedp/headless-shell` base alone is 517MB). For Latin-only documents:

```bash
docker build --build-arg FONTS="fonts-liberation fonts-noto-core" -t pdf-renderer:latin .
```

That measures **628MB** — 191MB smaller — at the cost of rendering CJK, emoji and other
non-Latin scripts as tofu boxes.
