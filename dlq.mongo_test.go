// File: dlq.mongo_test.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

const testMongoURI = "mongodb://root:mongo_password@localhost:27018/?directConnection=true"

func newTestMongoDLQHandler(t *testing.T, maxRetries int, retryDelay time.Duration) DLQHandler {
	t.Helper()
	return newTestMongoDLQHandlerWithConfig(t, MongoDLQHandlerConfig{
		MaxRetries: maxRetries, RetryDelay: retryDelay, MaxRetryDelay: time.Second,
	})
}

func newTestMongoDLQHandlerWithConfig(t *testing.T, cfg MongoDLQHandlerConfig) DLQHandler {
	t.Helper()
	cfg.URI = testMongoURI
	cfg.Database = "grpop_test"
	if cfg.CollectionName == "" {
		cfg.CollectionName = fmt.Sprintf("dlq_%d", time.Now().UnixNano())
	}
	h, err := NewMongoDLQHandler(cfg)
	if err != nil {
		t.Skipf("MongoDB not available at %s, skipping: %v", testMongoURI, err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestNewMongoDLQHandler_Defaults(t *testing.T) {
	h := newTestMongoDLQHandler(t, 0, 0).(*mongoDLQHandler)
	if h.maxRetries != 3 {
		t.Fatalf("maxRetries = %d, want 3 (the default)", h.maxRetries)
	}
	if h.maxAttemptHistory != 20 {
		t.Fatalf("maxAttemptHistory = %d, want 20 (the default)", h.maxAttemptHistory)
	}
}

func TestNewMongoDLQHandler_EmptyURI(t *testing.T) {
	_, err := NewMongoDLQHandler(MongoDLQHandlerConfig{Database: "grpop_test"})
	if err == nil {
		t.Fatal("NewMongoDLQHandler(empty URI) = nil error, want non-nil")
	}
}

func TestNewMongoDLQHandler_EmptyDatabase(t *testing.T) {
	_, err := NewMongoDLQHandler(MongoDLQHandlerConfig{URI: testMongoURI})
	if err == nil {
		t.Fatal("NewMongoDLQHandler(empty Database) = nil error, want non-nil")
	}
}

func TestNewMongoDLQHandler_MalformedURI(t *testing.T) {
	_, err := NewMongoDLQHandler(MongoDLQHandlerConfig{URI: "not-a-valid-mongo-uri", Database: "grpop_test"})
	if err == nil {
		t.Fatal("NewMongoDLQHandler(malformed URI) = nil error, want non-nil")
	}
}

func TestMongoDLQHandler_ClaimRetryableEvents_DefaultsLimit(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "e-limit", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}
	claimed, err := h.ClaimRetryableEvents(ctx, 0) // <=0 -> defaults to 10
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimRetryableEvents(limit=0) = %v, want 1 claimed event (default limit applied)", claimed)
	}
}

func TestMongoDLQHandler_PublishAndClaim(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "e1", msg, "smtp unavailable"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 || claimed[0].SendID != "e1" {
		t.Fatalf("ClaimRetryableEvents() = %v, want [e1]", claimed)
	}
	if claimed[0].Status != DLQStatusRetrying {
		t.Fatalf("claimed status = %s, want %s", claimed[0].Status, DLQStatusRetrying)
	}
	if claimed[0].MessageData.Channel != ChannelEmail {
		t.Fatalf("claimed MessageData.Channel = %s, want %s", claimed[0].MessageData.Channel, ChannelEmail)
	}
	if claimed[0].MessageData.Email == nil || claimed[0].MessageData.Email.To != "a@example.com" {
		t.Fatalf("claimed MessageData.Email = %+v, want To=a@example.com", claimed[0].MessageData.Email)
	}

	againClaimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil || len(againClaimed) != 0 {
		t.Fatalf("second ClaimRetryableEvents = (%v, %v), want empty (already claimed)", againClaimed, err)
	}
}

func TestMongoDLQHandler_PublishToDLQ_DuplicateAppendsHistory(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, time.Hour) // long delay so it stays pending, not claimable
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	_ = h.PublishToDLQ(ctx, "e1", msg, "first failure")
	_ = h.PublishToDLQ(ctx, "e1", msg, "second failure")

	got, err := h.GetEventByID(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if len(got.AttemptHistory) != 2 {
		t.Fatalf("AttemptHistory length = %d, want 2", len(got.AttemptHistory))
	}
	if got.FailureReason != "second failure" {
		t.Fatalf("FailureReason = %q, want %q (most recent)", got.FailureReason, "second failure")
	}
	if got.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0 (PublishToDLQ never increments it, only MarkRetried does)", got.RetryCount)
	}
}

func TestMongoDLQHandler_AttemptHistoryCapped(t *testing.T) {
	h := newTestMongoDLQHandlerWithConfig(t, MongoDLQHandlerConfig{
		MaxRetries: 100, RetryDelay: 0, MaxRetryDelay: 0, MaxAttemptHistoryEntries: 3,
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "e-cap", msg, "boom")

	for i := 0; i < 10; i++ {
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		_ = h.MarkRetried(ctx, "e-cap", false, errors.New("still failing"))
	}

	got, err := h.GetEventByID(ctx, "e-cap")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if len(got.AttemptHistory) != 3 {
		t.Fatalf("AttemptHistory length = %d, want capped at 3", len(got.AttemptHistory))
	}
	if last := got.AttemptHistory[len(got.AttemptHistory)-1]; last.AttemptNumber != 10 {
		t.Fatalf("last AttemptHistory entry AttemptNumber = %d, want 10", last.AttemptNumber)
	}
}

func TestMongoDLQHandler_MarkRetried_RequiresClaim(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, time.Hour)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "e1", msg, "boom")

	if err := h.MarkRetried(ctx, "e1", true, nil); !errors.Is(err, ErrDLQEventNotClaimed) {
		t.Fatalf("MarkRetried(unclaimed) error = %v, want ErrDLQEventNotClaimed", err)
	}
}

func TestMongoDLQHandler_MarkRetried_NotFound(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	if err := h.MarkRetried(context.Background(), "never-existed", true, nil); !errors.Is(err, ErrDLQEventNotFound) {
		t.Fatalf("MarkRetried(nonexistent) error = %v, want ErrDLQEventNotFound", err)
	}
}

func TestMongoDLQHandler_MarkRetried_Success(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "e1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "e1", true, nil); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, err := h.GetEventByID(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.Status != DLQStatusResolved {
		t.Fatalf("Status = %s, want %s", got.Status, DLQStatusResolved)
	}
	if got.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", got.RetryCount)
	}
}

func TestMongoDLQHandler_MarkRetried_ExhaustsAfterMaxRetries(t *testing.T) {
	h := newTestMongoDLQHandler(t, 1, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "e1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "e1", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "e1")
	if got.Status != DLQStatusExhausted {
		t.Fatalf("Status = %s, want %s", got.Status, DLQStatusExhausted)
	}
}

func TestMongoDLQHandler_MarkRetried_GoesBackToPending(t *testing.T) {
	h := newTestMongoDLQHandler(t, 5, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "e1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "e1", false, errors.New("retry me")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "e1")
	if got.Status != DLQStatusPending {
		t.Fatalf("Status = %s, want %s", got.Status, DLQStatusPending)
	}
	if got.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", got.RetryCount)
	}
}

// TestMongoDLQHandler_MarkRetried_ExpiresBeforeExhausting proves the
// expiry-before-exhaustion ordering against a real MongoDB instance.
func TestMongoDLQHandler_MarkRetried_ExpiresBeforeExhausting(t *testing.T) {
	h := newTestMongoDLQHandler(t, 100, 0)
	ctx := context.Background()
	// A generous margin (not 1ms) so the Publish+Claim round trip below
	// reliably completes before the deadline passes — otherwise
	// ClaimRetryableEvents' own expiry sweep can beat the claim to it,
	// which is a timing flake, not the behavior this test exists to prove.
	msg := testDLQMessage(time.Now().Add(100 * time.Millisecond))
	_ = h.PublishToDLQ(ctx, "e-expire", msg, "boom")
	if claimed, err := h.ClaimRetryableEvents(ctx, 10); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimRetryableEvents() = (%v, %v), want exactly 1 claimed before its deadline", claimed, err)
	}

	time.Sleep(150 * time.Millisecond)

	if err := h.MarkRetried(ctx, "e-expire", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "e-expire")
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status = %s, want %s (not %s, despite ample retry budget remaining)", got.Status, DLQStatusExpired, DLQStatusExhausted)
	}
}

// TestMongoDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents
// proves ClaimRetryableEvents' step 1 against a real MongoDB instance.
func TestMongoDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, time.Hour)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Millisecond))
	_ = h.PublishToDLQ(ctx, "e-sweep", msg, "boom")

	time.Sleep(5 * time.Millisecond)

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimRetryableEvents() = %v, want empty (event should be Expired, not claimable)", claimed)
	}

	got, err := h.GetEventByID(ctx, "e-sweep")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status after deadline sweep = %s, want %s", got.Status, DLQStatusExpired)
	}
}

func TestMongoDLQHandler_ZeroExpiresAt_NeverExpires(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Time{}) // zero value: no deadline

	if err := h.PublishToDLQ(ctx, "e-no-deadline", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ(zero ExpiresAt): %v", err)
	}

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 || claimed[0].SendID != "e-no-deadline" {
		t.Fatalf("ClaimRetryableEvents() = %v, want [e-no-deadline] (an absent deadline must not block claiming)", claimed)
	}
	if !claimed[0].MessageData.ExpiresAt.IsZero() {
		t.Fatalf("MessageData.ExpiresAt = %v, want zero value round-tripped", claimed[0].MessageData.ExpiresAt)
	}
}

func TestMongoDLQHandler_PurgeExpiredEvents(t *testing.T) {
	h := newTestMongoDLQHandler(t, 1, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "resolved", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)
	_ = h.MarkRetried(ctx, "resolved", true, nil)

	_ = h.PublishToDLQ(ctx, "still-pending", msg, "boom")

	purged, err := h.PurgeExpiredEvents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeExpiredEvents() = %d, want 1", purged)
	}
}

// TestMongoDLQHandler_ConcurrentClaimNeverDoubleClaims proves the atomic
// per-document claim design against a real MongoDB instance, not just the
// in-memory backend's mutex.
func TestMongoDLQHandler_ConcurrentClaimNeverDoubleClaims(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	const numEvents = 100
	for i := 0; i < numEvents; i++ {
		_ = h.PublishToDLQ(ctx, fmt.Sprintf("evt-%d", i), msg, "boom")
	}

	const numWorkers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimedIDs := make(map[string]int)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := h.ClaimRetryableEvents(ctx, 20)
			if err != nil {
				t.Errorf("ClaimRetryableEvents: %v", err)
				return
			}
			mu.Lock()
			for _, e := range claimed {
				claimedIDs[e.SendID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	for id, count := range claimedIDs {
		if count > 1 {
			t.Fatalf("event %s was claimed %d times, want at most 1", id, count)
		}
	}
	if len(claimedIDs) != numEvents {
		t.Fatalf("claimed %d distinct events across all workers, want %d", len(claimedIDs), numEvents)
	}
}

func TestMongoDLQHandler_Encryptor_RoundTrips(t *testing.T) {
	h := newTestMongoDLQHandlerWithConfig(t, MongoDLQHandlerConfig{
		MaxRetries: 3, Encryptor: fakeXOREncryptor{},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "e-enc", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}

	got, err := h.GetEventByID(ctx, "e-enc")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.MessageData.Email == nil || got.MessageData.Email.To != "a@example.com" {
		t.Fatalf("decrypted MessageData.Email = %+v, want To=a@example.com", got.MessageData.Email)
	}

	hh := h.(*mongoDLQHandler)
	var raw struct {
		MessageData []byte `bson:"message_data"`
	}
	if err := hh.collection.FindOne(ctx, map[string]any{"send_id": "e-enc"}).Decode(&raw); err != nil {
		t.Fatalf("raw find: %v", err)
	}
	if len(raw.MessageData) == 0 {
		t.Fatal("stored message_data is empty")
	}
	if containsBytes(raw.MessageData, []byte("a@example.com")) {
		t.Fatal("stored message_data contains the plaintext recipient address, want it encrypted")
	}
}

func TestNewMongoDLQHandler_UnreachableURI(t *testing.T) {
	_, err := NewMongoDLQHandler(MongoDLQHandlerConfig{
		URI: "mongodb://127.0.0.1:1/?connectTimeoutMS=100&serverSelectionTimeoutMS=100", Database: "grpop_test",
	})
	if err == nil {
		t.Fatal("NewMongoDLQHandler(unreachable URI) = nil error, want non-nil")
	}
}

func TestMongoDLQHandler_Encryptor_EncryptError(t *testing.T) {
	h := newTestMongoDLQHandlerWithConfig(t, MongoDLQHandlerConfig{
		MaxRetries: 3, Encryptor: erroringEncryptor{encryptErr: errors.New("key unavailable")},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "e-enc-err", msg, "boom"); err == nil {
		t.Fatal("PublishToDLQ(Encrypt error) = nil error, want non-nil")
	}
}

// TestMongoDLQHandler_ClaimRetryableEvents_DecodeErrorPropagates proves a
// malformed document's decode error surfaces through the claim path too,
// not only GetEventByID.
func TestMongoDLQHandler_ClaimRetryableEvents_DecodeErrorPropagates(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	hh := h.(*mongoDLQHandler)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := hh.collection.InsertOne(ctx, map[string]any{
		"send_id": "e-claim-malformed", "channel": "email", "message_data": []byte("not valid json"),
		"failure_reason": "boom", "retry_count": 0, "max_retries": 3,
		"first_failure_at": now, "last_attempt_at": now, "next_retry_at": now,
		"status": DLQStatusPending, "attempt_history": []DLQRetryAttempt{}, "created_at": now, "updated_at": now,
	})
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := h.ClaimRetryableEvents(ctx, 10); err == nil {
		t.Fatal("ClaimRetryableEvents(malformed doc among candidates) = nil error, want non-nil")
	}
}

func TestMongoDLQHandler_Encryptor_DecryptError(t *testing.T) {
	h := newTestMongoDLQHandlerWithConfig(t, MongoDLQHandlerConfig{
		MaxRetries: 3, Encryptor: erroringEncryptor{decryptErr: errors.New("key unavailable")},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "e-dec-err", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}
	if _, err := h.GetEventByID(ctx, "e-dec-err"); err == nil {
		t.Fatal("GetEventByID(Decrypt error) = nil error, want non-nil")
	}
}

func TestMongoDLQHandler_Close_Idempotent(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
	if _, err := h.GetEventByID(context.Background(), "e1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("GetEventByID after Close error = %v, want ErrClosed", err)
	}
}

// TestMongoDLQHandler_GenericQueryError uses an already-canceled context to
// force a real query-level error.
func TestMongoDLQHandler_GenericQueryError(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "e1", msg, "boom"); err == nil {
		t.Error("PublishToDLQ(canceled ctx) = nil error, want non-nil")
	}
	if _, err := h.ClaimRetryableEvents(ctx, 10); err == nil {
		t.Error("ClaimRetryableEvents(canceled ctx) = nil error, want non-nil")
	}
	if _, err := h.GetEventByID(ctx, "e1"); err == nil {
		t.Error("GetEventByID(canceled ctx) = nil error, want non-nil")
	}
	if _, err := h.PurgeExpiredEvents(ctx, time.Hour); err == nil {
		t.Error("PurgeExpiredEvents(canceled ctx) = nil error, want non-nil")
	}
	if err := h.MarkRetried(ctx, "e1", true, nil); err == nil {
		t.Error("MarkRetried(canceled ctx) = nil error, want non-nil")
	}
}

func TestMongoDLQHandler_AfterClose_EveryMethodReturnsErrClosed(t *testing.T) {
	h := newTestMongoDLQHandler(t, 3, 0)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "e1", msg, "boom"); !errors.Is(err, ErrClosed) {
		t.Errorf("PublishToDLQ after Close = %v, want ErrClosed", err)
	}
	if _, err := h.ClaimRetryableEvents(ctx, 10); !errors.Is(err, ErrClosed) {
		t.Errorf("ClaimRetryableEvents after Close = %v, want ErrClosed", err)
	}
	if err := h.MarkRetried(ctx, "e1", true, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("MarkRetried after Close = %v, want ErrClosed", err)
	}
	if _, err := h.PurgeExpiredEvents(ctx, time.Hour); !errors.Is(err, ErrClosed) {
		t.Errorf("PurgeExpiredEvents after Close = %v, want ErrClosed", err)
	}
}
