## Stage 1: Builder image
FROM golang:1-alpine AS builder

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
RUN make image

## Stage 2: Production image
FROM alpine:latest AS final

WORKDIR /app

COPY --from=builder /app/tmp/build/openstack-management-api /app/

EXPOSE 8083

# Run as a non-root user (numeric UID so Kubernetes can enforce runAsNonRoot
# without resolving names). Nothing in this image is written at runtime; the
# binary only needs to be readable and executable.
USER 65532:65532

CMD ["./openstack-management-api"]
