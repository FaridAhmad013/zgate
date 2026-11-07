package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/transport"
)

func startTestServer(t *testing.T, ctx context.Context) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, ln)
	}()
	return "http://" + ln.Addr().String(), errCh
}

func TestHTTPServerHardeningHeadersBodyAndLongStream(t *testing.T) {
	setupBridge(t)
	t.Setenv("ZCODE_MAX_HEADER_BYTES", "2048")
	t.Setenv("ZCODE_READ_HEADER_TIMEOUT_MS", "90")
	t.Setenv("ZCODE_BODY_READ_TIMEOUT_MS", "100")
	t.Setenv("ZCODE_MAX_BODY_BYTES", "1024")
	t.Setenv("ZCODE_RATE_LIMIT_RPS", "0")

	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseURL, errCh := startTestServer(t, ctx)
	defer func() {
		cancel()
		<-errCh
	}()

	// 1. Oversized HTTP header (> ZCODE_MAX_HEADER_BYTES + 4096 stdlib slop) -> 431 Request Header Fields Too Large.
	req, _ := http.NewRequest("GET", baseURL+"/healthz", nil)
	req.Header.Set("X-Huge", strings.Repeat("H", 8192))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversized header request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized header status = %d, want 431", resp.StatusCode)
	}

	// 2. Slow header (> ZCODE_READ_HEADER_TIMEOUT_MS) -> server closes connection.
	hostPort := strings.TrimPrefix(baseURL, "http://")
	conn, err := net.Dial("tcp", hostPort)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	_, _ = conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n"))
	time.Sleep(180 * time.Millisecond) // exceeds 90ms ReadHeaderTimeout
	_, _ = conn.Write([]byte("X-Late: 1\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 256)
	n, readErr := conn.Read(buf)
	_ = conn.Close()
	if readErr == nil && strings.Contains(string(buf[:n]), "200 OK") {
		t.Fatalf("slow header request unexpectedly succeeded: %s", string(buf[:n]))
	}

	// 3. Oversized request body (> ZCODE_MAX_BODY_BYTES=1024) -> 413 in OpenAI dialect.
	bigPayload := fmt.Sprintf(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"%s"}]}`, strings.Repeat("x", 2048))
	resp, err = http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(bigPayload))
	if err != nil {
		t.Fatalf("oversized body request failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413 (%s)", resp.StatusCode, string(bodyBytes))
	}

	// 4. Slow request body (> ZCODE_BODY_READ_TIMEOUT_MS=100ms) -> 408 Request Timeout.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"model":"glm-5.3-flash","messages":[`))
		time.Sleep(220 * time.Millisecond) // exceeds 100ms body read timeout
		_, _ = pw.Write([]byte(`{"role":"user","content":"late"}]}`))
		_ = pw.Close()
	}()
	slowReq, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", pr)
	slowReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(slowReq)
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("slow body status = %d, want 408 (%s)", resp.StatusCode, string(b))
		}
	}

	// 5. Gradual request body within deadline followed by a long SSE response stream
	// (> 2x ReadHeaderTimeout and > 2x BodyReadTimeout) completes without interruption!
	slowStreamUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"part-%d \"}}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(45 * time.Millisecond)
		}
		_, _ = fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer slowStreamUpstream.Close()
	origInf := transport.InferenceOrigin
	transport.InferenceOrigin = slowStreamUpstream.URL
	defer func() { transport.InferenceOrigin = origInf }()

	gpr, gpw := io.Pipe()
	go func() {
		_, _ = gpw.Write([]byte(`{"model":"glm-5.3-flash","stream":true,`))
		time.Sleep(25 * time.Millisecond) // gradual body within 100ms limit
		_, _ = gpw.Write([]byte(`"messages":[{"role":"user","content":"go"}]}`))
		_ = gpw.Close()
	}()
	gradReq, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", gpr)
	gradReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(gradReq)
	if err != nil {
		t.Fatalf("gradual body + long stream failed: %v", err)
	}
	streamOut, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("long SSE stream interrupted (status=%d, err=%v): %s", resp.StatusCode, readErr, string(streamOut))
	}
	if !strings.Contains(string(streamOut), "part-4") || !strings.Contains(string(streamOut), "[DONE]") {
		t.Fatalf("long SSE stream incomplete: %s", string(streamOut))
	}
}

func TestRateLimitingAndTrustedProxyHandling(t *testing.T) {
	setupBridge(t)
	t.Setenv("ZCODE_RATE_LIMIT_RPS", "1")
	t.Setenv("ZCODE_RATE_LIMIT_BURST", "2")
	t.Setenv("ZCODE_TRUSTED_PROXIES", "")

	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})

	// 1. Burst of 2 succeeds; 3rd request fails with 429 + Retry-After header.
	for i := 0; i < 2; i++ {
		st, _ := rawRequest(t, "GET", "/v1/models", "", nil, nil)
		if st != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, st)
		}
	}

	// Attempting to bypass rate limit by spoofing X-Forwarded-For, X-Real-IP, or X-Session-Id
	// when ZCODE_TRUSTED_PROXIES is empty must STILL be blocked with 429!
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
		req.Host = "127.0.0.1:20131"
		req.RemoteAddr = "192.0.2.10:54321"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		Route().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("initial req %d status = %d", i+1, rec.Code)
		}
	}
	bypassReq := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`))
	bypassReq.Host = "127.0.0.1:20131"
	bypassReq.RemoteAddr = "192.0.2.10:54321"
	bypassReq.Header.Set("Content-Type", "application/json")
	bypassReq.Header.Set("X-Forwarded-For", "8.8.8.8")
	bypassReq.Header.Set("X-Real-IP", "8.8.4.4")
	bypassReq.Header.Set("X-Session-Id", "new-session-999")
	bypassReq.Header.Set("User-Agent", "different-agent")
	rec := httptest.NewRecorder()
	Route().ServeHTTP(rec, bypassReq)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 when untrusted peer spoofs headers, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}
	var oaiErr map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &oaiErr)
	if errObj, _ := oaiErr["error"].(map[string]any); errObj == nil || errObj["type"] != "rate_limit_error" {
		t.Fatalf("expected OpenAI rate_limit_error envelope, got: %s", rec.Body.String())
	}

	// /healthz liveness endpoint remains exempt from rate limiting.
	hzRec := httptest.NewRecorder()
	hzReq := httptest.NewRequest("GET", "/healthz", nil)
	hzReq.Host = "127.0.0.1:20131"
	hzReq.RemoteAddr = "192.0.2.10:54321"
	Route().ServeHTTP(hzRec, hzReq)
	if hzRec.Code != http.StatusOK {
		t.Fatalf("/healthz should not be rate limited, got %d", hzRec.Code)
	}

	// 2. Trusted proxy mode: right-most untrusted IP in X-Forwarded-For is used.
	t.Setenv("ZCODE_TRUSTED_PROXIES", "127.0.0.1,10.0.0.0/8")
	resetRateLimiter()

	sendViaProxy := func(xff string) int {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Host = "127.0.0.1:20131"
		r.RemoteAddr = "127.0.0.1:40000" // trusted proxy
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		Route().ServeHTTP(w, r)
		return w.Code
	}

	// Client 203.0.113.50 sends 2 requests while spoofing different leading XFF IPs.
	if st := sendViaProxy("1.1.1.1, 203.0.113.50"); st != http.StatusOK {
		t.Fatalf("proxy req 1 status = %d", st)
	}
	if st := sendViaProxy("2.2.2.2, 203.0.113.50"); st != http.StatusOK {
		t.Fatalf("proxy req 2 status = %d", st)
	}
	// 3rd request from 203.0.113.50 (even with a new spoofed prefix 3.3.3.3) is blocked!
	if st := sendViaProxy("3.3.3.3, 203.0.113.50"); st != http.StatusTooManyRequests {
		t.Fatalf("proxy req 3 from same real client IP should be 429, got %d", st)
	}
	// Different real client IP (203.0.113.51) through the same proxy succeeds!
	if st := sendViaProxy("203.0.113.51"); st != http.StatusOK {
		t.Fatalf("different client IP through trusted proxy should succeed, got %d", st)
	}
}

func TestGracefulShutdownIdleActiveStreamAndTimeout(t *testing.T) {
	setupBridge(t)
	t.Setenv("ZCODE_RATE_LIMIT_RPS", "0")

	// 1. Shutdown when idle -> returns nil promptly.
	t.Setenv("ZCODE_SHUTDOWN_TIMEOUT_MS", "500")
	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 100 * time.Millisecond})
	idleCtx, idleCancel := context.WithCancel(context.Background())
	_, idleErrCh := startTestServer(t, idleCtx)
	idleCancel()
	if err := <-idleErrCh; err != nil {
		t.Fatalf("idle shutdown returned error: %v", err)
	}

	// 2. Shutdown while normal request and active SSE stream are in flight (completing within timeout).
	streamStarted := make(chan struct{})
	allowFinish := make(chan struct{})
	var upstreamAborted atomic.Bool

	graceUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_grace\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case streamStarted <- struct{}{}:
		default:
		}
		select {
		case <-allowFinish:
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"FINISHED\"}}\n\n")
			_, _ = fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			upstreamAborted.Store(true)
		}
	}))
	defer graceUpstream.Close()

	origInf := transport.InferenceOrigin
	transport.InferenceOrigin = graceUpstream.URL
	defer func() { transport.InferenceOrigin = origInf }()

	t.Setenv("ZCODE_SHUTDOWN_TIMEOUT_MS", "1500")
	graceCtx, graceCancel := context.WithCancel(context.Background())
	baseURL, graceErrCh := startTestServer(t, graceCtx)

	streamDone := make(chan string, 1)
	go func() {
		resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"grace"}]}`))
		if err != nil {
			streamDone <- "ERR:" + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		streamDone <- string(b)
	}()

	<-streamStarted
	// Trigger graceful shutdown while SSE stream is active.
	graceCancel()
	// Let the active stream finish 60ms later (well within 1500ms shutdown window).
	time.Sleep(60 * time.Millisecond)
	close(allowFinish)

	out := <-streamDone
	if !strings.Contains(out, "FINISHED") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("active SSE stream did not finish cleanly during graceful shutdown: %s", out)
	}
	if err := <-graceErrCh; err != nil {
		t.Fatalf("graceful shutdown after stream drain returned error: %v", err)
	}

	// 3. Shutdown when upstream hangs past ZCODE_SHUTDOWN_TIMEOUT_MS -> cancels stream context & releases resources.
	t.Setenv("ZCODE_SHUTDOWN_TIMEOUT_MS", "120")
	hangStarted := make(chan struct{})
	hangUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_hang\"}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		close(hangStarted)
		<-r.Context().Done()
		upstreamAborted.Store(true)
	}))
	defer hangUpstream.Close()
	transport.InferenceOrigin = hangUpstream.URL

	upstreamAborted.Store(false)
	hangCtx, hangCancel := context.WithCancel(context.Background())
	hangURL, hangErrCh := startTestServer(t, hangCtx)

	go func() {
		resp, err := http.Post(hangURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"hang"}]}`))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		_, _ = bufio.NewReader(resp.Body).ReadString('\n')
		_, _ = io.ReadAll(resp.Body)
	}()

	<-hangStarted
	hangCancel()
	shutErr := <-hangErrCh
	if !errors.Is(shutErr, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded when shutdown timeout expires on hanging stream, got: %v", shutErr)
	}
	time.Sleep(30 * time.Millisecond)
	if !upstreamAborted.Load() {
		t.Fatal("expected hanging upstream stream context to be canceled when shutdown timeout expires")
	}
	runtime.GC()
}

func TestObservabilityLoggingNeverLeaksSecretsOrPayloads(t *testing.T) {
	setupBridge(t)
	t.Setenv("ZCODE_RATE_LIMIT_RPS", "0")

	var logMu sync.Mutex
	var logs bytes.Buffer
	secretToken := "bridge-secret-token-xyz987"
	secretPrompt := "MY_SUPER_SECRET_USER_PROMPT_DO_NOT_LOG"
	secretImage := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAUA"

	Configure(Config{
		Port:        0,
		Host:        "127.0.0.1",
		BridgeToken: secretToken,
		ModelsCache: 100 * time.Millisecond,
		LogFunc: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(&logs, format+"\n", args...)
		},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"text","text":"%s"},{"type":"image_url","image_url":{"url":"%s"}}]}]}`,
		secretPrompt, secretImage)))
	req.Host = "127.0.0.1:20131"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secretToken)
	req.Header.Set("X-Request-Id", "req_test_observability_01")

	rec := httptest.NewRecorder()
	Route().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if gotID := rec.Header().Get("X-Request-Id"); gotID != "req_test_observability_01" {
		t.Fatalf("X-Request-Id = %q, want req_test_observability_01", gotID)
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "req_id=req_test_observability_01") ||
		!strings.Contains(logOutput, "method=POST") ||
		!strings.Contains(logOutput, "path=/v1/chat/completions") ||
		!strings.Contains(logOutput, "status=200") ||
		!strings.Contains(logOutput, "duration_ms=") {
		t.Fatalf("missing required observability fields in log: %s", logOutput)
	}
	for _, forbidden := range []string{secretToken, secretPrompt, secretImage, "eyJhbGci"} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("observability log leaked sensitive value %q in: %s", forbidden, logOutput)
		}
	}
}
