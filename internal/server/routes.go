package server

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/identity"
	"github.com/apissaj/zcode-gateway/internal/models"
	"github.com/apissaj/zcode-gateway/internal/quota"
	"github.com/apissaj/zcode-gateway/internal/transport"
)

// Route dispatches one request; handler errors that happen after the
// headers are sent are impossible here because every handler writes its
// own status first.
func Route() http.Handler {
	return http.HandlerFunc(route)
}

func route(w http.ResponseWriter, r *http.Request) {
	if !validHostHeader(r, getConfig()) {
		anthropicError(w, http.StatusForbidden, "Invalid Host header", "forbidden_error")
		return
	}
	if !cors(w, r) {
		return // preflight or rejected cross-origin request handled
	}
	pathName := strings.TrimRight(r.URL.Path, "/")
	if pathName == "" {
		pathName = "/"
	}
	accountID := r.URL.Query().Get("account")
	if accountID == "" {
		accountID = r.Header.Get("X-Zcode-Account")
	}

	switch {
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
		anthropicError(w, http.StatusNotFound, fmt.Sprintf("No route for %s %s", r.Method, r.URL.Path), "not_found_error")
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
		openAIError(w, status, message, typ)
		return
	}
	anthropicError(w, status, message, typ)
}

func handlePost(w http.ResponseWriter, r *http.Request, accountID, dialect string, handler func(http.ResponseWriter, *http.Request, map[string]any, string)) {
	if !isJSONContentType(r) {
		writeDialectError(w, dialect, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "invalid_request_error")
		return
	}
	raw, err := readBody(r)
	if err != nil {
		writeDialectError(w, dialect, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	if len(raw) > maxBodyBytes {
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
func cors(w http.ResponseWriter, r *http.Request) bool {
	corsOrigins := strings.TrimSpace(os.Getenv("ZCODE_CORS_ORIGIN"))
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	secFetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))

	if origin != "" {
		if !isAllowedOrigin(origin, corsOrigins) {
			anthropicError(w, http.StatusForbidden, "Cross-origin request rejected", "forbidden_error")
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	} else if secFetchSite != "" && secFetchSite != "none" && secFetchSite != "same-origin" {
		anthropicError(w, http.StatusForbidden, "Cross-site browser request rejected", "forbidden_error")
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

// ListenAndServe validates the server configuration and starts the HTTP server.
// keepAlive timeouts mirror the JS server's 65s values so long SSE streams
// survive intermediary idle cuts.
func ListenAndServe() error {
	c := getConfig()
	if err := ValidateConfig(c); err != nil {
		return err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           Route(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       66 * time.Second,
	}
	return srv.ListenAndServe()
}
