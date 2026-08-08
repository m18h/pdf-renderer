#!/usr/bin/env bash
#
# Verifies that fonts mounted at runtime are actually used by the renderer.
#
# The trick is to build a deliberately CJK-less image, then render Japanese text
# through it. Without a mounted font that produces tofu boxes; with one it
# produces a real embedded CJK subset. The resulting PDFs differ by roughly 4x in
# size, which is a far more honest signal than "the request returned 200".
#
# Usage: deploy/smoke-fonts.sh [base-image]
set -euo pipefail

BASE_IMAGE="${1:-pdf-renderer:smoke}"
LATIN_IMAGE="pdf-renderer:fonts-latin"
CONTAINER="pdf-renderer-fonts-$$"
PORT="${SMOKE_PORT:-18111}"
WORKDIR="$(mktemp -d)"
SECCOMP="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/chrome-seccomp.json"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Japanese text: renders as tofu without a CJK face.
PAYLOAD='{"htmlBody":"<div style=\"font-size:32px\">日本語のテキスト 漢字かな</div>"}'

cleanup() {
  local code=$?
  if [ $code -ne 0 ]; then
    echo "--- container logs ---" >&2
    docker logs "$CONTAINER" 2>&1 | tail -30 >&2 || true
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
  exit $code
}
trap cleanup EXIT

fail() { echo "FONTS SMOKE FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

echo "==> building a CJK-less image"
docker build --build-arg FONTS="fonts-liberation fonts-noto-core" \
  -t "$LATIN_IMAGE" "$REPO_ROOT" >/dev/null 2>&1

echo "==> extracting a CJK font to stand in for a customer-supplied one"
mkdir -p "$WORKDIR/fonts"
CID=$(docker create "$BASE_IMAGE")
docker cp "$CID:/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc" "$WORKDIR/fonts/" >/dev/null
docker rm "$CID" >/dev/null
[ -s "$WORKDIR/fonts/NotoSansCJK-Regular.ttc" ] || fail "could not extract a CJK font from $BASE_IMAGE"

# render <output-name> [extra docker args...]
render() {
  local out="$1"; shift
  docker run -d --name "$CONTAINER" --shm-size=1g \
    --security-opt "seccomp=${SECCOMP}" \
    -p "127.0.0.1:${PORT}:8080" "$@" "$LATIN_IMAGE" >/dev/null

  local ready=""
  for _ in $(seq 1 60); do
    if curl -fsS "localhost:${PORT}/readyz" >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  [ -n "$ready" ] || fail "service never became ready"

  curl -fsS -X POST "localhost:${PORT}/api/render_html" \
    -H 'Content-Type: application/json' -H 'Accept: application/pdf' \
    -d "$PAYLOAD" -o "$WORKDIR/${out}.pdf"

  docker logs "$CONTAINER" > "$WORKDIR/${out}.log" 2>&1
  docker rm -f "$CONTAINER" >/dev/null 2>&1
}

size() { wc -c < "$WORKDIR/$1.pdf" | tr -d ' '; }

echo "==> baseline: no fonts mounted (expect tofu)"
render baseline
BASE_SIZE=$(size baseline)
echo "    ${BASE_SIZE} bytes"
grep -q 'loaded extra fonts' "$WORKDIR/baseline.log" \
  && fail "reported loading extra fonts when none were mounted"
pass "no extra fonts reported"

echo "==> mounted at the default path (/usr/local/share/fonts)"
render mounted -v "$WORKDIR/fonts:/usr/local/share/fonts:ro"
MOUNTED_SIZE=$(size mounted)
echo "    ${MOUNTED_SIZE} bytes"

grep -q 'loaded extra fonts' "$WORKDIR/mounted.log" || fail "the mounted font was not detected"
pass "font detected at startup"

# A real CJK subset is dramatically larger than a page of .notdef boxes.
if [ "$MOUNTED_SIZE" -le $((BASE_SIZE * 2)) ]; then
  fail "PDF grew from ${BASE_SIZE} to only ${MOUNTED_SIZE} bytes; the mounted font does not appear to be embedded"
fi
pass "CJK glyphs embedded (${BASE_SIZE} -> ${MOUNTED_SIZE} bytes)"

# fontconfig already scans this path, so no generated config should be needed.
grep -q 'registered font directory' "$WORKDIR/mounted.log" \
  && fail "generated a fontconfig file for a path fontconfig already scans"
pass "no redundant fontconfig file generated"

echo "==> mounted at a custom path (/opt/brand-fonts)"
render custom \
  -v "$WORKDIR/fonts:/opt/brand-fonts:ro" \
  -e PDFRENDER_FONTS_DIR=/opt/brand-fonts
CUSTOM_SIZE=$(size custom)
echo "    ${CUSTOM_SIZE} bytes"

grep -q 'registered font directory' "$WORKDIR/custom.log" \
  || fail "a path outside fontconfig's scan roots must be registered via FONTCONFIG_FILE"
pass "custom path registered with fontconfig"

if [ "$CUSTOM_SIZE" -le $((BASE_SIZE * 2)) ]; then
  fail "custom-path PDF is only ${CUSTOM_SIZE} bytes; the font was not used"
fi
pass "CJK glyphs embedded from the custom path (${CUSTOM_SIZE} bytes)"

echo "==> opting out with an empty PDFRENDER_FONTS_DIR"
render optout -v "$WORKDIR/fonts:/usr/local/share/fonts:ro" -e PDFRENDER_FONTS_DIR=
grep -q 'loaded extra fonts' "$WORKDIR/optout.log" \
  && fail "scanned for fonts despite an empty PDFRENDER_FONTS_DIR"
pass "font scan skipped"

echo
echo "FONTS SMOKE PASS"
