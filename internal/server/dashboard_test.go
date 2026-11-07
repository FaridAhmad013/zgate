package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardUIAndAuthFlow(t *testing.T) {
	setupBridge(t)
	defer func() {
		Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: ""})
		t.Setenv("GATEWAY_ADMIN_KEY", "")
	}()

	// 1. Without GATEWAY_ADMIN_KEY: /api/dashboard/auth/verify returns requiresAuth=false
	t.Setenv("GATEWAY_ADMIN_KEY", "")
	reqVerify := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20131/api/dashboard/auth/verify", nil)
	wVerify := httptest.NewRecorder()
	Route().ServeHTTP(wVerify, reqVerify)
	if wVerify.Code != http.StatusOK {
		t.Fatalf("auth verify returned %d, want 200", wVerify.Code)
	}
	var resVerify map[string]any
	_ = json.Unmarshal(wVerify.Body.Bytes(), &resVerify)
	if resVerify["requiresAuth"] != false || resVerify["authorized"] != true {
		t.Fatalf("expected open dashboard, got: %+v", resVerify)
	}

	// 2. With GATEWAY_ADMIN_KEY: unauthenticated /api/dashboard/status returns 401
	adminSecret := "vps-super-admin-secret"
	t.Setenv("GATEWAY_ADMIN_KEY", adminSecret)

	reqStatusBlocked := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20131/api/dashboard/status", nil)
	wStatusBlocked := httptest.NewRecorder()
	Route().ServeHTTP(wStatusBlocked, reqStatusBlocked)
	if wStatusBlocked.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when GATEWAY_ADMIN_KEY set without session, got %d", wStatusBlocked.Code)
	}

	// 3. Login with correct GATEWAY_ADMIN_KEY sets cookie
	loginBody, _ := json.Marshal(map[string]any{"key": adminSecret})
	reqLogin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20131/api/dashboard/auth/login", bytes.NewReader(loginBody))
	wLogin := httptest.NewRecorder()
	Route().ServeHTTP(wLogin, reqLogin)
	if wLogin.Code != http.StatusOK {
		t.Fatalf("expected 200 on correct login, got %d", wLogin.Code)
	}
	cookies := wLogin.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "gateway_admin_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil || sessionCookie.Value != adminSecret {
		t.Fatalf("expected session cookie set to adminSecret, got %+v", sessionCookie)
	}

	// 4. Authenticated request with cookie succeeds
	reqStatusAuth := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20131/api/dashboard/status", nil)
	reqStatusAuth.AddCookie(sessionCookie)
	wStatusAuth := httptest.NewRecorder()
	Route().ServeHTTP(wStatusAuth, reqStatusAuth)
	if wStatusAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 with session cookie, got %d", wStatusAuth.Code)
	}

	// 5. Verify HTML UI does not contain emojis
	reqUI := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20131/dashboard", nil)
	wUI := httptest.NewRecorder()
	Route().ServeHTTP(wUI, reqUI)
	if !strings.Contains(wUI.Body.String(), "<svg") {
		t.Fatal("expected dashboard HTML to use SVG icons")
	}
}
