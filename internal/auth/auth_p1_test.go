package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func buildJWT(hdr, claims map[string]any) string {
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." +
		base64.RawURLEncoding.EncodeToString(c) + ".sig"
}

func TestJWTExpirationAndValidationCases(t *testing.T) {
	now := time.Now()
	validHdr := map[string]any{"alg": "HS256", "typ": "JWT"}

	// 1. Valid exp (1 hour in the future).
	futureExp := now.Add(time.Hour).Unix()
	tokValidExp := buildJWT(validHdr, map[string]any{"user_id": "u1", "exp": futureExp})
	if IsExpired(tokValidExp, now) {
		t.Fatal("JWT with future exp must not be expired")
	}
	if expMs, ok := ExpiryUnixMilli(tokValidExp); !ok || expMs != futureExp*1000 {
		t.Fatalf("ExpiryUnixMilli(valid exp) = (%d, %v), want (%d, true)", expMs, ok, futureExp*1000)
	}

	// 2. Expired exp (1 hour in the past, and within 120s ExpirySkew).
	pastExp := now.Add(-time.Hour).Unix()
	tokExpired := buildJWT(validHdr, map[string]any{"user_id": "u1", "exp": pastExp})
	if !IsExpired(tokExpired, now) {
		t.Fatal("JWT with past exp must be expired")
	}
	skewExp := now.Add(30 * time.Second).Unix()
	tokSkew := buildJWT(validHdr, map[string]any{"user_id": "u1", "exp": skewExp})
	if !IsExpired(tokSkew, now) {
		t.Fatal("JWT expiring within ExpirySkew must be treated as expired")
	}

	// 3. Missing exp (ZCode plan JWT contract: {user_id, token_version, sub, iat}).
	tokNoExp := buildJWT(validHdr, map[string]any{"user_id": "u1", "token_version": 1, "iat": now.Add(-72 * time.Hour).Unix()})
	if IsExpired(tokNoExp, now) {
		t.Fatal("plan JWT without exp must NOT be treated as expired after 72h")
	}
	if expMs, ok := ExpiryUnixMilli(tokNoExp); !ok || expMs != 0 {
		t.Fatalf("ExpiryUnixMilli(missing exp) = (%d, %v), want (0, true) — no synthetic expiry", expMs, ok)
	}

	// 4. Invalid exp claims (string, zero, negative).
	for _, badExp := range []any{"tomorrow", "0", 0, -500} {
		tokBadExp := buildJWT(validHdr, map[string]any{"user_id": "u1", "exp": badExp})
		if !IsExpired(tokBadExp, now) {
			t.Fatalf("JWT with invalid exp=%v must be treated as expired/invalid", badExp)
		}
		if _, ok := ExpiryUnixMilli(tokBadExp); ok {
			t.Fatalf("ExpiryUnixMilli(bad exp=%v) succeeded, want false", badExp)
		}
	}

	// 5. Malformed JWTs.
	for _, malformed := range []string{
		"",
		"not-a-jwt",
		"only.two",
		"a.b.c.d",
		"..sig",
		"!!!.@@@.###",
		base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u1"}`)) + ".sig",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + ".sig",
		base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u1"}`)) + ".sig",
	} {
		if _, ok := DecodeJWT(malformed); ok {
			t.Fatalf("DecodeJWT(%q) succeeded, want false", malformed)
		}
		if !IsExpired(malformed, now) {
			t.Fatalf("IsExpired(%q) = false, want true for malformed JWT", malformed)
		}
		if _, ok := ExpiryUnixMilli(malformed); ok {
			t.Fatalf("ExpiryUnixMilli(%q) succeeded, want false", malformed)
		}
	}
}

func TestValidateOriginURLAndFlowIDAndRedirectProtection(t *testing.T) {
	// Valid origins.
	validOrigins := []string{
		"https://zcode.z.ai",
		"https://zcode.z.ai/",
		"http://127.0.0.1:20131",
		"http://localhost:8080",
		"http://[::1]:9000",
	}
	for _, u := range validOrigins {
		if _, err := ValidateOriginURL(u); err != nil {
			t.Fatalf("ValidateOriginURL(%q) failed: %v", u, err)
		}
	}

	// Dangerous / invalid origins.
	badOrigins := []string{
		"",
		"http://zcode.z.ai",
		"http://169.254.169.254",
		"http://192.168.1.1:8080",
		"ftp://zcode.z.ai",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"https://user:pass@zcode.z.ai",
		"https://zcode.z.ai/api/v1",
		"https://zcode.z.ai?redirect=http://evil.com",
		"https://zcode.z.ai#frag",
	}
	for _, u := range badOrigins {
		if _, err := ValidateOriginURL(u); err == nil {
			t.Fatalf("ValidateOriginURL(%q) succeeded, want error", u)
		}
	}

	// Dangerous flow_id values must be rejected before polling.
	for _, badFlowID := range []string{"", "   ", "../admin", "../../etc/passwd", "flow/sub", `flow\sub`, "flow?x=1", "flow#1", "flow%2f1"} {
		err := validateFlowID(badFlowID)
		if err == nil {
			t.Fatalf("validateFlowID(%q) succeeded, want error", badFlowID)
		}
		_, pollErr := PollDeviceFlow(context.Background(), &DeviceFlow{
			FlowID:      badFlowID,
			ExpiresAt:   time.Now().Add(time.Minute),
			IntervalSec: 1,
			PollToken:   "tok",
		})
		if pollErr == nil {
			t.Fatalf("PollDeviceFlow with flow_id=%q succeeded, want error", badFlowID)
		}
	}

	// Redirect protection: HTTP client must not follow 302 redirects on OAuth calls.
	var redirectTargetHit int32
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&redirectTargetHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+"/steal-token", http.StatusFound)
	}))
	defer redirectSrv.Close()

	origBase := OAuthBase
	OAuthBase = redirectSrv.URL
	defer func() { OAuthBase = origBase }()

	_, err := RefreshPlanJWT(context.Background(), "rt-test")
	if err == nil {
		t.Fatal("expected RefreshPlanJWT to fail on 302 redirect")
	}
	if atomic.LoadInt32(&redirectTargetHit) != 0 {
		t.Fatal("OAuth HTTP client followed redirect to external target!")
	}
}
