package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
)

//go:embed ui/dashboard.html
var dashboardHTML []byte

type loginFlowEntry struct {
	flow      *auth.DeviceFlow
	label     string
	createdAt time.Time
}

type trafficLogEntry struct {
	Timestamp string `json:"timestamp"`
	ReqID     string `json:"req_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Duration  int64  `json:"duration_ms"`
	EventTag  string `json:"event_tag,omitempty"`
}

var (
	loginFlowsMu sync.Mutex
	loginFlows   = map[string]*loginFlowEntry{}

	trafficMu   sync.RWMutex
	trafficLogs []trafficLogEntry
)

func recordTrafficLog(reqID, method, path string, status int, dur time.Duration, eventTag string) {
	trafficMu.Lock()
	defer trafficMu.Unlock()
	entry := trafficLogEntry{
		Timestamp: time.Now().Format("15:04:05"),
		ReqID:     reqID,
		Method:    method,
		Path:      path,
		Status:    status,
		Duration:  dur.Milliseconds(),
		EventTag:  eventTag,
	}
	trafficLogs = append(trafficLogs, entry)
	if len(trafficLogs) > 40 {
		trafficLogs = trafficLogs[len(trafficLogs)-40:]
	}
}

func resolveAdminKey() string {
	return strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_KEY"))
}

func isDashboardAuthorized(r *http.Request) bool {
	adminKey := resolveAdminKey()
	if adminKey == "" {
		return true // No password set: open dashboard
	}
	if authHeader := r.Header.Get("Authorization"); authHeader != "" {
		presented := strings.TrimPrefix(authHeader, "Bearer ")
		presented = strings.TrimPrefix(presented, "bearer ")
		if constantTimeTokenEqual(strings.TrimSpace(presented), adminKey) {
			return true
		}
	}
	if cookie, err := r.Cookie("gateway_admin_session"); err == nil && cookie != nil {
		if constantTimeTokenEqual(strings.TrimSpace(cookie.Value), adminKey) {
			return true
		}
	}
	return false
}

func handleDashboardUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

func handleDashboardAuthVerify(w http.ResponseWriter, r *http.Request) {
	adminKey := resolveAdminKey()
	requiresAuth := adminKey != ""
	authorized := isDashboardAuthorized(r)

	sendJSON(w, http.StatusOK, map[string]any{
		"requiresAuth": requiresAuth,
		"authorized":   authorized,
	})
}

func handleDashboardAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid payload"})
		return
	}
	adminKey := resolveAdminKey()
	if adminKey == "" || constantTimeTokenEqual(strings.TrimSpace(req.Key), adminKey) {
		http.SetCookie(w, &http.Cookie{
			Name:     "gateway_admin_session",
			Value:    req.Key,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   86400 * 7, // 7 days
		})
		sendJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid admin password"})
}

func handleDashboardAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "gateway_admin_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleDashboardStatus(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}

	c := getConfig()
	ring := accounts.Summarize()
	for i := range ring {
		ring[i].Email = ""
	}
	laneWidth := "1"
	if v := strings.TrimSpace(os.Getenv("ZCODE_LANE_WIDTH")); v != "" {
		laneWidth = v
	}

	authMode := "Open (Localhost)"
	if c.BridgeToken != "" {
		authMode = "Bridge Token Required"
	}

	trafficMu.RLock()
	logsCopy := make([]trafficLogEntry, len(trafficLogs))
	copy(logsCopy, trafficLogs)
	trafficMu.RUnlock()

	sendJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"auth":        authMode,
		"bridgeToken": c.BridgeToken,
		"laneWidth":   laneWidth,
		"accounts":    ring,
		"logs":        logsCopy,
	})
}

func handleDashboardLoginStart(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}

	var req struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	flow, err := auth.StartDeviceFlow(r.Context())
	if err != nil {
		sendJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	flowID := fmt.Sprintf("%d", time.Now().UnixNano())
	loginFlowsMu.Lock()
	loginFlows[flowID] = &loginFlowEntry{
		flow:      flow,
		label:     req.Label,
		createdAt: time.Now(),
	}
	for k, v := range loginFlows {
		if time.Since(v.createdAt) > 10*time.Minute {
			delete(loginFlows, k)
		}
	}
	loginFlowsMu.Unlock()

	sendJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"flowId":  flowID,
		"authUrl": flow.AuthorizeURL,
	})
}

func handleDashboardLoginPoll(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}

	flowID := r.URL.Query().Get("flowId")
	label := r.URL.Query().Get("label")

	loginFlowsMu.Lock()
	entry, ok := loginFlows[flowID]
	loginFlowsMu.Unlock()

	if !ok {
		sendJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "flow expired or not found"})
		return
	}

	pollCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := auth.PollDeviceFlow(pollCtx, entry.flow)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "context deadline") || strings.Contains(strings.ToLower(err.Error()), "waiting") {
			sendJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": err.Error()})
		return
	}

	expiresAt, _ := auth.ExpiryUnixMilli(result.JWT)
	if label == "" {
		label = entry.label
	}
	if label == "" {
		label = "Web Account " + strconv.FormatInt(time.Now().Unix()%1000, 10)
	}

	_, upsertErr := accounts.UpsertAccount("", label, result.JWT, result.RefreshToken, result.AccessToken, expiresAt)
	if upsertErr != nil {
		sendJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": upsertErr.Error()})
		return
	}

	loginFlowsMu.Lock()
	delete(loginFlows, flowID)
	loginFlowsMu.Unlock()

	sendJSON(w, http.StatusOK, map[string]any{"status": "success"})
}

func handleDashboardAccountDefault(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}
	id := r.URL.Query().Get("id")
	if id != "" {
		accounts.SetDefault(id)
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func handleDashboardAccountRemove(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}
	id := r.URL.Query().Get("id")
	if id != "" {
		accounts.Remove(id)
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true})
}
