package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedactPatterns(t *testing.T) {
	fakeJWT := "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoidV9zZWNyZXQiLCJpYXQiOjE3MDAwMDAwMDB9.c2lnbmF0dXJl"
	fakeEnc := "enc:v1:aXYxMjM0NTY.dGFnMTIzNDU2.Y2lwaGVydGV4dDEyMzQ1Ng"
	raw := `upstream failed: Authorization: Bearer secret-bearer-token-999, jwt=` + fakeJWT +
		`, cred=` + fakeEnc +
		`, body={"token":"tok-abc-123","refresh_token":"rt-secret-456","access_token":"at-secret-789","api_key":"sk-abcdefghijklmnop"}` +
		` x-api-key=my-raw-api-key-val refresh_token=rt-kv-999`

	redacted := RedactSecrets(raw, "custom-bridge-secret-xyz")
	forbidden := []string{
		fakeJWT,
		"aXYxMjM0NTY",
		"secret-bearer-token-999",
		"tok-abc-123",
		"rt-secret-456",
		"at-secret-789",
		"sk-abcdefghijklmnop",
		"my-raw-api-key-val",
		"rt-kv-999",
	}
	for _, s := range forbidden {
		if strings.Contains(redacted, s) {
			t.Fatalf("Redact leaked %q in %q", s, redacted)
		}
	}
}

func TestOAuthErrorsNeverRetainRawSecrets(t *testing.T) {
	leakedJWT := "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyX2lkIjoidV9sZWFrIn0.leaksig"
	leakedRT := "rt-super-secret-value-12345"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"bad token Bearer ` + leakedJWT + `","data":{"token":"` + leakedJWT + `","zai":{"refresh_token":"` + leakedRT + `"}}}`))
	}))
	defer srv.Close()

	origBase := OAuthBase
	OAuthBase = srv.URL
	defer func() { OAuthBase = origBase }()

	ctx := context.Background()
	if _, err := StartDeviceFlow(ctx); err == nil {
		t.Fatal("expected StartDeviceFlow error")
	} else {
		checkNoLeak(t, err, leakedJWT, leakedRT)
	}

	flow := &DeviceFlow{
		FlowID:      "flow_1",
		ExpiresAt:   time.Now().Add(time.Minute),
		IntervalSec: 1,
		PollToken:   "poll-secret-token-123",
	}
	if _, err := PollDeviceFlow(ctx, flow); err == nil {
		t.Fatal("expected PollDeviceFlow error")
	} else {
		checkNoLeak(t, err, leakedJWT, leakedRT, "poll-secret-token-123")
	}

	if _, err := RefreshPlanJWT(ctx, leakedRT); err == nil {
		t.Fatal("expected RefreshPlanJWT error")
	} else {
		checkNoLeak(t, err, leakedJWT, leakedRT)
	}
}

func checkNoLeak(t *testing.T, err error, secrets ...string) {
	t.Helper()
	msg := err.Error()
	for _, s := range secrets {
		if strings.Contains(msg, s) {
			t.Fatalf("error string leaked %q: %s", s, msg)
		}
	}
	if ae, ok := err.(*AuthError); ok && ae.Body != nil {
		t.Fatalf("AuthError.Body must be nil to prevent raw OAuth payload leakage, got: %#v", ae.Body)
	}
}
