# Production Runbook & Operational Observability

## 1. Scope & Purpose
This runbook defines the operational lifecycle, health monitoring, log analysis, backup/restore procedures, and incident response for `zcode-gateway` in production environments (Linux systemd or Docker container behind Nginx/Caddy).

---

## 2. Service Management (systemd)

### 2.1 Basic Operations
```bash
# Check service status and sandbox state
systemctl status zcode-gateway

# View real-time logs with request IDs and event tags
journalctl -u zcode-gateway -f

# Gracefully restart service (drains in-flight SSE streams up to 15s)
sudo systemctl restart zcode-gateway

# Stop service cleanly
sudo systemctl stop zcode-gateway
```

### 2.2 Sandboxing & Privilege Verification
Ensure systemd sandbox directives remain active:
```bash
systemd-analyze security zcode-gateway
```

---

## 3. Health Monitoring & Observability

### 3.1 Endpoints
- **Liveness Check (`GET /healthz`):**
  - Unauthenticated, returns `{"ok":true}` with status `200`.
  - Zero disk I/O, zero upstream calls. Use for load balancers and container orchestrators.
- **Detailed System Health (`GET /health`):**
  - Requires `Authorization: Bearer <ZCODE_BRIDGE_TOKEN>`.
  - Returns current plan lane status, registered account count, cooldown countdowns, and token expiry without leaking secrets or emails.

### 3.2 CLI Healthcheck Subcommand
The gateway binary provides a self-contained healthcheck for container orchestration or cron watchdog:
```bash
/usr/local/bin/zcode-gateway healthcheck
echo $? # Returns 0 on success, 1 on failure
```

### 3.3 Log Tagging & Telemetry
Operational logs follow structured key-value formatting:
```
[access] req_id=req_1720000000_1 method=POST path=/v1/messages status=200 duration_ms=412
[access] req_id=req_1720000000_2 method=POST path=/v1/messages status=429 duration_ms=2 event=rate_limited
[access] req_id=req_1720000000_3 method=POST path=/v1/messages status=504 duration_ms=120002 event=upstream_timeout
```
Key operational event tags:
- `rate_limited`: Client exceeded IP token-bucket rate limits.
- `body_too_large`: Client exceeded `ZCODE_MAX_BODY_BYTES`.
- `invalid_host`: DNS rebinding attack or host header mismatch (`ZCODE_ALLOWED_HOSTS`).
- `upstream_error`: Upstream Z.ai returned an unrecoverable failure.
- `upstream_timeout`: Response header or idle stream timeout exceeded.

---

## 4. Backup, Rotation & Disaster Recovery

### 4.1 Account Store (`accounts.json`)
The account store is located at `/var/lib/zcode-gateway/accounts.json` (`0600` permissions owned by `zcode:zcode`).
- **Atomic Writes:** Every update writes to `accounts.json.tmp.<rand>` and atomically renames to `accounts.json`.
- **Corrupt File Auto-Backup:** If corrupted by an external tool, the gateway preserves the damaged file as `accounts.json.corrupt.<timestamp>.<hash8>` and refuses destructive overwrites.

### 4.2 Backup Procedure
To back up registered credentials:
```bash
sudo cp -a /var/lib/zcode-gateway/accounts.json /var/backups/zcode-accounts-$(date +%F).json
sudo chmod 0600 /var/backups/zcode-accounts-$(date +%F).json
```

### 4.3 Recovery Procedure
If restoring from backup:
```bash
sudo systemctl stop zcode-gateway
sudo cp /var/backups/zcode-accounts-YYYY-MM-DD.json /var/lib/zcode-gateway/accounts.json
sudo chown zcode:zcode /var/lib/zcode-gateway/accounts.json
sudo chmod 0600 /var/lib/zcode-gateway/accounts.json
sudo systemctl start zcode-gateway
```

---

## 5. Reverse Proxy Maintenance

### 5.1 Nginx
- Ensure `proxy_buffering off;` and `proxy_read_timeout 300s;` remain enabled in `/etc/nginx/sites-available/`.
- Verify reload:
  ```bash
  sudo nginx -t && sudo systemctl reload nginx
  ```

### 5.2 Caddy
- Ensure `flush_interval -1` is configured on the upstream reverse proxy directive.
- Verify reload:
  ```bash
  caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy
  ```
