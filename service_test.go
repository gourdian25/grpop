// File: service_test.go

package grpop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeIdempotencyStore is a fake IdempotencyStore letting tests inject
// IsProcessed/MarkProcessed errors and observe exactly what ttl
// MarkProcessed was called with.
type fakeIdempotencyStore struct {
	mu sync.Mutex

	processed        map[string]bool
	isProcessedErr   error
	markProcessedErr error

	isProcessedCalls     int
	markProcessedCalls   int
	lastMarkProcessedTTL time.Duration
}

func (s *fakeIdempotencyStore) IsProcessed(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isProcessedCalls++
	if s.isProcessedErr != nil {
		return false, s.isProcessedErr
	}
	return s.processed[key], nil
}

func (s *fakeIdempotencyStore) MarkProcessed(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markProcessedCalls++
	s.lastMarkProcessedTTL = ttl
	if s.markProcessedErr != nil {
		return s.markProcessedErr
	}
	if s.processed == nil {
		s.processed = make(map[string]bool)
	}
	s.processed[key] = true
	return nil
}

func (s *fakeIdempotencyStore) Close() error { return nil }

// fakeDLQHandler is a fake DLQHandler recording every PublishToDLQ call.
type fakeDLQHandler struct {
	mu sync.Mutex

	publishErr error
	published  []fakeDLQPublishCall
}

type fakeDLQPublishCall struct {
	sendID        string
	msg           DLQMessage
	failureReason string
}

func (h *fakeDLQHandler) PublishToDLQ(_ context.Context, sendID string, msg DLQMessage, failureReason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.published = append(h.published, fakeDLQPublishCall{sendID: sendID, msg: msg, failureReason: failureReason})
	return h.publishErr
}
func (h *fakeDLQHandler) ClaimRetryableEvents(_ context.Context, _ int) ([]*DLQEvent, error) {
	return nil, nil
}
func (h *fakeDLQHandler) MarkRetried(_ context.Context, _ string, _ bool, _ error) error { return nil }
func (h *fakeDLQHandler) GetEventByID(_ context.Context, _ string) (*DLQEvent, error) {
	return nil, ErrDLQEventNotFound
}
func (h *fakeDLQHandler) PurgeExpiredEvents(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}
func (h *fakeDLQHandler) Close() error { return nil }

func (h *fakeDLQHandler) calls() []fakeDLQPublishCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]fakeDLQPublishCall, len(h.published))
	copy(out, h.published)
	return out
}

// fakeControllableEmailSender fails its first failTimes calls (with
// transientErr, or permanentErr if set — permanentErr never clears), then
// succeeds.
type fakeControllableEmailSender struct {
	mu sync.Mutex

	failTimes    int
	transientErr error
	permanentErr error

	callCount int
	sent      []EmailMessage
}

func (s *fakeControllableEmailSender) Send(_ context.Context, msg EmailMessage) (SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	s.sent = append(s.sent, msg)
	if s.permanentErr != nil {
		return SendResult{Channel: ChannelEmail, Status: SendStatusFailed}, s.permanentErr
	}
	if s.callCount <= s.failTimes {
		return SendResult{Channel: ChannelEmail, Status: SendStatusFailed}, s.transientErr
	}
	return SendResult{Channel: ChannelEmail, ProviderMessageID: "fake-email-id", Status: SendStatusSent, SentAt: time.Now().UTC()}, nil
}
func (s *fakeControllableEmailSender) Close() error { return nil }
func (s *fakeControllableEmailSender) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

// fakeControllableWhatsAppSender mirrors fakeControllableEmailSender for
// WhatsAppSender.
type fakeControllableWhatsAppSender struct {
	mu sync.Mutex

	failTimes    int
	transientErr error
	permanentErr error

	callCount int
	sent      []WhatsAppMessage
}

func (s *fakeControllableWhatsAppSender) Send(_ context.Context, msg WhatsAppMessage) (SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	s.sent = append(s.sent, msg)
	if s.permanentErr != nil {
		return SendResult{Channel: ChannelWhatsApp, Status: SendStatusFailed}, s.permanentErr
	}
	if s.callCount <= s.failTimes {
		return SendResult{Channel: ChannelWhatsApp, Status: SendStatusFailed}, s.transientErr
	}
	return SendResult{Channel: ChannelWhatsApp, ProviderMessageID: "fake-wa-id", Status: SendStatusSent, SentAt: time.Now().UTC()}, nil
}
func (s *fakeControllableWhatsAppSender) Close() error { return nil }
func (s *fakeControllableWhatsAppSender) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

func newTestService(t *testing.T, mutate func(*ServiceDeps)) (Service, *fakeIdempotencyStore, *fakeDLQHandler, *fakeControllableEmailSender, *fakeControllableWhatsAppSender, *fakeEventBus, *fakeMetrics) {
	t.Helper()
	idem := &fakeIdempotencyStore{}
	dlq := &fakeDLQHandler{}
	emailSender := &fakeControllableEmailSender{}
	waSender := &fakeControllableWhatsAppSender{}
	bus := &fakeEventBus{}
	metrics := &fakeMetrics{}

	deps := ServiceDeps{
		IdempotencyStore: idem,
		DLQHandler:       dlq,
		EmailSender:      emailSender,
		WhatsAppSender:   waSender,
		EventBus:         bus,
		Metrics:          metrics,
		Config: ServiceConfig{
			MaxInlineRetries:     2,
			InlineRetryBaseDelay: time.Millisecond,
			InlineRetryMaxDelay:  5 * time.Millisecond,
		},
	}
	if mutate != nil {
		mutate(&deps)
	}
	svc, err := NewService(deps)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, idem, dlq, emailSender, waSender, bus, metrics
}

func validEmailMsg() EmailMessage {
	return EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}
}

func TestNewService_RequiresAllDeps(t *testing.T) {
	base := ServiceDeps{
		IdempotencyStore: &fakeIdempotencyStore{},
		DLQHandler:       &fakeDLQHandler{},
		EmailSender:      &fakeControllableEmailSender{},
		WhatsAppSender:   &fakeControllableWhatsAppSender{},
	}

	tests := []struct {
		name   string
		mutate func(*ServiceDeps)
	}{
		{"no idempotency store", func(d *ServiceDeps) { d.IdempotencyStore = nil }},
		{"no dlq handler", func(d *ServiceDeps) { d.DLQHandler = nil }},
		{"no email sender", func(d *ServiceDeps) { d.EmailSender = nil }},
		{"no whatsapp sender", func(d *ServiceDeps) { d.WhatsAppSender = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := base
			tt.mutate(&deps)
			if _, err := NewService(deps); err == nil {
				t.Fatal("NewService() err = nil, want non-nil")
			}
		})
	}
}

func TestService_SendEmail_RequiresIdempotencyKey(t *testing.T) {
	svc, _, _, _, _, _, _ := newTestService(t, nil)
	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{})
	if !errors.Is(err, ErrIdempotencyKeyRequired) {
		t.Fatalf("SendEmail() err = %v, want ErrIdempotencyKeyRequired", err)
	}
}

func TestService_SendEmail_AfterClose(t *testing.T) {
	svc, _, _, _, _, _, _ := newTestService(t, nil)
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("SendEmail() after Close err = %v, want ErrClosed", err)
	}
	// Idempotent: a second Close must not panic or error.
	if err := svc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestService_SendEmail_IdempotencyStoreErrorFailsClosed(t *testing.T) {
	svc, idem, _, emailSender, _, _, _ := newTestService(t, nil)
	idem.isProcessedErr = errors.New("redis down")

	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err == nil {
		t.Fatal("SendEmail() err = nil, want non-nil (idempotency store failure should fail closed)")
	}
	if emailSender.calls() != 0 {
		t.Fatalf("emailSender.calls() = %d, want 0 (should never attempt a send when dedup status is unknown)", emailSender.calls())
	}
}

func TestService_SendEmail_DuplicateShortCircuits(t *testing.T) {
	svc, idem, dlq, emailSender, _, bus, metrics := newTestService(t, nil)
	idem.processed = map[string]bool{"k1": true}

	result, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if !result.Duplicate {
		t.Fatal("result.Duplicate = false, want true")
	}
	if emailSender.calls() != 0 {
		t.Fatalf("emailSender.calls() = %d, want 0", emailSender.calls())
	}
	if len(dlq.calls()) != 0 {
		t.Fatalf("dlq publishes = %d, want 0", len(dlq.calls()))
	}
	if len(bus.events()) != 0 {
		t.Fatalf("lifecycle events published = %d, want 0 (a dedup hit did no new work)", len(bus.events()))
	}
	if metrics.dedupHits != 1 {
		t.Fatalf("metrics.dedupHits = %d, want 1", metrics.dedupHits)
	}
}

func TestService_SendEmail_SuccessOnFirstAttempt(t *testing.T) {
	svc, idem, dlq, emailSender, _, bus, _ := newTestService(t, nil)

	result, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if emailSender.calls() != 1 {
		t.Fatalf("emailSender.calls() = %d, want 1", emailSender.calls())
	}
	if len(dlq.calls()) != 0 {
		t.Fatalf("dlq publishes = %d, want 0", len(dlq.calls()))
	}
	events := bus.events()
	if len(events) != 1 || events[0].Topic != TopicMessageSent {
		t.Fatalf("events = %v, want exactly one TopicMessageSent", events)
	}
	if idem.markProcessedCalls != 1 {
		t.Fatalf("markProcessedCalls = %d, want 1", idem.markProcessedCalls)
	}
	if idem.lastMarkProcessedTTL != defaultServiceIdempotencyTTL {
		t.Fatalf("lastMarkProcessedTTL = %v, want default %v", idem.lastMarkProcessedTTL, defaultServiceIdempotencyTTL)
	}
}

func TestService_SendEmail_UsesCustomIdempotencyTTL(t *testing.T) {
	svc, idem, _, _, _, _, _ := newTestService(t, nil)
	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1", IdempotencyTTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if idem.lastMarkProcessedTTL != 10*time.Minute {
		t.Fatalf("lastMarkProcessedTTL = %v, want 10m", idem.lastMarkProcessedTTL)
	}
}

func TestService_SendEmail_TransientFailureSucceedsOnRetry(t *testing.T) {
	svc, _, dlq, _, _, bus, _ := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{failTimes: 1, transientErr: errors.New("temporary smtp error")}
	})
	emailSender := svcEmailSender(t, svc)

	result, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if emailSender.calls() != 2 {
		t.Fatalf("emailSender.calls() = %d, want 2 (1 failure + 1 success)", emailSender.calls())
	}
	if len(dlq.calls()) != 0 {
		t.Fatalf("dlq publishes = %d, want 0 (retry succeeded before exhaustion)", len(dlq.calls()))
	}
	events := bus.events()
	if len(events) != 1 || events[0].Topic != TopicMessageSent {
		t.Fatalf("events = %v, want exactly one TopicMessageSent", events)
	}
}

// svcEmailSender is a small test-only escape hatch: newTestService's mutate
// callback replaces EmailSender with a fresh fake, so the test needs a way
// to get back a typed reference to the one actually wired into svc for
// call-count assertions.
func svcEmailSender(t *testing.T, svc Service) *fakeControllableEmailSender {
	t.Helper()
	impl, ok := svc.(*service)
	if !ok {
		t.Fatalf("svc is %T, want *service", svc)
	}
	sender, ok := impl.emailSender.(*fakeControllableEmailSender)
	if !ok {
		t.Fatalf("impl.emailSender is %T, want *fakeControllableEmailSender", impl.emailSender)
	}
	return sender
}

func TestService_SendEmail_TransientFailureExhaustsRetriesThenDLQs(t *testing.T) {
	transientErr := errors.New("smtp: connection refused")
	svc, idem, dlq, _, _, bus, metrics := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{failTimes: 999, transientErr: transientErr}
		d.Config = ServiceConfig{MaxInlineRetries: 2, InlineRetryBaseDelay: time.Millisecond, InlineRetryMaxDelay: 5 * time.Millisecond}
	})
	emailSender := svcEmailSender(t, svc)

	result, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail() err = %v, want nil (delivery failures are reported via SendResult.Status, not the error return)", err)
	}
	if result.Status != SendStatusFailed {
		t.Fatalf("Status = %v, want SendStatusFailed", result.Status)
	}
	if emailSender.calls() != 3 {
		t.Fatalf("emailSender.calls() = %d, want 3 (1 + MaxInlineRetries=2)", emailSender.calls())
	}

	calls := dlq.calls()
	if len(calls) != 1 {
		t.Fatalf("dlq publishes = %d, want 1", len(calls))
	}
	if calls[0].sendID != "k1" {
		t.Fatalf("dlq sendID = %q, want k1 (must be the caller's own IdempotencyKey)", calls[0].sendID)
	}
	if calls[0].msg.Channel != ChannelEmail || calls[0].msg.Email == nil {
		t.Fatalf("dlq msg = %+v, want Channel=email with Email set", calls[0].msg)
	}
	if calls[0].failureReason != transientErr.Error() {
		t.Fatalf("dlq failureReason = %q, want %q", calls[0].failureReason, transientErr.Error())
	}

	events := bus.events()
	if len(events) != 1 || events[0].Topic != TopicMessageFailed {
		t.Fatalf("events = %v, want exactly one TopicMessageFailed", events)
	}
	if metrics.sendResults != nil {
		t.Fatalf("metrics.sendResults = %v, want nil (Service must not call IncSendResult itself, only the dispatcher does)", metrics.sendResults)
	}
	if idem.markProcessedCalls != 1 {
		t.Fatalf("markProcessedCalls = %d, want 1 (a DLQ'd failure still gets marked processed)", idem.markProcessedCalls)
	}
}

func TestService_SendEmail_PermanentErrorSkipsRetryDLQAndIdempotencyMark(t *testing.T) {
	svc, idem, dlq, _, _, bus, _ := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{permanentErr: ErrRecipientRequired}
	})
	emailSender := svcEmailSender(t, svc)

	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if !errors.Is(err, ErrRecipientRequired) {
		t.Fatalf("SendEmail() err = %v, want ErrRecipientRequired", err)
	}
	if emailSender.calls() != 1 {
		t.Fatalf("emailSender.calls() = %d, want 1 (a permanent error must not be retried)", emailSender.calls())
	}
	if len(dlq.calls()) != 0 {
		t.Fatalf("dlq publishes = %d, want 0 (a permanent error is a caller bug, not worth a DLQ entry)", len(dlq.calls()))
	}
	if len(bus.events()) != 0 {
		t.Fatalf("lifecycle events = %d, want 0", len(bus.events()))
	}
	if idem.markProcessedCalls != 0 {
		t.Fatalf("markProcessedCalls = %d, want 0 (caller should be able to retry the SAME key once they fix the bug)", idem.markProcessedCalls)
	}
}

func TestService_SendEmail_RetryExpiresAtInPastStillPublishesToDLQ(t *testing.T) {
	svc, _, dlq, _, _, _, _ := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{failTimes: 999, transientErr: errors.New("boom")}
		d.Config = ServiceConfig{MaxInlineRetries: 0}
	})

	past := time.Now().Add(-time.Hour)
	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1", RetryExpiresAt: past})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	calls := dlq.calls()
	if len(calls) != 1 {
		t.Fatalf("dlq publishes = %d, want 1", len(calls))
	}
	if !calls[0].msg.ExpiresAt.Equal(past) {
		t.Fatalf("dlq msg.ExpiresAt = %v, want the caller-supplied past deadline %v", calls[0].msg.ExpiresAt, past)
	}
}

func TestService_SendEmail_DLQExpiresAtDefaultsWhenUnset(t *testing.T) {
	svc, _, dlq, _, _, _, _ := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{failTimes: 999, transientErr: errors.New("boom")}
		d.Config = ServiceConfig{MaxInlineRetries: 0, DefaultMaxRetryAge: time.Hour}
	})

	before := time.Now()
	_, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail: %v", err)
	}
	calls := dlq.calls()
	if len(calls) != 1 {
		t.Fatalf("dlq publishes = %d, want 1", len(calls))
	}
	wantAround := before.Add(time.Hour)
	diff := calls[0].msg.ExpiresAt.Sub(wantAround)
	if diff < -time.Second || diff > time.Second {
		t.Fatalf("dlq msg.ExpiresAt = %v, want approximately %v", calls[0].msg.ExpiresAt, wantAround)
	}
}

func TestService_SendWhatsApp_SuccessAndFailurePaths(t *testing.T) {
	svc, _, dlq, _, waSender, bus, _ := newTestService(t, nil)

	msg := WhatsAppMessage{To: "15551234567", TemplateName: "t", LanguageCode: "en_US"}
	result, err := svc.SendWhatsApp(context.Background(), msg, SendOptions{IdempotencyKey: "wa-1"})
	if err != nil {
		t.Fatalf("SendWhatsApp: %v", err)
	}
	if result.Status != SendStatusSent || result.Channel != ChannelWhatsApp {
		t.Fatalf("result = %+v, want Status=sent Channel=whatsapp", result)
	}
	if waSender.calls() != 1 {
		t.Fatalf("waSender.calls() = %d, want 1", waSender.calls())
	}
	events := bus.events()
	if len(events) != 1 || events[0].Topic != TopicMessageSent {
		t.Fatalf("events = %v, want one TopicMessageSent", events)
	}
	if len(dlq.calls()) != 0 {
		t.Fatalf("dlq publishes = %d, want 0", len(dlq.calls()))
	}
}

func TestService_SendWhatsApp_FailureDLQCarriesWhatsAppMessage(t *testing.T) {
	svc, _, dlq, _, _, _, _ := newTestService(t, func(d *ServiceDeps) {
		d.WhatsAppSender = &fakeControllableWhatsAppSender{failTimes: 999, transientErr: errors.New("graph api down")}
		d.Config = ServiceConfig{MaxInlineRetries: 0}
	})

	msg := WhatsAppMessage{To: "15551234567", TemplateName: "t", LanguageCode: "en_US"}
	_, err := svc.SendWhatsApp(context.Background(), msg, SendOptions{IdempotencyKey: "wa-2"})
	if err != nil {
		t.Fatalf("SendWhatsApp: %v", err)
	}
	calls := dlq.calls()
	if len(calls) != 1 {
		t.Fatalf("dlq publishes = %d, want 1", len(calls))
	}
	if calls[0].msg.Channel != ChannelWhatsApp || calls[0].msg.WhatsApp == nil || calls[0].msg.Email != nil {
		t.Fatalf("dlq msg = %+v, want Channel=whatsapp with WhatsApp set and Email nil", calls[0].msg)
	}
	if calls[0].msg.WhatsApp.TemplateName != "t" {
		t.Fatalf("dlq msg.WhatsApp.TemplateName = %q, want t", calls[0].msg.WhatsApp.TemplateName)
	}
}

func TestIsPermanentSendError(t *testing.T) {
	permanent := []error{
		ErrRecipientRequired, ErrNoContentModeSet, ErrMultipleContentModesSet,
		ErrEmailFromRequired, ErrEmailTemplateEngineRequired,
		ErrWhatsAppTemplateNameRequired, ErrWhatsAppLanguageCodeRequired,
		ErrWhatsAppTemplateNotApproved, ErrWhatsAppTemplateArityMismatch,
	}
	for _, err := range permanent {
		if !isPermanentSendError(err) {
			t.Errorf("isPermanentSendError(%v) = false, want true", err)
		}
	}

	transient := []error{errors.New("connection refused"), ErrCircuitOpen, ErrRateLimited}
	for _, err := range transient {
		if isPermanentSendError(err) {
			t.Errorf("isPermanentSendError(%v) = true, want false", err)
		}
	}
}

func TestDefaultServiceConfig(t *testing.T) {
	cfg := DefaultServiceConfig()
	if cfg.DefaultIdempotencyTTL <= 0 || cfg.DefaultMaxRetryAge <= 0 || cfg.InlineRetryBaseDelay <= 0 || cfg.InlineRetryMaxDelay <= 0 {
		t.Fatalf("DefaultServiceConfig() = %+v, want every duration field positive", cfg)
	}
	if cfg.MaxInlineRetries < 0 {
		t.Fatalf("MaxInlineRetries = %d, want >= 0", cfg.MaxInlineRetries)
	}
}

func TestService_SendEmail_DLQPublishItselfFailingIsLoggedNotPropagated(t *testing.T) {
	svc, idem, dlq, _, _, bus, metrics := newTestService(t, func(d *ServiceDeps) {
		d.EmailSender = &fakeControllableEmailSender{failTimes: 999, transientErr: errors.New("boom")}
		d.Config = ServiceConfig{MaxInlineRetries: 0}
	})
	dlq.publishErr = errors.New("dlq backend unavailable")

	result, err := svc.SendEmail(context.Background(), validEmailMsg(), SendOptions{IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("SendEmail() err = %v, want nil (a DLQ publish failure must not surface as a pipeline error)", err)
	}
	if result.Status != SendStatusFailed {
		t.Fatalf("Status = %v, want SendStatusFailed", result.Status)
	}
	if len(dlq.calls()) != 1 {
		t.Fatalf("dlq publish attempts = %d, want 1", len(dlq.calls()))
	}
	if metrics.dedupHits != 0 {
		t.Fatalf("dedupHits = %d, want 0", metrics.dedupHits)
	}
	// The lifecycle "failed" event still fires — it reports the original
	// send failure, independent of whether the DLQ write itself succeeded.
	events := bus.events()
	if len(events) != 1 || events[0].Topic != TopicMessageFailed {
		t.Fatalf("events = %v, want exactly one TopicMessageFailed", events)
	}
	// The idempotency key is still marked processed even though the DLQ
	// write failed — Service has done everything it's going to do for this
	// key regardless.
	if idem.markProcessedCalls != 1 {
		t.Fatalf("markProcessedCalls = %d, want 1", idem.markProcessedCalls)
	}
}
