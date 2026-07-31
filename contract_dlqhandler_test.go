// File: contract_dlqhandler_test.go

package grpop

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testDLQHandlerContract is the shared behavioral contract every DLQHandler
// backend must satisfy identically. The atomic-claim semantics themselves
// get their own dedicated concurrent stress test per backend
// (TestMemoryDLQHandler_ConcurrentClaimNeverDoubleClaims,
// TestMongoDLQHandler_ConcurrentClaimNeverDoubleClaims,
// TestPostgresDLQHandler_ConcurrentClaimNeverDoubleClaims), and the
// ExpiresAt/DLQStatusExpired deadline state machine gets its own dedicated
// per-backend tests too — both are exactly the kind of thing this
// contract suite's "can't be expressed generically without real
// backend-specific timing/concurrency" carve-out is for. This suite covers
// the sequential lifecycle contract shared by all three.
func testDLQHandlerContract(t *testing.T, newHandler func(t *testing.T) DLQHandler) {
	t.Helper()
	ctx := context.Background()
	msg := func() DLQMessage {
		return DLQMessage{Channel: ChannelEmail, Email: &EmailMessage{To: "a@example.com", Subject: "hi"}, ExpiresAt: time.Now().Add(time.Hour)}
	}

	t.Run("PublishThenClaim", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		if err := h.PublishToDLQ(ctx, "contract-dlq1", msg(), "boom"); err != nil {
			t.Fatalf("PublishToDLQ: %v", err)
		}
		claimed, err := h.ClaimRetryableEvents(ctx, 10)
		if err != nil {
			t.Fatalf("ClaimRetryableEvents: %v", err)
		}
		if len(claimed) != 1 || claimed[0].SendID != "contract-dlq1" {
			t.Fatalf("ClaimRetryableEvents() = %v, want [contract-dlq1]", claimed)
		}
		if claimed[0].Status != DLQStatusRetrying {
			t.Fatalf("claimed Status = %s, want %s", claimed[0].Status, DLQStatusRetrying)
		}
		if claimed[0].MessageData.Channel != ChannelEmail {
			t.Fatalf("claimed MessageData.Channel = %s, want %s", claimed[0].MessageData.Channel, ChannelEmail)
		}
	})

	t.Run("ClaimedEventNotReclaimed", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		_ = h.PublishToDLQ(ctx, "contract-dlq2", msg(), "boom")
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		again, err := h.ClaimRetryableEvents(ctx, 10)
		if err != nil {
			t.Fatalf("ClaimRetryableEvents (second call): %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("ClaimRetryableEvents (second call) = %v, want empty", again)
		}
	})

	t.Run("MarkRetried_RequiresClaim", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		_ = h.PublishToDLQ(ctx, "contract-dlq3", msg(), "boom")
		if err := h.MarkRetried(ctx, "contract-dlq3", true, nil); !errors.Is(err, ErrDLQEventNotClaimed) {
			t.Fatalf("MarkRetried(unclaimed) error = %v, want ErrDLQEventNotClaimed", err)
		}
	})

	t.Run("MarkRetried_NotFound", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		if err := h.MarkRetried(ctx, "contract-never-existed", true, nil); !errors.Is(err, ErrDLQEventNotFound) {
			t.Fatalf("MarkRetried(nonexistent) error = %v, want ErrDLQEventNotFound", err)
		}
	})

	t.Run("MarkRetried_SuccessResolves", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		_ = h.PublishToDLQ(ctx, "contract-dlq4", msg(), "boom")
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		if err := h.MarkRetried(ctx, "contract-dlq4", true, nil); err != nil {
			t.Fatalf("MarkRetried: %v", err)
		}
		got, err := h.GetEventByID(ctx, "contract-dlq4")
		if err != nil || got.Status != DLQStatusResolved {
			t.Fatalf("GetEventByID() = (%+v, %v), want Status=%s", got, err, DLQStatusResolved)
		}
	})

	t.Run("MarkRetried_FailureGoesBackToPending", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		_ = h.PublishToDLQ(ctx, "contract-dlq5", msg(), "boom")
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		if err := h.MarkRetried(ctx, "contract-dlq5", false, errors.New("retry me")); err != nil {
			t.Fatalf("MarkRetried: %v", err)
		}
		got, err := h.GetEventByID(ctx, "contract-dlq5")
		if err != nil || got.Status != DLQStatusPending || got.RetryCount != 1 {
			t.Fatalf("GetEventByID() = (%+v, %v), want Status=%s RetryCount=1", got, err, DLQStatusPending)
		}
	})

	t.Run("GetEventByID_NotFound", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		if _, err := h.GetEventByID(ctx, "contract-never-existed-2"); !errors.Is(err, ErrDLQEventNotFound) {
			t.Fatalf("GetEventByID(missing) error = %v, want ErrDLQEventNotFound", err)
		}
	})

	t.Run("PurgeExpiredEvents_ResolvedIsPurged", func(t *testing.T) {
		h := newHandler(t)
		defer h.Close()
		_ = h.PublishToDLQ(ctx, "contract-dlq6", msg(), "boom")
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		_ = h.MarkRetried(ctx, "contract-dlq6", true, nil)

		purged, err := h.PurgeExpiredEvents(ctx, time.Hour)
		if err != nil {
			t.Fatalf("PurgeExpiredEvents: %v", err)
		}
		if purged < 1 {
			t.Fatalf("PurgeExpiredEvents() = %d, want >= 1 (the resolved event)", purged)
		}
		if _, err := h.GetEventByID(ctx, "contract-dlq6"); !errors.Is(err, ErrDLQEventNotFound) {
			t.Fatalf("GetEventByID(purged event) error = %v, want ErrDLQEventNotFound", err)
		}
	})
}

func TestDLQHandler_Contract(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		testDLQHandlerContract(t, func(t *testing.T) DLQHandler {
			return NewMemoryDLQHandler(3, 0, time.Second, 0)
		})
	})
	t.Run("Mongo", func(t *testing.T) {
		testDLQHandlerContract(t, func(t *testing.T) DLQHandler {
			collection := contractCollectionName(t)
			h, err := NewMongoDLQHandler(MongoDLQHandlerConfig{
				URI: testMongoURI, Database: "grpop_test", CollectionName: collection,
				MaxRetries: 3, RetryDelay: 0, MaxRetryDelay: time.Second,
			})
			if err != nil {
				t.Skipf("MongoDB not available, skipping: %v", err)
			}
			cleanupContractCollection(t, "grpop_test", collection)
			return h
		})
	})
	t.Run("Postgres", func(t *testing.T) {
		testDLQHandlerContract(t, func(t *testing.T) DLQHandler {
			h, err := NewPostgresDLQHandler(PostgresDLQHandlerConfig{
				PostgresConfig: PostgresConfig{DSN: testPostgresDSN},
				MaxRetries:     3, RetryDelay: 0, MaxRetryDelay: time.Second,
			})
			if err != nil {
				t.Skipf("PostgreSQL not available, skipping: %v", err)
			}
			cleanupContractRows(t, "grpop_dlq", "send_id")
			return h
		})
	})
}
