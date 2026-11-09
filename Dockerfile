# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b

# CI builds each arch on a native runner. The build stage still runs on
# $BUILDPLATFORM and cross-compiles, so a local `buildx --platform
# linux/amd64,linux/arm64` only emulates the runtime stage's `apk add`.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
# Modules first, so source edits don't re-download them. All pure Go
# (zebrash and its barcode/font deps), so CGO stays off and the cross-compile
# needs no target toolchain.
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/labelprint .

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

LABEL org.opencontainers.image.title="labelprint" \
      org.opencontainers.image.description="Upload ZPL, PDF, PNG, JPEG or GIF files and print them on a label printer via LPrint" \
      org.opencontainers.image.source="https://github.com/owine/labelprint" \
      org.opencontainers.image.vendor="owine" \
      org.opencontainers.image.licenses="MIT"

# Package versions are pinned to Alpine 3.24 repository versions; Renovate
# extracts these pins natively (datasource `apk`) against the registryUrls
# branch in .github/renovate.json.
# - poppler-utils: pdfinfo + pdftoppm, which render PDFs to PNG
# - font-liberation: metric-compatible stand-ins for Helvetica/Times/Courier.
#   Carrier label PDFs often reference those fonts without embedding them;
#   with no fonts installed poppler would draw that text as nothing.
# - fontconfig: how poppler finds those stand-ins
#
# APK_CLOSURE is a cache key, not a setting: CI passes a hash of the package
# set this `apk add` resolves to today, so the layer rebuilds when a
# transitive package gets a new build. Empty for local builds.
ARG APK_CLOSURE=
RUN apk add --no-cache \
    poppler-utils=25.12.0-r1 \
    font-liberation=2.1.5-r2 \
    fontconfig=2.17.1-r1 && \
    # Build the font cache now: the root filesystem is read-only at runtime.
    fc-cache -f && \
    pdftoppm -v

COPY --from=build /out/labelprint /usr/local/bin/labelprint

# Unprivileged, and HOME on the tmpfs so nothing tries to write elsewhere.
USER 65532:65532
ENV HOME=/tmp
# Go's heap limit, under the 512 MB container in deploy/compose.yaml: two
# image decodes at their 96 MB estimate, the uploads and the tmpfs PDFs all
# count against it. Raise it together with mem_limit.
ENV GOMEMLIMIT=400MiB
EXPOSE 8080

# Checks only that this process serves HTTP; LPrint being down is a printer
# problem, not a reason for Docker to restart the container.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/labelprint", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/labelprint"]
