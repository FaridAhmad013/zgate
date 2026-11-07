package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apissaj/zcode-gateway/internal/accounts"
)

// slowChunkReader feeds data in controlled byte slices to test SSE chunk splitting.
type slowChunkReader struct {
	chunks [][]byte
	idx    int
}

func (r *slowChunkReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.idx]
	n := copy(p, c)
	if n < len(c) {
		r.chunks[r.idx] = c[n:]
	} else {
		r.idx++
	}
	return n, nil
}

func TestSSEReaderLF_CRLF_Multiline_ChunkBoundaries_AndLimit(t *testing.T) {
	rawStream := "" +
		": keepalive comment\r\n" +
		"\r\n" +
		"event: message_start\r\n" +
		"data: {\"type\":\"message_start\",\r\n" +
		"data: \"id\":\"msg_1\"}\r\n" +
		"\r\n" +
		"event: content_block_delta\n" +
		"data:{\"type\":\"content_block_delta\",\"index\":0}\n\n" +
		"data: [DONE]" // no trailing newline before EOF

	// Split across 1-byte and 3-byte chunks so \r\n is split across Read calls.
	var chunks [][]byte
	rawBytes := []byte(rawStream)
	for i := 0; i < len(rawBytes); {
		step := 1
		if i%2 == 1 && i+3 <= len(rawBytes) {
			step = 3
		}
		chunks = append(chunks, rawBytes[i:i+step])
		i += step
	}

	sse := NewSSEReader(&slowChunkReader{chunks: chunks}, 1024)
	var events []SSEEvent
	for {
		ev, err := sse.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected SSEReader error: %v", err)
		}
		events = append(events, ev)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Event != "message_start" || events[0].Data != "{\"type\":\"message_start\",\n\"id\":\"msg_1\"}" {
		t.Fatalf("unexpected event[0]: %+v", events[0])
	}
	if events[1].Event != "content_block_delta" || events[1].Data != "{\"type\":\"content_block_delta\",\"index\":0}" {
		t.Fatalf("unexpected event[1]: %+v", events[1])
	}
	if events[2].Event != "" || events[2].Data != "[DONE]" {
		t.Fatalf("unexpected event[2]: %+v", events[2])
	}

	// Verify bounded buffer protection against unbounded malformed SSE streams.
	huge := "data: " + strings.Repeat("A", 512) + "\n"
	limited := NewSSEReader(strings.NewReader(huge), 128)
	if _, err := limited.Next(); !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected ErrSSEEventTooLarge, got: %v", err)
	}
}

func TestStreamingActiveVsIdleTimeoutAndHeaderTimeout(t *testing.T) {
	jwt := testJWT("u_stream")
	var upstreamCanceled atomic.Bool

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = jsonRead(r.Body, &body)
		msgs, _ := body["messages"].([]any)
		mode := ""
		if len(msgs) > 0 {
			if m, ok := msgs[0].(map[string]any); ok {
				mode, _ = m["content"].(string)
			}
		}

		switch mode {
		case "header-timeout":
			select {
			case <-time.After(300 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			writeOK(w)
		case "active-long-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			// Total duration = 6 * 35ms = 210ms, which exceeds the 90ms idle/header
			// timeout more than 2x while staying active on every read!
			for i := 0; i < 6; i++ {
				fmt.Fprintf(w, "data: {\"chunk\":%d}\n\n", i)
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(35 * time.Millisecond)
			}
		case "idle-stall":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"chunk\":0}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			// Stall longer than IdleTimeout.
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				upstreamCanceled.Store(true)
			}
		case "client-disconnect":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"chunk\":0}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			<-r.Context().Done()
			upstreamCanceled.Store(true)
		}
	})

	_, _ = accounts.UpsertAccount("s", "Stream", jwt, "", "", 0)
	t.Setenv("ZCODE_MAX_ATTEMPTS", "1")

	// 1. Header timeout triggers 504 when upstream does not send headers in time.
	_, err := SendAnthropic(SendOpts{
		Body:            map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "header-timeout"}}},
		UpstreamTimeout: 70 * time.Millisecond,
		IdleTimeout:     70 * time.Millisecond,
	})
	if ue, ok := err.(*UpstreamError); !ok || ue.Status != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 UpstreamError on header timeout, got: %#v (%v)", err, err)
	}

	// 2. Active stream running >2x longer than UpstreamTimeout/IdleTimeout completes cleanly.
	res, err := SendAnthropic(SendOpts{
		Body:            map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "active-long-stream"}}},
		UpstreamTimeout: 90 * time.Millisecond,
		IdleTimeout:     90 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("active stream failed to start: %v", err)
	}
	sse := NewSSEReader(res.Body, 0)
	count := 0
	for {
		_, err := sse.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Body.Close()
			t.Fatalf("active long stream was cut prematurely at chunk %d: %v", count, err)
		}
		count++
	}
	res.Body.Close()
	if count != 6 {
		t.Fatalf("expected all 6 chunks from active long stream, got %d", count)
	}

	// 3. Stalled stream terminates with ErrStreamIdleTimeout and cancels upstream context.
	upstreamCanceled.Store(false)
	res, err = SendAnthropic(SendOpts{
		Body:            map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "idle-stall"}}},
		UpstreamTimeout: 80 * time.Millisecond,
		IdleTimeout:     80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("idle-stall stream failed to start: %v", err)
	}
	_, readErr := io.ReadAll(res.Body)
	res.Body.Close()
	if !errors.Is(readErr, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout on stalled stream, got: %v", readErr)
	}
	time.Sleep(20 * time.Millisecond)
	if !upstreamCanceled.Load() {
		t.Fatal("expected upstream request context to be canceled on stream idle timeout")
	}

	// 4. Client disconnect / context cancellation aborts stream and upstream immediately.
	upstreamCanceled.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	res, err = SendAnthropic(SendOpts{
		Body:    map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "client-disconnect"}}},
		Context: ctx,
	})
	if err != nil {
		t.Fatalf("client-disconnect stream failed to start: %v", err)
	}
	buf := make([]byte, 64)
	_, _ = res.Body.Read(buf)
	cancel()
	res.Body.Close()
	time.Sleep(20 * time.Millisecond)
	if !upstreamCanceled.Load() {
		t.Fatal("expected upstream context to cancel on client disconnect")
	}
}

func TestLaneConcurrencyEnforcementAndLifecycle(t *testing.T) {
	jwt := testJWT("u_lane")
	var currentInflight, maxSeenInflight int32

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&currentInflight, 1)
		for {
			m := atomic.LoadInt32(&maxSeenInflight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxSeenInflight, m, cur) {
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"ok\":true}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(45 * time.Millisecond)
		atomic.AddInt32(&currentInflight, -1)
	})
	_, _ = accounts.UpsertAccount("lane", "Lane", jwt, "", "", 0)
	t.Setenv("ZCODE_ACCOUNT_CONCURRENCY", "4")

	runParallel := func(width, n int) int32 {
		t.Setenv("ZCODE_LANE_WIDTH", fmt.Sprint(width))
		atomic.StoreInt32(&maxSeenInflight, 0)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := SendAnthropic(SendOpts{
					Body: map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
				})
				if err != nil {
					t.Errorf("SendAnthropic failed: %v", err)
					return
				}
				_, _ = io.ReadAll(res.Body)
				res.Body.Close()
			}()
		}
		wg.Wait()
		return atomic.LoadInt32(&maxSeenInflight)
	}

	// 1. Width = 1 across 5 parallel streaming requests -> maxInflight must be 1.
	if got := runParallel(1, 5); got != 1 {
		t.Fatalf("with ZCODE_LANE_WIDTH=1 across 5 parallel requests, maxInflight = %d, want 1", got)
	}

	// 2. Width = 3 across 5 parallel streaming requests -> maxInflight must be <= 3 and > 1.
	if got := runParallel(3, 5); got < 2 || got > 3 {
		t.Fatalf("with ZCODE_LANE_WIDTH=3 across 5 parallel requests, maxInflight = %d, want 2..3", got)
	}

	// 3. Context cancellation while waiting for lane slot aborts immediately without leaking slot.
	t.Setenv("ZCODE_LANE_WIDTH", "1")
	holder, err := SendAnthropic(SendOpts{
		Body: map[string]any{"model": "glm-5.3-flash", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hold"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, waitErr := SendAnthropic(SendOpts{
		Body:    map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "wait"}}},
		Context: waitCtx,
	})
	if waitErr == nil {
		t.Fatal("expected waiting caller to fail when context expires before lane is free")
	}
	// Release holder and verify next request succeeds immediately (no deadlock).
	holder.Body.Close()
	nextRes, err := SendAnthropic(SendOpts{
		Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "after"}}},
	})
	if err != nil {
		t.Fatalf("request after lane release failed: %v", err)
	}
	nextRes.Body.Close()

	// Verify no goroutines leaked.
	runtime.GC()
}

func TestLaneAndAccountConcurrencyBehavior(t *testing.T) {
	jwt1 := testJWT("u_acct1")
	jwt2 := testJWT("u_acct2")
	jwt3 := testJWT("u_acct3")

	var mu sync.Mutex
	inFlightByToken := map[string]int{}
	maxInFlightByToken := map[string]int{}
	attemptsByToken := map[string]int{}
	var totalInFlight, maxTotalInFlight int32

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		curTotal := atomic.AddInt32(&totalInFlight, 1)
		for {
			m := atomic.LoadInt32(&maxTotalInFlight)
			if curTotal <= m || atomic.CompareAndSwapInt32(&maxTotalInFlight, m, curTotal) {
				break
			}
		}
		mu.Lock()
		attemptsByToken[tok]++
		inFlightByToken[tok]++
		curAcct := inFlightByToken[tok]
		if curAcct > maxInFlightByToken[tok] {
			maxInFlightByToken[tok] = curAcct
		}
		mu.Unlock()

		defer func() {
			mu.Lock()
			inFlightByToken[tok]--
			mu.Unlock()
			atomic.AddInt32(&totalInFlight, -1)
		}()

		// If per-account concurrency > 1 ever reaches upstream, fail with 3008!
		if curAcct > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":3008,"msg":"account concurrency limit exceeded"}`))
			return
		}
		time.Sleep(35 * time.Millisecond)
		writeOK(w)
	})

	_, _ = accounts.UpsertAccount("a1", "Account-1", jwt1, "", "", 0)
	_, _ = accounts.UpsertAccount("a2", "Account-2", jwt2, "", "", 0)
	accounts.SetDefault("a1")
	t.Setenv("ZCODE_ACCOUNT_CONCURRENCY", "1")
	t.Setenv("ZCODE_LANE_WIDTH", "8")

	// 1. Fair distribution across 2 healthy accounts (6 sequential requests -> 3 on a1, 3 on a2).
	for i := 0; i < 6; i++ {
		res, err := SendAnthropic(SendOpts{
			Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "seq"}}},
		})
		if err != nil {
			t.Fatalf("seq request %d failed: %v", i, err)
		}
		res.Body.Close()
	}
	mu.Lock()
	if attemptsByToken[jwt1] != 3 || attemptsByToken[jwt2] != 3 {
		t.Fatalf("expected fair 3/3 round-robin distribution across a1 and a2, got a1=%d a2=%d", attemptsByToken[jwt1], attemptsByToken[jwt2])
	}
	attemptsByToken = map[string]int{}
	maxInFlightByToken = map[string]int{}
	mu.Unlock()

	// 2. Per-account concurrency semaphore enforcement: 8 parallel requests with
	// ZCODE_LANE_WIDTH=8 and only 2 accounts (ZCODE_ACCOUNT_CONCURRENCY=1).
	// All 8 requests must succeed (0 errors), maxInFlight per account must be <= 1,
	// and maxTotalInFlight must be 2 (1 per account).
	atomic.StoreInt32(&maxTotalInFlight, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SendAnthropic(SendOpts{
				Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "parallel"}}},
			})
			if err != nil {
				t.Errorf("parallel request with per-account semaphore failed: %v", err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()

	mu.Lock()
	max1, max2 := maxInFlightByToken[jwt1], maxInFlightByToken[jwt2]
	cnt1, cnt2 := attemptsByToken[jwt1], attemptsByToken[jwt2]
	attemptsByToken = map[string]int{}
	maxInFlightByToken = map[string]int{}
	mu.Unlock()
	if max1 != 1 || max2 != 1 {
		t.Fatalf("per-account concurrency limit violated: maxInFlight a1=%d a2=%d, want 1 and 1", max1, max2)
	}
	if cnt1+cnt2 != 8 || cnt1 < 3 || cnt2 < 3 {
		t.Fatalf("expected balanced parallel utilization across a1 and a2, got a1=%d a2=%d", cnt1, cnt2)
	}

	// 3. Cooldown account receives 0 requests while healthy account serves all.
	accounts.MarkCooldown("a1", 5*time.Second)
	for i := 0; i < 4; i++ {
		res, err := SendAnthropic(SendOpts{
			Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "cooldown"}}},
		})
		if err != nil {
			t.Fatalf("cooldown test request %d failed: %v", i, err)
		}
		res.Body.Close()
	}
	mu.Lock()
	c1, c2 := attemptsByToken[jwt1], attemptsByToken[jwt2]
	attemptsByToken = map[string]int{}
	mu.Unlock()
	if c1 != 0 || c2 != 4 {
		t.Fatalf("cooled-down account a1 must receive 0 requests, got a1=%d a2=%d", c1, c2)
	}
	accounts.ClearCooldown("a1")

	// 4. Global lane still caps total concurrency when accounts > lane width (3 accounts, ZCODE_LANE_WIDTH=2).
	_, _ = accounts.UpsertAccount("a3", "Account-3", jwt3, "", "", 0)
	t.Setenv("ZCODE_LANE_WIDTH", "2")
	atomic.StoreInt32(&maxTotalInFlight, 0)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SendAnthropic(SendOpts{
				Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "cap2"}}},
			})
			if err != nil {
				t.Errorf("cap2 request failed: %v", err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxTotalInFlight); got != 2 {
		t.Fatalf("with 3 accounts and ZCODE_LANE_WIDTH=2, maxTotalInFlight = %d, want 2", got)
	}

	// 5. Client disconnect while waiting for per-account semaphore & while streaming
	// releases all semaphores cleanly without deadlock.
	t.Setenv("ZCODE_LANE_WIDTH", "4")
	_ = accounts.Remove("a2")
	_ = accounts.Remove("a3")
	holder, err := SendAnthropic(SendOpts{
		Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hold-acct"}}},
	})
	if err != nil {
		t.Fatalf("holder failed: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, waitErr := SendAnthropic(SendOpts{
		Body:    map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "wait-acct"}}},
		Context: waitCtx,
	})
	if waitErr == nil {
		t.Fatal("expected caller waiting on per-account slot to abort on context cancellation")
	}
	holder.Body.Close()

	// Next request on a1 succeeds immediately (proving both account slot and lane slot are free).
	afterRes, err := SendAnthropic(SendOpts{
		Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "after-disconnect"}}},
	})
	if err != nil {
		t.Fatalf("request after disconnect failed (semaphore leak): %v", err)
	}
	afterRes.Body.Close()
}

func jsonRead(r io.Reader, out any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
