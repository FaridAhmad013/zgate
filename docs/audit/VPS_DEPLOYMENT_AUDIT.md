# VPS Deployment & Environment Audit

## 1. Scope & Execution Baseline
This audit evaluates the deployment models (systemd unit, Docker container, and reverse proxy) for `zcode-gateway` across personal and VPS Linux targets.

- **Target OS Baseline:** Linux (systemd, cgroup v2, glibc/musl)
- **Runtime Dependencies:** 0 external runtime dependencies (pure Go standard library static binary)
- **Deployment Artifacts:**
  - `deployments/systemd/zcode-gateway.service`
  - `deployments/systemd/zcode-gateway.env.example`
  - `deployments/docker/Dockerfile`
  - `deployments/docker/docker-compose.yml`
  - `deployments/proxy/nginx.conf`
  - `deployments/proxy/Caddyfile`
  - `deployments/validate.sh`

---

## 2. Head-of-Line (HoL) Blocking & Scheduler Analysis

### 2.1 Reproducible Pinned-Account HoL Scenario
During extended testing in Phase 7.1 (`internal/transport/sse_timeout_lane_test.go`), an HoL condition was confirmed and measured under the following setup:
- `ZCODE_ACCOUNT_CONCURRENCY=1` for Account A and Account B.
- `ZCODE_LANE_WIDTH=2` (matches total accounts).
- R1 pins to Account A for a 120ms stream.
- R2 pins to Account A (`?account=a`).
- R3 is unpinned and can use either Account A or B. Account B is 100% idle (`inFlight=0`).

**Behavior:**
R2 enters the global lane (`laneSem` acquires slot 2), but blocks inside `AcquireCandidate` waiting on Account A's concurrency semaphore. R3 arrives: even though Account B is completely idle, R3 cannot acquire a global lane slot because R1 holds slot 1 and R2 holds slot 2.
**Measurement:** R3 suffered an HoL queue delay of **`118.9 ms`** (processing duration alone is `~10 ms`).

### 2.2 Architectural Decision: Retention of Global Lane -> Account Slot
We evaluated alternative scheduler architectures:
1. **Account Reservation First -> Global Lane:** Rejected. If a request claims an account slot first and then waits for a global lane slot, that account is locked from servicing other requests even though no I/O is running. This shifts HoL from the lane to the account pool.
2. **Coordinated Admission Scheduler:** Rejected. Introducing a centralized multiplexer goroutine with per-request admission queues violates the YAGNI principle and adds channel allocation overhead for a marginal edge case.

**Final Decision:** Preserve `Global Lane -> Account Slot -> Request Processing`.
- **Circular-Wait Freedom:** Enforcing strict unidirectional semaphore acquisition (`Global Lane -> Single Account Slot`) prevents circular waits between the two semaphore classes. Deadlock freedom is contingent on caller cancellation and stream release via `sync.Once`.
- **Operational Constraint:** To prevent HoL, operators must instruct clients (Claude Code, 9Router, SDKs) to use **unpinned routing** (omit `?account=`). Pinned requests should not exceed `ZCODE_ACCOUNT_CONCURRENCY`. Setting `ZCODE_LANE_WIDTH >= accounts * ZCODE_ACCOUNT_CONCURRENCY` guarantees starvation-free scheduling **only for unpinned traffic**.

---

## 3. Systemd Hardening & Credential Security Audit

### 3.1 Security Directives Review
`deployments/systemd/zcode-gateway.service` is configured with strict defense-in-depth isolation:
- **User & Group:** Runs under dedicated unprivileged `zcode:zcode` system user.
- **StateDirectory:** `zcode-gateway` automatically manages `/var/lib/zcode-gateway` with `0700` permissions.
- **Filesystem Isolation:** `ProtectSystem=strict` (read-only OS tree), `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`.
- **Kernel & Execution Sandboxing:** `NoNewPrivileges=yes`, `MemoryDenyWriteExecute=yes`, `ProtectKernelTunables=yes`, `ProtectKernelModules=yes`, `ProtectControlGroups=yes`, `CapabilityBoundingSet=`, `SystemCallFilter=@system-service`.
- **Graceful Shutdown:** `KillMode=mixed`, `KillSignal=SIGTERM`, `TimeoutStopSec=20s`, giving the gateway `15s` (`ZCODE_SHUTDOWN_TIMEOUT_MS`) to finish in-flight SSE streams before SIGKILL.

### 3.2 Credential Protection (ADR 0001 & systemd-creds)
- Gateway supports `EnvironmentFile=-/etc/zcode-gateway/zcode-gateway.env` (`chmod 0600`).
- Gateway integrates with `systemd-creds` via `LoadCredential=-bridge_token:/etc/credstore/bridge_token` and `LoadCredentialEncrypted`. `cmd/zcode-gateway/main.go` reads `$CREDENTIALS_DIRECTORY/bridge_token` to prevent secrets from being exposed in `/proc/$PID/environ` or CLI args.

---

## 4. Docker & Container Security Audit

### 4.1 Multi-Stage Static Scratch / Alpine Build
- **Builder Stage:** `golang:1.24-alpine` builds with `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`.
- **Runtime Stage:** Minimal unprivileged runtime with `USER 65532:65532` (`nonroot:nonroot`).
- **Internal Healthcheck:** Does not rely on `curl` or shell binaries. Uses the built-in Go sub-command `zcode-gateway healthcheck` (`GET /healthz`), returning exit code 0 on HTTP 200 and exit code 1 on failure. Verified by `internal/server/healthcheck_test.go`.
- **Storage Persistence:** `/data` volume mount (`ZCODE_ACCOUNTS_FILE=/data/accounts.json`).

---

## 5. Reverse Proxy & Long-Running SSE Streaming Audit

### 5.1 Nginx (`deployments/proxy/nginx.conf`)
- `proxy_buffering off;` and `proxy_cache off;` prevent SSE chunk buffering.
- `proxy_read_timeout 300s;` and `proxy_send_timeout 300s;` support extended multi-minute reasoning streams.
- `proxy_http_version 1.1;` and `proxy_set_header Connection "";` enable upstream keep-alive connection pooling.
- Tested via `nginx -t` with zero configuration errors.

### 5.2 Caddy (`deployments/proxy/Caddyfile`)
- `flush_interval -1` forces immediate chunk flushes to the client.
- `read_timeout 300s` and `write_timeout 300s` match reasoning duration requirements.
- Automatic HTTPS via Let's Encrypt / ZeroSSL.

---

## 6. Resource Benchmarking & cgroup Qualifications

### 6.1 Status Classification
All resource measurements conducted on Windows development hosts are classified as **SIMULATED** (Go runtime constraints `GOMAXPROCS` and `debug.SetMemoryLimit`). Actual Linux cgroup v2 kernel measurements remain **PREPARED** until executed on a live Linux kernel host.

### 6.2 Target Deployment Verification Status
| Verification Target | Category | Qualification | Notes |
|---|---|---|---|
| CLI Healthcheck Subcommand | VERIFIED | PASS | Tested via `TestGatewayHealthcheckSubcommand` |
| SSE Streaming Idle Timeout | VERIFIED | PASS | Tested in `internal/transport` unit tests |
| Pinned-Account HoL Blocking | VERIFIED | REPRODUCED | Tested in `sse_timeout_lane_test.go` |
| Nginx Configuration Syntax | VERIFIED | PASS | Validated with `nginx -t` |
| Caddyfile Configuration | PREPARED | UNVERIFIED | Syntax conforms to Caddy v2 specification |
| systemd Service Unit | PREPARED | UNVERIFIED | Requires Linux host with `systemd-analyze` |
| Docker Container Execution | PREPARED | UNVERIFIED | Requires active Docker Engine daemon |
| Linux cgroup v2 VPS Benchmarks| PREPARED | UNVERIFIED | Requires Linux VPS deployment |
