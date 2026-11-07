# zcode-gateway

Local OpenAI & Anthropic-compatible gateway for the ZCode Start-Plan (GLM-5.3 & GLM-5.3-Flash) with customizable reasoning effort levels (`low`, `high`, `max`).

## Highlights

- **Dual Dialects**: Fully supports standard OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) protocols.
- **Reasoning Effort Control**: Fine-tune thinking budget directly via OpenAI parameter `reasoning_effort` or model suffix (`:low`, `:high`, `:max`).
- **Standard Reasoning Output**: Returns thinking blocks mapped to OpenAI standard `reasoning_content` delta and non-streaming message fields.
- **Zero Configuration Discovery**: Automatic device mid detection from local desktop installation (`%APPDATA%\ZCode\.updaterId`).
- **Optimized for Proxy Gateways**: Ready to connect with 9Router, Next.js AI SDK, Claude Code, and OpenAI-compatible tools.

## Supported Models

| Model ID | Reasoning Effort | Best For |
|---|---|---|
| `glm-5.3-flash` | Default (`low`) | Fast everyday coding and high-throughput chat |
| `glm-5.3-flash:low` | Low | Minimal token consumption, lowest latency |
| `glm-5.3-flash:high` | High | Balanced deep reasoning |
| `glm-5.3-flash:max` | Max | Complex algorithmic problems and architecture design |
| `glm-5.3` | Default (`low`) | High parameter precision |
| `glm-5.3:low` | Low | Standard coding with brief thoughts |
| `glm-5.3:high` | High | Detailed chain of thought |
| `glm-5.3:max` | Max | Comprehensive reasoning |

## Quick Start

### Build & Run

```bash
git clone https://github.com/apissaj/zcode-gateway.git
cd zcode-gateway

# Build binary
go build -o zcode-gateway.exe ./cmd/zcode-gateway

# Run gateway on custom port (default: 20131 or set PORT)
PORT=8085 ZCODE_DEFAULT_EFFORT=low ./zcode-gateway.exe
```

### Automatic Token Sync (Windows)

Synchronize session JWT directly from your signed-in ZCode desktop IDE:

```bash
node sync-token.js
```

### Usage Examples

#### OpenAI Dialect with Stream (`/v1/chat/completions`)

```bash
curl -X POST http://127.0.0.1:8085/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.3-flash:low",
    "messages": [
      {"role": "user", "content": "Explain binary search in 2 sentences."}
    ],
    "stream": true
  }'
```

#### Anthropic Dialect (`/v1/messages`)

```bash
curl -X POST http://127.0.0.1:8085/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.3-flash",
    "thinking": {"type": "enabled", "effort": "low"},
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello!"}
    ]
  }'
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `20131` | Listening port (`1`–`65535`) |
| `BIND_HOST` | `127.0.0.1` | Network interface to bind (takes priority over `HOST` if both are set) |
| `HOST` | `127.0.0.1` | Fallback network interface to bind when `BIND_HOST` is unset |
| `ZCODE_BRIDGE_TOKEN` | - | API token (`Authorization: Bearer <token>` or `X-Api-Key`). **Mandatory** when binding to any non-loopback address (`0.0.0.0`, LAN/public IP, or external hostname) |
| `ZCODE_ALLOWED_HOSTS` | - | Comma-separated exact `Host` header allowlist for reverse-proxy deployments (`X-Forwarded-Host` is not trusted unless rewritten into `Host` by a trusted reverse proxy) |
| `ZCODE_CORS_ORIGIN` | - | Comma-separated exact browser origins (`https://...`) allowed for CORS (`*` wildcards are rejected) |
| `ZCODE_TRUSTED_PROXIES` | - | Comma-separated trusted proxy IPs or CIDRs (e.g. `127.0.0.1,::1,10.0.0.0/8`). `X-Forwarded-For` / `X-Real-IP` are ignored unless the TCP peer matches this list |
| `ZCODE_RATE_LIMIT_RPS` | `20` | Token-bucket refill rate (requests/sec) per client IP. Set `0` to disable rate limiting |
| `ZCODE_RATE_LIMIT_BURST` | `40` | Token-bucket burst capacity per client IP |
| `ZCODE_MAX_HEADER_BYTES` | `65536` | Maximum HTTP request header size in bytes (64 KiB) |
| `ZCODE_READ_HEADER_TIMEOUT_MS` | `10000` | Maximum time in ms allowed to read request headers (10s) |
| `ZCODE_BODY_READ_TIMEOUT_MS` | `30000` | Maximum time in ms allowed to read the request JSON body (30s; cleared before SSE streaming begins) |
| `ZCODE_MAX_BODY_BYTES` | `33554432` | Maximum HTTP request body size in bytes (32 MiB) |
| `ZCODE_SERVER_IDLE_TIMEOUT_MS` | `66000` | HTTP keep-alive idle connection timeout in ms (66s) |
| `ZCODE_SHUTDOWN_TIMEOUT_MS` | `15000` | Graceful shutdown deadline in ms on `SIGINT`/`SIGTERM` (15s) |
| `ZCODE_HEADER_TIMEOUT_MS` | `120000` | Upstream Z.ai response header timeout in ms (2m; falls back to `ZCODE_UPSTREAM_TIMEOUT_MS`) |
| `ZCODE_STREAM_IDLE_TIMEOUT_MS` | `120000` | Per-read idle timeout in ms for upstream SSE streams (2m; active streams have no total wall-clock cap) |
| `ZCODE_LANE_WIDTH` | `1` | Maximum concurrent upstream inference streams per gateway process |
| `ZCODE_MAX_ATTEMPTS` | `5` | Maximum in-place retry attempts on transient single-account `3008` errors |
| `ZCODE_CONCURRENCY_COOLDOWN_MS` | `5000` | Short cooldown in ms applied when rotating accounts on multi-account `3008` errors |
| `ZCODE_ACCESS_LOG` | - | Set `1`/`true` to log all requests, `0`/`false` to disable all access logs, or leave unset to log operational events (`4xx`/`5xx`, rate limits, timeouts) |
| `ZCODE_DEFAULT_EFFORT` | `low` | Default reasoning effort when unspecified (`low`, `high`, `max`) |
| `ZCODE_PLAN_JWT` | - | Manual Plan JWT override |
| `ZCODE_IDENTITY_DEVICE_MID` | Auto | Device identifier (auto-detected on Windows) |

### Configuration Examples

#### Localhost Deployment (Claude Code / 9Router / Local SDKs)

```bash
BIND_HOST=127.0.0.1 \
PORT=20131 \
ZCODE_LANE_WIDTH=2 \
./zcode-gateway
```

#### VPS Deployment Behind a Local Reverse Proxy (Caddy / Nginx)

```bash
BIND_HOST=127.0.0.1 \
PORT=20131 \
ZCODE_BRIDGE_TOKEN="replace-with-32-byte-random-secret" \
ZCODE_ALLOWED_HOSTS="ai.example.com" \
ZCODE_TRUSTED_PROXIES="127.0.0.1,::1" \
ZCODE_RATE_LIMIT_RPS=20 \
ZCODE_RATE_LIMIT_BURST=40 \
ZCODE_SHUTDOWN_TIMEOUT_MS=15000 \
./zcode-gateway
```

### Reverse Proxy, Rate Limiting & Lifecycle Notes

- **Default Binding & Startup Gate**: Binds to `127.0.0.1:20131` (`BIND_HOST` -> `HOST` -> `127.0.0.1`). Binding to any non-loopback address (`0.0.0.0`, LAN/public IP, or external hostname) without `ZCODE_BRIDGE_TOKEN` aborts startup immediately.
- **Endpoint Authentication**: When `ZCODE_BRIDGE_TOKEN` is set, `/health`, `/v1/models`, `/models`, `/v1/quota`, `/quota`, `/v1/messages`, and `/v1/chat/completions` require `Authorization: Bearer <token>` or `X-Api-Key: <token>`. `/healthz` remains an unauthenticated minimal liveness check (`{"ok":true}`).
- **Trusted Proxy & Rate Limiting**: By default (`ZCODE_TRUSTED_PROXIES` empty), `X-Forwarded-For` and `X-Real-IP` are ignored and rate limiting keys strictly on the TCP peer IP (`r.RemoteAddr`). When `ZCODE_TRUSTED_PROXIES` is configured, the gateway inspects `X-Forwarded-For` from right to left and selects the right-most non-trusted client IP so clients cannot spoof leading `X-Forwarded-For` entries or rotate `X-Session-Id`/`User-Agent` headers to bypass limits. Exceeding the bucket returns `429 Too Many Requests` with a `Retry-After` header in the route's API dialect.
- **Request Timeouts & Long SSE Streams**: Request headers (`ZCODE_READ_HEADER_TIMEOUT_MS`) and request bodies (`ZCODE_BODY_READ_TIMEOUT_MS`, `ZCODE_MAX_BODY_BYTES`) enforce strict deadlines and size limits, then clear the socket read deadline before streaming begins so multi-minute SSE responses are never cut by server read timeouts.
- **Graceful Shutdown**: On `SIGINT` or `SIGTERM`, the server stops accepting new connections immediately and waits up to `ZCODE_SHUTDOWN_TIMEOUT_MS` for in-flight requests and active SSE streams to finish. If the deadline expires, remaining stream contexts are canceled cleanly and upstream connections/lanes are released.
- **Reverse Proxy Recommendations**: Configure Nginx/Caddy/Traefik to disable response buffering (`proxy_buffering off;` / `X-Accel-Buffering: no`), set upstream read timeouts to at least `300s` for long reasoning streams, and either rewrite `Host` to `127.0.0.1:20131` or list your public domain in `ZCODE_ALLOWED_HOSTS`.

## License

[MIT](LICENSE) © 2026 apissaj
