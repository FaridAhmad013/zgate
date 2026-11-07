# Multi-stage Dockerfile for zcode-gateway (Zero external Go dependencies)
# Target binary is statically compiled using Go stdlib.

# ---------------------------------------------------------------------
# Builder Stage
# ---------------------------------------------------------------------
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Copy go.mod and go.sum (stdlib only, no external modules)
COPY go.mod ./

# Copy application source code
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Compile static binary with optimizations and stripped debug symbols
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -extldflags '-static'" \
    -trimpath \
    -o /build/zcode-gateway \
    ./cmd/zcode-gateway

# ---------------------------------------------------------------------
# Runtime Stage (Minimal distroless-like scratch image)
# ---------------------------------------------------------------------
FROM alpine:3.21 AS runner

# Create dedicated non-root application user and group
RUN addgroup -g 65532 -S zcode && \
    adduser -u 65532 -S zcode -G zcode -h /data -s /sbin/nologin && \
    mkdir -p /data && \
    chown -R zcode:zcode /data && \
    chmod 0700 /data

# Install ca-certificates for secure outbound TLS connections to zcode.z.ai
RUN apk --no-cache add ca-certificates curl

WORKDIR /data

# Copy compiled static binary
COPY --from=builder /build/zcode-gateway /usr/local/bin/zcode-gateway

# Run as unprivileged user
USER zcode:zcode

# Persistent volume for accounts.json storage
VOLUME ["/data"]

# Default environment
ENV BIND_HOST=0.0.0.0 \
    PORT=20131 \
    ZCODE_ACCOUNTS_FILE=/data/accounts.json \
    ZCODE_LANE_WIDTH=2 \
    ZCODE_ACCOUNT_CONCURRENCY=1 \
    ZCODE_SHUTDOWN_TIMEOUT_MS=15000

# Expose default HTTP proxy port
EXPOSE 20131

# Internal healthcheck using self-contained sub-command (zero dependencies, works on scratch / non-curl)
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/zcode-gateway", "healthcheck"]

# Start gateway
ENTRYPOINT ["/usr/local/bin/zcode-gateway"]
