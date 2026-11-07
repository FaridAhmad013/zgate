# AGENTS.md — Root AI Agent Guidelines

> Universal instructions, architectural boundaries, and operational rules for AI coding agents operating on the `ZGate` repository.

---

## 1. Project Identity & Purpose

- **Project Name:** ZGate (ZCode Gateway)
- **Type:** Hardened, high-performance, single-binary AI Gateway & Proxy.
- **Upstream Origin:** Independent hardened fork evolved from the original `zcode-gateway`.
- **Core Purpose:** Bridge Anthropic (`/v1/messages`) and OpenAI (`/v1/chat/completions`) wire protocols directly to the Z.ai Start-Plan lane with multi-account load balancing, concurrency safety, zero external runtime dependencies, and an integrated management dashboard.
- **Target Integrations:** 9Router, Claude Code, OpenAI/Anthropic SDKs, LibreChat, and custom local/VPS agent setups.

---

## 2. Hardened Fork Lineage & Value Added

This repository is not a plain rebuild; it contains rigorous architectural improvements over the original upstream implementation:

1. **Security & Data Isolation (P0 Hardening):**
   - Timing-attack safe authentication via `crypto/subtle.ConstantTimeCompare`.
   - DNS rebinding prevention via strict `Host` header validation (`ZCODE_ALLOWED_HOSTS`).
   - Untrusted proxy spoofing mitigation via right-to-left `X-Forwarded-For` evaluation (`ZCODE_TRUSTED_PROXIES`).
   - Memory-bounded rate limiting with hard capacity caps to prevent DoS via random IP flooding (`SEC-01`).
2. **Storage Reliability & Atomic Operations (P1 Reliability):**
   - Multi-account credential store (`accounts.json`) uses atomic temporary writes and safe replace (`SEC-04`).
   - Damaged storage files are preserved via `.corrupt.<timestamp>.<hash8>` without data loss.
   - Per-account singleflight token refreshes preventing OAuth token family invalidation races (`REL-07`).
3. **Concurrency & Head-of-Line Prevention (ADR 0002):**
   - Strict one-way semaphore acquisition (`Global Lane -> Single Account Slot`) guaranteeing freedom from circular-wait deadlocks.
   - Per-account concurrency semaphore (`ZCODE_ACCOUNT_CONCURRENCY`) preventing upstream `429 code 3008` blocks.
   - Dynamic Least-InFlight Round-Robin candidate selection.
4. **Embedded Web Management Dashboard:**
   - Single-binary zero-dependency web interface for account management, token generation, 9Router configuration, and live traffic logging.
   - Protected by `GATEWAY_ADMIN_KEY` for secure public VPS deployments.

---

## 3. Strict Engineering Constraints

AI agents working on this codebase MUST adhere to these rules:

1. **Standard Library Only:**
   - Absolute zero external runtime Go dependencies (`go.mod` must contain only `module github.com/apissaj/zcode-gateway` and Go version directives).
   - Use `//go:embed` for frontend UI assets (HTML/CSS/SVG/JS).
2. **No Emojis in Code / UI:**
   - Use clean, scalable inline SVG icons (Lucide / Heroicons style) instead of decorative emojis across all HTML, logs, and interfaces.
3. **API & Dialect Compatibility:**
   - Retain 100% wire compatibility with Anthropic (`/v1/messages`) and OpenAI (`/v1/chat/completions`) APIs.
   - Ensure proper dialect-specific error schemas (Anthropic format for Anthropic paths; OpenAI format for OpenAI paths).
4. **Credential Redaction:**
   - Never log or return unmasked JWTs, bridge tokens, or API keys. Always filter strings through `auth.RedactSecrets()`.
5. **Quality Verification Before Completion:**
   - Always verify changes with:
     ```bash
     gofmt -l .
     go vet ./...
     go test -count=1 ./...
     go test -race -count=1 ./...
     ```

---

## 4. Repository Structure

```
zcode-gateway/
├── AGENTS.md                          # Root AI Agent guidelines (this file)
├── README.md                          # Primary onboarding, lineage, and operational manual
├── cmd/
│   └── zcode-gateway/
│       └── main.go                    # CLI entrypoint, subcommand routing, banner
├── internal/
│   ├── accounts/                      # Multi-account store, concurrency semaphores, cache
│   ├── auth/                          # OAuth device flow, JWT parsing, secret redaction
│   ├── identity/                      # Hardware / device identity headers
│   ├── models/                        # Dialect translation (OpenAI <-> Anthropic) & validation
│   ├── quota/                         # Upstream quota readouts
│   ├── server/                        # HTTP router, middleware, rate limiter, dashboard handlers
│   │   ├── dashboard.go               # Embedded dashboard API & auth gate
│   │   └── ui/
│   │       └── dashboard.html         # Modern SVG-based web dashboard
│   ├── systemprompt/                  # ZCode identity prompt formatting
│   └── transport/                     # Upstream client, SSE reader, lane management
├── deployments/                       # Systemd unit, Dockerfile, Nginx/Caddy proxies
└── docs/
    ├── adr/                           # Architectural Decision Records (ADR 0001, ADR 0002)
    ├── audit/                         # Security, reliability, and release readiness audits
    └── ops/                           # Production runbook and recovery guides
```

---

## 5. Environment Configuration Standards

| Variable | Default | Purpose |
|---|---|---|
| `BIND_HOST` | `127.0.0.1` | Network interface IP to bind |
| `PORT` | `20131` | Listening TCP port |
| `GATEWAY_ADMIN_KEY` | *(empty)* | Web Dashboard admin password. If empty, dashboard is open on localhost. If set, login is required. |
| `ZCODE_BRIDGE_TOKEN` | *(empty)* | API key required for client requests (`/v1/*`). |
| `ZCODE_ALLOWED_HOSTS` | `127.0.0.1,localhost` | DNS rebinding protection allowed hosts list. |
| `ZCODE_TRUSTED_PROXIES` | *(empty)* | Reverse proxy IP list for trusted X-Forwarded-For parsing. |
| `ZCODE_LANE_WIDTH` | `1` | Global upstream inference concurrency limit. |
| `ZCODE_ACCOUNT_CONCURRENCY`| `1` | Maximum concurrent streams per individual ZCode account. |
| `ZCODE_ACCOUNTS_FILE` | *(auto)* | Path to persistent storage JSON file. |
