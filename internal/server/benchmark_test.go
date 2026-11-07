package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
	"github.com/apissaj/zcode-gateway/internal/auth"
	"github.com/apissaj/zcode-gateway/internal/models"
	"github.com/apissaj/zcode-gateway/internal/transport"
)

func setupBenchEnv(b testing.TB) func() {
	b.Helper()
	dir := b.TempDir()
	prevFile := os.Getenv("ZCODE_ACCOUNTS_FILE")
	prevMid := os.Getenv("ZCODE_IDENTITY_DEVICE_MID")
	prevRate := os.Getenv("ZCODE_RATE_LIMIT_RPS")
	prevLane := os.Getenv("ZCODE_LANE_WIDTH")
	prevAcctConc := os.Getenv("ZCODE_ACCOUNT_CONCURRENCY")
	prevLog := os.Getenv("ZCODE_ACCESS_LOG")

	_ = os.Setenv("ZCODE_ACCOUNTS_FILE", filepath.Join(dir, "accounts.json"))
	_ = os.Setenv("ZCODE_IDENTITY_DEVICE_MID", "bench-device-mid-0001")
	_ = os.Setenv("ZCODE_RATE_LIMIT_RPS", "0")
	_ = os.Setenv("ZCODE_LANE_WIDTH", "128")
	_ = os.Setenv("ZCODE_ACCOUNT_CONCURRENCY", "128")
	_ = os.Setenv("ZCODE_ACCESS_LOG", "0")

	jwt1 := fakeJWT("bench_u1")
	jwt2 := fakeJWT("bench_u2")
	_, _ = accounts.UpsertAccount("b1", "Bench-1", jwt1, "rt_bench_1", "at_1", time.Now().Add(24*time.Hour).UnixMilli())
	_, _ = accounts.UpsertAccount("b2", "Bench-2", jwt2, "rt_bench_2", "at_2", time.Now().Add(24*time.Hour).UnixMilli())

	// Advance past the 25ms post-write verification window so benchmarks measure
	// steady-state in-memory cache performance.
	time.Sleep(30 * time.Millisecond)
	_ = accounts.Usable(nil)

	Configure(Config{Port: 0, Host: "127.0.0.1", BridgeToken: "", ModelsCache: 5 * time.Minute})

	return func() {
		_ = os.Setenv("ZCODE_ACCOUNTS_FILE", prevFile)
		_ = os.Setenv("ZCODE_IDENTITY_DEVICE_MID", prevMid)
		_ = os.Setenv("ZCODE_RATE_LIMIT_RPS", prevRate)
		_ = os.Setenv("ZCODE_LANE_WIDTH", prevLane)
		_ = os.Setenv("ZCODE_ACCOUNT_CONCURRENCY", prevAcctConc)
		_ = os.Setenv("ZCODE_ACCESS_LOG", prevLog)
	}
}

// --- Section 2: Internal Component Benchmarks (testing.B) ---

func BenchmarkSSEReader(b *testing.B) {
	payload := strings.Repeat(
		"event: content_block_delta\r\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world chunk\"}}\r\n\r\n",
		20,
	) + "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	raw := []byte(payload)

	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := transport.NewSSEReader(bytes.NewReader(raw), 0)
		for {
			_, err := r.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkValidateRequest(b *testing.B) {
	reqBody := map[string]any{
		"model":            "glm-5.3-flash:high",
		"max_tokens":       float64(4096),
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful coding assistant."},
			map[string]any{"role": "user", "content": "Write a concurrent worker pool in Go."},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "run_test",
					"parameters": map[string]any{"type": "object"},
				},
			},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := models.ValidateRequest(reqBody, "openai"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionKeyFor(b *testing.B) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	body := map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "System prompt instructions here."},
			map[string]any{"role": "user", "content": "First user message in conversation."},
			map[string]any{"role": "assistant", "content": "Assistant response."},
			map[string]any{"role": "user", "content": "Follow-up question."},
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if k := SessionKeyFor(req, body); k == "" {
			b.Fatal("empty key")
		}
	}
}

func BenchmarkRateLimiter(b *testing.B) {
	cleanup := setupBenchEnv(b)
	defer cleanup()
	_ = os.Setenv("ZCODE_RATE_LIMIT_RPS", "1000000")
	_ = os.Setenv("ZCODE_RATE_LIMIT_BURST", "1000000")
	resetRateLimiter()

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:54321"

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, _ := allowRequest(req)
		if !ok {
			b.Fatal("rate limited unexpectedly")
		}
	}
}

func BenchmarkAccountSelection(b *testing.B) {
	cleanup := setupBenchEnv(b)
	defer cleanup()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cands, err := transport.Candidates("", "")
		if err != nil || len(cands) == 0 {
			b.Fatalf("Candidates failed: %v", err)
		}
	}
}

func BenchmarkTokenRefreshCoordination(b *testing.B) {
	cleanup := setupBenchEnv(b)
	defer cleanup()

	accts := accounts.List()
	target := accts[0]
	currentJWT := target.JWT

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Pass a synthetic staleJWT that differs from currentJWT so the fast
			// singleflight coordination check sees the token is already fresh.
			_, _, err := accounts.RefreshAccountSingleflight(context.Background(), target.ID, "stale-jwt-value")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	_ = currentJWT
}

func BenchmarkErrorRedaction(b *testing.B) {
	sample := `upstream failed: Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1c2VyX2lkIjoidTEifQ.sig and {"refreshToken":"rt_secret_999"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := auth.RedactSecrets(sample, "my-bridge-secret-token")
		if strings.Contains(out, "rt_secret_999") {
			b.Fatal("unredacted")
		}
	}
}

func BenchmarkJSONRequestParsingAndShaping(b *testing.B) {
	raw := []byte(`{
		"model":"glm-5.3-flash:high",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":"Explain quicksort."}
		],
		"max_tokens":1024
	}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body, ok := parseJSON(raw)
		if !ok {
			b.Fatal("parseJSON failed")
		}
		anth := models.OpenAIToAnthropic(body)
		shaped, err := transport.ShapePlanBody(anth, "session_0123456789abcdef0123456789abcdef", "dev-1", "user-1")
		if err != nil || shaped == nil {
			b.Fatal(err)
		}
	}
}

// --- Section 1: End-to-End Gateway Benchmarks (Mock Upstream) ---

func newMockUpstreamServer() *httptest.Server {
	nonStreamResp := []byte(`{"id":"msg_bench","type":"message","role":"assistant","model":"glm-5.3-flash","content":[{"type":"thinking","thinking":"step 1"},{"type":"text","text":"Hello from benchmark"}],"stop_reason":"end_turn","usage":{"input_tokens":16,"output_tokens":12}}`)
	toolResp := []byte(`{"id":"msg_tool","type":"message","role":"assistant","model":"glm-5.3-flash","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Jakarta"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":15}}`)
	streamPayload := []byte(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"model\":\"glm-5.3-flash\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think...\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"world!\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":8}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(streamPayload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if bytes.Contains(raw, []byte(`"tools"`)) {
			_, _ = w.Write(toolResp)
			return
		}
		_, _ = w.Write(nonStreamResp)
	}))
}

func runGatewayBenchmark(b *testing.B, path string, payload []byte) {
	cleanup := setupBenchEnv(b)
	defer cleanup()

	upstream := newMockUpstreamServer()
	defer upstream.Close()
	origInf := transport.InferenceOrigin
	transport.InferenceOrigin = upstream.URL
	defer func() { transport.InferenceOrigin = origInf }()

	handler := Route()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", path, bytes.NewReader(payload))
		req.Host = "127.0.0.1:20131"
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", "req_bench_fixed_01")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	}
}

func BenchmarkGatewayOpenAINonStreaming(b *testing.B) {
	runGatewayBenchmark(b, "/v1/chat/completions", []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"Hello"}]}`))
}

func BenchmarkGatewayAnthropicNonStreaming(b *testing.B) {
	runGatewayBenchmark(b, "/v1/messages", []byte(`{"model":"glm-5.3-flash","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`))
}

func BenchmarkGatewayOpenAIStreaming(b *testing.B) {
	runGatewayBenchmark(b, "/v1/chat/completions", []byte(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"Hello"}]}`))
}

func BenchmarkGatewayAnthropicStreaming(b *testing.B) {
	runGatewayBenchmark(b, "/v1/messages", []byte(`{"model":"glm-5.3-flash","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`))
}

func BenchmarkGatewayToolCalling(b *testing.B) {
	runGatewayBenchmark(b, "/v1/chat/completions", []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"Weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`))
}

func BenchmarkGatewayImagePayload(b *testing.B) {
	imgData := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAUA", 128)
	payload := fmt.Sprintf(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image_url","image_url":{"url":"%s"}}]}]}`, imgData)
	runGatewayBenchmark(b, "/v1/chat/completions", []byte(payload))
}

// --- Section 1, 3, 4, 6: Reproducible Load, Streaming & Lane Width Harness ---

type benchSummary struct {
	Name        string
	Reqs        int
	Errors      int
	RPS         float64
	AvgMs       float64
	P50Ms       float64
	P95Ms       float64
	P99Ms       float64
	QueueP50Ms  float64
	QueueP95Ms  float64
	ProcP50Ms   float64
	ProcP95Ms   float64
	HeapAllocKB int64
	Goroutines  int
}

func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return float64(sorted[idx].Microseconds()) / 1000.0
}

func statsFloat(vals []float64) (median, minVal, maxVal, cvPct float64) {
	if len(vals) == 0 {
		return 0, 0, 0, 0
	}
	cp := append([]float64(nil), vals...)
	sort.Float64s(cp)
	median = cp[len(cp)/2]
	minVal = cp[0]
	maxVal = cp[len(cp)-1]
	var sum float64
	for _, v := range cp {
		sum += v
	}
	mean := sum / float64(len(cp))
	if mean > 0 {
		var sqDiff float64
		for _, v := range cp {
			d := v - mean
			sqDiff += d * d
		}
		stddev := math.Sqrt(sqDiff / float64(len(cp)))
		cvPct = (stddev / mean) * 100.0
	}
	return median, minVal, maxVal, cvPct
}

func TestRunBaselineAndStressSuite(t *testing.T) {
	if os.Getenv("ZCODE_RUN_BENCH_REPORT") != "1" {
		t.Skip("skipping full load/stress benchmark report; set ZCODE_RUN_BENCH_REPORT=1 to execute")
	}
	cleanup := setupBenchEnv(t)
	defer cleanup()

	var activeUpstreamConns int32
	var maxUpstreamConns int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterNano := time.Now().UnixNano()
		cur := atomic.AddInt32(&activeUpstreamConns, 1)
		for {
			m := atomic.LoadInt32(&maxUpstreamConns)
			if cur <= m || atomic.CompareAndSwapInt32(&maxUpstreamConns, m, cur) {
				break
			}
		}
		defer atomic.AddInt32(&activeUpstreamConns, -1)

		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		w.Header().Set("X-Upstream-Enter-Nano", strconv.FormatInt(enterNano, 10))
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			chunks := 6
			chunkText := "token-chunk "
			if bytes.Contains(raw, []byte(`"large-chunks"`)) {
				chunkText = strings.Repeat("X", 4096)
			}
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"model\":\"glm-5.3-flash\"}}\n\n", enterNano)
			if flusher != nil {
				flusher.Flush()
			}
			for i := 0; i < chunks; i++ {
				if bytes.Contains(raw, []byte(`"paced-stream"`)) {
					time.Sleep(2 * time.Millisecond)
				}
				_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", chunkText)
				if flusher != nil {
					flusher.Flush()
				}
			}
			_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":18}}\n\n")
			_, _ = fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		if bytes.Contains(raw, []byte(`"paced-work"`)) {
			time.Sleep(5 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_%d","type":"message","role":"assistant","model":"glm-5.3-flash","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, enterNano)
	}))
	defer upstream.Close()

	origInf := transport.InferenceOrigin
	transport.InferenceOrigin = upstream.URL
	defer func() { transport.InferenceOrigin = origInf }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, ln) }()
	baseURL := "http://" + ln.Addr().String()
	defer func() {
		cancel()
		<-errCh
	}()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	// Warm-up phase (60 requests across 10 workers) so TCP pools and caches are warm.
	_ = os.Setenv("ZCODE_LANE_WIDTH", "64")
	var warmWG sync.WaitGroup
	for w := 0; w < 10; w++ {
		warmWG.Add(1)
		go func() {
			defer warmWG.Done()
			for i := 0; i < 6; i++ {
				resp, err := client.Post(baseURL+"/v1/chat/completions", "application/json",
					strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"warmup"}]}`))
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}
		}()
	}
	warmWG.Wait()
	t.Logf("BENCH ENV | GOMAXPROCS=%d | NumCPU=%d | Warmup=60 reqs complete", runtime.GOMAXPROCS(0), runtime.NumCPU())

	runOnce := func(name, path, body string, totalReqs, concurrency, laneWidth int) benchSummary {
		_ = os.Setenv("ZCODE_LANE_WIDTH", fmt.Sprint(laneWidth))
		runtime.GC()

		latencies := make([]time.Duration, totalReqs)
		queueWaits := make([]time.Duration, totalReqs)
		procTimes := make([]time.Duration, totalReqs)
		var errCount int32
		var idx int32

		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(atomic.AddInt32(&idx, 1)) - 1
					if i >= totalReqs {
						return
					}
					t0 := time.Now()
					req, _ := http.NewRequest("POST", baseURL+path, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Request-Id", "req_load_01")
					resp, err := client.Do(req)
					if err != nil {
						atomic.AddInt32(&errCount, 1)
						latencies[i] = time.Since(t0)
						continue
					}
					rawResp, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					tDone := time.Now()
					totalDur := tDone.Sub(t0)
					latencies[i] = totalDur

					// Extract upstream entry timestamp to split Queue Wait vs Active Processing time.
					enterNs := extractEnterNano(rawResp)
					if enterNs > 0 {
						enterTime := time.Unix(0, enterNs)
						qWait := enterTime.Sub(t0)
						if qWait < 0 {
							qWait = 0
						}
						pDur := tDone.Sub(enterTime)
						if pDur < 0 {
							pDur = 0
						}
						queueWaits[i] = qWait
						procTimes[i] = pDur
					} else {
						procTimes[i] = totalDur
					}

					if resp.StatusCode != http.StatusOK {
						atomic.AddInt32(&errCount, 1)
					}
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)

		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)

		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		sort.Slice(queueWaits, func(i, j int) bool { return queueWaits[i] < queueWaits[j] })
		sort.Slice(procTimes, func(i, j int) bool { return procTimes[i] < procTimes[j] })
		var sum time.Duration
		for _, d := range latencies {
			sum += d
		}
		avgMs := float64(sum.Microseconds()) / float64(len(latencies)) / 1000.0
		return benchSummary{
			Name:        name,
			Reqs:        totalReqs,
			Errors:      int(errCount),
			RPS:         float64(totalReqs) / elapsed.Seconds(),
			AvgMs:       avgMs,
			P50Ms:       percentile(latencies, 0.50),
			P95Ms:       percentile(latencies, 0.95),
			P99Ms:       percentile(latencies, 0.99),
			QueueP50Ms:  percentile(queueWaits, 0.50),
			QueueP95Ms:  percentile(queueWaits, 0.95),
			ProcP50Ms:   percentile(procTimes, 0.50),
			ProcP95Ms:   percentile(procTimes, 0.95),
			HeapAllocKB: int64(memAfter.HeapAlloc) / 1024,
			Goroutines:  runtime.NumGoroutine(),
		}
	}

	run5Stats := func(name, path, body string, totalReqs, concurrency, laneWidth int) {
		const runs = 5
		var rps, avg, p50, p95, p99, qp50, qp95, pp50, pp95 []float64
		var last benchSummary
		totalErrs := 0
		for r := 1; r <= runs; r++ {
			s := runOnce(name, path, body, totalReqs, concurrency, laneWidth)
			last = s
			totalErrs += s.Errors
			rps = append(rps, s.RPS)
			avg = append(avg, s.AvgMs)
			p50 = append(p50, s.P50Ms)
			p95 = append(p95, s.P95Ms)
			p99 = append(p99, s.P99Ms)
			qp50 = append(qp50, s.QueueP50Ms)
			qp95 = append(qp95, s.QueueP95Ms)
			pp50 = append(pp50, s.ProcP50Ms)
			pp95 = append(pp95, s.ProcP95Ms)
		}
		medRPS, minRPS, maxRPS, cvRPS := statsFloat(rps)
		medP50, _, _, _ := statsFloat(p50)
		medP95, _, _, _ := statsFloat(p95)
		medP99, _, _, _ := statsFloat(p99)
		medQP50, _, _, _ := statsFloat(qp50)
		medQP95, _, _, _ := statsFloat(qp95)
		medPP50, _, _, _ := statsFloat(pp50)
		medPP95, _, _, _ := statsFloat(pp95)
		t.Logf("RUNS(5) %-36s | medRPS=%7.1f [min=%.0f max=%.0f CV=%.1f%% runs:%.0f,%.0f,%.0f,%.0f,%.0f] | total p50=%5.2fms p95=%6.2fms p99=%6.2fms | queue p50=%5.2fms p95=%6.2fms | proc p50=%5.2fms p95=%5.2fms | heap=%4dKB gor=%d err=%d",
			name, medRPS, minRPS, maxRPS, cvRPS, rps[0], rps[1], rps[2], rps[3], rps[4],
			medP50, medP95, medP99,
			medQP50, medQP95,
			medPP50, medPP95,
			last.HeapAllocKB, last.Goroutines, totalErrs)
	}

	// 1. Core Scenarios (5x repeated, concurrency=10, laneWidth=64, 250 reqs/run)
	img64 := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUgAAAAUA", 128)
	run5Stats("OpenAI Non-Streaming", "/v1/chat/completions", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"Hello"}]}`, 250, 10, 64)
	run5Stats("Anthropic Non-Streaming", "/v1/messages", `{"model":"glm-5.3-flash","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`, 250, 10, 64)
	run5Stats("OpenAI Streaming (Small Chunks)", "/v1/chat/completions", `{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"Hello"}]}`, 250, 10, 64)
	run5Stats("OpenAI Streaming (4KB Chunks)", "/v1/chat/completions", `{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"large-chunks"}]}`, 250, 10, 64)
	run5Stats("Anthropic Streaming", "/v1/messages", `{"model":"glm-5.3-flash","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`, 250, 10, 64)
	run5Stats("Tool Calling (OpenAI)", "/v1/chat/completions", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"Weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`, 250, 10, 64)
	run5Stats("Image Payload (Base64)", "/v1/chat/completions", fmt.Sprintf(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image_url","image_url":{"url":"%s"}}]}]}`, img64), 250, 10, 64)

	// 2. Streaming TTFB & Inter-Chunk Latency Measurement
	_ = os.Setenv("ZCODE_LANE_WIDTH", "16")
	t0 := time.Now()
	sreq, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"paced-stream"}]}`))
	sreq.Header.Set("Content-Type", "application/json")
	sresp, err := client.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	ttfb := time.Since(t0)
	scanner := bufio.NewScanner(sresp.Body)
	lastChunkAt := time.Now()
	var interChunk []time.Duration
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			now := time.Now()
			interChunk = append(interChunk, now.Sub(lastChunkAt))
			lastChunkAt = now
		}
	}
	sresp.Body.Close()
	sort.Slice(interChunk, func(i, j int) bool { return interChunk[i] < interChunk[j] })
	t.Logf("STREAM TIMING | TTFB=%v | chunks=%d | p50_inter_chunk=%.2fms | p95_inter_chunk=%.2fms | total=%v",
		ttfb, len(interChunk), percentile(interChunk, 0.50), percentile(interChunk, 0.95), time.Since(t0))

	// 3. Lane Width Comparison (5x repeated: ZCODE_LANE_WIDTH = 1, 2, 4, 8 with 16 concurrent callers, 5ms upstream work)
	for _, lw := range []int{1, 2, 4, 8} {
		run5Stats(fmt.Sprintf("LaneWidth=%d (16 conc, 5ms upstream)", lw), "/v1/chat/completions", `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"paced-work"}]}`, 128, 16, lw)
	}

	// 4. Concurrency Scaling / Stress Test (5x repeated: 5, 10, 25, 50, 100 concurrent callers, laneWidth=128)
	for _, conc := range []int{5, 10, 25, 50, 100} {
		run5Stats(fmt.Sprintf("Stress Concurrency=%d (Stream)", conc), "/v1/chat/completions", `{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"Hello"}]}`, 300, conc, 128)
	}

	// 5. Resource-Constrained Go Runtime Simulation (GOMAXPROCS=1/512MB, GOMAXPROCS=1/1GB, GOMAXPROCS=2/2GB)
	origProcs := runtime.GOMAXPROCS(0)
	origMemLimit := debug.SetMemoryLimit(-1)
	defer func() {
		runtime.GOMAXPROCS(origProcs)
		debug.SetMemoryLimit(origMemLimit)
	}()
	for _, prof := range []struct {
		label string
		procs int
		memMB int64
	}{
		{"Sim-1vCPU-512MB", 1, 512},
		{"Sim-1vCPU-1GB", 1, 1024},
		{"Sim-2vCPU-2GB", 2, 2048},
	} {
		runtime.GOMAXPROCS(prof.procs)
		debug.SetMemoryLimit(prof.memMB << 20)
		runtime.GC()
		var idleMem runtime.MemStats
		runtime.ReadMemStats(&idleMem)
		run5Stats(fmt.Sprintf("%s (idleHeap=%dKB)", prof.label, idleMem.HeapAlloc/1024), "/v1/chat/completions", `{"model":"glm-5.3-flash","stream":true,"messages":[{"role":"user","content":"Hello"}]}`, 200, 10, 16)
	}

	t.Logf("MAX UPSTREAM CONNS OBSERVED=%d | FINAL GOROUTINES=%d", atomic.LoadInt32(&maxUpstreamConns), runtime.NumGoroutine())
}

func extractEnterNano(raw []byte) int64 {
	for _, marker := range [][]byte{[]byte(`"id":"msg_`), []byte(`"enter_ns":`)} {
		idx := bytes.Index(raw, marker)
		if idx < 0 {
			continue
		}
		rest := raw[idx+len(marker):]
		var n int64
		digits := 0
		for i := 0; i < len(rest); i++ {
			if rest[i] >= '0' && rest[i] <= '9' {
				n = n*10 + int64(rest[i]-'0')
				digits++
			} else {
				break
			}
		}
		if digits >= 10 {
			return n
		}
	}
	return 0
}
