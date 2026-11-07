# ZGate (ZCode Gateway)

Hardened, high-performance, single-binary AI Gateway & Proxy for the ZCode Start-Plan (GLM-5.3 & GLM-5.3-Flash) with native multi-account scheduling, built-in Web Management Dashboard, and 100% Go standard library implementation.

> **Project Lineage:** **ZGate** is an independent, production-hardened fork evolved from `zcode-gateway`. We acknowledge and credit the original research by `apissaj`. This project resolves critical security, concurrency, and reliability limitations—adding atomic credential storage, Least-InFlight account scheduling, bounded rate limiting, timing-safe authentication, and an embedded management dashboard.

---

## Key Features & Hardening Overview

- **Dual Wire Dialects:** Native compatibility with both Anthropic (`/v1/messages`) and OpenAI (`/v1/chat/completions`) endpoints.
- **Embedded Web Dashboard:** Single-binary Web GUI (accessible at `http://127.0.0.1:20131/dashboard`) with SVG icons, one-click Z.ai OAuth login, live request traffic monitoring, and instant 9Router configuration generator.
- **VPS Administration Protection (`GATEWAY_ADMIN_KEY`):** Secure the Web Dashboard with password authentication for public remote deployments while keeping it accessible on localhost.
- **Multi-Account Concurrency & Scheduling (ADR 0002):** Dynamic Least-InFlight Round-Robin account selection with per-account concurrency semaphores (`ZCODE_ACCOUNT_CONCURRENCY`) to prevent upstream `429 code 3008` blocks.
- **Zero Runtime Dependencies:** Built strictly with the Go standard library (`//go:embed` for frontend UI). No Node.js, CGO, or external runtime required.
- **Production Hardened:** Timing-attack safe comparisons (`crypto/subtle`), DNS rebinding protection (`ZCODE_ALLOWED_HOSTS`), trusted reverse proxy header evaluation, atomic file persistence, and memory-bounded token-bucket rate limiting.

---

## Supported Models

| Model ID | Reasoning Effort | Target Use Case |
|---|---|---|
| `glm-5.3-flash` | Default (`low`) | High-throughput everyday coding, code completions |
| `glm-5.3-flash:low` | Low | Fast responses with minimal thinking overhead |
| `glm-5.3-flash:high`| High | Balanced, deeper step-by-step reasoning |
| `glm-5.3-flash:max` | Max | Complex algorithmic problems and architecture design |
| `glm-5.3` | Default (`low`) | High-precision flagship reasoning model |
| `glm-5.3:low` | Low | Flagship precision with brief thoughts |
| `glm-5.3:high` | High | Detailed chain-of-thought exploration |
| `glm-5.3:max` | Max | Comprehensive long-form multi-step reasoning |

---

## How to Run

### Mode 1: Interactive Terminal (Standard)

Compile and run directly in your console:

```bash
# Build static binary
go build -o zcode-gateway.exe ./cmd/zcode-gateway

# Run on default port 20131
./zcode-gateway.exe
```

When started, the terminal will print interactive, clickable URLs:
```text
  ┌────────────────────────────────────────────────────────┐
  │ ZGate v0.1.0 (Hardened AI Gateway)                     │
  │ Status: Running                                        │
  │ Web Dashboard : http://127.0.0.1:20131/dashboard      │
  │ API Endpoint  : http://127.0.0.1:20131/v1              │
  └────────────────────────────────────────────────────────┘
```
Hold `Ctrl` and click the Dashboard link to open the GUI in your browser.

---

### Mode 2: Background / Daemon Mode (Headless)

Run the gateway silently in the background without keeping a terminal window open:

#### Windows (PowerShell Background Job)
```powershell
Start-Process -FilePath ".\zcode-gateway.exe" -WindowStyle Hidden
```
To stop the background process:
```powershell
Stop-Process -Name "zcode-gateway"
```

#### Linux VPS (systemd Service)
Use the included production unit:
```bash
sudo cp deployments/systemd/zcode-gateway.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now zcode-gateway
```

#### Docker Container
```bash
docker compose -f deployments/docker/docker-compose.yml up -d
```

---

## Embedded Web Dashboard

Access the graphical dashboard in your browser:
**`http://127.0.0.1:20131/dashboard`**

- **Quick 9Router Setup:** Displays ready-to-use Base URL, model mappings, active API key, and a 1-click **Copy for 9Router** button.
- **Account Management:** Add Z.ai accounts via browser OAuth approval, view token status, cooldown countdowns, and set default fallback accounts.
- **Live Traffic Logs:** Real-time log console showing model calls, latency, and handling accounts.
- **Daemon Control:** Visual status indicators for listener address and active accounts.

### Securing the Dashboard on a VPS (`GATEWAY_ADMIN_KEY`)
When hosting on a remote server, set `GATEWAY_ADMIN_KEY`:
```bash
export GATEWAY_ADMIN_KEY="your-strong-dashboard-password"
./zcode-gateway
```
- If `GATEWAY_ADMIN_KEY` is set, visiting `/dashboard` requires entering this key before viewing or managing accounts.
- If left empty on localhost (`127.0.0.1`), the dashboard is accessible directly without an extra login step.

---

## Connecting to 9Router

In your 9Router configuration:
1. **Provider Type:** `OpenAI` / `Chat Completions`
2. **Base URL:** `http://127.0.0.1:20131/v1`
3. **Prefix:** `zcode`
4. **API Key:** Your `ZCODE_BRIDGE_TOKEN` (or any string if running open on localhost).
5. **Models:** `glm-5.3-flash`, `glm-5.3`

Calls from 9Router will look like `zcode/glm-5.3-flash`.

---

## Configuration Reference

| Environment Variable | Default | Description |
|---|---|---|
| `BIND_HOST` | `127.0.0.1` | Network interface to bind (`127.0.0.1`, `0.0.0.0`, LAN IP) |
| `PORT` | `20131` | TCP port to listen on |
| `GATEWAY_ADMIN_KEY` | *(empty)* | Admin password required to access the Web Dashboard |
| `ZCODE_BRIDGE_TOKEN` | *(empty)* | Secret token required for client API requests (`/v1/*`) |
| `ZCODE_ALLOWED_HOSTS` | `127.0.0.1,localhost` | Allowed HTTP `Host` header values (DNS rebinding protection) |
| `ZCODE_TRUSTED_PROXIES` | *(empty)* | Reverse proxy IPs/CIDRs trusted for `X-Forwarded-For` client resolution |
| `ZCODE_LANE_WIDTH` | `1` | Global maximum concurrent upstream inference streams |
| `ZCODE_ACCOUNT_CONCURRENCY` | `1` | Maximum concurrent requests allowed per individual stored ZCode account |
| `ZCODE_RATE_LIMIT_RPS` | `20` | Token-bucket rate limit refill rate per client IP |
| `ZCODE_RATE_LIMIT_BURST` | `40` | Token-bucket burst limit per client IP |
| `ZCODE_SHUTDOWN_TIMEOUT_MS` | `15000` | Graceful shutdown timeout in milliseconds (15s) |
| `ZCODE_ACCOUNTS_FILE` | *(auto)* | Custom filesystem path for `accounts.json` storage |

---

## License

[MIT](LICENSE) © 2026 apissaj
