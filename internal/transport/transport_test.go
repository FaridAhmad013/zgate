package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
)

func testJWT(user string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"user_id":%q,"iat":1700000000}`, user)))
	return hdr + "." + pay + ".sig"
}

func setupTransportTest(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	t.Setenv("ZCODE_INFERENCE_ORIGIN", srv.URL)
	origInf := InferenceOrigin
	InferenceOrigin = srv.URL
	t.Cleanup(func() { InferenceOrigin = origInf })

	t.Setenv("ZCODE_OAUTH_ORIGIN", srv.URL)
	origOAuth := auth.OAuthBase
	auth.OAuthBase = srv.URL
	t.Cleanup(func() { auth.OAuthBase = origOAuth })

	t.Setenv("ZCODE_PLAN_JWT", "")
	t.Setenv("ZCODE_PLAN_JWT_FILE", filepath.Join(dir, "no-jwt.json"))
	t.Setenv("ZCODE_STATE_DIR", dir)
	t.Setenv("ZCODE_ACCOUNTS_FILE", filepath.Join(dir, "accounts.json"))
	t.Setenv("ZCODE_RETRY_BASE_MS", "5")
	t.Setenv("ZCODE_MAX_ATTEMPTS", "3")
	accounts.ResetForTest()
	t.Cleanup(accounts.ResetForTest)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`)
}

func TestSingleAccountTransientAndRepeated3008DoesNotLockOutAccount(t *testing.T) {
	var calls int32
	mode := "transient" // transient -> fail once with 3008 then 200; repeated -> always 3008; healthy -> 200
	var modeMu sync.Mutex

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		modeMu.Lock()
		m := mode
		modeMu.Unlock()
		if (m == "transient" && n == 1) || m == "repeated" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":3008,"msg":"account busy on desktop"}`)
			return
		}
		writeOK(w)
	})

	jwtSolo := testJWT("u_solo")
	if _, err := accounts.UpsertAccount("solo", "Solo", jwtSolo, "rt-solo", "", 0); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// 1. Transient 3008 on single account retries in-place and succeeds.
	res, err := SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err != nil {
		t.Fatalf("expected transient 3008 on single account to recover via retry, got err: %v", err)
	}
	res.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 upstream attempts (1 fail + 1 retry success), got %d", got)
	}

	// 2. Repeated 3008 stops after ZCODE_MAX_ATTEMPTS (3) and does NOT park the single account for 15m.
	atomic.StoreInt32(&calls, 0)
	modeMu.Lock()
	mode = "repeated"
	modeMu.Unlock()

	_, err = SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err == nil {
		t.Fatal("expected error after repeated 3008 exhausts maxAttempts")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected bounded 3 attempts on repeated 3008, got %d", got)
	}
	if usable := accounts.Usable(nil); len(usable) != 1 {
		t.Fatalf("single account must NOT be locked out in 15m cooldown after 3008, usable=%d", len(usable))
	}

	// 3. Immediately after desktop client finishes, the next request on the single account succeeds.
	modeMu.Lock()
	mode = "healthy"
	modeMu.Unlock()
	res, err = SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err != nil {
		t.Fatalf("single account failed to serve after 3008 cleared: %v", err)
	}
	res.Body.Close()
}

func TestMultiAccount3008RotationQuotaExhaustionAndRecovery(t *testing.T) {
	jwtA := testJWT("u_a")
	jwtB := testJWT("u_b")
	var bearers []string
	var mu sync.Mutex
	failModeA := "3008"

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		authHdr := r.Header.Get("Authorization")
		mu.Lock()
		bearers = append(bearers, authHdr)
		modeA := failModeA
		mu.Unlock()

		if authHdr == "Bearer "+jwtA {
			switch modeA {
			case "3008":
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":3008,"msg":"account concurrency"}`)
				return
			case "1113":
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"code":1113,"msg":"quota exhausted"}`)
				return
			}
		}
		writeOK(w)
	})

	t.Setenv("ZCODE_CONCURRENCY_COOLDOWN_MS", "40")
	_, _ = accounts.UpsertAccount("a", "Account A", jwtA, "rt-a", "", 0)
	_, _ = accounts.UpsertAccount("b", "Account B", jwtB, "rt-b", "", 0)
	accounts.SetDefault("a")

	body := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// 1. Account A hits 3008 -> switches immediately to Account B (1 call each).
	res, err := SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err != nil {
		t.Fatalf("multi-account 3008 rotation failed: %v", err)
	}
	res.Body.Close()

	mu.Lock()
	if len(bearers) != 2 || bearers[0] != "Bearer "+jwtA || bearers[1] != "Bearer "+jwtB {
		t.Fatalf("expected [jwtA, jwtB], got %v", bearers)
	}
	mu.Unlock()

	// 2. After the short 40ms concurrency cooldown expires, Account A recovers and is tried first again.
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	bearers = nil
	failModeA = "ok"
	mu.Unlock()

	res, err = SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err != nil {
		t.Fatalf("recovered account A call failed: %v", err)
	}
	res.Body.Close()

	mu.Lock()
	if len(bearers) != 1 || bearers[0] != "Bearer "+jwtA {
		t.Fatalf("expected recovered account A to serve first, got %v", bearers)
	}

	// 3. Quota exhaustion (1113) parks Account A in quota cooldown and rotates to Account B.
	accounts.SetDefault("a")
	bearers = nil
	failModeA = "1113"
	mu.Unlock()

	res, err = SendAnthropic(SendOpts{Body: body, Context: context.Background()})
	if err != nil {
		t.Fatalf("quota exhaustion rotation failed: %v", err)
	}
	res.Body.Close()

	// Account A should still be in quota cooldown after 60ms (default quota cooldown is 15m).
	time.Sleep(60 * time.Millisecond)
	usable := accounts.Usable(nil)
	if len(usable) != 1 || usable[0].ID != "b" {
		t.Fatalf("expected account A to stay in quota cooldown and only B usable, got %+v", usable)
	}
}

func TestContextCancellationRespectedDuring3008Backoff(t *testing.T) {
	var attempts int32
	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":3008,"msg":"busy"}`)
	})
	t.Setenv("ZCODE_RETRY_BASE_MS", "500")
	_, _ = accounts.UpsertAccount("solo", "Solo", testJWT("u_solo"), "", "", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	body := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	_, err := SendAnthropic(SendOpts{Body: body, Context: ctx})
	if err == nil {
		t.Fatal("expected error on context cancellation")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected backoff to abort on context cancellation after 1 attempt, got %d", got)
	}
}

func TestTenParallel401RequestsTriggerSingleRefresh(t *testing.T) {
	freshJWT := testJWT("u_refreshed")
	var refreshCount int32

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth/token") {
			atomic.AddInt32(&refreshCount, 1)
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"token": freshJWT,
					"zai":   map[string]any{"access_token": "at-2", "refresh_token": "rt-2"},
				},
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+freshJWT {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"msg":"token revoked"}`)
			return
		}
		writeOK(w)
	})

	_, err := accounts.UpsertAccount("shared", "Shared", testJWT("u_stale"), "rt-1", "at-1", 0)
	if err != nil {
		t.Fatal(err)
	}

	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			res, err := SendAnthropic(SendOpts{Body: body, Context: context.Background()})
			if err != nil {
				errs <- err
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("parallel request failed: %v", err)
	}

	if got := atomic.LoadInt32(&refreshCount); got != 1 {
		t.Fatalf("expected exactly 1 token refresh across %d parallel 401 requests, got %d", N, got)
	}
	stored := accounts.List()
	if len(stored) != 1 || stored[0].JWT != freshJWT || stored[0].RefreshToken != "rt-2" {
		t.Fatalf("unexpected stored account after refresh: %+v", stored)
	}
}
