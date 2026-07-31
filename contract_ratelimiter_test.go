// File: contract_ratelimiter_test.go

package grpop

import (
	"context"
	"testing"
	"time"
)

// testRateLimiterContract is the shared behavioral contract every
// RateLimiter backend must satisfy identically. newLimiter must construct a
// limiter configured for requestsPerSecond/burstSize on both the
// per-channel and per-(channel,recipient) tiers, matching
// NewLocalRateLimiter's own parameter meaning.
func testRateLimiterContract(t *testing.T, newLimiter func(t *testing.T, requestsPerSecond, burstSize int) RateLimiter) {
	t.Helper()
	ctx := context.Background()

	t.Run("AllowWithinBurst", func(t *testing.T) {
		rl := newLimiter(t, 5, 10)
		allowedOnce := false
		for i := 0; i < 5; i++ {
			ok, err := rl.Allow(ctx, ChannelEmail, "contract-recipient-1")
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
	})

	t.Run("BlocksOnceBurstExhausted", func(t *testing.T) {
		rl := newLimiter(t, 1, 1)
		ok, err := rl.Allow(ctx, ChannelEmail, "contract-recipient-2")
		if err != nil || !ok {
			t.Fatalf("first Allow() = (%v, %v), want (true, nil)", ok, err)
		}
		ok, err = rl.Allow(ctx, ChannelEmail, "contract-recipient-2")
		if err != nil {
			t.Fatalf("second Allow: %v", err)
		}
		if ok {
			t.Fatal("second Allow() = true immediately after exhausting burst=1, want false")
		}
	})

	t.Run("Wait_Succeeds", func(t *testing.T) {
		rl := newLimiter(t, 1000, 1000)
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := rl.Wait(waitCtx, ChannelEmail, "contract-recipient-3"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("GetStats_ReflectsAllowedCount", func(t *testing.T) {
		rl := newLimiter(t, 10, 10)
		_, _ = rl.Allow(ctx, ChannelEmail, "contract-recipient-4")
		stats, err := rl.GetStats(ctx, ChannelEmail, "contract-recipient-4")
		if err != nil {
			t.Fatalf("GetStats: %v", err)
		}
		if stats.AllowedCount == 0 {
			t.Fatal("GetStats().AllowedCount = 0, want > 0 after a successful Allow")
		}
	})

	t.Run("DistinctRecipientsAreIndependent", func(t *testing.T) {
		// burst=2, not 1: a two-tier limiter's channel bucket is shared
		// across all recipients on that channel (see RateLimiter's own doc
		// comment), so with burst=1 a second, distinct recipient would
		// correctly be blocked by channel-level exhaustion regardless of
		// per-recipient independence — burst=2 gives the channel tier
		// enough headroom for two distinct recipients' first request each,
		// isolating what this subtest actually means to prove.
		rl := newLimiter(t, 2, 2)
		if ok, _ := rl.Allow(ctx, ChannelEmail, "contract-recipient-5a"); !ok {
			t.Fatal("Allow(recipient-5a) = false, want true")
		}
		if ok, _ := rl.Allow(ctx, ChannelEmail, "contract-recipient-5b"); !ok {
			t.Fatal("Allow(recipient-5b) = false, want true (independent recipient bucket)")
		}
	})

	t.Run("DistinctChannelsAreIndependent", func(t *testing.T) {
		rl := newLimiter(t, 1, 1)
		if ok, _ := rl.Allow(ctx, ChannelEmail, "contract-recipient-6"); !ok {
			t.Fatal("Allow(email) = false, want true")
		}
		if ok, _ := rl.Allow(ctx, ChannelWhatsApp, "contract-recipient-6"); !ok {
			t.Fatal("Allow(whatsapp) = false, want true (independent channel bucket)")
		}
	})
}

func TestRateLimiter_Contract(t *testing.T) {
	t.Run("Local", func(t *testing.T) {
		testRateLimiterContract(t, func(t *testing.T, requestsPerSecond, burstSize int) RateLimiter {
			t.Helper()
			rl, err := NewLocalRateLimiter(requestsPerSecond, burstSize, 0)
			if err != nil {
				t.Fatalf("NewLocalRateLimiter: %v", err)
			}
			return rl
		})
	})
	// A "Redis" subtest is added alongside ratelimiter.redis.go (Stage 9) —
	// intentionally not present yet, since that backend doesn't exist in
	// this stage.
}
