# Repository Analysis — `zcode-gateway`

Audit snapshot: branch `feature/security-hardening`, commit `9e6279e` (`feat: initial commit for zcode-gateway`).

## 1. Package Inventory & Responsibilities

| Path | LOC | Responsibility | Shared State / Concurrency Primitives |
|---|---:|---|---|
| `cmd/zcode-gateway/main.go` | 236 | CLI entry point (`serve`, `login`, `version`), signal handling (`SIGINT`/`SIGTERM`), 15m background refresh ticker. | 2 background goroutines (`time.NewTicker`, `<-ctx.Done()`). |
| `internal/server/routes.go` | 193 | HTTP server (`ReadHeaderTimeout: 10s`, `IdleTimeout: 66s`), route table, `requireAuth`, `handlePost` (32 MiB limit), `/health`, `/v1/quota`, CORS preflight (`ZCODE_CORS_ORIGIN`). | Reads package global `cfg`. |
| `internal/server/server.go` | 624 | `Config`, `authorized`, `SessionKeyFor`, `handleMessages`, `handleChatCompletions`, `pipeStream`, `toOpenAIChunk`, `liveModels` cache. | `cfg` (unprotected write in `Configure`, concurrent reads); `modelsMu sync.Mutex` + `modelsCache`. |
| `internal/transport/transport.go` | 569 | Upstream plan-lane client, `Candidates`, `ShapePlanBody`, account rotation (`SendAnthropic`), per-account retry/refresh (`runAccount`), `attemptOnce`, `cancelBody`, `WithLane`. | `InferenceOrigin` (package var); `laneOnce sync.Once`, `laneSem chan struct{}`, `laneWg sync.WaitGroup`, `laneMu` (unused). **Note:** `WithLane` is exported but never called. |
| `internal/accounts/accounts.go` | 426 | Multi-account store (`data/accounts.json`, mode `0600`), `Upsert`, `Remove`, `SetDefault`, `List`, `Usable`, `MarkCooldown`, `MarkReauth`, `ClearCooldown`, `Summarize`, `TryRefreshAccount`. | `mu sync.Mutex` guarding file I/O and `runtime map[string]*state`. Drops lock across network call in `TryRefreshAccount`. |
| `internal/auth/auth.go` | 404 | Unverified JWT claim decoding (`DecodeJWT`), `IsExpired`, OAuth device flow (`StartDeviceFlow`, `PollDeviceFlow`), `RefreshPlanJWT`, `ResolvePlanJWT`, `ReadBridgeTokenFile`, desktop store AES-256-GCM decryption (`DesktopStoreJWT`). | `OAuthBase` (package var). |
| `internal/auth/http.go` | 55 | Thin `http.Client{Timeout: 30s}` wrapper (`newRequest`, `doRequest` with 1 MiB response cap). | `randRead` (test seam). |
| `internal/identity/identity.go` | 199 | Builds desktop companion headers: LLM face (`BuildLLMHeaders`, no `X-Device-Mid`) and control/billing face (`BuildControlHeaders`, with `X-Device-Mid`). Printable-ASCII gate (`Printable`). Reads `%APPDATA%\ZCode\.updaterId`. | `asciiPrintable` regex. |
| `internal/systemprompt/systemprompt.go` | 146 | Embeds `identity_system.json` (`//go:embed`), enforces 3-block `system` array (`BuildSystem`), HMAC-SHA256 `session_<32hex>` (`SessionIDFor`), `MetadataUserID`. | `identity Asset` populated in `init()`. |
| `internal/ quota/quota.go` | 415 | Queries `/api/v1/zcode-plan/billing/balance` (control headers) and fallback `/billing/current` (bare auth), merges entitlements + usage (`ReadQuota`, `ByModel`). | Stateless; creates `http.Client{Timeout: 20s}` per call. |
| `internal/models/models.go` | 580 | Model normalization (`NormalizeModel`, `ExtractModelAndEffort`), OpenAI <-> Anthropic body/stream/tool/image/usage translation (`OpenAIToAnthropic`, `AnthropicToOpenAI`, `ToAnthropicContent`). | Read-only maps/slices/regexes. |
| `sync-token.js` | 35 | Standalone Node.js helper: decrypts `~/.zcode/v2/credentials.json` and writes `./plan-jwt.json`. | Direct `fs.writeFileSync` (no mode set). |

## 2. Architecture Diagram

```
+-------------------------------------------------------------------------+
| Clients: 9Router / Claude Code / OpenAI SDK / curl                      |
+------------------------------------+------------------------------------+
                                     | HTTP (default 127.0.0.1:20131)
                                     v
+-------------------------------------------------------------------------+
| internal/server (routes.go, server.go)                                  |
|  cors() -> route()                                                      |
|   +-- GET  /health, /healthz         (NO AUTH) -> handleHealth          |
|   +-- GET  /v1/models, /models       (NO AUTH) -> liveModels (30s TTL)  |
|   +-- GET  /v1/quota, /quota         (NO AUTH) -> handleQuota           |
|   +-- POST /v1/messages              (requireAuth) -> handleMessages    |
|   +-- POST /v1/chat/completions      (requireAuth) -> handleChatComp.   |
+-----------------+-----------------------------------+-------------------+
                  |                                   |
                  | /v1/models, /v1/quota             | Inference POST
                  v                                   v
+----------------------------------+  +-----------------------------------+
| internal/quota                   |  | internal/models                   |
|  ReadQuota()                     |  |  ExtractModelAndEffort()          |
|   1. GET .../billing/balance     |  |  NormalizeModel()                 |
|      (identity.BuildControlHdrs) |  |  OpenAIToAnthropic()              |
|   2. Fallback: .../billing/curr  |  |  AnthropicToOpenAI()              |
|      (bare Bearer header)        |  +-----------------+-----------------+
+-----------------+----------------+                    |
                  |                                     v
                  |                   +-----------------------------------+
                  |                   | internal/transport                |
                  +------------------>|  Candidates() -> SendAnthropic()  |
                                      |   -> runAccount() -> attemptOnce()|
                                      |   * ShapePlanBody (systemprompt)  |
                                      |   * BuildLLMHeaders (identity)    |
                                      +--------+-----------------+--------+
                                               |                 |
                      +------------------------+                 +--------+
                      v                                                   v
+---------------------------------------------+   +-----------------------+
| Credential Stores                           |   | Upstream Z.ai         |
|  1. ZCODE_PLAN_JWT env                      |   |  https://zcode.z.ai   |
|  2. plan-jwt.json (or ZCODE_PLAN_JWT_FILE)  |   |  - /api/v1/zcode-plan |
|  3. data/accounts.json (internal/accounts)  |   |    /anthropic/v1/     |
|  4. ~/.zcode/v2/credentials.json            |   |    messages           |
|     (internal/auth AES-256-GCM fallback)    |   |  - /api/v1/oauth/...  |
+---------------------------------------------+   +-----------------------+
```

## 3. Request Lifecycle (`POST /v1/chat/completions` & `POST /v1/messages`)

1. **Ingress (`internal/server/routes.go:25-55`)**:
   - `cors(w, r)` evaluates `ZCODE_CORS_ORIGIN`. `OPTIONS` returns `204` immediately.
   - `accountID` extracted from `?account=` or `X-Zcode-Account`.
   - `requireAuth` checks `authorized(r)`: if `cfg.BridgeToken != ""`, compares against `Authorization: Bearer <tok>` or `X-Api-Key`.
   - `handlePost` reads up to `32 MiB + 1` via `io.LimitReader`, rejects `> 32 MiB` with `413`, unmarshals JSON into `map[string]any`.
2. **Dialect Translation (`internal/server/server.go:184-251`, `internal/models/models.go`)**:
   - `/v1/messages`: normalizes `model` to lowercase, extracts `:low|:high|:max|:medium` suffix or `reasoning_effort`, injects `thinking: {type:"enabled", effort:...}` when absent.
   - `/v1/chat/completions`: `models.OpenAIToAnthropic(body)`:
     - Hoists `system`/`developer` messages into top-level `system` string.
     - Converts `role:"tool"` into `role:"user"` with `type:"tool_result"` blocks; merges consecutive same-role messages via `asBlockList`.
     - Converts assistant `tool_calls` into `type:"tool_use"` blocks.
     - Converts `image_url` (`data:...;base64,...` -> base64 block; `http(s)://...` -> url block).
     - Maps OpenAI `tools` and `tool_choice` (`auto`/`required`/`none`/named function) to Anthropic `input_schema` and `tool_choice`.
   - `SessionKeyFor(r, body)` derives conversation key from `body["user"]` -> `metadata.user_id` -> `X-Session-Id` -> `anon:<first_user_384B>:<len(messages)>`.
3. **Candidate Selection & Shaping (`internal/transport/transport.go:193-255, 488-539`)**:
   - `Candidates(jwt, accountID)` resolves candidate slice: explicit `JWT` -> pinned `accountID` in `accounts.List()` -> `accounts.Usable(nil)` -> `auth.ResolvePlanJWT("")`.
   - `attemptOnce`:
     - Derives `session_<32hex>` via `systemprompt.SessionIDFor(clientSessionID, acct.SessionSeed)`.
     - Builds headers via `identity.BuildLLMHeaders` (no `X-Device-Mid`, no signing headers).
     - `ShapePlanBody` deep-copies body via JSON round-trip, replaces `system` with the mandatory 3-block array (`systemprompt.BuildSystem`), sets `metadata.user_id`, defaults `max_tokens` to `8192`.
     - Wraps `ctx` in `context.WithTimeout(ctx, timeout)` (default 5m) and executes `client.Do(req)` where `client = &http.Client{Timeout: timeout}`. Wraps `resp.Body` in `cancelBody`.
4. **Retry, Refresh & Rotation (`internal/transport/transport.go:338-485`)**:
   - Per account (`runAccount`), loops up to `ZCODE_MAX_ATTEMPTS` (default 3) with exponential backoff `baseMs * 2^n` (default `1500ms`):
     - `2xx`: returns response; `SendAnthropic` calls `accounts.ClearCooldown(acct.ID)`.
     - `401` (once per `runAccount`): if stored account, calls `auth.RefreshPlanJWT` and `accounts.Upsert`, decrements `n`, retries; if legacy token, re-runs `auth.ResolvePlanJWT("")`.
     - Switchable (`1113`, `3008`, `401`, `403`, or `429` with `code != 3009`): sets `AccountSwitch = true`, breaks out of `runAccount`. `SendAnthropic` marks `MarkReauth` (`401`/`403`) or `MarkCooldown` (15m default for `1113`/`429`; 2m otherwise) and advances to next candidate.
     - Transient (`3009`, `429`, `5xx`): backs off in-place up to `maxAttempts`.
5. **Egress / SSE Streaming (`internal/server/server.go:208-393`)**:
   - Non-stream: reads up to 32 MiB; passes through raw JSON for Anthropic or translates via `models.AnthropicToOpenAI`.
   - Stream (`pipeStream`): reads 16 KiB chunks.
     - Anthropic: raw byte passthrough + `Flush()`.
     - OpenAI: splits buffer on `"\n\n"`, extracts first `data:` line per frame, unmarshals JSON, maps `message_start` / `content_block_start` (`tool_use`) / `content_block_delta` (`text_delta`, `thinking_delta`, `input_json_delta`) / `message_delta` to `chat.completion.chunk`, and emits `data: [DONE]\n\n` on `message_stop`.

## 4. Authentication & Token Lifecycle

```
[CLI: zcode-gateway login]
  -> auth.StartDeviceFlow: POST /api/v1/oauth/cli/init (32B random PollToken)
  -> User approves AuthorizeURL in browser
  -> auth.PollDeviceFlow:  GET  /api/v1/oauth/cli/poll/{flow_id}
  -> accounts.Upsert:      writes data/accounts.json (0600)
                           (sets synthetic 24h ExpiresAt if JWT has no exp!)

[Runtime Resolution: transport.Candidates]
  1. Explicit SendOpts.JWT
  2. Pinned ?account= / X-Zcode-Account (from accounts.List(), bypasses Usable checks)
  3. accounts.Usable(nil): skips NeedsReauth, cooledDown, or ExpiresAt <= now
  4. auth.ResolvePlanJWT(""):
       a. ZCODE_PLAN_JWT env
       b. ZCODE_PLAN_JWT_FILE (default ./plan-jwt.json)
       c. Desktop store ~/.zcode/v2/credentials.json (AES-256-GCM via SHA-256 fallback KDF)
       (Note: AGENTS.md states accounts store is in ResolvePlanJWT; in actual code,
        ResolvePlanJWT does NOT read accounts.json — Candidates() checks accounts.Usable first.)

[Refresh Paths]
  A. Reactive (on upstream 401 in transport.runAccount):
     - Stored account: auth.RefreshPlanJWT(POST /api/v1/oauth/token) -> accounts.Upsert (exp=0 if no exp claim)
     - Legacy token:   re-reads auth.ResolvePlanJWT("") (e.g. desktop app rotated credentials.json)
  B. Proactive (every 15m in cmd/zcode-gateway/main.go:refreshExpiring):
     - Loops accounts.List(); if RefreshToken != "" && ExpiresAt > 0 && ExpiresAt - now <= 2h:
       calls accounts.TryRefreshAccount -> on failure calls accounts.MarkReauth(id)
```

## 5. Dependency Inventory

| Dependency | Type | Version | Status / Vulns | Notes |
|---|---|---|---|---|
| Go Standard Library | Runtime / Stdlib | `go 1.27.2` in `go.mod` (local toolchain `go1.27.3`) | `govulncheck`: **0 vulnerabilities** | Zero external Go modules (`go.sum` absent by design). |
| Node.js `fs`, `os`, `crypto`, `path` | Optional helper (`sync-token.js`) | Node built-ins | None | Redundant with `auth.DesktopStoreJWT()` already implemented in Go. |

- **Unused external dependencies:** None (0 external Go dependencies).
- **Go version compatibility risk:** `go.mod` declares `go 1.27.2` and `.github/workflows/release.yml` uses `actions/setup-go@v5` with `go-version-file: go.mod`. Standard upstream Go releases are currently on the `1.22`–`1.24` line; `go 1.27.2` will fail on standard GitHub Actions runners unless lowered to a released Go version (e.g., `go 1.22` or `1.23` — code only uses stdlib features available since Go 1.21/1.22 `min`/`max`).
