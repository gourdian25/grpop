// File: memory_test.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestDLQMessage(expiresAt time.Time) DLQMessage {
	return DLQMessage{
		Channel:   ChannelEmail,
		Email:     &EmailMessage{To: "a@example.com", Subject: "hi"},
		ExpiresAt: expiresAt,
	}
}

func TestMemoryDLQHandler_PublishAndClaim(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0) // retryDelay=0 so it's immediately claimable
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "s1", msg, "smtp unavailable"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 || claimed[0].SendID != "s1" {
		t.Fatalf("ClaimRetryableEvents() = %v, want [s1]", claimed)
	}
	if claimed[0].Status != DLQStatusRetrying {
		t.Fatalf("claimed event status = %s, want %s", claimed[0].Status, DLQStatusRetrying)
	}

	// A second claim must not re-claim the same event — it's already
	// DLQStatusRetrying, not DLQStatusPending.
	claimedAgain, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents (second call): %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("ClaimRetryableEvents (second call) = %v, want empty (already claimed)", claimedAgain)
	}
}

func TestMemoryDLQHandler_PublishToDLQ_ExistingEventAppendsHistory(t *testing.T) {
	h := NewMemoryDLQHandler(3, time.Hour, time.Hour, 0) // long delay: stays pending, not claimable
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))

	_ = h.PublishToDLQ(ctx, "s1", msg, "first failure")
	_ = h.PublishToDLQ(ctx, "s1", msg, "second failure")

	got, err := h.GetEventByID(ctx, "s1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if len(got.AttemptHistory) != 2 {
		t.Fatalf("AttemptHistory length = %d, want 2 (republish appends, doesn't replace)", len(got.AttemptHistory))
	}
	if got.FailureReason != "second failure" {
		t.Fatalf("FailureReason = %q, want %q (most recent)", got.FailureReason, "second failure")
	}
}

func TestMemoryDLQHandler_AttemptHistoryCapped(t *testing.T) {
	h := NewMemoryDLQHandler(100, 0, 0, 3) // cap at 3
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")

	for i := 0; i < 10; i++ {
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		_ = h.MarkRetried(ctx, "s1", false, errors.New("still failing"))
	}

	got, err := h.GetEventByID(ctx, "s1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if len(got.AttemptHistory) != 3 {
		t.Fatalf("AttemptHistory length = %d, want capped at 3", len(got.AttemptHistory))
	}
	// FIFO: the oldest entries should have been dropped, so the last entry
	// should be the most recent attempt (AttemptNumber 10).
	if last := got.AttemptHistory[len(got.AttemptHistory)-1]; last.AttemptNumber != 10 {
		t.Fatalf("last AttemptHistory entry AttemptNumber = %d, want 10", last.AttemptNumber)
	}
}

func TestNewMemoryDLQHandler_Defaults(t *testing.T) {
	h := NewMemoryDLQHandler(0, 0, 0, 0).(*memoryDLQHandler)
	if h.config.maxRetries != 3 {
		t.Fatalf("config.maxRetries = %d, want 3 (the default)", h.config.maxRetries)
	}
	if h.config.maxAttemptHistory != 20 {
		t.Fatalf("config.maxAttemptHistory = %d, want 20 (the default)", h.config.maxAttemptHistory)
	}
}

func TestMemoryDLQHandler_MarkRetried_RequiresClaim(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")

	// s1 is DLQStatusPending, never claimed.
	err := h.MarkRetried(ctx, "s1", true, nil)
	if !errors.Is(err, ErrDLQEventNotClaimed) {
		t.Fatalf("MarkRetried(unclaimed event) error = %v, want ErrDLQEventNotClaimed", err)
	}
}

func TestMemoryDLQHandler_MarkRetried_Success(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "s1", true, nil); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, err := h.GetEventByID(ctx, "s1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.Status != DLQStatusResolved {
		t.Fatalf("Status after successful retry = %s, want %s", got.Status, DLQStatusResolved)
	}
}

func TestMemoryDLQHandler_MarkRetried_ExhaustsAfterMaxRetries(t *testing.T) {
	h := NewMemoryDLQHandler(1, 0, 0, 0) // maxRetries=1: the first failed retry exhausts it
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "s1", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "s1")
	if got.Status != DLQStatusExhausted {
		t.Fatalf("Status after exhausting retries = %s, want %s", got.Status, DLQStatusExhausted)
	}
}

func TestMemoryDLQHandler_MarkRetried_GoesBackToPending(t *testing.T) {
	h := NewMemoryDLQHandler(5, 0, time.Second, 0) // retryDelay=0 so PublishToDLQ's event is immediately claimable
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "s1", false, errors.New("retry me")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "s1")
	if got.Status != DLQStatusPending {
		t.Fatalf("Status after a retryable failure = %s, want %s", got.Status, DLQStatusPending)
	}
	if got.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", got.RetryCount)
	}
}

// TestMemoryDLQHandler_MarkRetried_ExpiresBeforeExhausting proves the
// expiry-before-exhaustion ordering from DLQHandler's doc comment: a
// message that expires with retry budget still unused is reported as
// Expired, not Exhausted.
func TestMemoryDLQHandler_MarkRetried_ExpiresBeforeExhausting(t *testing.T) {
	h := NewMemoryDLQHandler(100, 0, time.Hour, 0) // huge retry budget, retryDelay=0 so the initial claim below succeeds
	ctx := context.Background()
	// ExpiresAt is already in the past relative to "now + backoff", so the
	// very next recomputed NextRetryAt necessarily lands at or past it.
	msg := newTestDLQMessage(time.Now().Add(time.Millisecond))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	time.Sleep(5 * time.Millisecond)

	if err := h.MarkRetried(ctx, "s1", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "s1")
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status after expiry-during-retry = %s, want %s (not %s, despite ample retry budget remaining)",
			got.Status, DLQStatusExpired, DLQStatusExhausted)
	}
}

// TestMemoryDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents
// proves ClaimRetryableEvents' step 1 (DLQHandler's doc comment): a Pending
// event whose deadline has passed is transitioned to Expired even if
// nothing else ever calls MarkRetried on it.
func TestMemoryDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents(t *testing.T) {
	h := NewMemoryDLQHandler(3, time.Hour, time.Hour, 0) // long retryDelay: NextRetryAt is far off
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Millisecond))
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")

	time.Sleep(5 * time.Millisecond)

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimRetryableEvents() = %v, want empty (event should be Expired, not claimable)", claimed)
	}

	got, err := h.GetEventByID(ctx, "s1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status after deadline sweep = %s, want %s", got.Status, DLQStatusExpired)
	}
}

func TestMemoryDLQHandler_ClaimRetryableEvents_ZeroExpiresAtNeverSwept(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	ctx := context.Background()
	msg := newTestDLQMessage(time.Time{}) // zero value: no deadline
	_ = h.PublishToDLQ(ctx, "s1", msg, "boom")

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimRetryableEvents() = %v, want [s1] (zero ExpiresAt means no deadline)", claimed)
	}
}

func TestMemoryDLQHandler_GetEventByID_NotFound(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	if _, err := h.GetEventByID(context.Background(), "never-existed"); !errors.Is(err, ErrDLQEventNotFound) {
		t.Fatalf("GetEventByID(missing) error = %v, want ErrDLQEventNotFound", err)
	}
}

func TestMemoryDLQHandler_PurgeExpiredEvents(t *testing.T) {
	h := NewMemoryDLQHandler(1, 0, 0, 0)
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "resolved", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)
	_ = h.MarkRetried(ctx, "resolved", true, nil)

	_ = h.PublishToDLQ(ctx, "still-pending", msg, "boom")

	purged, err := h.PurgeExpiredEvents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeExpiredEvents() = %d, want 1 (only the resolved event)", purged)
	}
	if _, err := h.GetEventByID(ctx, "still-pending"); err != nil {
		t.Fatalf("still-pending event was purged unexpectedly: %v", err)
	}
}

func TestMemoryDLQHandler_Close(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMemoryDLQHandler_ConcurrentClaimNeverDoubleClaims is the core
// correctness proof for the atomic-claim contract: N workers concurrently
// calling ClaimRetryableEvents against the same pool of pending events
// must partition them disjointly — no event may ever be returned to two
// different callers.
func TestMemoryDLQHandler_ConcurrentClaimNeverDoubleClaims(t *testing.T) {
	h := NewMemoryDLQHandler(3, 0, 0, 0)
	ctx := context.Background()
	msg := newTestDLQMessage(time.Now().Add(time.Hour))

	const numEvents = 200
	for i := 0; i < numEvents; i++ {
		_ = h.PublishToDLQ(ctx, fmt.Sprintf("evt-%d", i), msg, "boom")
	}

	const numWorkers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimedIDs := make(map[string]int) // sendID -> claim count

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := h.ClaimRetryableEvents(ctx, 50)
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

	if len(claimedIDs) != numEvents {
		t.Fatalf("total distinct claimed events = %d, want %d", len(claimedIDs), numEvents)
	}
	for id, count := range claimedIDs {
		if count != 1 {
			t.Fatalf("event %s was claimed %d times, want exactly 1", id, count)
		}
	}
}

func TestMemoryEmailSender_RecordsSentMessages(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx := context.Background()
	msg := EmailMessage{To: "a@example.com", Subject: "hi", HTMLBody: "<p>hi</p>"}

	result, err := s.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %s, want %s", result.Status, SendStatusSent)
	}
	if result.Channel != ChannelEmail {
		t.Fatalf("Channel = %s, want %s", result.Channel, ChannelEmail)
	}
	if result.ProviderMessageID == "" {
		t.Fatal("ProviderMessageID is empty, want a generated ID")
	}

	sent := s.Sent()
	if len(sent) != 1 || sent[0].To != "a@example.com" {
		t.Fatalf("Sent() = %v, want [msg]", sent)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMemoryEmailSender_Send_CanceledContext(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Send(ctx, EmailMessage{To: "a@example.com", Subject: "hi"}); err == nil {
		t.Fatal("Send(canceled ctx) = nil error, want non-nil")
	}
	if len(s.Sent()) != 0 {
		t.Fatal("Sent() is non-empty after a canceled-context Send, want no recording")
	}
}

func TestMemoryEmailSender_Sent_ReturnsCopyNotAlias(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx := context.Background()
	_, _ = s.Send(ctx, EmailMessage{To: "a@example.com", Subject: "hi"})

	sent := s.Sent()
	sent[0].To = "mutated@example.com"

	sentAgain := s.Sent()
	if sentAgain[0].To != "a@example.com" {
		t.Fatalf("internal state mutated via Sent()'s returned slice: got %q, want %q", sentAgain[0].To, "a@example.com")
	}
}

func TestMemoryWhatsAppSender_RecordsSentMessages(t *testing.T) {
	s := NewMemoryWhatsAppSender()
	ctx := context.Background()
	msg := WhatsAppMessage{To: "+15550001111", TemplateName: "welcome", LanguageCode: "en_US"}

	result, err := s.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %s, want %s", result.Status, SendStatusSent)
	}
	if result.Channel != ChannelWhatsApp {
		t.Fatalf("Channel = %s, want %s", result.Channel, ChannelWhatsApp)
	}

	sent := s.Sent()
	if len(sent) != 1 || sent[0].To != "+15550001111" {
		t.Fatalf("Sent() = %v, want [msg]", sent)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMemoryWhatsAppSender_Send_CanceledContext(t *testing.T) {
	s := NewMemoryWhatsAppSender()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := WhatsAppMessage{To: "+15550001111", TemplateName: "welcome", LanguageCode: "en_US"}
	if _, err := s.Send(ctx, msg); err == nil {
		t.Fatal("Send(canceled ctx) = nil error, want non-nil")
	}
	if len(s.Sent()) != 0 {
		t.Fatal("Sent() is non-empty after a canceled-context Send, want no recording")
	}
}

func TestGenerateMemoryID_Unique(t *testing.T) {
	a := generateMemoryID()
	b := generateMemoryID()
	if a == b {
		t.Fatalf("generateMemoryID() produced the same value twice: %q", a)
	}
	if a == "" {
		t.Fatal("generateMemoryID() = empty string")
	}
}
