# syntax=docker/dockerfile:1

##############################################################################
# build — runs natively and cross-compiles via GOOS/GOARCH, so the Go build
# never goes through QEMU on a multi-arch build.
##############################################################################
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine3.24 AS builder

ENV CGO_ENABLED=0
WORKDIR /src

# Dependencies first, so editing source doesn't re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/pdf-renderer .

##############################################################################
# runtime
##############################################################################
# chromedp/headless-shell is Chromium's headless shell on debian trixie-slim
# with just five runtime libs. The Chrome team recommends this binary (rather
# than full Chrome in new-headless mode) for exactly this workload: it needs no
# X11, no Wayland and no D-Bus.
FROM chromedp/headless-shell:151.0.7922.109 AS runtime

LABEL org.opencontainers.image.title="pdf-renderer" \
      org.opencontainers.image.description="HTML to PDF micro-service (headless Chromium)" \
      org.opencontainers.image.source="https://github.com/m18h/pdf-renderer" \
      org.opencontainers.image.authors="Michael K. Essandoh"

# The base image ships NO FONTS. Chromium with an empty fontconfig renders tofu
# boxes or nothing at all, so these are required, not optional. tini reaps the
# zygote/renderer/GPU orphans that reparent to PID 1 — the Go runtime will not
# wait() for arbitrary children.
#
# Build with --build-arg FONTS="fonts-liberation" for a Latin-only image roughly
# 145MB smaller. fonts-noto-cjk-extra (215MB installed) is deliberately excluded.
ARG FONTS="fonts-liberation fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji"
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      tini ca-certificates fontconfig ${FONTS} \
 && fc-cache -f \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*

# Non-root. Chromium writes profile and crash data under $HOME, and an
# unwritable home fails in confusing ways, hence --create-home and the XDG vars.
RUN groupadd --system --gid 10001 render \
 && useradd --system --uid 10001 --gid 10001 \
      --create-home --home-dir /home/render --shell /usr/sbin/nologin render

COPY --from=builder /out/pdf-renderer /usr/local/bin/pdf-renderer

# PORT is 8080, not 80: a non-root UID cannot bind a privileged port.
ENV GIN_MODE=release \
    PORT=8080 \
    PDFRENDER_EXEC_PATH=/headless-shell/headless-shell \
    PDFRENDER_LOG_FORMAT=json \
    HOME=/home/render \
    XDG_CONFIG_HOME=/home/render/.config \
    XDG_CACHE_HOME=/home/render/.cache

USER 10001:10001
WORKDIR /home/render
EXPOSE 8080

# The image has no curl or wget, so the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/pdf-renderer", "-healthcheck"]

# Override the base image's ENTRYPOINT, which is /headless-shell/run.sh and
# starts socat plus headless-shell with --remote-debugging-port. We manage the
# browser in-process and want neither.
#
# tini -g puts itself in its own process group and forwards signals to the whole
# group, so SIGTERM reaches the Go process and any stray Chromium.
ENTRYPOINT ["/usr/bin/tini", "-g", "--"]
CMD ["/usr/local/bin/pdf-renderer"]
