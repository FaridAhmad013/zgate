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

	var mu sync.Mutex
	inFlightByToken := map[string]int{}
	maxInFlightByToken := map[string]int{}
	attemptsByToken := map[string]int{}
	enforce3008OnConcurrentSameAccount := false

	setupTransportTest(t, func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		attemptsByToken[tok]++
		inFlightByToken[tok]++
		cur := inFlightByToken[tok]
		if cur > maxInFlightByToken[tok] {
			maxInFlightByToken[tok] = cur
		}
		reject := enforce3008OnConcurrentSameAccount && cur > 1
		mu.Unlock()

		defer func() {
			mu.Lock()
			inFlightByToken[tok]--
			mu.Unlock()
		}()

		if reject {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":3008,"msg":"account concurrency limit exceeded"}`))
			return
		}
		time.Sleep(40 * time.Millisecond)
		writeOK(w)
	})

	_, _ = accounts.UpsertAccount("a1", "Account-1", jwt1, "", "", 0)
	_, _ = accounts.UpsertAccount("a2", "Account-2", jwt2, "", "", 0)
	accounts.SetDefault("a1")

	// Case 1: With ZCODE_LANE_WIDTH=2 and no upstream 3008 rejection, Usable() is
	// failover-ordered (default account first, NOT round-robin), so both concurrent
	// lane slots select the default account (a1) simultaneously!
	t.Setenv("ZCODE_LANE_WIDTH", "2")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SendAnthropic(SendOpts{
				Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
			})
			if err != nil {
				t.Errorf("Case 1 SendAnthropic failed: %v", err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()

	mu.Lock()
	maxA1 := maxInFlightByToken["Bearer "+jwt1]
	if maxA1 == 0 {
		maxA1 = maxInFlightByToken[jwt1]
	}
	attemptsA2 := attemptsByToken[jwt2]
	inFlightByToken = map[string]int{}
	maxInFlightByToken = map[string]int{}
	attemptsByToken = map[string]int{}
	enforce3008OnConcurrentSameAccount = true
	mu.Unlock()

	if maxA1 < 2 || attemptsA2 != 0 {
		t.Fatalf("expected both concurrent lane slots to hit default account a1 first (maxA1=%d, attemptsA2=%d)", maxA1, attemptsA2)
	}

	// Case 2: When upstream enforces 1 concurrent request per account (429 code 3008)
	// and ZCODE_LANE_WIDTH=2 with 2 accounts, the 2nd concurrent request receives 3008
	// on a1, places a1 in short concurrency cooldown, and immediately rotates to a2!
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SendAnthropic(SendOpts{
				Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "rotate"}}},
			})
			if err != nil {
				t.Errorf("Case 2 SendAnthropic failed: %v", err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()

	mu.Lock()
	usedA1 := attemptsByToken[jwt1]
	usedA2 := attemptsByToken[jwt2]
	mu.Unlock()
	if usedA1 == 0 || usedA2 == 0 {
		t.Fatalf("expected 3008 on a1 to rotate the second concurrent request to a2 (usedA1=%d, usedA2=%d)", usedA1, usedA2)
	}

	// Case 3: Why ZCODE_LANE_WIDTH=8 is unsafe with only 2 accounts when upstream
	// enforces 1 active stream per account and ZCODE_MAX_ATTEMPTS=1:
	// 8 concurrent requests enter the lane at once, exhausting both a1 and a2!
	accounts.ClearCooldown("a1")
	accounts.ClearCooldown("a2")
	t.Setenv("ZCODE_LANE_WIDTH", "8")
	t.Setenv("ZCODE_MAX_ATTEMPTS", "1")
	var successes, failures int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SendAnthropic(SendOpts{
				Body: map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "user", "content": "over-lane"}}},
			})
			if err != nil {
				atomic.AddInt32(&failures, 1)
				return
			}
			res.Body.Close()
			atomic.AddInt32(&successes, 1)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&failures) == 0 {
		t.Fatalf("expected concurrent 3008 failures when ZCODE_LANE_WIDTH=8 exceeds 2 accounts' upstream capacity (successes=%d, failures=%d)", successes, failures)
	}
}

func jsonRead(r io.Reader, out any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
