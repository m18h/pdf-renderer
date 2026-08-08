#!/usr/bin/env bash
#
# Container smoke test.
#
# No Go test covers what this does: fonts actually installed, the ENTRYPOINT
# override, a non-root user with a writable $HOME, PID-1 reaping, and missing
# shared libraries. Only running the image proves those.
#
# Usage: deploy/smoke.sh [image] [--no-sandbox]
set -euo pipefail

IMAGE="${1:-pdf-renderer:smoke}"
CONTAINER="pdf-renderer-smoke-$$"
PORT="${SMOKE_PORT:-18080}"
WORKDIR="$(mktemp -d)"
SECCOMP="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/chrome-seccomp.json"

cleanup() {
  local code=$?
  if [ $code -ne 0 ]; then
    echo "--- container logs ---" >&2
    docker logs "$CONTAINER" 2>&1 | tail -50 >&2 || true
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
  exit $code
}
trap cleanup EXIT

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

echo "==> starting $IMAGE"
# --shm-size: Docker's default /dev/shm is 64MB and Chromium exhausting it
# produces BUS_ADRERR crashes rather than graceful degradation.
# --security-opt seccomp: Docker's default profile denies clone(CLONE_NEWUSER),
# which Chromium's namespace sandbox needs. This keeps the sandbox ON.
DOCKER_ARGS=(
  -d --name "$CONTAINER"
  --shm-size=1g
  -p "127.0.0.1:${PORT}:8080"
)
if [ "${2:-}" = "--no-sandbox" ]; then
  echo "    (sandbox disabled via PDFRENDER_NO_SANDBOX)"
  DOCKER_ARGS+=(-e PDFRENDER_NO_SANDBOX=1)
else
  DOCKER_ARGS+=(--security-opt "seccomp=${SECCOMP}")
fi
docker run "${DOCKER_ARGS[@]}" "$IMAGE" >/dev/null

echo "==> waiting for readiness"
ready=""
for _ in $(seq 1 60); do
  if curl -fsS "localhost:${PORT}/readyz" >/dev/null 2>&1; then ready=1; break; fi
  if [ -z "$(docker ps -q -f name="$CONTAINER")" ]; then fail "container exited during startup"; fi
  sleep 1
done
[ -n "$ready" ] || fail "/readyz never became ready"
pass "/readyz responded"

# The sandbox must not have silently fallen back.
if docker logs "$CONTAINER" 2>&1 | grep -q 'No usable sandbox'; then
  fail "Chromium reported 'No usable sandbox'"
fi
pass "no sandbox errors in logs"

echo "==> rendering (Accept: application/pdf)"
BODY='{"htmlBody":"<h1 style=\"background:#f00;color:#fff\">héllo 漢字</h1><p>Smoke test.</p>","pageSize":"A4"}'
curl -fsS -X POST "localhost:${PORT}/api/render_html" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/pdf' \
  -d "$BODY" -o "$WORKDIR/out.pdf" \
  -D "$WORKDIR/headers.txt"

head -c 5 "$WORKDIR/out.pdf" | grep -q '%PDF-' || fail "response is not a PDF"
pass "%PDF- magic bytes"

grep -qi 'content-type: application/pdf' "$WORKDIR/headers.txt" || fail "Content-Type is not application/pdf"
pass "Content-Type: application/pdf"

# Size floor: a PDF whose fonts are missing is dramatically smaller, so this is a
# surprisingly effective tofu detector. Real text plus a red background and CJK
# glyphs comfortably exceeds 3KB.
SIZE=$(wc -c < "$WORKDIR/out.pdf" | tr -d ' ')
[ "$SIZE" -gt 3000 ] || fail "PDF is only ${SIZE} bytes; fonts or backgrounds are probably missing"
pass "size ${SIZE} bytes (> 3000)"

grep -q 'ToUnicode' "$WORKDIR/out.pdf" || fail "no /ToUnicode CMap: text would not be searchable"
pass "text is searchable (/ToUnicode present)"

echo "==> rendering (legacy JSON, no Accept header)"
curl -fsS -X POST "localhost:${PORT}/api/render_html" \
  -H 'Content-Type: application/json' \
  -d "$BODY" -o "$WORKDIR/out.json"

grep -q '"data":"JVBER' "$WORKDIR/out.json" \
  || fail "legacy response is not base64-encoded PDF (expected a data field starting JVBER)"
pass "legacy {\"data\":\"<base64>\"} shape preserved"

echo "==> checking error handling"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "localhost:${PORT}/api/render_html" \
  -H 'Content-Type: application/json' -d '{')
[ "$CODE" = "400" ] || fail "malformed JSON returned $CODE, want 400"
pass "malformed JSON -> 400 (and the service is still up)"

echo "==> checking the container runs as non-root"
UID_IN_CONTAINER=$(docker exec "$CONTAINER" id -u 2>/dev/null || echo "unknown")
[ "$UID_IN_CONTAINER" = "10001" ] || fail "container UID is $UID_IN_CONTAINER, want 10001"
pass "running as UID 10001"

echo "==> checking for zombie processes"
ZOMBIES=$(docker exec "$CONTAINER" sh -c "ps -eo stat 2>/dev/null | grep -c '^Z' || true" | tr -d ' ')
[ "${ZOMBIES:-0}" -eq 0 ] || fail "$ZOMBIES zombie processes; tini is not reaping"
pass "no zombie processes"

echo
echo "SMOKE PASS"
