package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/identity"
	"github.com/apissaj/zcode-gateway/internal/models"
	"github.com/apissaj/zcode-gateway/internal/quota"
	"github.com/apissaj/zcode-gateway/internal/transport"
)

// statusRecorder wraps http.ResponseWriter to capture HTTP status and event
// tags for operational logging while preserving http.Flusher and
// http.ResponseController (via Unwrap).
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	eventTag    string
}

func (sr *statusRecorder) WriteHeader(code int) {
	if !sr.wroteHeader {
		sr.status = code
		sr.wroteHeader = true
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if !sr.wroteHeader {
		sr.status = http.StatusOK
		sr.wroteHeader = true
	}
	return sr.ResponseWriter.Write(b)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

func setEventTag(w http.ResponseWriter, tag string) {
	if sr, ok := w.(*statusRecorder); ok && sr.eventTag == "" {
		sr.eventTag = tag
	}
}

func requestIDFor(r *http.Request) string {
	if incoming := strings.TrimSpace(r.Header.Get("X-Request-Id")); incoming != "" && len(incoming) <= 64 {
		valid := true
		for i := 0; i < len(incoming); i++ {
			c := incoming[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				valid = false
				break
			}
		}
		if valid {
			return incoming
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "req_" + hex.EncodeToString(b[:])
	}
	seq := atomic.AddUint64(&reqCounter, 1)
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), seq)
}

func logRequest(reqID, method, path string, status int, dur time.Duration, eventTag string) {
	c := getConfig()
	logFn := c.LogFunc
	if logFn == nil {
		rawToggle := strings.ToLower(strings.TrimSpace(os.Getenv("ZCODE_ACCESS_LOG")))
		if rawToggle == "0" || rawToggle == "false" || rawToggle == "off" {
			return
		}
		if rawToggle == "" {
			// Default: only log when ZCODE_ACCESS_LOG is enabled or on operational events (429/408/5xx).
			if status < 400 && eventTag == "" {
				return
			}
		}
		logFn = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	safePath := redactMsg(path)
	if eventTag != "" {
		logFn("[access] req_id=%s method=%s path=%s status=%d duration_ms=%d event=%s",
			reqID, method, safePath, status, dur.Milliseconds(), eventTag)
	} else {
		logFn("[access] req_id=%s method=%s path=%s status=%d duration_ms=%d",
			reqID, method, safePath, status, dur.Milliseconds())
	}
}

// --- Token-bucket rate limiter & trusted proxy handling ---

type rateBucket struct {
	tokens   float64
	lastSeen time.Time
}

var (
	rateMu      sync.Mutex
	rateBuckets = map[string]*rateBucket{}
)

func resetRateLimiter() {
	rateMu.Lock()
	rateBuckets = map[string]*rateBucket{}
	rateMu.Unlock()
}

func rateLimitConfig() (rps float64, burst int) {
	rps = DefaultRateLimitRPS
	if v := strings.TrimSpace(os.Getenv("ZCODE_RATE_LIMIT_RPS")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			rps = f
		}
	}
	burst = DefaultRateLimitBurst
	if v := strings.TrimSpace(os.Getenv("ZCODE_RATE_LIMIT_BURST")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			burst = n
		}
	}
	if rps > 0 && float64(burst) < 1 {
		burst = 1
	}
	return rps, burst
}

// isTrustedProxy checks whether remoteIP matches an explicit entry (IP or CIDR)
// in ZCODE_TRUSTED_PROXIES. When ZCODE_TRUSTED_PROXIES is empty, no proxy is
// trusted and X-Forwarded-For / X-Real-IP are ignored.
func isTrustedProxyList(remoteIP net.IP, raw string) bool {
	if remoteIP == nil || raw == "" {
		return false
	}
	for _, item := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(item)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(remoteIP) {
				return true
			}
			continue
		}
		if ip := net.ParseIP(entry); ip != nil && ip.Equal(remoteIP) {
			return true
		}
	}
	return false
}

// resolveClientIP determines the caller's IP address for rate limiting.
// Forwarded headers are consulted ONLY when r.RemoteAddr belongs to
// ZCODE_TRUSTED_PROXIES, walking X-Forwarded-For right-to-left so a client
// cannot spoof leading X-Forwarded-For entries to bypass rate limits.
func resolveClientIP(r *http.Request) string {
	host := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "unknown"
	}
	trustedRaw := strings.TrimSpace(os.Getenv("ZCODE_TRUSTED_PROXIES"))
	if trustedRaw == "" {
		return host
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return "unknown"
	}
	if !isTrustedProxyList(remoteIP, trustedRaw) {
		return remoteIP.String()
	}

	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			cand := strings.Trim(strings.TrimSpace(parts[i]), "[]")
			if h, _, err := net.SplitHostPort(cand); err == nil {
				cand = h
			}
			ip := net.ParseIP(cand)
			if ip == nil {
				continue
			}
			if !isTrustedProxyList(ip, trustedRaw) {
				return ip.String()
			}
		}
	}
	if xri := strings.Trim(strings.TrimSpace(r.Header.Get("X-Real-IP")), "[]"); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return ip.String()
		}
	}
	return remoteIP.String()
}

// clientRateKey derives the rate-limit bucket key. Unauthenticated callers are
// keyed strictly by resolved client IP so rotating arbitrary headers (X-Session-Id,
// User-Agent, etc.) cannot bypass limits.
func clientRateKey(r *http.Request) string {
	ip := resolveClientIP(r)
	if expected := strings.TrimSpace(getConfig().BridgeToken); expected != "" && authorized(r) {
		sum := sha256.Sum256([]byte(expected))
		return "ip:" + ip + ":auth:" + hex.EncodeToString(sum[:4])
	}
	return "ip:" + ip
}

func allowRequest(r *http.Request) (bool, int) {
	rps, burst := rateLimitConfig()
	if rps <= 0 {
		return true, 0
	}
	key := clientRateKey(r)
	now := time.Now()

	rateMu.Lock()
	defer rateMu.Unlock()

	const maxRateBuckets = 8192
	if len(rateBuckets) > 4096 {
		for k, b := range rateBuckets {
			if now.Sub(b.lastSeen) > 5*time.Minute {
				delete(rateBuckets, k)
			}
		}
		// SEC-01 remediation: If flood of unique IPs prevents 5-minute expiry,
		// enforce hard ceiling by pruning oldest entries.
		if len(rateBuckets) >= maxRateBuckets {
			type entry struct {
				key      string
				lastSeen time.Time
			}
			entries := make([]entry, 0, len(rateBuckets))
			for k, b := range rateBuckets {
				entries = append(entries, entry{key: k, lastSeen: b.lastSeen})
			}
			// Sort ascending by lastSeen to evict oldest first
			for i := 0; i < len(entries)-1; i++ {
				for j := i + 1; j < len(entries); j++ {
					if entries[j].lastSeen.Before(entries[i].lastSeen) {
						entries[i], entries[j] = entries[j], entries[i]
					}
				}
			}
			// Evict oldest quarter of the map to free capacity
			evictCount := len(entries) / 4
			for i := 0; i < evictCount; i++ {
				delete(rateBuckets, entries[i].key)
			}
		}
	}

	b, ok := rateBuckets[key]
	if !ok {
		rateBuckets[key] = &rateBucket{
			tokens:   float64(burst) - 1,
			lastSeen: now,
		}
		return true, 0
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(float64(burst), b.tokens+elapsed*rps)
		b.lastSeen = now
	}
	if b.tokens >= 1 {
		b.tokens -= 1
		return true, 0
	}
	deficit := 1.0 - b.tokens
	retrySec := int(math.Ceil(deficit / rps))
	if retrySec < 1 {
		retrySec = 1
	}
	return false, retrySec
}

// Route dispatches one request; handler errors that happen after the
// headers are sent are impossible here because every handler writes its
// own status first.
func Route() http.Handler {
	return http.HandlerFunc(route)
}

func dialectForPath(path string) string {
	p := strings.TrimRight(path, "/")
	if strings.HasPrefix(p, "/v1/chat/completions") || p == "/v1/models" || p == "/models" {
		return "openai"
	}
	return "anthropic"
}

func route(origW http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := requestIDFor(r)
	origW.Header().Set("X-Request-Id", reqID)
	w := &statusRecorder{ResponseWriter: origW, status: http.StatusOK}
	pathName := strings.TrimRight(r.URL.Path, "/")
	if pathName == "" {
		pathName = "/"
	}
	defer func() {
		logRequest(reqID, r.Method, pathName, w.status, time.Since(start), w.eventTag)
	}()

	reqDialect := dialectForPath(r.URL.Path)
	if !validHostHeader(r, getConfig()) {
		setEventTag(w, "invalid_host")
		writeDialectError(w, reqDialect, http.StatusForbidden, "Invalid Host header", "forbidden_error")
		return
	}
	if !cors(w, r, reqDialect) {
		return // preflight or rejected cross-origin request handled
	}

	if pathName != "/healthz" {
		if ok, retryAfter := allowRequest(r); !ok {
			setEventTag(w, "rate_limited")
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeDialectError(w, reqDialect, http.StatusTooManyRequests, "Rate limit exceeded; retry later", "rate_limit_error")
			return
		}
	}

	accountID := r.URL.Query().Get("account")
	if accountID == "" {
		accountID = r.Header.Get("X-Zcode-Account")
	}

	switch {
	case r.Method == http.MethodGet && (pathName == "/" || pathName == "/dashboard"):
		handleDashboardUI(w, r)
	case r.Method == http.MethodGet && pathName == "/api/dashboard/auth/verify":
		handleDashboardAuthVerify(w, r)
	case r.Method == http.MethodPost && pathName == "/api/dashboard/auth/login":
		handleDashboardAuthLogin(w, r)
	case r.Method == http.MethodPost && pathName == "/api/dashboard/auth/logout":
		handleDashboardAuthLogout(w, r)
	case r.Method == http.MethodGet && pathName == "/api/dashboard/status":
		handleDashboardStatus(w, r)
	case r.Method == http.MethodPost && pathName == "/api/dashboard/login/start":
		handleDashboardLoginStart(w, r)
	case r.Method == http.MethodGet && pathName == "/api/dashboard/login/poll":
		handleDashboardLoginPoll(w, r)
	case r.Method == http.MethodPost && pathName == "/api/dashboard/account/default":
		handleDashboardAccountDefault(w, r)
	case r.Method == http.MethodPost && pathName == "/api/dashboard/account/remove":
		handleDashboardAccountRemove(w, r)
	case r.Method == http.MethodGet && pathName == "/healthz":
		sendJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && pathName == "/health":
		requireAuth(w, r, "anthropic", func() {
			handleHealth(w)
		})
	case r.Method == http.MethodGet && (pathName == "/v1/models" || pathName == "/models"):
		requireAuth(w, r, "openai", func() {
			sendJSON(w, http.StatusOK, map[string]any{"object": "list", "data": liveModels(r.Context())})
		})
	case r.Method == http.MethodGet && (pathName == "/v1/quota" || pathName == "/quota"):
		requireAuth(w, r, "anthropic", func() {
			handleQuota(w, r, accountID)
		})
	case r.Method == http.MethodPost && pathName == "/v1/messages":
		requireAuth(w, r, "anthropic", func() {
			handlePost(w, r, accountID, "anthropic", handleMessages)
		})
	case r.Method == http.MethodPost && pathName == "/v1/chat/completions":
		requireAuth(w, r, "openai", func() {
			handlePost(w, r, accountID, "openai", handleChatCompletions)
		})
	default:
		writeDialectError(w, reqDialect, http.StatusNotFound, fmt.Sprintf("No route for %s %s", r.Method, r.URL.Path), "not_found_error")
	}
}

// requireAuth gates sensitive routes with constant-time ZCODE_BRIDGE_TOKEN
// verification when authentication is configured (or on non-loopback binds).
func requireAuth(w http.ResponseWriter, r *http.Request, dialect string, next func()) {
	if !authorized(r) {
		if dialect == "openai" {
			openAIError(w, http.StatusUnauthorized, "Invalid bridge token", "invalid_api_key")
		} else {
			anthropicError(w, http.StatusUnauthorized, "Invalid bridge token", "authentication_error")
		}
		return
	}
	next()
}

func isJSONContentType(r *http.Request) bool {
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

func writeDialectError(w http.ResponseWriter, dialect string, status int, message, typ string) {
	if dialect == "openai" {
		openAIErrorWithType(w, status, message, typ, typ)
		return
	}
	anthropicError(w, status, message, typ)
}

func handlePost(w http.ResponseWriter, r *http.Request, accountID, dialect string, handler func(http.ResponseWriter, *http.Request, map[string]any, string)) {
	if !isJSONContentType(r) {
		writeDialectError(w, dialect, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "invalid_request_error")
		return
	}
	raw, err := readBody(w, r)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			setEventTag(w, "body_too_large")
			writeDialectError(w, dialect, http.StatusRequestEntityTooLarge, "Request body too large", "invalid_request_error")
			return
		}
		if errors.Is(err, errBodyReadTimeout) {
			setEventTag(w, "body_read_timeout")
			writeDialectError(w, dialect, http.StatusRequestTimeout, "Request body read timeout", "timeout_error")
			return
		}
		writeDialectError(w, dialect, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	if int64(len(raw)) > maxBodyBytesLimit() {
		setEventTag(w, "body_too_large")
		writeDialectError(w, dialect, http.StatusRequestEntityTooLarge, "Request body too large", "invalid_request_error")
		return
	}
	body, ok := parseJSON(raw)
	if !ok {
		writeDialectError(w, dialect, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error")
		return
	}
	if err := models.ValidateRequest(body, dialect); err != nil {
		writeDialectError(w, dialect, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	handler(w, r, body, accountID)
}

// handleHealth reports lane, credential and rotation state (no secrets or PII).
func handleHealth(w http.ResponseWriter) {
	c := getConfig()
	ring := accounts.Summarize()
	for i := range ring {
		ring[i].Email = ""
	}
	deviceMid := identity.ResolveDeviceMid()
	planJwt := map[string]any{"present": false}
	if len(ring) > 0 {
		planJwt = map[string]any{"present": true, "count": len(ring)}
	} else if jwt := auth.ResolvePlanJWT(""); jwt != "" {
		claims, _ := auth.DecodeJWT(jwt)
		entry := map[string]any{"present": true}
		if iat, ok := claims["iat"].(float64); ok {
			entry["issuedAt"] = time.Unix(int64(iat), 0).UTC().Format(time.RFC3339)
		}
		planJwt = entry
	}
	authMode := "open (localhost)"
	if c.BridgeToken != "" {
		authMode = "bridge token required"
	}
	sendJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"lane":          "zcode-plan/anthropic",
		"upstream":      transport.InferenceOrigin + transport.PlanAnthropicPath,
		"clientVersion": identity.ResolveAppVersion(),
		"deviceMid": func() string {
			if deviceMid != "" {
				return "set"
			}
			return "MISSING (control calls may fail 3001)"
		}(),
		"accounts": ring,
		"planJwt":  planJwt,
		"auth":     authMode,
		"version":  identity.ResolveAppVersion(),
	})
}

// handleQuota serves the billing readout for the first usable account (or
// the ?account= pin).
func handleQuota(w http.ResponseWriter, r *http.Request, accountID string) {
	cands, err := transport.Candidates(accountID, "")
	if err != nil || len(cands) == 0 {
		sendJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "no usable plan credential: run login"})
		return
	}
	acct := cands[0]
	q, err := quota.ReadQuota(r.Context(), acct.JWT, acct.DeviceID)
	if err != nil {
		sendJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": redactMsg(err.Error())})
		return
	}
	q.Account = firstNonEmpty(acct.Label, acct.ID, accountID, "default")
	sendJSON(w, http.StatusOK, q)
}

func extractRequestHost(rawHost string) string {
	h := strings.TrimSpace(rawHost)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(host, "[]")
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return h[1 : len(h)-1]
	}
	return h
}

// validHostHeader enforces DNS rebinding protection by validating r.Host
// against the configured bind host and optional ZCODE_ALLOWED_HOSTS.
// X-Forwarded-Host is intentionally ignored unless the reverse proxy rewrites Host.
func validHostHeader(r *http.Request, c Config) bool {
	reqHost := extractRequestHost(r.Host)
	if reqHost == "" || ValidateHost(reqHost) != nil {
		return false
	}
	for _, allowed := range strings.Split(os.Getenv("ZCODE_ALLOWED_HOSTS"), ",") {
		a := extractRequestHost(allowed)
		if a != "" && a != "*" && strings.EqualFold(reqHost, a) {
			return true
		}
	}
	bindHost := extractRequestHost(c.Host)
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	if IsLoopbackHost(bindHost) {
		return IsLoopbackHost(reqHost)
	}
	if bindHost == "0.0.0.0" || bindHost == "::" {
		return IsLoopbackHost(reqHost) || net.ParseIP(reqHost) != nil
	}
	return strings.EqualFold(reqHost, bindHost) || IsLoopbackHost(reqHost)
}

func isAllowedOrigin(origin, rawAllowlist string) bool {
	if origin == "" || rawAllowlist == "" {
		return false
	}
	parsedOrigin, err := url.Parse(origin)
	if err != nil || (parsedOrigin.Scheme != "http" && parsedOrigin.Scheme != "https") || parsedOrigin.Host == "" {
		return false
	}
	for _, entry := range strings.Split(rawAllowlist, ",") {
		candidate := strings.TrimSpace(entry)
		if candidate == "" || candidate == "*" || strings.Contains(candidate, "*") {
			continue // reject wildcard origins
		}
		if strings.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

// cors enforces browser Origin / Sec-Fetch-Site validation and opt-in CORS
// via ZCODE_CORS_ORIGIN (explicit comma-separated origin list; wildcards are
// not accepted). Returns false when the request was handled (preflight or
// rejected cross-origin request).
func cors(w http.ResponseWriter, r *http.Request, dialect string) bool {
	corsOrigins := strings.TrimSpace(os.Getenv("ZCODE_CORS_ORIGIN"))
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	secFetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))

	if origin != "" {
		if !isAllowedOrigin(origin, corsOrigins) {
			writeDialectError(w, dialect, http.StatusForbidden, "Cross-origin request rejected", "forbidden_error")
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	} else if secFetchSite != "" && secFetchSite != "none" && secFetchSite != "same-origin" {
		writeDialectError(w, dialect, http.StatusForbidden, "Cross-site browser request rejected", "forbidden_error")
		return false
	}

	if r.Method == http.MethodOptions {
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Session-Id, X-Zcode-Account")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// NewHTTPServer constructs a hardened *http.Server with bounded header size,
// ReadHeaderTimeout, and IdleTimeout, while deliberately omitting a connection-wide
// ReadTimeout/WriteTimeout so long-lived SSE streams are never severed.
func NewHTTPServer(baseCtx context.Context) (*http.Server, error) {
	c := getConfig()
	if err := ValidateConfig(c); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           Route(),
		ReadHeaderTimeout: readHeaderTimeout(),
		MaxHeaderBytes:    maxHeaderBytesLimit(),
		IdleTimeout:       serverIdleTimeout(),
	}
	if baseCtx != nil {
		srv.BaseContext = func(net.Listener) context.Context {
			return baseCtx
		}
	}
	return srv, nil
}

// Serve runs the HTTP server on ln until ctx is canceled, then performs a
// bounded graceful shutdown (stopping new connections, letting in-flight
// requests finish up to ShutdownTimeout, and canceling active stream contexts
// if the shutdown deadline is exceeded).
func Serve(ctx context.Context, ln net.Listener) error {
	reqCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	srv, err := NewHTTPServer(reqCtx)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout())
		defer cancel()

		// Signal active streams/handlers when the graceful shutdown window expires.
		stopAfter := time.AfterFunc(ShutdownTimeout(), func() {
			cancelRequests()
		})
		defer stopAfter.Stop()

		shutdownErr := srv.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			cancelRequests()
			_ = srv.Close()
		}
		serveErr := <-errCh
		if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
			return shutdownErr
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	}
}

// ListenAndServeContext binds the configured address and serves until ctx is
// canceled, executing graceful shutdown on cancellation.
func ListenAndServeContext(ctx context.Context) error {
	c := getConfig()
	if err := ValidateConfig(c); err != nil {
		return err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, ln)
}

// ListenAndServe validates the server configuration and starts the HTTP server.
func ListenAndServe() error {
	return ListenAndServeContext(context.Background())
}
