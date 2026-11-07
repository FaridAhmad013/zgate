package server

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/quota"
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

const loginPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>ZGate - System Access Locked</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600;700&family=Space+Grotesk:wght@500;700&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg-chassis: #141413;
      --bg-module: #1b1b19;
      --bg-screen: #0c0d0c;
      --border-groove: #090908;
      --border-strong: #33332f;
      --orange: #ff5500;
      --orange-shadow: #b33c00;
      --text-main: #f0f0ea;
      --text-dim: #73736c;
      --led-red: #ff1744;
      --font-mono: 'JetBrains Mono', monospace;
      --font-sans: 'Space Grotesk', sans-serif;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      background-color: var(--bg-chassis);
      background-image: radial-gradient(#262622 1px, transparent 1px);
      background-size: 16px 16px;
      color: var(--text-main);
      font-family: var(--font-sans);
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 20px;
    }
    .login-chassis {
      background: var(--bg-module);
      border: 1px solid var(--border-strong);
      box-shadow: 0 8px 0 var(--border-groove), 0 25px 50px rgba(0,0,0,0.8);
      border-radius: 4px;
      width: 100%;
      max-width: 380px;
      padding: 24px;
    }
    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      border-bottom: 2px solid var(--border-groove);
      padding-bottom: 14px;
      margin-bottom: 20px;
    }
    .brand {
      font-family: var(--font-mono);
      font-weight: 700;
      font-size: 14px;
      letter-spacing: 0.08em;
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .badge {
      background: #261616;
      border: 1px solid rgba(255, 23, 68, 0.4);
      color: var(--led-red);
      font-family: var(--font-mono);
      font-size: 10px;
      padding: 2px 6px;
      border-radius: 2px;
    }
    .input {
      background: var(--bg-screen);
      border: 1px solid #2f332f;
      box-shadow: inset 0 2px 6px rgba(0,0,0,0.8);
      border-radius: 2px;
      padding: 12px 14px;
      color: var(--text-main);
      font-family: var(--font-mono);
      font-size: 13px;
      width: 100%;
      margin-bottom: 16px;
      outline: none;
    }
    .input:focus { border-color: var(--orange); }
    .btn {
      background: var(--orange);
      color: #0d0d0c;
      border: 1px solid #ff7733;
      border-radius: 2px;
      padding: 10px;
      width: 100%;
      font-size: 12px;
      font-weight: 700;
      font-family: var(--font-mono);
      text-transform: uppercase;
      letter-spacing: 0.06em;
      cursor: pointer;
      box-shadow: 0 3px 0 var(--orange-shadow);
    }
    .btn:active {
      transform: translateY(2px);
      box-shadow: 0 1px 0 var(--orange-shadow);
    }
    .error-msg {
      color: var(--led-red);
      font-family: var(--font-mono);
      font-size: 11px;
      margin-top: 12px;
      text-align: center;
      min-height: 16px;
    }
  </style>
</head>
<body>
  <div class="login-chassis">
    <div class="header">
      <div class="brand">
        <span style="color:var(--orange);">●</span> ZGATE SYSTEM
      </div>
      <span class="badge">AUTH_REQUIRED</span>
    </div>
    <form id="loginForm" onsubmit="handleLogin(event)">
      <label style="display:block; font-size:11px; font-family:var(--font-mono); color:var(--text-dim); margin-bottom:8px;">MASTER ADMIN PASSWORD</label>
      <input type="password" id="adminKey" class="input" placeholder="Enter GATEWAY_ADMIN_KEY..." required autofocus />
      <button type="submit" class="btn">AUTHENTICATE CHASSIS</button>
      <div id="errMsg" class="error-msg"></div>
    </form>
  </div>
  <script>
    async function handleLogin(e) {
      e.preventDefault();
      const key = document.getElementById('adminKey').value.trim();
      const errEl = document.getElementById('errMsg');
      errEl.textContent = '';
      try {
        const res = await fetch('/api/dashboard/auth/login', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ key })
        });
        const data = await res.json();
        if (data.ok) {
          window.location.reload();
        } else {
          errEl.textContent = 'ACCESS DENIED // INVALID KEY';
        }
      } catch (err) {
        errEl.textContent = 'COMMUNICATION ERROR';
      }
    }
  </script>
</body>
</html>`

func handleDashboardUI(w http.ResponseWriter, r *http.Request) {
	adminKey := resolveAdminKey()
	// Server-side enforcement: if GATEWAY_ADMIN_KEY is configured and session is unauthorized,
	// never send the dashboard HTML payload. Send dedicated login page only.
	if adminKey != "" && !isDashboardAuthorized(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginPageHTML))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
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

type accountQuotaView struct {
	ID            string  `json:"id"`
	Label         string  `json:"label"`
	Default       bool    `json:"default"`
	HasRefresh    bool    `json:"hasRefresh"`
	ExpiresAt     int64   `json:"expiresAt"`
	Expired       bool    `json:"expired"`
	NeedsReauth   bool    `json:"needsReauth"`
	CooldownUntil string  `json:"cooldownUntil"`
	TotalUnits    float64 `json:"totalUnits"`
	UsedUnits     float64 `json:"usedUnits"`
	RemainUnits   float64 `json:"remainUnits"`
	UsagePercent  int     `json:"usagePercent"`
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

	// Read Quota for each active account
	rawList := accounts.List()
	quotaViews := make([]accountQuotaView, 0, len(ring))
	for _, a := range ring {
		view := accountQuotaView{
			ID:            a.ID,
			Label:         a.Label,
			Default:       a.Default,
			HasRefresh:    a.HasRefresh,
			ExpiresAt:     a.ExpiresAt,
			Expired:       a.Expired,
			NeedsReauth:   a.NeedsReauth,
			CooldownUntil: a.CooldownUntil,
		}
		// Find matching full account to read quota
		for _, raw := range rawList {
			if raw.ID == a.ID && raw.JWT != "" {
				q, err := quota.ReadQuota(r.Context(), raw.JWT, raw.DeviceID)
				if err == nil && len(q.Rows) > 0 {
					var total, used, remain float64
					for _, row := range q.Rows {
						if u, ok := row.TotalUnits.(float64); ok && u > total {
							total = u
						}
						if u, ok := row.UsedUnits.(float64); ok {
							used += u
						}
						if u, ok := row.RemainingUnits.(float64); ok && u > remain {
							remain = u
						}
					}
					view.TotalUnits = total
					view.UsedUnits = used
					view.RemainUnits = remain
					if total > 0 {
						pct := int((used / total) * 100)
						if pct > 100 {
							pct = 100
						}
						view.UsagePercent = pct
					}
				}
				break
			}
		}
		quotaViews = append(quotaViews, view)
	}

	token := c.BridgeToken
	if token == "" {
		token = resolvePersistentBridgeToken()
	}

	sendJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"auth":        authMode,
		"bridgeToken": token,
		"laneWidth":   laneWidth,
		"accounts":    quotaViews,
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
		errMsg := err.Error()
		if errMsg == "" {
			errMsg = fmt.Sprintf("%v", err)
		}
		sendJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errMsg})
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

	pollCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	result, ready, err := auth.PollOnce(pollCtx, entry.flow)
	if err != nil {
		errMsg := err.Error()
		if errMsg == "" {
			errMsg = fmt.Sprintf("%v", err)
		}
		sendJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": errMsg})
		return
	}
	if !ready || result == nil {
		sendJSON(w, http.StatusOK, map[string]any{"status": "pending"})
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

func resolvePersistentBridgeToken() string {
	tokenFile := filepath.Join(filepath.Dir(accounts.AccountsFile()), "bridge_token.txt")
	if raw, err := os.ReadFile(tokenFile); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return ""
}

func generateAndStoreBridgeToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to generate entropy: %w", err)
	}
	tok := fmt.Sprintf("sk-zgate-%x", b[:])
	tokenFile := filepath.Join(filepath.Dir(accounts.AccountsFile()), "bridge_token.txt")
	if err := os.WriteFile(tokenFile, []byte(tok), 0600); err != nil {
		return "", fmt.Errorf("failed to save token: %w", err)
	}
	cfgMu.Lock()
	cfg.BridgeToken = tok
	cfgMu.Unlock()
	return tok, nil
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

func handleDashboardTokenGenerate(w http.ResponseWriter, r *http.Request) {
	if !isDashboardAuthorized(r) {
		sendJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Authentication required"})
		return
	}
	newToken, err := generateAndStoreBridgeToken()
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"ok": true, "token": newToken})
}

