package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterHardCapEviction(t *testing.T) {
	resetRateLimiter()
	t.Setenv("ZCODE_RATE_LIMIT_RPS", "100")
	t.Setenv("ZCODE_RATE_LIMIT_BURST", "100")

	// Inject 8200 unique IP keys directly to test hard ceiling and eviction
	now := time.Now()
	rateMu.Lock()
	for i := 0; i < 8200; i++ {
		ipKey := fmt.Sprintf("ip:10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		rateBuckets[ipKey] = &rateBucket{
			tokens:   100,
			lastSeen: now.Add(time.Duration(i) * time.Millisecond), // ascending timestamps
		}
	}
	initialCount := len(rateBuckets)
	rateMu.Unlock()

	if initialCount != 8200 {
		t.Fatalf("expected 8200 rate buckets, got %d", initialCount)
	}

	// Trigger allowRequest with a new key; must trigger hard ceiling eviction
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20131/health", nil)
	req.RemoteAddr = "192.168.100.1:12345"

	allowed, _ := allowRequest(req)
	if !allowed {
		t.Fatal("expected request to be allowed")
	}

	rateMu.Lock()
	finalCount := len(rateBuckets)
	rateMu.Unlock()

	// Should have evicted 1/4 of map (~2050 entries), so final count < 6200
	if finalCount >= 8192 {
		t.Fatalf("expected hard cap eviction to reduce bucket count below 8192, got %d", finalCount)
	}
	t.Logf("Rate limiter hard cap test passed: count reduced from %d to %d", initialCount, finalCount)
}
