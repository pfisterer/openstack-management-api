## Stage 1: Builder image
# The build stage runs on the machine that builds (BUILDPLATFORM) and cross-compiles for the target: on an arm64 Mac that is native speed instead of emulation. No cgo is needed, so the binary is static.
# Pinned to exact versions: a floating tag rebuilds the image on whatever compiler and base happen to be current, so a release cannot be reproduced and a security fix of the toolchain (Go >= 1.27.2) is not guaranteed to be in it. Bump deliberately.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.24 AS builder
ARG TARGETOS TARGETARCH

RUN apk add --no-cache git nodejs npm make build-base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY package.json package-lock.json ./
RUN npm install

COPY Makefile ./
COPY VERSION ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# `image` builds the bundle + binary WITHOUT running the Go test suite.
# Tests are a local/dev concern (`make all`) and are intentionally not run
# during the image build.
RUN CGO_ENABLED=0 BUILD_GOOS=$TARGETOS BUILD_GOARCH=$TARGETARCH make image

## Stage 2: Production image
FROM alpine:3.24.2 AS final

WORKDIR /app

COPY --from=builder /app/tmp/build/openstack-management-api /app/

EXPOSE 8083

# Run as a non-root user (numeric UID so Kubernetes can enforce runAsNonRoot
# without resolving names). Nothing in this image is written at runtime; the
# binary only needs to be readable and executable.
USER 65532:65532

CMD ["./openstack-management-api"]
