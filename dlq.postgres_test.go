// File: dlq.postgres_test.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestPostgresDLQHandler(t *testing.T, maxRetries int, retryDelay time.Duration) DLQHandler {
	t.Helper()
	return newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: maxRetries, RetryDelay: retryDelay, MaxRetryDelay: time.Second,
	})
}

func newTestPostgresDLQHandlerWithConfig(t *testing.T, cfg PostgresDLQHandlerConfig) DLQHandler {
	t.Helper()
	cfg.DSN = testPostgresDSN
	h, err := NewPostgresDLQHandler(cfg)
	if err != nil {
		t.Skipf("PostgreSQL not available, skipping: %v", err)
	}
	t.Cleanup(func() {
		if hh, ok := h.(*postgresDLQHandler); ok {
			_, _ = hh.pool.Exec(context.Background(), "DELETE FROM grpop_dlq")
		}
		_ = h.Close()
	})
	return h
}

func testDLQMessage(expiresAt time.Time) DLQMessage {
	return DLQMessage{
		Channel:   ChannelEmail,
		Email:     &EmailMessage{To: "a@example.com", Subject: "hi"},
		ExpiresAt: expiresAt,
	}
}

func TestNewPostgresDLQHandler_Defaults(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 0, 0).(*postgresDLQHandler)
	if h.maxRetries != 3 {
		t.Fatalf("maxRetries = %d, want 3 (the default)", h.maxRetries)
	}
	if h.maxAttemptHistory != 20 {
		t.Fatalf("maxAttemptHistory = %d, want 20 (the default)", h.maxAttemptHistory)
	}
}

func TestNewPostgresDLQHandler_ConnectError(t *testing.T) {
	if _, err := NewPostgresDLQHandler(PostgresDLQHandlerConfig{}); err == nil {
		t.Fatal("NewPostgresDLQHandler(empty DSN) = nil error, want non-nil")
	}
}

func TestPostgresDLQHandler_ClaimRetryableEvents_DefaultsLimit(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "pge-limit", msg, "boom"); err != nil {
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

func TestPostgresDLQHandler_PublishAndClaim(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "pge1", msg, "smtp unavailable"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 || claimed[0].SendID != "pge1" {
		t.Fatalf("ClaimRetryableEvents() = %v, want [pge1]", claimed)
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

	again, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("second ClaimRetryableEvents = (%v, %v), want empty", again, err)
	}
}

func TestPostgresDLQHandler_PublishToDLQ_DuplicateAppendsHistory(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, time.Hour)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	_ = h.PublishToDLQ(ctx, "pge2", msg, "first failure")
	_ = h.PublishToDLQ(ctx, "pge2", msg, "second failure")

	got, err := h.GetEventByID(ctx, "pge2")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if len(got.AttemptHistory) != 2 {
		t.Fatalf("AttemptHistory length = %d, want 2", len(got.AttemptHistory))
	}
	if got.FailureReason != "second failure" {
		t.Fatalf("FailureReason = %q, want second failure", got.FailureReason)
	}
	if got.RetryCount != 0 {
		t.Fatalf("RetryCount = %d, want 0", got.RetryCount)
	}
}

func TestPostgresDLQHandler_AttemptHistoryCapped(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 100, RetryDelay: 0, MaxRetryDelay: 0, MaxAttemptHistoryEntries: 3,
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pge-cap", msg, "boom")

	for i := 0; i < 10; i++ {
		_, _ = h.ClaimRetryableEvents(ctx, 10)
		_ = h.MarkRetried(ctx, "pge-cap", false, errors.New("still failing"))
	}

	got, err := h.GetEventByID(ctx, "pge-cap")
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

func TestPostgresDLQHandler_MarkRetried_RequiresClaim(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, time.Hour)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pge3", msg, "boom")

	if err := h.MarkRetried(ctx, "pge3", true, nil); !errors.Is(err, ErrDLQEventNotClaimed) {
		t.Fatalf("MarkRetried(unclaimed) error = %v, want ErrDLQEventNotClaimed", err)
	}
}

func TestPostgresDLQHandler_MarkRetried_NotFound(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	if err := h.MarkRetried(context.Background(), "never-existed-pg", true, nil); !errors.Is(err, ErrDLQEventNotFound) {
		t.Fatalf("MarkRetried(nonexistent) error = %v, want ErrDLQEventNotFound", err)
	}
}

func TestPostgresDLQHandler_MarkRetried_Success(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pge4", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "pge4", true, nil); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, err := h.GetEventByID(ctx, "pge4")
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

func TestPostgresDLQHandler_MarkRetried_ExhaustsAfterMaxRetries(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 1, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pge5", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "pge5", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "pge5")
	if got.Status != DLQStatusExhausted {
		t.Fatalf("Status = %s, want %s", got.Status, DLQStatusExhausted)
	}
}

func TestPostgresDLQHandler_MarkRetried_GoesBackToPending(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 5, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pge6", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)

	if err := h.MarkRetried(ctx, "pge6", false, errors.New("retry me")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "pge6")
	if got.Status != DLQStatusPending {
		t.Fatalf("Status = %s, want %s", got.Status, DLQStatusPending)
	}
	if got.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1", got.RetryCount)
	}
	if got.NextRetryAt.IsZero() {
		t.Fatal("NextRetryAt was never set by the pending-finalize path")
	}
}

// TestPostgresDLQHandler_MarkRetried_ExpiresBeforeExhausting proves the
// expiry-before-exhaustion ordering against a real Postgres instance: a
// message that expires with retry budget still unused is reported as
// Expired, not Exhausted.
func TestPostgresDLQHandler_MarkRetried_ExpiresBeforeExhausting(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 100, 0) // huge retry budget, retryDelay=0 so the initial claim below succeeds
	ctx := context.Background()
	// A generous margin (not 1ms) so the Publish+Claim round trip below
	// reliably completes before the deadline passes — otherwise
	// ClaimRetryableEvents' own expiry sweep can beat the claim to it,
	// which is a timing flake, not the behavior this test exists to prove.
	msg := testDLQMessage(time.Now().Add(100 * time.Millisecond))
	_ = h.PublishToDLQ(ctx, "pge-expire", msg, "boom")
	if claimed, err := h.ClaimRetryableEvents(ctx, 10); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimRetryableEvents() = (%v, %v), want exactly 1 claimed before its deadline", claimed, err)
	}

	time.Sleep(150 * time.Millisecond)

	if err := h.MarkRetried(ctx, "pge-expire", false, errors.New("still failing")); err != nil {
		t.Fatalf("MarkRetried: %v", err)
	}
	got, _ := h.GetEventByID(ctx, "pge-expire")
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status = %s, want %s (not %s, despite ample retry budget remaining)", got.Status, DLQStatusExpired, DLQStatusExhausted)
	}
}

// TestPostgresDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents
// proves ClaimRetryableEvents' step 1 against a real Postgres instance: a
// Pending event whose deadline has passed is transitioned to Expired even
// if nothing else ever calls MarkRetried on it.
func TestPostgresDLQHandler_ClaimRetryableEvents_SweepsExpiredPendingEvents(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, time.Hour) // long retryDelay: NextRetryAt is far off
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Millisecond))
	_ = h.PublishToDLQ(ctx, "pge-sweep", msg, "boom")

	time.Sleep(5 * time.Millisecond)

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("ClaimRetryableEvents() = %v, want empty (event should be Expired, not claimable)", claimed)
	}

	got, err := h.GetEventByID(ctx, "pge-sweep")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.Status != DLQStatusExpired {
		t.Fatalf("Status after deadline sweep = %s, want %s", got.Status, DLQStatusExpired)
	}
}

// TestPostgresDLQHandler_ZeroExpiresAt_NeverExpires proves a zero-value
// ExpiresAt (only reachable by constructing a DLQMessage directly,
// bypassing Service) is accepted (stored as SQL NULL, not rejected by a
// NOT NULL constraint) and behaves as "no deadline": never swept to
// Expired, and remains claimable indefinitely — matching
// memoryDLQHandler's identical treatment of a zero ExpiresAt.
func TestPostgresDLQHandler_ZeroExpiresAt_NeverExpires(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Time{}) // zero value: no deadline

	if err := h.PublishToDLQ(ctx, "pge-no-deadline", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ(zero ExpiresAt): %v", err)
	}

	claimed, err := h.ClaimRetryableEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimRetryableEvents: %v", err)
	}
	if len(claimed) != 1 || claimed[0].SendID != "pge-no-deadline" {
		t.Fatalf("ClaimRetryableEvents() = %v, want [pge-no-deadline] (a NULL deadline must not block claiming)", claimed)
	}
	if !claimed[0].MessageData.ExpiresAt.IsZero() {
		t.Fatalf("MessageData.ExpiresAt = %v, want zero value round-tripped", claimed[0].MessageData.ExpiresAt)
	}
}

func TestPostgresDLQHandler_PurgeExpiredEvents(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 1, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	_ = h.PublishToDLQ(ctx, "pg-resolved", msg, "boom")
	_, _ = h.ClaimRetryableEvents(ctx, 10)
	_ = h.MarkRetried(ctx, "pg-resolved", true, nil)

	_ = h.PublishToDLQ(ctx, "pg-still-pending", msg, "boom")

	purged, err := h.PurgeExpiredEvents(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredEvents: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeExpiredEvents() = %d, want 1", purged)
	}
}

// TestPostgresDLQHandler_ConcurrentClaimNeverDoubleClaims proves the FOR
// UPDATE SKIP LOCKED claim design against a real Postgres instance — the
// central correctness guarantee this backend exists to provide.
func TestPostgresDLQHandler_ConcurrentClaimNeverDoubleClaims(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	const numEvents = 100
	for i := 0; i < numEvents; i++ {
		_ = h.PublishToDLQ(ctx, fmt.Sprintf("pg-evt-%d", i), msg, "boom")
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

// TestRowToDomain_MalformedMessageData inserts a row whose message_data is
// valid JSON (JSONB guarantees that) but not the expected shape —
// exercising rowToDomain's json.Unmarshal error branch, unreachable via a
// plain malformed-syntax insert since Postgres rejects non-JSON text at the
// JSONB column level.
func TestRowToDomain_MalformedMessageData(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	hh, ok := h.(*postgresDLQHandler)
	if !ok {
		t.Fatal("handler is not *postgresDLQHandler")
	}
	ctx := context.Background()
	_, err := hh.pool.Exec(ctx, `INSERT INTO grpop_dlq
		(send_id, channel, message_data, failure_reason, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, created_at, updated_at)
		VALUES ($1, 'email', '[1,2,3]', 'boom', 3, now(), now(), now(), now() + interval '1 hour', 'pending', now(), now())`, "pge-malformed")
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}

	if _, err := h.GetEventByID(ctx, "pge-malformed"); err == nil {
		t.Fatal("GetEventByID(malformed message_data) = nil error, want non-nil")
	}
}

// fakeXOREncryptor is a trivial, insecure "encryptor" used only to prove
// MessageEncryptor is actually wired through the encode/decode path — not
// a real cryptographic implementation.
type fakeXOREncryptor struct{}

func (fakeXOREncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0x42
	}
	return out, nil
}

func (fakeXOREncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	return fakeXOREncryptor{}.Encrypt(ciphertext) // XOR is its own inverse
}

func TestPostgresDLQHandler_Encryptor_RoundTrips(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 3, Encryptor: fakeXOREncryptor{},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "pge-enc", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}

	got, err := h.GetEventByID(ctx, "pge-enc")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.MessageData.Email == nil || got.MessageData.Email.To != "a@example.com" {
		t.Fatalf("decrypted MessageData.Email = %+v, want To=a@example.com", got.MessageData.Email)
	}

	hh := h.(*postgresDLQHandler)
	raw, err := hh.pool.Query(ctx, "SELECT message_data FROM grpop_dlq WHERE send_id = $1", "pge-enc")
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	defer raw.Close()
	if !raw.Next() {
		t.Fatal("no row returned")
	}
	var stored []byte
	if err := raw.Scan(&stored); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if string(stored) == "" {
		t.Fatal("stored message_data is empty")
	}
	// The plaintext "a@example.com" must not appear in what's actually on
	// disk — proving Encrypt was really applied, not silently skipped.
	if containsBytes(stored, []byte("a@example.com")) {
		t.Fatal("stored message_data contains the plaintext recipient address, want it encrypted")
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

type erroringEncryptor struct {
	encryptErr error
	decryptErr error
}

func (e erroringEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	if e.encryptErr != nil {
		return nil, e.encryptErr
	}
	return fakeXOREncryptor{}.Encrypt(plaintext)
}

func (e erroringEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	if e.decryptErr != nil {
		return nil, e.decryptErr
	}
	return fakeXOREncryptor{}.Decrypt(ciphertext)
}

func TestPostgresDLQHandler_Encryptor_EncryptError(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 3, Encryptor: erroringEncryptor{encryptErr: errors.New("key unavailable")},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "pge-enc-err", msg, "boom"); err == nil {
		t.Fatal("PublishToDLQ(Encrypt error) = nil error, want non-nil")
	}
}

func TestPostgresDLQHandler_Encryptor_DecryptError(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 3, Encryptor: erroringEncryptor{decryptErr: errors.New("key unavailable")},
	})
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))
	if err := h.PublishToDLQ(ctx, "pge-dec-err", msg, "boom"); err != nil {
		t.Fatalf("PublishToDLQ: %v", err)
	}
	if _, err := h.GetEventByID(ctx, "pge-dec-err"); err == nil {
		t.Fatal("GetEventByID(Decrypt error) = nil error, want non-nil")
	}
}

// TestPostgresDLQHandler_Encryptor_MalformedEnvelope inserts a row whose
// message_data is valid JSON but not the base64-wrapped-string envelope an
// Encryptor-configured handler expects — exercising decodeMessageData's
// envelope-unmarshal error branch.
func TestPostgresDLQHandler_Encryptor_MalformedEnvelope(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 3, Encryptor: fakeXOREncryptor{},
	})
	hh := h.(*postgresDLQHandler)
	ctx := context.Background()
	_, err := hh.pool.Exec(ctx, `INSERT INTO grpop_dlq
		(send_id, channel, message_data, failure_reason, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, created_at, updated_at)
		VALUES ($1, 'email', '[1,2,3]', 'boom', 3, now(), now(), now(), now() + interval '1 hour', 'pending', now(), now())`, "pge-bad-envelope")
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := h.GetEventByID(ctx, "pge-bad-envelope"); err == nil {
		t.Fatal("GetEventByID(non-string envelope, encryptor configured) = nil error, want non-nil")
	}
}

// TestPostgresDLQHandler_Encryptor_MalformedBase64 inserts a row whose
// message_data is a valid JSON string but not valid base64 — exercising
// decodeMessageData's base64-decode error branch specifically.
func TestPostgresDLQHandler_Encryptor_MalformedBase64(t *testing.T) {
	h := newTestPostgresDLQHandlerWithConfig(t, PostgresDLQHandlerConfig{
		MaxRetries: 3, Encryptor: fakeXOREncryptor{},
	})
	hh := h.(*postgresDLQHandler)
	ctx := context.Background()
	_, err := hh.pool.Exec(ctx, `INSERT INTO grpop_dlq
		(send_id, channel, message_data, failure_reason, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, created_at, updated_at)
		VALUES ($1, 'email', '"not valid base64!!"', 'boom', 3, now(), now(), now(), now() + interval '1 hour', 'pending', now(), now())`, "pge-bad-b64")
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := h.GetEventByID(ctx, "pge-bad-b64"); err == nil {
		t.Fatal("GetEventByID(invalid base64, encryptor configured) = nil error, want non-nil")
	}
}

// TestPostgresDLQHandler_ClaimRetryableEvents_DecodeErrorPropagates proves
// a malformed row's decode error surfaces through the claim path too, not
// only GetEventByID.
func TestPostgresDLQHandler_ClaimRetryableEvents_DecodeErrorPropagates(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	hh := h.(*postgresDLQHandler)
	ctx := context.Background()
	_, err := hh.pool.Exec(ctx, `INSERT INTO grpop_dlq
		(send_id, channel, message_data, failure_reason, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, created_at, updated_at)
		VALUES ($1, 'email', '[1,2,3]', 'boom', 3, now(), now(), now(), now() + interval '1 hour', 'pending', now(), now())`, "pge-claim-malformed")
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := h.ClaimRetryableEvents(ctx, 10); err == nil {
		t.Fatal("ClaimRetryableEvents(malformed row among candidates) = nil error, want non-nil")
	}
}

// TestRowToDomain_MalformedAttemptHistory exercises rowToDomain's
// attempt_history json.Unmarshal error branch specifically (separate from
// TestRowToDomain_MalformedMessageData, which only breaks message_data).
func TestRowToDomain_MalformedAttemptHistory(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	hh := h.(*postgresDLQHandler)
	ctx := context.Background()
	_, err := hh.pool.Exec(ctx, `INSERT INTO grpop_dlq
		(send_id, channel, message_data, failure_reason, max_retries, first_failure_at, last_attempt_at, next_retry_at, expires_at, status, attempt_history, created_at, updated_at)
		VALUES ($1, 'email', '{}', 'boom', 3, now(), now(), now(), now() + interval '1 hour', 'pending', '{"not":"an array"}', now(), now())`, "pge-bad-history")
	if err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	if _, err := h.GetEventByID(ctx, "pge-bad-history"); err == nil {
		t.Fatal("GetEventByID(malformed attempt_history) = nil error, want non-nil")
	}
}

func TestPostgresDLQHandler_Close_Idempotent(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
	if _, err := h.GetEventByID(context.Background(), "pge1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("GetEventByID after Close error = %v, want ErrClosed", err)
	}
}

func TestPostgresDLQHandler_GenericQueryError(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
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

func TestPostgresDLQHandler_AfterClose_EveryMethodReturnsErrClosed(t *testing.T) {
	h := newTestPostgresDLQHandler(t, 3, 0)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	msg := testDLQMessage(time.Now().Add(time.Hour))

	if err := h.PublishToDLQ(ctx, "pge1", msg, "boom"); !errors.Is(err, ErrClosed) {
		t.Errorf("PublishToDLQ after Close = %v, want ErrClosed", err)
	}
	if _, err := h.ClaimRetryableEvents(ctx, 10); !errors.Is(err, ErrClosed) {
		t.Errorf("ClaimRetryableEvents after Close = %v, want ErrClosed", err)
	}
	if err := h.MarkRetried(ctx, "pge1", true, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("MarkRetried after Close = %v, want ErrClosed", err)
	}
	if _, err := h.PurgeExpiredEvents(ctx, time.Hour); !errors.Is(err, ErrClosed) {
		t.Errorf("PurgeExpiredEvents after Close = %v, want ErrClosed", err)
	}
}
