# syntax=docker/dockerfile:1

# ---- build ---------------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first so the layer is reused whenever only source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO is off so the binary is fully static and runs on a bare image.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/proxy ./cmd/proxy

# ---- runtime -------------------------------------------------------------
FROM alpine:3.20

# wget (busybox) backs the container healthcheck; ca-certificates lets the
# proxy reach an HTTPS upstream such as httpbin.org.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 proxy

COPY --from=build /out/proxy /usr/local/bin/proxy

USER proxy
EXPOSE 8080

ENV PORT=8080 \
    REDIS_ADDR=redis:6379 \
    RATE_LIMIT_RPS=10 \
    WINDOW_SIZE_SECONDS=1 \
    UPSTREAM_URL=mock://internal \
    LOG_FORMAT=json

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}${ADMIN_PREFIX}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/proxy"]
