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
| `ZCODE_DEFAULT_EFFORT` | `low` | Default reasoning effort when unspecified (`low`, `high`, `max`) |
| `ZCODE_PLAN_JWT` | - | Manual Plan JWT override |
| `ZCODE_IDENTITY_DEVICE_MID` | Auto | Device identifier (auto-detected on Windows) |

### Reverse Proxy & Network Security Notes

- **Default Binding**: Binds to `127.0.0.1:20131`. If both `BIND_HOST` and `HOST` are set, `BIND_HOST` takes precedence.
- **Non-Loopback Startup Gate**: Binding to a non-loopback address (e.g., `0.0.0.0` or a private/public IP) without setting `ZCODE_BRIDGE_TOKEN` causes startup to fail immediately.
- **Endpoint Authentication**: When `ZCODE_BRIDGE_TOKEN` is set, `/health`, `/v1/models`, `/models`, `/v1/quota`, `/quota`, `/v1/messages`, and `/v1/chat/completions` all require `Authorization: Bearer <token>` or `X-Api-Key: <token>`. `/healthz` remains an unauthenticated minimal liveness check (`{"ok":true}`).
- **Trusted Reverse Proxy**: The gateway validates the HTTP `Host` header against the bind host and `ZCODE_ALLOWED_HOSTS` and does **not** trust `X-Forwarded-Host` from clients. Configure your reverse proxy (Nginx/Caddy/Traefik) to set `Host` to `127.0.0.1:20131` or add the public hostname to `ZCODE_ALLOWED_HOSTS`.

## License

[MIT](LICENSE) © 2026 apissaj
