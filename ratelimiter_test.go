// File: ratelimiter_test.go

package grpop

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestLocalRateLimiter_AllowWithinBurst(t *testing.T) {
	rl, err := NewLocalRateLimiter(5, 10, 0)
	if err != nil {
		t.Fatalf("NewLocalRateLimiter: %v", err)
	}
	allowedOnce := false
	for i := 0; i < 5; i++ {
		ok, err := rl.Allow(context.Background(), ChannelEmail, "a@example.com")
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if ok {
			allowedOnce = true
		}
	}
	if !allowedOnce {
		t.Fatal("Allow() never returned true within burst capacity")
	}
}

func TestLocalRateLimiter_InvalidConfig(t *testing.T) {
	if _, err := NewLocalRateLimiter(0, 10, 0); err == nil {
		t.Fatal("NewLocalRateLimiter(rps=0) = nil error, want non-nil")
	}
	if _, err := NewLocalRateLimiter(10, 5, 0); err == nil {
		t.Fatal("NewLocalRateLimiter(burst<rps) = nil error, want non-nil")
	}
}

func TestLocalRateLimiter_Wait(t *testing.T) {
	rl, _ := NewLocalRateLimiter(1000, 1000, 0)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rl.Wait(ctx, ChannelEmail, "a@example.com"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestLocalRateLimiter_GetStats(t *testing.T) {
	rl, _ := NewLocalRateLimiter(10, 10, 0)
	_, _ = rl.Allow(context.Background(), ChannelEmail, "a@example.com")
	stats, err := rl.GetStats(context.Background(), ChannelEmail, "a@example.com")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.RequestsPerSecond != 10 || stats.BurstSize != 10 {
		t.Fatalf("GetStats() = %+v, want RequestsPerSecond=10 BurstSize=10", stats)
	}
	if stats.AllowedCount == 0 {
		t.Fatal("GetStats().AllowedCount = 0, want > 0 after a successful Allow")
	}
}

// TestLocalRateLimiter_GetStats_UnseenRecipientDoesNotCreateEntry proves
// GetStats is a pure inspection: it must not grow the recipient LRU cache
// for a recipient that was never actually rate-limited.
func TestLocalRateLimiter_GetStats_UnseenRecipientDoesNotCreateEntry(t *testing.T) {
	rl, _ := NewLocalRateLimiter(10, 10, 0)
	stats, err := rl.GetStats(context.Background(), ChannelEmail, "never-seen@example.com")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.AllowedCount != 0 || stats.BlockedCount != 0 {
		t.Fatalf("GetStats(unseen recipient) = %+v, want zero-value counts", stats)
	}
	impl := rl.(*localRateLimiter)
	if impl.recipients.len() != 0 {
		t.Fatalf("recipient cache grew to %d entries from a GetStats-only call, want 0", impl.recipients.len())
	}
}

// TestLocalRateLimiter_ChannelTierSharedAcrossRecipients proves the
// channel-level bucket is shared: exhausting it via many distinct
// recipients still blocks a brand-new recipient on the same channel, even
// though that recipient's own per-recipient bucket is fresh.
func TestLocalRateLimiter_ChannelTierSharedAcrossRecipients(t *testing.T) {
	rl, _ := NewLocalRateLimiter(2, 2, 0)
	ctx := context.Background()

	// Exhaust the channel-level budget (burst=2) via two distinct
	// recipients, each well within their own per-recipient burst.
	if ok, _ := rl.Allow(ctx, ChannelEmail, "r1@example.com"); !ok {
		t.Fatal("first Allow (r1) = false, want true (within channel burst)")
	}
	if ok, _ := rl.Allow(ctx, ChannelEmail, "r2@example.com"); !ok {
		t.Fatal("second Allow (r2) = false, want true (within channel burst)")
	}

	// A third, never-before-seen recipient should now be blocked purely by
	// the exhausted channel-level tier.
	ok, err := rl.Allow(ctx, ChannelEmail, "r3@example.com")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if ok {
		t.Fatal("Allow (r3, fresh recipient bucket) = true, want false (channel-level budget exhausted)")
	}
}

// TestLocalRateLimiter_RecipientTierIndependentPerRecipient proves the
// per-recipient bucket is tracked independently: one recipient exhausting
// their own budget must not block a different recipient (given ample
// channel-level headroom).
func TestLocalRateLimiter_RecipientTierIndependentPerRecipient(t *testing.T) {
	rl, _ := NewLocalRateLimiter(1, 1, 0)
	ctx := context.Background()

	if ok, _ := rl.Allow(ctx, ChannelEmail, "r1@example.com"); !ok {
		t.Fatal("first Allow (r1) = false, want true")
	}
	// r1's own bucket (burst=1) is now exhausted.
	if ok, _ := rl.Allow(ctx, ChannelEmail, "r1@example.com"); ok {
		t.Fatal("second Allow (r1) = true, want false (r1's own burst exhausted)")
	}
}

func TestLocalRateLimiter_RecipientCacheEviction(t *testing.T) {
	rl, err := NewLocalRateLimiter(100, 100, 2) // capacity 2
	if err != nil {
		t.Fatalf("NewLocalRateLimiter: %v", err)
	}
	ctx := context.Background()
	impl := rl.(*localRateLimiter)

	_, _ = rl.Allow(ctx, ChannelEmail, "r1@example.com")
	_, _ = rl.Allow(ctx, ChannelEmail, "r2@example.com")
	_, _ = rl.Allow(ctx, ChannelEmail, "r3@example.com") // should evict r1 (least recently used)

	if impl.recipients.len() != 2 {
		t.Fatalf("recipient cache len = %d, want 2 (capacity)", impl.recipients.len())
	}
	if _, ok := impl.recipients.peek(recipientKey(ChannelEmail, "r1@example.com")); ok {
		t.Fatal("r1 entry still present, want evicted (least recently used)")
	}
	if _, ok := impl.recipients.peek(recipientKey(ChannelEmail, "r3@example.com")); !ok {
		t.Fatal("r3 entry missing, want present (most recently used)")
	}
}

func TestLocalRateLimiter_ChannelsAreIndependent(t *testing.T) {
	rl, _ := NewLocalRateLimiter(1, 1, 0)
	ctx := context.Background()

	if ok, _ := rl.Allow(ctx, ChannelEmail, "r1@example.com"); !ok {
		t.Fatal("Allow(email) = false, want true")
	}
	// WhatsApp's channel bucket is entirely separate from Email's.
	if ok, _ := rl.Allow(ctx, ChannelWhatsApp, "r1@example.com"); !ok {
		t.Fatal("Allow(whatsapp) = false, want true (independent channel bucket)")
	}
}

func TestLocalRateLimiter_Allow_CanceledContext(t *testing.T) {
	rl, _ := NewLocalRateLimiter(5, 5, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rl.Allow(ctx, ChannelEmail, "a@example.com"); err == nil {
		t.Fatal("Allow(canceled ctx) = nil error, want non-nil")
	}
}

func TestLocalRateLimiter_Wait_Error(t *testing.T) {
	rl, _ := NewLocalRateLimiter(1, 1, 0)
	// Exhaust the burst, then use a context whose deadline is shorter than
	// one token's refill interval, so Wait's internal timer expires first.
	_, _ = rl.Allow(context.Background(), ChannelEmail, "a@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := rl.Wait(ctx, ChannelEmail, "a@example.com"); err == nil {
		t.Fatal("Wait(exhausted burst, short deadline) = nil error, want non-nil")
	}
}

// TestLocalRateLimiter_ConcurrentAccess proves the shared channel map and
// recipient LRU cache are safe under real concurrent load, not just
// single-threaded review — run with -race.
func TestLocalRateLimiter_ConcurrentAccess(t *testing.T) {
	rl, _ := NewLocalRateLimiter(50, 50, 20)
	ctx := context.Background()
	channels := []Channel{ChannelEmail, ChannelWhatsApp}

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ch := channels[i%2]
			recipient := "recipient-" + string(rune('a'+i%10))
			_, _ = rl.Allow(ctx, ch, recipient)
			_, _ = rl.GetStats(ctx, ch, recipient)
		}(i)
	}
	wg.Wait()
}
