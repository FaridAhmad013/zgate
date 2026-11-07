package accounts

import (
	"context"
	"testing"
	"time"
)

func TestSlotWaitersCleanupOnContextCancel(t *testing.T) {
	ResetForTest()
	t.Setenv("ZCODE_ACCOUNT_CONCURRENCY", "1")
	_, _ = UpsertAccount("a1", "Account-1", "jwt1", "", "", 0)

	// Acquire slot 1 on Account 1
	ctx := context.Background()
	_, _, release1, err := AcquireCandidate(ctx, "a1", nil)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release1()

	// Launch a request that will wait on Account 1 slot, but cancel its context
	cancelCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, _, _, err2 := AcquireCandidate(cancelCtx, "a1", nil)
	if err2 == nil {
		t.Fatal("expected second acquire to fail on context deadline")
	}

	// Verify that waitCh was cleaned up from slotWaiters slice
	mu.Lock()
	waiterCount := len(slotWaiters)
	mu.Unlock()

	if waiterCount != 0 {
		t.Fatalf("expected 0 slotWaiters after canceled context, got %d", waiterCount)
	}
}
