# Functional & Reliability Audit Report — `zcode-gateway`

Audit scope: commit `9e6279e` on branch `feature/security-hardening`. Verified via source inspection and local deterministic test cases against an in-process stub gateway.

## Summary of Functional & Reliability Findings

| ID | Severity | Component | Summary |
|---|---|---|---|
| REL-01 | High | `internal/transport` | `WithLane` concurrency gate is dead code (never called); concurrent requests bypass lane serialization and trigger upstream `429 3008` / `3009` |
| REL-02 | High | `internal/server` (`pipeStream`) | OpenAI SSE parser fails completely on `\r\n` (CRLF) line endings and multi-line `data:` frames |
| REL-03 | High | `internal/server` (`pipeStream`) | Upstream mid-stream `event: error` frames are silently dropped in OpenAI streaming mode, and stream truncation returns no error or `[DONE]` |
| REL-04 | High | `internal/transport` | `http.Client{Timeout: timeout}` (default 5m) acts as a hard total-request deadline, killing active SSE streams mid-generation |
| REL-05 | High | `cmd/zcode-gateway`, `internal/accounts` | Synthetic 24h `ExpiresAt` assigned to `exp`-less plan JWTs violates Rule #4 (`AGENTS.md`) and permanently parks non-refreshable accounts after 24h |
| REL-06 | Medium | `internal/transport` | Single-account setup gets cooled down for 15 minutes on a transient `429 3008` (desktop app busy), causing subsequent requests to fail `500 No zcode plan JWT available` |
| REL-07 | Medium | `internal/transport`, `internal/accounts` | Concurrent `401` responses trigger parallel refresh-token grants (thundering herd; no per-account singleflight lock), risking refresh token invalidation |
| REL-08 | Medium | `internal/server` (`SessionKeyFor`) | Anonymous session affinity key includes `len(msgs)`, changing `session_<32hex>` on every turn of a conversation and tripping upstream session-churn heuristics |
| REL-09 | Medium | `internal/server` (`routes.go`, `server.go`) | Error dialect & HTTP status mapping bugs: `handlePost` returns Anthropic error format on OpenAI route; upstream `401`/`405` forwarded raw to clients |
| REL-10 | Medium | `internal/server`, `internal/models` | Missing request validation: empty/non-array `messages` forwarded upstream; OpenAI streaming `message_start` chunk omits upstream message `id` |
| REL-11 | Low | `internal/transport`, `internal/quota`, `internal/auth` | New `http.Client` (and new TCP/TLS connection pool) allocated on every request; `cmd/zcode-gateway` lacks `http.Server.Shutdown` |

---

## Detailed Findings

### REL-01 — `transport.WithLane` Is Dead Code (Never Called)
- **Location:** `internal/transport/transport.go:257-292, 312-387`
- **Problem:** `AGENTS.md` (Section 2 & 5) and the doc comment on `SendAnthropic` (`transport.go:305-306`) state that all inference calls are serialized through `WithLane` (default width `1`, configurable via `ZCODE_LANE_WIDTH`) so concurrent bridge requests queue instead of tripping upstream per-account concurrency limits (`429 3008`). In reality, `WithLane` is never called anywhere in the repository (`laneMu` is also unused).
- **Evidence:** `TestPoC_LaneNotEnforced`: 5 concurrent `POST /v1/chat/completions` requests resulted in `maxInflight = 5` simultaneous upstream requests with default `ZCODE_LANE_WIDTH=1`.
- **Impact:** Parallel requests from 9Router or IDE agents hit `zcode.z.ai` concurrently, triggering `429 3008` (account concurrency limit) and `429 3009` (model concurrency limit).
- **Note on Streaming & Lane Scope:** Because `SendAnthropic` returns an unread `*http.Response` body for streaming, holding the lane slot only until `SendAnthropic` returns would release the slot before the SSE stream finishes; holding it until `cancelBody.Close()` serializes the entire stream lifetime. Furthermore, a single global lane blocks Account B while Account A is streaming. Per-account semaphore (or global lane released on `cancelBody.Close()`) should be used.

---

### REL-02 — OpenAI SSE Parser Breaks on CRLF (`\r\n`) and Multi-Line `data:` Frames
- **Location:** `internal/server/server.go:299-331, 365-393`
- **Problem:**
  1. `pipeStream` searches only for `strings.Index(carry, "\n\n")`. Under the SSE specification (WHATWG HTML), frames may be delimited by `\r\n\r\n`, `\n\n`, or `\r\n\n`. If an upstream proxy, CDN, or HTTP layer emits `\r\n\r\n`, `strings.Index(carry, "\n\n")` returns `-1` for the entire stream, buffering everything until EOF and then processing only the first `data:` line in `flushOpenAIFrame`, dropping all subsequent tokens and `[DONE]`.
  2. Within a frame, `for _, line := range strings.Split(frame, "\n")` breaks on the first `data:` line (`server.go:307-312`), ignoring multi-line `data:` payloads which SSE specifies should be joined with `\n`.
- **Evidence:** `TestPoC_StreamCRLF`: an upstream SSE response using standard `\r\n\r\n` delimiters produced `HELLO=false WORLD=false DONE=false` (zero OpenAI chunks emitted during streaming).
- **Impact:** Complete silent failure of OpenAI streaming if upstream or an intermediary uses CRLF line endings.
- **Remediation:** Normalize `\r\n` -> `\n` (or scan lines with a proper SSE frame scanner) and join all `data:` lines within a single event frame before JSON unmarshaling.

---

### REL-03 — Mid-Stream Upstream `error` Events Dropped in OpenAI Mode; Unclean Stream Termination
- **Location:** `internal/server/server.go:320-358, 405-489`
- **Problem:**
  1. When Anthropic upstream encounters an error mid-stream (after HTTP `200 OK` headers are sent), it sends an SSE event `event: error\ndata: {"type":"error","error":{"type":"overloaded_error","message":"..."}}\n\n` and closes the stream without `message_stop`.
  2. In `toOpenAIChunk` (`server.go:417-488`), `case "error":` is not handled—it returns `nil`. The stream then hits `io.EOF` and returns cleanly (`server.go:347`), so the OpenAI client receives partial tokens, **no error frame**, and **no `data: [DONE]`**.
- **Evidence:** `TestPoC_StreamErrorDropped`: upstream emitted `message_start`, one `text_delta` (`"PART"`), and an Anthropic `error` event -> OpenAI client received `"PART"` and EOF with `contains error=false DONE=false`.
- **Impact:** Clients (9Router, OpenAI SDK, Claude Code) treat truncated/failed streams as complete responses or hang waiting for `[DONE]`.
- **Remediation:** Handle `case "error":` in `toOpenAIChunk` (or `pipeStream`) by forwarding a structured OpenAI error frame (`data: {"error": ...}\n\n`) and `data: [DONE]\n\n`.

---

### REL-04 — `http.Client.Timeout` Kills Long-Running SSE Streams
- **Location:** `internal/transport/transport.go:333-336, 517-538`
- **Problem:** `SendAnthropic` creates `client = &http.Client{Timeout: timeout}` AND `callCtx, cancel := context.WithTimeout(ctx, timeout)` (default 5 minutes). In Go's `net/http`, `http.Client.Timeout` and `req.Context()` both cover the **entire lifetime of the response body read**, not just response header receipt. Any reasoning-heavy (`:max` effort, 128k output) stream taking longer than `timeout` is forcibly aborted mid-stream with `context deadline exceeded`.
- **Evidence:** `TestPoC_StreamCutByClientTimeout`: a healthy stream actively flushing tokens every 100ms was killed at 300ms when `ZCODE_UPSTREAM_TIMEOUT_MS=300`, emitting `"context deadline exceeded"` mid-sentence.
- **Impact:** Long thinking/coding generations (`glm-5.3:max`) are prematurely terminated even while actively streaming tokens.
- **Remediation:** Use a shared `http.Transport` with `ResponseHeaderTimeout` for time-to-first-byte, and wrap `resp.Body` in an idle-timeout reader (resets timer on every `Read(p)` that makes progress) rather than a fixed wall-clock timeout across the entire stream.

---

### REL-05 — Synthetic 24-Hour `ExpiresAt` Breaks `exp`-less Plan JWTs
- **Location:** `cmd/zcode-gateway/main.go:128-134`, `internal/accounts/accounts.go:274-276, 394-396`
- **Problem:**
  - `AGENTS.md` Rule #4 explicitly states: *"The plan JWT has no `exp`. Claims are `{user_id, token_version, sub, iat}` only - a valid token does not expire on its own. Do not add expiry heuristics."*
  - Despite this, `deviceLogin` (`main.go:132-134`) and `TryRefreshAccount` (`accounts.go:394-396`) set `expiresAt = time.Now().Add(24 * time.Hour).UnixMilli()` whenever the JWT has no `exp` claim!
  - Worse:
    1. Within 22 hours, `refreshExpiring` (`main.go:217-235`) sees `ExpiresAt - now <= 2h` and attempts a proactive refresh (`TryRefreshAccount`). As documented in `auth.go:233-236`, the refresh grant is undocumented/best-effort; if the refresh endpoint rejects it, `TryRefreshAccount` calls `MarkReauth(id)` (`accounts.go:384`), permanently disabling a still-valid JWT!
    2. If the account had no `RefreshToken`, after 24 hours `accounts.Usable` (`accounts.go:274`) skips the account (`a.ExpiresAt > 0 && a.ExpiresAt <= now.UnixMilli()`), permanently dropping a valid non-expiring plan JWT.
- **Evidence:** `TestPoC_SyntheticExpiryExcludes`: an `exp`-less JWT stored with past `expiresAt` returned `usable = 0` from `accounts.Usable(nil)`.
- **Impact:** Healthy accounts stop working after 22–24 hours even when their plan JWT has not been revoked upstream.
- **Remediation:** Leave `ExpiresAt = 0` when the JWT has no `exp` claim (matching `runAccount` at `transport.go:445-450` and `AGENTS.md` Rule #4), and rely on reactive 401 refresh (`runAccount`) when a token has no real `exp`.

---

### REL-06 — Single Account Cooled Down for 15 Minutes on Transient `429 3008`
- **Location:** `internal/transport/transport.go:138-152, 371-380, 426-428, 469-471`
- **Problem:**
  - Upstream code `3008` ("account concurrency limit - the desktop client is busy on this account") is classified as `isSwitchable = true` (`transport.go:141`).
  - In `runAccount` (`transport.go:469-471`), `if lastErr.AccountSwitch { return nil, lastErr }` executes **before** the transient retry loop (`isTransient` at line 472)! Thus, `3008` gets **zero in-place retries** even when there is only 1 account configured.
  - Then, in `SendAnthropic` (`transport.go:375-376`), because `3008` arrives with HTTP status `429`, `case ue.Code == float64(1113) || ue.Status == http.StatusTooManyRequests:` calls `accounts.MarkCooldown(acct.ID, 0)`, which parks the account for **15 minutes** (`cooldownDefault()`)!
  - On the very next request, `accounts.Usable(nil)` returns empty, `auth.ResolvePlanJWT("")` returns empty (if using `zcode-gateway login`), and the gateway returns `500 No zcode plan JWT available` for the next 15 minutes!
- **Evidence:** `TestPoC_Transient3008ParksOnlyAccount`: with a single stored account `"solo"`, one `429 {"code": 3008}` on request #1 caused request #1 to fail immediately without retry (`upstream calls = 1`) and parked `"solo"` for 15 minutes; request #2 immediately failed with `500 No zcode plan JWT available`.
- **Impact:** A momentary overlap with the ZCode desktop app locks out a single-account user for 15 minutes.
- **Remediation:**
  1. Treat `3008` as transient in-place retry first (or only switch immediately when `len(cands) > 1`), and never apply a 15-minute quota cooldown for `3008` (at most a few seconds, or no cooldown when it is the last/only candidate).
  2. In `Candidates()`, if all stored accounts in `accounts.List()` are in cooldown (and none are `NeedsReauth`), fall back to the soonest-to-wake account rather than returning a false `"No zcode plan JWT available"` error.

---

### REL-12 — Unbounded Per-Account Concurrency & Failover Selection Bias
- **Location:** `internal/transport/transport.go:415-460`, `internal/accounts/accounts.go:490-530`
- **Problem:** `accounts.Usable()` ordered accounts in strict failover order (default account always first, never round-robin), while `SendAnthropic` only enforced a global process lane (`ZCODE_LANE_WIDTH`). When `ZCODE_LANE_WIDTH > 1` (e.g. 4 or 8), all concurrent requests selected the same default account simultaneously, triggering upstream account-concurrency blocks (`429 code 3008`).
- **Remediation (Phase 6.1, ADR 0002):** Implemented `accounts.AcquireCandidate` combining a per-account concurrency semaphore (`ZCODE_ACCOUNT_CONCURRENCY`, default `1`), least-in-flight sorting, and round-robin sequence tracking (`selectSeq`).
- **Operational Limitation (Phase 7.1 HoL Audit):** When requests are pinned explicitly (`?account=a`) to an account that is already at its concurrency limit, the pinned request holds its acquired global lane slot while waiting for that account, blocking unpinned requests from entering the global lane even if other accounts are idle (`TestExtendedHeadOfLineBlockingWithPinnedRequestHoldingLane`). Unpinned routing is strongly recommended.
- **Status:** **Fixed & Documented** in Phase 6.1 and Phase 7.1.

---

### REL-13 — Redundant File I/O & Storage Lock Contention on Account Selection
- **Location:** `internal/accounts/accounts.go:190-270`
- **Problem:** `loadStore()` read and unmarshaled `accounts.json` from disk on every candidate selection, becoming a CPU and mutex bottleneck under concurrent load.
- **Remediation (Phase 6.1, ADR 0002):** Implemented content-validated cache with burst suppression (<5ms) and `os.Stat` metadata validation (>25ms), retaining byte-level verification on writes and external edits.
- **Status:** **Fixed** in Phase 6.1. Verified by `TestAccountCacheConsistencyOnInsertDeleteRefreshExternalEditAndCorrupt` and benchmarks.

---

### REL-07 — Concurrent `401` Responses Trigger Duplicate Token Refreshes (Race / Thundering Herd)
- **Location:** `internal/transport/transport.go:435-468`, `internal/accounts/accounts.go:365-399`
- **Problem:** Neither `runAccount` nor `TryRefreshAccount` serializes refresh calls per account. When 5 concurrent requests use the same expired/revoked account, all 5 receive `401`, and all 5 simultaneously send `POST /api/v1/oauth/token` with the same `refresh_token` and overwrite `accounts.json`. OAuth servers with refresh-token rotation invalidate the `refresh_token` on first use, causing the other 4 concurrent refresh calls to fail and potentially revoking the token family.
- **Evidence:** `TestPoC_ConcurrentRefresh`: 5 concurrent requests on account `"a"` resulted in `refreshCalls = 5` simultaneous `POST /api/v1/oauth/token` calls.
- **Impact:** Refresh token rotation race can invalidate the account and force manual re-login.
- **Remediation:** Guard token refresh with a per-account mutex / singleflight inside `accounts.TryRefreshAccount(ctx, id, failedJWT)`: if the stored `JWT` has already changed from `failedJWT` while waiting for the lock, return the newly stored JWT without calling the OAuth endpoint again.

---

### REL-08 — Anonymous `SessionKeyFor` Changes Every Turn
- **Location:** `internal/server/server.go:121-138`
- **Problem:** The doc comment on `SessionKeyFor` (`server.go:99-104`) explains: *"The gateway meters plan usage per session and flags brand-new session churn, so one client conversation must keep one derived `session_<32hex>`."* However, when the client does not send `user`, `metadata.user_id`, or `X-Session-Id` (the default for OpenAI SDK, 9Router, and curl), `SessionKeyFor` returns `fmt.Sprintf("anon:%s:%d", first, len(msgs))`. Because `len(msgs)` grows on every turn (`1`, `3`, `5`, ...), every turn of the same conversation generates a brand-new `session_<32hex>`!
- **Evidence:** `TestPoC_SessionKeyChangesPerTurn`: turn 1 produced `"anon:q:1"` and turn 2 of the same conversation produced `"anon:q:3"`.
- **Impact:** Defeats session affinity and prompt caching on every multi-turn conversation for standard OpenAI clients, increasing the risk of upstream `405 3012` session-churn blocks.
- **Remediation:** Hash the system prompt + first user message (excluding `len(msgs)`) so all turns of the same conversation share the same derived `session_<32hex>`.

---

### REL-09 — Dialect Error Mismatch & Confusing Upstream Status Forwarding
- **Location:** `internal/server/routes.go:72-88`, `internal/server/server.go:162-181`, `internal/server/server.go:406-409`
- **Problem:**
  1. `handlePost` (`routes.go:72-88`) always calls `anthropicError(w, 400/413, ...)` even when handling `POST /v1/chat/completions` (OpenAI dialect).
  2. `relayUpstreamError` (`server.go:162-181`) forwards `ue.Status` directly to the client. If upstream returns `401` (all ZCode accounts revoked) or `405` (code `3012`), the client receives HTTP `401` or `405` from the gateway—making 9Router/OpenAI SDK think its local `ZCODE_BRIDGE_TOKEN` is invalid or that `POST /v1/chat/completions` is not an allowed HTTP method (`405 Method Not Allowed`).
  3. In `toOpenAIChunk` (`server.go:406-409`), `id := stringOf(event["id"])` looks for `event["id"]` at the top level, whereas Anthropic's `message_start` event nests the ID at `event["message"]["id"]`. Consequently, every OpenAI streaming chunk falls back to the hardcoded `"id": "chatcmpl-stream"`.
- **Evidence:** `TestPoC_OpenAIErrorShape` and `TestPoC_UpstreamStatusPassthrough`.
- **Remediation:**
  1. Pass `dialect` into `handlePost` so `400`/`413` errors on `/v1/chat/completions` use `openAIError`.
  2. Map upstream `401`/`403` (upstream credential failure) to `502 Bad Gateway` (with a clear message that upstream ZCode credentials need re-login, distinct from client `401 Invalid bridge token`), and map upstream `405` (`3012`) to `429` or `502`.
  3. Extract `event["message"]["id"]` on `message_start` and persist it in `chunkState` for all chunks in that stream.

---

### REL-10 — Missing Input Validation on `messages` Field
- **Location:** `internal/server/server.go:184-251`, `internal/models/models.go:179-283`
- **Problem:** Neither `handleMessages` nor `handleChatCompletions` validates that `body["messages"]` is a non-empty JSON array before calling `transport.SendAnthropic`. Sending `{"model":"glm-5.3-flash","messages":"nope"}` or `{}` wastes an upstream call (or hits upstream `400`/`3001`).
- **Evidence:** `TestPoC_InputValidation`: `{"model":"../../etc/passwd","messages":"nope"}` was forwarded straight to upstream with `messages: null`.
- **Remediation:** Validate that `messages` is a non-empty slice of objects at the handler boundary and return HTTP `400` (`invalid_request_error`) immediately if not.

---

### REL-11 — Per-Request `http.Client` Allocation & Ungraceful Process Exit
- **Location:** `internal/transport/transport.go:335`, `internal/quota/quota.go:99`, `internal/auth/http.go:42`, `cmd/zcode-gateway/main.go:204-209`
- **Problem:**
  1. Every call to `SendAnthropic`, `fetchEnvelope` (quota), and `doRequest` (auth) constructs a brand-new `&http.Client{Timeout: ...}`. While zero-value `http.Client.Transport` falls back to `http.DefaultTransport`, constructing clients ad-hoc prevents tuning `ResponseHeaderTimeout`, `MaxIdleConnsPerHost`, and TLS timeouts for the upstream proxy.
  2. `cmd/zcode-gateway/main.go:204-209` exits via `time.Sleep(500ms); os.Exit(0)` on `SIGINT`/`SIGTERM` instead of `http.Server.Shutdown(ctx)`.
- **Remediation:** Share a configured `http.Transport` / `http.Client` and implement `server.Shutdown(ctx)` on signal receipt.
