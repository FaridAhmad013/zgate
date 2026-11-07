# Security Audit Report — `zcode-gateway`

Audit scope: commit `9e6279e` on branch `feature/security-hardening`. All tests executed locally against in-process `httptest` stubs using synthetic unsigned JWT payloads. No external requests to `zcode.z.ai` were made.

## Automated Scanner Baseline

| Tool | Result |
|---|---|
| `go vet ./...` | Pass (0 warnings) |
| `govulncheck ./...` | Pass (0 known vulnerabilities; stdlib-only module) |
| `gitleaks dir .` / `gitleaks git .` | Pass (0 hardcoded secrets in working tree or git history) |
| `staticcheck ./...` | 5 findings: `identity.go:121` (S1025 redundant `fmt.Sprintf`), `models.go:372` (`hasType` unused), `server.go:616` (`accountIDOf` unused), `server.go:624` (`accountsSummary` unused), `transport.go:258` (`laneMu` unused) |
| `gofmt -l .` | 5 unformatted files (`cmd/zcode-gateway/main.go`, `internal/auth/auth.go`, `internal/identity/identity.go`, `internal/server/routes.go`, `internal/transport/transport.go`) |

---

## Findings Summary

| ID | Severity | Status | Title | Location |
|---|---|---|---|---|
| SEC-01 | High | Confirmed | Cross-Origin Simple-Request CSRF & DNS Rebinding allow browser tabs to spend plan quota when bound to localhost without token | `internal/server/routes.go:25-55, 72-88`, `cmd/zcode-gateway/main.go:158-168` |
| SEC-02 | High | Confirmed | Non-localhost binding (`0.0.0.0`) starts without requiring `ZCODE_BRIDGE_TOKEN`, plus `BIND_HOST` env var mismatch with README | `cmd/zcode-gateway/main.go:158-168`, `README.md:85` |
| SEC-03 | High | Confirmed | Observability & billing routes (`/health`, `/v1/quota`, `/v1/models`) bypass `requireAuth` even when `ZCODE_BRIDGE_TOKEN` is set | `internal/server/routes.go:39-44`, `internal/server/routes.go:90-146` |
| SEC-04 | Medium | Confirmed | Non-atomic `accounts.json` writes and silent overwrite on corrupt/partial read cause permanent credential loss; `sync-token.js` & `plan-jwt.json` lack `0600` enforcement | `internal/accounts/accounts.go:96-122`, `sync-token.js:30` |
| SEC-05 | Medium | Confirmed | Non-constant-time token comparison (`==`) and legacy `Configure()` data race in `authorized()` | `internal/server/server.go:42-50, 72-85` |
| SEC-06 | Medium | Confirmed | Upstream URL image passthrough and unvalidated `ZCODE_INFERENCE_ORIGIN` / `ZCODE_OAUTH_ORIGIN` scheme | `internal/models/models.go:131-147`, `internal/transport/transport.go:50`, `internal/auth/auth.go:33` |
| SEC-07 | Low | Confirmed | Potential token/PII exposure in `AuthError.Body` and upstream error messages | `internal/auth/auth.go:128-131, 201-216, 260`, `internal/quota/quota.go:114-120` |
| SEC-08 | Low | Confirmed | Missing HTTP server `WriteTimeout` / `ReadTimeout` / `MaxHeaderBytes` enables Slowloris body and connection exhaustion | `internal/server/routes.go:182-191` |
| SEC-09 | Informational | Confirmed | Desktop credential store uses deterministic local KDF (`desktopStoreKey`); plaintext at rest in `accounts.json` / `plan-jwt.json` | `internal/auth/auth.go:334-342`, `sync-token.js:11-12` |
| SEC-10 | Informational | Confirmed | `.github/workflows/release.yml` grants `contents: write` at workflow level (including `pull_request` trigger) and references non-existent `go 1.27.2` | `.github/workflows/release.yml:9-10`, `go.mod:3` |

---

## Detailed Findings

### SEC-01 — Cross-Origin Simple-Request CSRF & DNS Rebinding on Localhost
- **Severity:** High
- **Status:** Confirmed
- **Location:** `internal/server/routes.go:25-55`, `internal/server/routes.go:72-88`, `internal/server/routes.go:151-178`
- **Problem:**
  1. `handlePost` (`routes.go:72-88`) reads and parses the request body as JSON regardless of the `Content-Type` header. A malicious website visited in the user's browser can send a cross-origin `fetch("http://127.0.0.1:20131/v1/chat/completions", {method: "POST", headers: {"Content-Type": "text/plain"}, body: JSON.stringify(...)})`. Because `text/plain` is a CORS "simple request", the browser sends NO `OPTIONS` preflight and executes the inference request immediately.
  2. When `ZCODE_CORS_ORIGIN` is unset (default), `cors()` returns `true` without inspecting `Origin` (`routes.go:152-155`), allowing cross-origin `POST` execution (and even if `ZCODE_CORS_ORIGIN` is set to a whitelist, `cors()` does not reject disallowed `Origin` headers on non-OPTIONS requests—it merely omits the `Access-Control-Allow-Origin` response header after letting the request run).
  3. The server does not validate the HTTP `Host` header, so a DNS rebinding attack (`attacker.com` -> `127.0.0.1`) allows a browser tab to bypass Same-Origin Policy entirely and read responses from `/v1/chat/completions`, `/v1/quota`, and `/health`.
- **Exploitation Scenario:** Developer runs `zcode-gateway` locally with default settings (`127.0.0.1:20131`, no `ZCODE_BRIDGE_TOKEN`) and visits any untrusted webpage. JavaScript on that page drains plan quota or exfiltrates account metadata via DNS rebinding.
- **Impact:** Unauthorized consumption of ZCode plan quota, account cooldown exhaustion, and disclosure of `/health` and `/v1/quota` account metadata from any browser tab.
- **Local Proof of Concept:**
  - `TestPoC_CSRF_TextPlain`: `POST /v1/chat/completions` with `Origin: https://evil.example` and `Content-Type: text/plain` returned `200 OK` and executed upstream inference.
  - `TestPoC_HostHeaderNotValidated`: `GET /health` with `Host: attacker.example:20131` returned `200 OK` with full health payload.
- **Remediation:**
  1. Reject `POST` requests whose `Content-Type` media type is not `application/json`.
  2. When an `Origin` header is present and not explicitly allowed by `ZCODE_CORS_ORIGIN`, reject the request with `403 Forbidden` before invoking handlers.
  3. Validate the `Host` header against configured bind host / `localhost` / `127.0.0.1` / `[::1]` when bound to loopback.

---

### SEC-02 — Non-Localhost Binding Permitted Without Authentication & `BIND_HOST` Env Mismatch
- **Severity:** High
- **Status:** Confirmed
- **Location:** `cmd/zcode-gateway/main.go:158-168`, `README.md:85`
- **Problem:**
  1. `serve()` reads `HOST` (`main.go:158`) and starts the server even if `HOST=0.0.0.0` (or a public/LAN IP) and `ZCODE_BRIDGE_TOKEN` is empty. Worse, `main.go:181` logs `auth: open (localhost)` whenever `BridgeToken == ""`, even when bound to `0.0.0.0`.
  2. `README.md:85` documents the environment variable as `BIND_HOST`, whereas `cmd/zcode-gateway/main.go:158` only reads `HOST`. An operator setting `BIND_HOST=127.0.0.1` alongside `HOST=0.0.0.0` in a container or service unit gets unexpected binding behavior.
- **Exploitation Scenario:** User deploys the gateway on a VPS or Docker container (`HOST=0.0.0.0`) for 9Router integration without setting `ZCODE_BRIDGE_TOKEN`. Any host on the network can use the gateway's stored Z.ai credentials without authentication.
- **Impact:** Complete unauthorized access to LLM inference and billing endpoints over the network.
- **Local Proof of Concept:** Verified in `cmd/zcode-gateway/main.go:158-184`: `authMode` is computed solely from `cfg.BridgeToken != ""` with no check on ` net.ParseIP(cfg.Host).IsLoopback()`.
- **Remediation:**
  1. Support both `BIND_HOST` and `HOST` (preferring `BIND_HOST` or `HOST` consistently).
  2. Refuse to start (`os.Exit(1)`) when binding to a non-loopback address (`cfg.Host != "127.0.0.1" && cfg.Host != "localhost" && cfg.Host != "::1"`) unless `ZCODE_BRIDGE_TOKEN` is set (or an explicit `ZCODE_ALLOW_INSECURE_PUBLIC=1` override is provided for trusted internal networks).

---

### SEC-03 — `/health`, `/v1/quota`, and `/v1/models` Bypass Authentication and Leak Account Metadata
- **Severity:** High
- **Status:** Confirmed
- **Location:** `internal/server/routes.go:39-44`, `internal/server/routes.go:90-146`
- **Problem:**
  - `route()` only wraps `POST /v1/messages` and `POST /v1/chat/completions` in `requireAuth`.
  - `GET /health`, `GET /v1/models`, and `GET /v1/quota` (including `?account=<id>`) are completely unauthenticated even when `ZCODE_BRIDGE_TOKEN` is configured.
  - `/health` (`routes.go:91-128`) returns `accounts.Summarize()` (which includes account `id`, `label`, **`email`**, `expiresAt`, `cooldownUntil`, `needsReauth`) and, on the legacy path, decodes the JWT and returns `userId` and `issuedAt`.
  - `/v1/quota` (`routes.go:132-146`) and `/v1/models` (`server.go:505-568`) trigger live upstream HTTP requests to `zcode.z.ai/api/v1/zcode-plan/billing/*` using the stored JWT and return plan names, entitlement IDs, remaining units, and account labels.
- **Exploitation Scenario:** On a VPS or shared network where `ZCODE_BRIDGE_TOKEN` is enabled, an unauthenticated attacker queries `/health` and `/v1/quota` to enumerate account emails, user IDs, and token balances, and repeatedly polls `/v1/quota?account=<id>` to trigger upstream billing requests.
- **Impact:** Information disclosure (account emails, user IDs, quota balances, upstream origin) and unauthenticated upstream request amplification.
- **Local Proof of Concept:** `TestPoC_TokenSetButReadRoutesOpen` and `TestPoC_HealthDisclosure`: with `BridgeToken: "s3cret"`, `POST /v1/chat/completions` returned `401`, while unauthenticated `GET /health`, `GET /v1/models`, and `GET /v1/quota` all returned `200 OK` with `userId: "u_legacy"`, account summaries, and quota data.
- **Remediation:**
  1. Require `requireAuth` on `/v1/models`, `/models`, `/v1/quota`, and `/quota` whenever `BridgeToken != ""`.
  2. Keep `/health` / `/healthz` minimal when unauthenticated (e.g., `{"ok": true}`), or require `BridgeToken` for detailed account/lane diagnostics, and redact `email` / `userId` from unauthenticated responses.

---

### SEC-04 — Non-Atomic Credential Storage, Destructive Corrupt-Read Fallback, and Lax File Permissions
- **Severity:** Medium
- **Status:** Confirmed
- **Location:** `internal/accounts/accounts.go:96-122`, `internal/auth/auth.go:293-309`, `sync-token.js:30`
- **Problem:**
  1. **Non-atomic write:** `writeStore` (`accounts.go:114`) calls `os.WriteFile(AccountsFile(), raw, 0o600)` directly on `data/accounts.json`. `os.WriteFile` truncates the target file before writing. If the process crashes, is killed (`SIGKILL`), or runs out of disk space mid-write, `data/accounts.json` is left empty or truncated.
  2. **Destructive fallback on read error:** `readStore()` (`accounts.go:96-103`) returns an empty `store{Version: 1, Accounts: []Account{}}` on ANY `os.ReadFile` or `json.Unmarshal` error, and callers (`Upsert`, `Remove`, `SetDefault`, `MarkReauth`, `ClearCooldown`) immediately call `writeStore(rec)`, overwriting the file and permanently destroying all other stored accounts.
  3. **Write errors ignored:** `Upsert`, `Remove`, `SetDefault`, `MarkReauth`, and `ClearCooldown` ignore the error returned by `writeStore(rec)` (`_ = writeStore(rec)`).
  4. **Ineffective `0600` window & `sync-token.js`:** `writeStore` writes to a pre-existing file before calling `os.Chmod(0o600)` (`accounts.go:114-120`), and `sync-token.js:30` writes `plan-jwt.json` with default umask (`0644` on Linux/macOS), leaving the raw JWT world-readable on multi-user systems.
- **Exploitation Scenario:**
  - Server crashes or hits disk full during background token refresh (`TryRefreshAccount` -> `Upsert`) or `ClearCooldown` on a normal request; `accounts.json` is truncated and the next request's `ClearCooldown` overwrites it with an empty account list.
  - On a shared Linux host, `node sync-token.js` creates `plan-jwt.json` with mode `0644`, allowing other local users to read the plan JWT.
- **Impact:** Permanent loss of all stored account credentials and refresh tokens; local credential disclosure via `plan-jwt.json`.
- **Local Proof of Concept:** `TestPoC_CorruptStoreWipesAccounts`: stored accounts `a` and `b`, simulated a partial write on `accounts.json`, then called `Upsert("c", ...)` -> `accounts.List()` contained only `[c]`, permanently wiping `a` and `b`.
- **Remediation:**
  1. Write `accounts.json` atomically: write to a temp file in the same directory with mode `0600`, `Sync()`, close, and `os.Rename()` over the target path.
  2. If `accounts.json` exists and is non-empty but fails `json.Unmarshal`, back it up (e.g. `accounts.json.corrupt.<timestamp>`) or refuse to clobber it silently, and log a warning.
  3. Set `{ mode: 0o600 }` in `sync-token.js` (`fs.writeFileSync(target, ..., { encoding: 'utf8', mode: 0o600 })`).

---

### SEC-05 — Non-Constant-Time Bridge Token Comparison and `cfg` Data Race
- **Severity:** Medium
- **Status:** Confirmed
- **Location:** `internal/server/server.go:42-50, 72-85`
- **Problem:**
  1. `authorized(r)` compares `presented == cfg.BridgeToken` using standard string equality (`==`), which short-circuits on length and first differing byte instead of `crypto/subtle.ConstantTimeCompare`.
  2. Package global `cfg` (`server.go:42`) is written by `Configure()` without synchronization while read by HTTP handler goroutines (`authorized`, `liveModels`, `handleHealth`).
- **Exploitation Scenario:** Remote attacker on a low-latency internal network measures response time variance across requests to infer `ZCODE_BRIDGE_TOKEN` length/bytes; concurrent `Configure()` calls in tests or dynamic reloads race with active requests.
- **Impact:** Theoretical timing side-channel against `ZCODE_BRIDGE_TOKEN`; data race under concurrent configuration.
- **Local Proof of Concept:** Code inspection of `internal/server/server.go:84` (`return presented == cfg.BridgeToken`).
- **Remediation:** Hash both `presented` and `cfg.BridgeToken` with `sha256.Sum256` and compare via `subtle.ConstantTimeCompare(a[:], b[:]) == 1` (hashing first also prevents leaking the secret token's length). Protect `cfg` or make it immutable after startup.

---

### SEC-06 — Unvalidated Upstream Origin URLs and Image URL Passthrough
- **Severity:** Medium
- **Status:** Confirmed
- **Location:** `internal/models/models.go:131-147`, `internal/transport/transport.go:50`, `internal/auth/auth.go:33`, `internal/identity/identity.go:194`
- **Problem:**
  1. `ZCODE_INFERENCE_ORIGIN` and `ZCODE_OAUTH_ORIGIN` are concatenated directly with path strings (`InferenceOrigin + PlanAnthropicPath`, `OAuthBase + "/api/v1/oauth/token"`). If misconfigured to an `http://` (plaintext) URL or a URL with embedded query/path components, the gateway transmits `Authorization: Bearer <plan JWT>` and `refresh_token` over cleartext or to unintended paths.
  2. In `ToAnthropicContent` (`models.go:131-147`), non-`data:` `image_url.url` values are forwarded verbatim as `{"type": "image", "source": {"type": "url", "url": u}}` without validating scheme (`https://` only) or length. While the gateway itself does not fetch the image URL (no direct gateway SSRF), forwarding arbitrary internal/file/javascript URLs upstream is unsafe.
  3. In `auth.PollDeviceFlow` (`auth.go:182`), `flow.FlowID` returned by the OAuth init endpoint is interpolated into `fmt.Sprintf("%s/api/v1/oauth/cli/poll/%s", OAuthBase, flow.FlowID)` without `url.PathEscape(flow.FlowID)`.
- **Exploitation Scenario:** Accidental `http://` origin configuration leaks the plan JWT in cleartext; untrusted client passes internal URLs in `image_url`; malicious/MITM OAuth server returns `../../...` in `flow_id` to redirect `PollToken` bearer requests.
- **Impact:** Credential exposure on misconfiguration; path traversal in OAuth poll URL.
- **Local Proof of Concept:** Verified in `internal/auth/auth.go:182` and `internal/models/models.go:142-146`.
- **Remediation:**
  1. Use `url.PathEscape(flow.FlowID)` in `PollDeviceFlow`.
  2. Validate that `image_url` is either a valid `data:image/...;base64,...` URL or a valid `https://` (or `http://`) URL.
  3. Validate origin URLs at startup: require `https://` unless the host is loopback (`127.0.0.1`, `localhost`, `::1`).

---

### SEC-07 — Potential Credential / Token Exposure in Error Responses and Logs
- **Severity:** Low
- **Status:** Confirmed
- **Location:** `internal/auth/auth.go:128-131, 201-216, 260`, `internal/quota/quota.go:114-120`, `cmd/zcode-gateway/main.go:230`
- **Problem:**
  1. `AuthError.Body` stores raw `string(body)` from OAuth endpoints (`auth.go:128, 131, 201, 206, 216, 260`). If an upstream OAuth response is partially malformed or unexpected, `Body` can contain `token`, `access_token`, or `refresh_token` in memory.
  2. `cmd/zcode-gateway/main.go:230` logs `fmt.Fprintf(os.Stderr, "[jwt-refresh] account %s refresh failed: %v — re-login required\n", a.ID, err)` and `server/routes.go:142` returns `err.Error()` on `/v1/quota`. If an `http.Client` error or upstream error echoes query parameters, headers, or raw body text, sensitive substrings (`eyJ...`, `Bearer ...`) are not redacted before logging or returning to clients.
- **Exploitation Scenario:** Upstream OAuth or proxy returns a verbose error message echoed into stderr logs or `/v1/quota` HTTP 502 responses.
- **Impact:** Potential leakage of tokens or upstream internals into systemd/Docker logs or client error payloads.
- **Local Proof of Concept:** Code inspection of `internal/auth/auth.go` and `cmd/zcode-gateway/main.go`.
- **Remediation:** Add a central `Redact(s string) string` helper that scrubs `Bearer <token>`, JWT shapes (`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`), and `enc:v1:...` strings from all log lines and outbound error messages, and truncate stored `AuthError.Body` snippets.

---

### SEC-08 — Missing HTTP Server Timeouts and Header Limits
- **Severity:** Low
- **Status:** Confirmed
- **Location:** `internal/server/routes.go:182-191`, `cmd/zcode-gateway/main.go:204-210`
- **Problem:**
  1. `http.Server` sets `ReadHeaderTimeout: 10s` and `IdleTimeout: 66s`, but sets neither `ReadTimeout` (or per-request `http.TimeoutHandler` / body read deadline via `http.ResponseController`) nor `MaxHeaderBytes`. A client can send headers in 1s and then drip a 32 MiB request body at 1 byte per minute indefinitely, tying up goroutines and memory.
  2. Shutdown in `cmd/zcode-gateway/main.go:204-209` does not call `srv.Shutdown(ctx)`—it sleeps `500ms` and calls `os.Exit(0)`, abruptly severing active connections and any in-progress `accounts.json` writes.
- **Exploitation Scenario:** Slow-body POST requests hold open goroutines and up to 32 MiB buffers per connection; `SIGTERM` during `login`/refresh kills the process without draining.
- **Impact:** Resource exhaustion (RAM/goroutines) and unclean shutdown.
- **Local Proof of Concept:** Code inspection of `internal/server/routes.go:182-191` and `cmd/zcode-gateway/main.go:204-209`.
- **Remediation:** Set `MaxHeaderBytes: 1 << 20` (1 MiB), apply a body read deadline via `http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))` inside `readBody(w, r)` before streaming begins, and replace `time.Sleep(500ms); os.Exit(0)` with `srv.Shutdown(shutdownCtx)`.

---

### SEC-09 — Desktop Credential Store Uses Deterministic KDF; Local Stores Are Plaintext at Rest
- **Severity:** Informational
- **Status:** Confirmed
- **Location:** `internal/auth/auth.go:334-342`, `internal/accounts/accounts.go:105-122`, `sync-token.js:11-12`
- **Problem:**
  - `desktopStoreKey()` derives the AES-256-GCM key as `SHA-256("zcode-credential-fallback:<os>:<homedir>:<username>")`. Because OS, home directory, and username are public to any process running on the same machine, this key provides obfuscation, not cryptographic protection against same-user or root processes (this is dictated by the upstream ZCode desktop app's fallback format).
  - `data/accounts.json` and `plan-jwt.json` store JWTs and refresh tokens in plaintext JSON protected only by OS file permissions (`0600`).
- **Impact:** Any process running under the same OS user (or root) can read stored JWTs and refresh tokens.
- **Remediation:**
  1. Document clearly in `SECURITY.md` that desktop store encryption is for compatibility with the ZCode desktop app's fallback format and does not protect against same-user access.
  2. Enforce strict `0700` directory and `0600` file permissions on all credential files, and optionally support encrypting `accounts.json` fields with a key supplied via `ZCODE_STORAGE_KEY` environment variable (AES-256-GCM with random 12-byte nonce, explicitly documenting that security depends on keeping `ZCODE_STORAGE_KEY` in the service manager/secret store and off the filesystem).

---

### SEC-10 — GitHub Actions Workflow Permissions & Unreleased Go Version in `go.mod`
- **Severity:** Informational
- **Status:** Confirmed
- **Location:** `.github/workflows/release.yml:9-10`, `go.mod:3`
- **Problem:**
  1. `.github/workflows/release.yml` sets top-level `permissions: contents: write` across all jobs, including the `test` job that runs on `pull_request`. Least-privilege practice is `permissions: contents: read` at the top level and `permissions: contents: write` only on the `release` job.
  2. `go.mod` specifies `go 1.27.2`, which does not exist in upstream Go distributions (`actions/setup-go` will fail on standard runners).
- **Remediation:** Scope `contents: write` to the `release` job only, and set `go 1.22` (or `go 1.23`) in `go.mod`.
