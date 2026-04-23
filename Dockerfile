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

RUN make all

## Stage 2: Production image
FROM alpine:latest AS final

WORKDIR /app

COPY --from=builder /app/tmp/build/openstack-management-api /app/

EXPOSE 8083

CMD ["./openstack-management-api"]
