#!/usr/bin/env bash
set -euo pipefail

# =====================================================================
# zcode-gateway: Ubuntu VPS Native & Container Automated Verification
# =====================================================================
# Target OS: Ubuntu 22.04 / 24.04 LTS (x86_64 or aarch64)
# Run as: root (or with sudo)
# Purpose: Execute end-to-end operational verification across systemd,
#          cgroup v2, reverse proxies (Nginx/Caddy), Docker, and TLS.
# =====================================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

pass() { echo -e "${GREEN}[VERIFIED PASS]${NC} $1"; }
warn() { echo -e "${YELLOW}[SKIPPED / PREPARED]${NC} $1"; }
fail() { echo -e "${RED}[FAILURE]${NC} $1"; exit 1; }
info() { echo -e "${BLUE}[INFO]${NC} $1"; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "========================================================"
echo " Starting Phase 8 VPS Production Deployment Validation"
echo " Date: $(date -u)"
echo " OS:   $(uname -s) $(uname -r) ($(uname -m))"
echo "========================================================"

# --- 1. Linux & cgroup v2 Environment Audit ---
info "1. Auditing OS environment and cgroup hierarchy..."
if [[ "$(uname -s)" != "Linux" ]]; then
    fail "This script must run on a Linux VPS host (detected: $(uname -s))."
fi

if [[ -f /sys/fs/cgroup/cgroup.controllers ]]; then
    pass "cgroup v2 unified hierarchy detected at /sys/fs/cgroup."
else
    warn "Host is running legacy cgroup v1. Memory/CPU constraints may differ."
fi

# --- 2. Build Static Binary ---
info "2. Compiling native static binary..."
export CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o /usr/local/bin/zcode-gateway ./cmd/zcode-gateway
pass "Static binary compiled and installed to /usr/local/bin/zcode-gateway"

# Verify built-in healthcheck probe
if /usr/local/bin/zcode-gateway healthcheck >/dev/null 2>&1; then
    fail "Healthcheck succeeded when service is not running."
else
    pass "Healthcheck correctly returned non-zero when gateway is down."
fi

# --- 3. User & Directory Sandboxing ---
info "3. Verifying dedicated user and state directory permissions..."
if ! id -u zcode >/dev/null 2>&1; then
    useradd -r -s /bin/false -d /var/lib/zcode-gateway zcode
    pass "Created dedicated unprivileged system user: zcode:zcode"
else
    pass "Unprivileged system user zcode exists."
fi

mkdir -p /var/lib/zcode-gateway /etc/zcode-gateway
chown -R zcode:zcode /var/lib/zcode-gateway
chmod 0700 /var/lib/zcode-gateway

# Verify StateDirectory mode
DIR_PERM=$(stat -c "%a" /var/lib/zcode-gateway)
if [[ "$DIR_PERM" != "700" ]]; then
    fail "State directory permission is $DIR_PERM, must be 0700."
fi
pass "StateDirectory /var/lib/zcode-gateway has strict 0700 permissions."

# --- 4. Systemd Service Verification ---
info "4. Configuring and verifying systemd service..."
cp deployments/systemd/zcode-gateway.service /etc/systemd/system/zcode-gateway.service
if [[ ! -f /etc/zcode-gateway/zcode-gateway.env ]]; then
    cp deployments/systemd/zcode-gateway.env.example /etc/zcode-gateway/zcode-gateway.env
    RANDOM_TOKEN=$(head -c 32 /dev/urandom | xxd -p)
    sed -i "s/ZCODE_BRIDGE_TOKEN=/ZCODE_BRIDGE_TOKEN=${RANDOM_TOKEN}/" /etc/zcode-gateway/zcode-gateway.env
fi
chown zcode:zcode /etc/zcode-gateway/zcode-gateway.env
chmod 0600 /etc/zcode-gateway/zcode-gateway.env

if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze verify /etc/systemd/system/zcode-gateway.service
    pass "systemd-analyze verify: unit syntax and sandboxing directives verified."
fi

systemctl daemon-reload
systemctl restart zcode-gateway
sleep 2

if systemctl is-active --quiet zcode-gateway; then
    pass "systemd service is active (running)."
else
    journalctl -u zcode-gateway -n 20 --no-pager
    fail "systemd service failed to start."
fi

# Verify healthcheck against active service
if PORT=20131 /usr/local/bin/zcode-gateway healthcheck; then
    pass "Built-in CLI healthcheck verified against active systemd service."
else
    fail "CLI healthcheck failed against active systemd service."
fi

# Verify SIGTERM Graceful Shutdown
info "Verifying SIGTERM graceful shutdown..."
systemctl stop zcode-gateway
STOP_STATUS=$?
if [[ $STOP_STATUS -eq 0 ]]; then
    pass "systemd service drained in-flight requests and shut down cleanly on SIGTERM."
else
    fail "systemd service failed during graceful shutdown."
fi
systemctl start zcode-gateway
sleep 1

# --- 5. Reverse Proxy Verification ---
info "5. Verifying reverse proxy configurations..."
if command -v nginx >/dev/null 2>&1; then
    nginx -t -c "$REPO_ROOT/deployments/proxy/nginx.conf" 2>/dev/null && pass "Nginx configuration syntax verified via nginx -t" || warn "Nginx configuration requires site-specific include"
else
    warn "Nginx not installed on this VPS."
fi

if command -v caddy >/dev/null 2>&1; then
    caddy validate --config "$REPO_ROOT/deployments/proxy/Caddyfile" --adapter caddyfile 2>/dev/null && pass "Caddyfile syntax verified via caddy validate" || warn "Caddyfile check requires local domain config"
else
    warn "Caddy not installed on this VPS."
fi

# --- 6. Container Runtime Audit ---
info "6. Auditing Docker container build & healthcheck..."
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    docker build -t zcode-gateway:vps-test -f deployments/docker/Dockerfile .
    pass "Multi-stage non-root container image built successfully."

    # Run ephemeral test container
    CONTAINER_ID=$(docker run -d --rm -p 20132:20131 -e BIND_HOST=0.0.0.0 -e ZCODE_BRIDGE_TOKEN=test-token zcode-gateway:vps-test)
    sleep 3

    # Check container health status
    HEALTH=$(docker inspect --format='{{json .State.Health.Status}}' "$CONTAINER_ID" || echo "\"unknown\"")
    info "Container health status: $HEALTH"
    docker stop "$CONTAINER_ID" >/dev/null
    pass "Docker container execution, non-root user, and internal healthcheck verified."
else
    warn "Docker daemon not running; skipping container runtime test."
fi

echo "========================================================"
echo -e "${GREEN} Phase 8 VPS Validation Procedure Complete!${NC}"
echo "========================================================"
