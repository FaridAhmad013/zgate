package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestValidateConfigAndBindHostPrecedence(t *testing.T) {
	// 1. Default localhost when neither BIND_HOST nor HOST is set.
	t.Setenv("BIND_HOST", "")
	t.Setenv("HOST", "")
	if got := ResolveBindHost(); got != "127.0.0.1" {
		t.Fatalf("default ResolveBindHost() = %q, want 127.0.0.1", got)
	}
	if err := ValidateConfig(Config{Host: ResolveBindHost(), Port: 20131}); err != nil {
		t.Fatalf("default localhost must be valid without token: %v", err)
	}

	// 2. HOST used when BIND_HOST is unset; BIND_HOST wins when both are set.
	t.Setenv("HOST", "localhost")
	if got := ResolveBindHost(); got != "localhost" {
		t.Fatalf("HOST fallback = %q, want localhost", got)
	}
	t.Setenv("BIND_HOST", "127.0.0.1")
	if got := ResolveBindHost(); got != "127.0.0.1" {
		t.Fatalf("BIND_HOST priority = %q, want 127.0.0.1", got)
	}

	// 3. Explicit loopback addresses valid without token.
	for _, h := range []string{"127.0.0.1", "127.0.0.2", "localhost", "::1", "[::1]"} {
		if !IsLoopbackHost(h) {
			t.Fatalf("IsLoopbackHost(%q) = false, want true", h)
		}
		if err := ValidateConfig(Config{Host: h, Port: 20131, BridgeToken: ""}); err != nil {
			t.Fatalf("ValidateConfig(%q) without token failed: %v", h, err)
		}
	}

	// 4. 0.0.0.0 and public/LAN IPs without token must fail startup validation.
	for _, h := range []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "10.0.0.5", "203.0.113.42", "gateway.example.com"} {
		if IsLoopbackHost(h) {
			t.Fatalf("IsLoopbackHost(%q) = true, want false", h)
		}
		if err := ValidateConfig(Config{Host: h, Port: 20131, BridgeToken: ""}); err == nil {
			t.Fatalf("ValidateConfig(%q) without token succeeded, want error", h)
		}
		Configure(Config{Host: h, Port: 0, BridgeToken: ""})
		if err := ListenAndServe(); err == nil {
			t.Fatalf("ListenAndServe(%q) without token succeeded, want error", h)
		}
	}

	// 5. 0.0.0.0 and public IPs with token must pass validation.
	for _, h := range []string{"0.0.0.0", "::", "192.168.1.10", "203.0.113.42", "gateway.example.com"} {
		if err := ValidateConfig(Config{Host: h, Port: 20131, BridgeToken: "strong-secret-token"}); err != nil {
			t.Fatalf("ValidateConfig(%q) with token failed: %v", h, err)
		}
	}

	// 6. Invalid host and port configurations must fail even with token.
	for _, badHost := range []string{"", "   ", "0.0.0.0:20131", "http://127.0.0.1", "127.0.0.1/path", "256.256.256.256", "999.1.1.1", "bad host", "-invalid.host", "invalid..host"} {
		if err := ValidateHost(badHost); err == nil {
			t.Fatalf("ValidateHost(%q) succeeded, want error", badHost)
		}
		if err := ValidateConfig(Config{Host: badHost, Port: 20131, BridgeToken: "tok"}); err == nil {
			t.Fatalf("ValidateConfig(%q) succeeded, want error", badHost)
		}
	}
	for _, badPort := range []int{-1, 65536, 100000} {
		if err := ValidateConfig(Config{Host: "127.0.0.1", Port: badPort, BridgeToken: "tok"}); err == nil {
			t.Fatalf("ValidateConfig(port=%d) succeeded, want error", badPort)
		}
	}

	// Restore test bridge config.
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})
}

func TestMainFailsStartupOnPublicBindWithoutToken(t *testing.T) {
	cmd := exec.Command("go", "run", "../../cmd/zcode-gateway")
	cmd.Env = append(os.Environ(), "BIND_HOST=0.0.0.0", "ZCODE_BRIDGE_TOKEN=", "CREDENTIALS_DIRECTORY=")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected binary to exit non-zero on BIND_HOST=0.0.0.0 without token, output: %s", string(out))
	}
	if !strings.Contains(string(out), "ZCODE_BRIDGE_TOKEN") {
		t.Fatalf("expected startup error mentioning ZCODE_BRIDGE_TOKEN, got: %s", string(out))
	}
}

func TestAuthenticationAndEndpointProtection(t *testing.T) {
	setupBridge(t)
	writeAccounts(t, []map[string]any{
		{"id": "good", "label": "good", "email": "secret-user@example.com", "jwt": JWT, "userId": "u_smoke", "deviceId": "0a7c2d9d-ce62-4456-aa65-625511ff1ef0", "sessionSeed": "seed-good", "expiresAt": time.Now().UnixMilli() + day(), "needsReauth": false},
	}, "good")
	defer func() {
		_ = os.Remove(os.Getenv("ZCODE_ACCOUNTS_FILE"))
		Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})
	}()

	secretToken := "bridge-secret-token-p0"
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: secretToken, ModelsCache: 100 * time.Millisecond})

	// 1. /healthz must return minimal response without auth and leak no details.
	st, text, parsed := doRequest(t, "GET", "/healthz", nil)
	if st != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", st)
	}
	if len(parsed) != 1 || parsed["ok"] != true {
		t.Fatalf("/healthz must return only {\"ok\":true}, got: %s", text)
	}

	// 2. All sensitive routes must reject unauthorized requests with 401 and never echo secretToken.
	sensitiveRoutes := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{"GET", "/health", nil},
		{"GET", "/v1/models", nil},
		{"GET", "/models", nil},
		{"GET", "/v1/quota", nil},
		{"GET", "/quota", nil},
		{"POST", "/v1/messages", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
		{"POST", "/v1/chat/completions", map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}},
	}

	for _, rt := range sensitiveRoutes {
		for _, hdr := range []map[string]string{
			nil,
			{"Authorization": "Bearer wrong-token"},
			{"X-Api-Key": "wrong-token"},
			{"Authorization": "Basic " + secretToken},
		} {
			status, respText := rawRequest(t, rt.method, rt.path, "application/json", rt.body, hdr)
			if status != http.StatusUnauthorized {
				t.Fatalf("%s %s with hdr %v -> status %d, want 401", rt.method, rt.path, hdr, status)
			}
			if strings.Contains(respText, secretToken) || strings.Contains(respText, "wrong-token") {
				t.Fatalf("%s %s 401 response leaked token: %s", rt.method, rt.path, respText)
			}
		}
	}

	// 3. Authorized requests (both Authorization: Bearer and X-Api-Key) must succeed.
	for _, authHdr := range []map[string]string{
		{"Authorization": "Bearer " + secretToken},
		{"X-Api-Key": secretToken},
	} {
		for _, rt := range sensitiveRoutes {
			status, respText := rawRequest(t, rt.method, rt.path, "application/json", rt.body, authHdr)
			if status != http.StatusOK {
				t.Fatalf("authorized %s %s failed with %d: %s", rt.method, rt.path, status, respText)
			}
			if rt.path == "/health" {
				if strings.Contains(respText, "secret-user@example.com") || strings.Contains(respText, "u_smoke") {
					t.Fatalf("/health leaked email or userId: %s", respText)
				}
			}
		}
	}
}

func TestCSRFAndDNSRebindingProtection(t *testing.T) {
	setupBridge(t)
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})
	defer Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	chatBody := map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}

	// 1. Browser text/plain, form-urlencoded, and missing Content-Type POSTs must be rejected (415).
	for _, ct := range []string{"text/plain", "text/plain; charset=utf-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=xxx", ""} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			status, _ := rawRequest(t, "POST", path, ct, chatBody, nil)
			if status != http.StatusUnsupportedMediaType {
				t.Fatalf("POST %s with Content-Type %q -> %d, want 415", path, ct, status)
			}
		}
	}

	// 2. Foreign Origin must be rejected on POST, GET, and OPTIONS preflight.
	t.Setenv("ZCODE_CORS_ORIGIN", "")
	for _, method := range []string{"POST", "GET", "OPTIONS"} {
		path := "/v1/chat/completions"
		var b map[string]any
		if method == "POST" {
			b = chatBody
		} else if method == "GET" {
			path = "/v1/models"
		}
		status, _ := rawRequest(t, method, path, "application/json", b, map[string]string{"Origin": "https://evil.example"})
		if status != http.StatusForbidden {
			t.Fatalf("%s %s with foreign Origin -> %d, want 403", method, path, status)
		}
	}

	// 3. Wildcard ZCODE_CORS_ORIGIN=* must NOT allow foreign Origins.
	t.Setenv("ZCODE_CORS_ORIGIN", "*")
	status, _ := rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatBody, map[string]string{"Origin": "https://evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("wildcard ZCODE_CORS_ORIGIN=* allowed foreign Origin: status %d, want 403", status)
	}

	// 4. Explicit allowed Origin succeeds; unlisted Origin is rejected.
	t.Setenv("ZCODE_CORS_ORIGIN", "https://router.internal.example")
	status, _ = rawRequest(t, "OPTIONS", "/v1/chat/completions", "application/json", nil, map[string]string{"Origin": "https://router.internal.example"})
	if status != http.StatusNoContent {
		t.Fatalf("allowed Origin preflight -> %d, want 204", status)
	}
	status, _ = rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatBody, map[string]string{"Origin": "https://router.internal.example"})
	if status != http.StatusOK {
		t.Fatalf("allowed Origin POST -> %d, want 200", status)
	}
	status, _ = rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatBody, map[string]string{"Origin": "https://evil.example"})
	if status != http.StatusForbidden {
		t.Fatalf("unlisted Origin POST -> %d, want 403", status)
	}
	t.Setenv("ZCODE_CORS_ORIGIN", "")

	// 5. Sec-Fetch-Site: cross-site without allowed Origin must be rejected.
	status, _ = rawRequest(t, "POST", "/v1/chat/completions", "application/json", chatBody, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if status != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site: cross-site -> %d, want 403", status)
	}

	// 6. Invalid Host header (DNS rebinding) must be rejected even if X-Forwarded-Host is localhost.
	for _, badHost := range []string{"attacker.example:20131", "evil.com", "127.0.0.1.evil.com:20131"} {
		status, _ := rawRequest(t, "GET", "/healthz", "application/json", nil, map[string]string{
			"Host":             badHost,
			"X-Forwarded-Host": "127.0.0.1:20131",
		})
		if status != http.StatusForbidden {
			t.Fatalf("Host %q -> status %d, want 403", badHost, status)
		}
	}

	// 7. Explicit ZCODE_ALLOWED_HOSTS permits a configured reverse-proxy Host header.
	t.Setenv("ZCODE_ALLOWED_HOSTS", "ai.internal.example")
	status, _ = rawRequest(t, "GET", "/healthz", "application/json", nil, map[string]string{"Host": "ai.internal.example:443"})
	if status != http.StatusOK {
		t.Fatalf("allowed Host header -> %d, want 200", status)
	}
	t.Setenv("ZCODE_ALLOWED_HOSTS", "")
}

func TestPublicBindWithTokenStartsAndServes(t *testing.T) {
	setupBridge(t)
	token := "public-bind-valid-token-123"
	Configure(Config{Port: 0, Host: "0.0.0.0", BridgeToken: token, ModelsCache: 100 * time.Millisecond})
	defer Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	if err := ValidateConfig(getConfig()); err != nil {
		t.Fatalf("public bind with token must pass ValidateConfig: %v", err)
	}
	status, _ := rawRequest(t, "GET", "/v1/models", "application/json", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request on 0.0.0.0 bind -> %d, want 401", status)
	}
	status, _ = rawRequest(t, "GET", "/v1/models", "application/json", nil, map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusOK {
		t.Fatalf("authenticated request on 0.0.0.0 bind -> %d, want 200", status)
	}
}

func TestHTTPErrorResponsesRedactCredentials(t *testing.T) {
	setupBridge(t)
	secretToken := "bridge-secret-xyz-98765"
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: secretToken, ModelsCache: 100 * time.Millisecond})
	defer Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	leakedJWT := "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoidV9sZWFrZWQifQ.sig12345"
	leakedEnc := "enc:v1:aXYxMjM.dGFnMTIz.ZGF0YTEyMw"
	msg := "failed with Bearer " + leakedJWT + " and " + leakedEnc + " and " + secretToken + " and refresh_token=rt-leak-999"

	rec := redactMsg(msg)
	for _, secret := range []string{leakedJWT, "aXYxMjM", secretToken, "rt-leak-999"} {
		if strings.Contains(rec, secret) {
			t.Fatalf("redactMsg leaked %q in %q", secret, rec)
		}
	}
}

func rawRequest(t *testing.T, method, path, contentType string, body map[string]any, headers map[string]string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, bridgeURL()+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}
