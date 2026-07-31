// File: service.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gourdian25/grevents"
)

// defaultServiceIdempotencyTTL is used whenever SendOptions.IdempotencyTTL
// is zero.
const defaultServiceIdempotencyTTL = 24 * time.Hour

// defaultServiceMaxRetryAge is used whenever SendOptions.RetryExpiresAt is
// zero — a genuine guess, not derived from any real token TTL. Callers who
// know their own message's real expiry (a password-reset link's TTL, an
// invite token's expiry) should set RetryExpiresAt explicitly rather than
// relying on this default.
const defaultServiceMaxRetryAge = 24 * time.Hour

// defaultServiceMaxInlineRetries/BaseDelay/MaxDelay are DefaultServiceConfig's
// inline-retry tuning: 3 total attempts (1 + 2 retries), a short jittered
// backoff between them.
const (
	defaultServiceMaxInlineRetries     = 2
	defaultServiceInlineRetryBaseDelay = 200 * time.Millisecond
	defaultServiceInlineRetryMaxDelay  = 2 * time.Second
)

// ServiceConfig tunes service's idempotency/retry behavior.
type ServiceConfig struct {
	// DefaultIdempotencyTTL is used when SendOptions.IdempotencyTTL is
	// zero. Defaults to defaultServiceIdempotencyTTL if <= 0.
	DefaultIdempotencyTTL time.Duration

	// DefaultMaxRetryAge is used when SendOptions.RetryExpiresAt is zero,
	// measured from the moment a send is published to the DLQ. Defaults to
	// defaultServiceMaxRetryAge if <= 0.
	DefaultMaxRetryAge time.Duration

	// MaxInlineRetries bounds Service's own inline Full-Jitter retry loop
	// for a transient per-call send failure, run before falling through to
	// DLQHandler.PublishToDLQ (docs.go: no message broker anywhere in
	// grpop — every send is direct and synchronous, so this retry runs on
	// the caller's own goroutine). Total attempts = MaxInlineRetries + 1.
	// Defaults to defaultServiceMaxInlineRetries if < 0 (0 is a valid,
	// deliberate choice: no inline retry at all, straight to DLQ on the
	// first failure).
	MaxInlineRetries int
	// InlineRetryBaseDelay/InlineRetryMaxDelay feed FullJitterBackoff
	// between inline retry attempts. Default to
	// defaultServiceInlineRetryBaseDelay/MaxDelay if <= 0.
	InlineRetryBaseDelay time.Duration
	InlineRetryMaxDelay  time.Duration
}

// DefaultServiceConfig returns a sane starting configuration.
func DefaultServiceConfig() ServiceConfig {
	return ServiceConfig{
		DefaultIdempotencyTTL: defaultServiceIdempotencyTTL,
		DefaultMaxRetryAge:    defaultServiceMaxRetryAge,
		MaxInlineRetries:      defaultServiceMaxInlineRetries,
		InlineRetryBaseDelay:  defaultServiceInlineRetryBaseDelay,
		InlineRetryMaxDelay:   defaultServiceInlineRetryMaxDelay,
	}
}

// ServiceDeps configures a Service constructed by NewService.
//
// RateLimiter is deliberately NOT a field here, even though an earlier
// draft of docs/plan/grpop-plan.md's §10 wiring example passed the same
// RateLimiter to both a dispatcher's Deps and ServiceDeps. That double-wires
// the same limiter: RateLimiter.Allow/Wait consumes a token per call, and
// Service's own inline retry (below) calls EmailSender.Send/
// WhatsAppSender.Send more than once per logical send — since
// dispatcher.email.smtp.go/dispatcher.whatsapp.metacloud.go already gate
// every individual Send attempt through their own configured RateLimiter
// (Stage 10/11), a second gate here would consume two-to-four tokens for
// one logical send with no way for either layer to know about the other's
// consumption. Rate limiting belongs at the dispatcher layer only, where it
// naturally covers every inline-retry attempt, not just the first. Configure
// RateLimiter on SMTPDispatcherDeps/MetaCloudDispatcherDeps instead.
//
// EmailTemplateEngine is similarly absent: template rendering was moved
// into dispatcher.email.smtp.go itself (SMTPDispatcherDeps.TemplateEngine)
// during Stage 10, since content-mode resolution has to happen immediately
// before the MIME message is built — Service never sees a template, only
// the EmailSender that already knows how to render one.
type ServiceDeps struct {
	// IdempotencyStore, DLQHandler, EmailSender, WhatsAppSender are
	// required.
	IdempotencyStore IdempotencyStore
	DLQHandler       DLQHandler
	EmailSender      EmailSender
	WhatsAppSender   WhatsAppSender

	// EventBus, if set, receives TopicMessageSent/TopicMessageFailed
	// lifecycle events (events.go). Optional and nil-safe — best-effort
	// only, per docs.go.
	EventBus grevents.Bus

	// Metrics is optional. Service only calls the two counters that are
	// exclusively its own responsibility: IncIdempotencyDedupHit (only
	// Service sees idempotency dedup hits) and IncDLQPublished (only
	// Service calls DLQHandler.PublishToDLQ). It deliberately does NOT call
	// ObserveSendLatency/IncSendResult itself, since
	// dispatcher.email.smtp.go/dispatcher.whatsapp.metacloud.go already
	// call those on every Send attempt when the same Metrics instance is
	// wired into their own Deps — calling them again here would
	// double-count, exactly the reasoning grnoti's own ServiceDeps.Metrics
	// doc comment gives for not calling IncInvalidTokens twice.
	Metrics Metrics

	Config ServiceConfig
	Logger Logger
}

// service implements Service: checks idempotency, dispatches via the
// channel-appropriate Sender directly (no queue/broker in between — see
// docs.go), retries transient failures inline, publishes to DLQHandler once
// those retries are exhausted, and best-effort-publishes a lifecycle event.
type service struct {
	idempotency    IdempotencyStore
	dlqHandler     DLQHandler
	emailSender    EmailSender
	whatsAppSender WhatsAppSender
	bus            grevents.Bus
	metrics        Metrics
	config         ServiceConfig
	logger         Logger

	closed    atomic.Bool
	closeOnce sync.Once
}

var _ Service = (*service)(nil)

// NewService constructs a Service.
//
// Parameters:
//   - deps: ServiceDeps — IdempotencyStore, DLQHandler, EmailSender,
//     WhatsAppSender are required
//
// Returns:
//   - Service
//   - error: non-nil if a required dependency is missing
func NewService(deps ServiceDeps) (Service, error) {
	if deps.IdempotencyStore == nil {
		return nil, errors.New("grpop: ServiceDeps.IdempotencyStore is required")
	}
	if deps.DLQHandler == nil {
		return nil, errors.New("grpop: ServiceDeps.DLQHandler is required")
	}
	if deps.EmailSender == nil {
		return nil, errors.New("grpop: ServiceDeps.EmailSender is required")
	}
	if deps.WhatsAppSender == nil {
		return nil, errors.New("grpop: ServiceDeps.WhatsAppSender is required")
	}

	config := deps.Config
	if config.DefaultIdempotencyTTL <= 0 {
		config.DefaultIdempotencyTTL = defaultServiceIdempotencyTTL
	}
	if config.DefaultMaxRetryAge <= 0 {
		config.DefaultMaxRetryAge = defaultServiceMaxRetryAge
	}
	if config.MaxInlineRetries < 0 {
		config.MaxInlineRetries = defaultServiceMaxInlineRetries
	}
	if config.InlineRetryBaseDelay <= 0 {
		config.InlineRetryBaseDelay = defaultServiceInlineRetryBaseDelay
	}
	if config.InlineRetryMaxDelay <= 0 {
		config.InlineRetryMaxDelay = defaultServiceInlineRetryMaxDelay
	}

	return &service{
		idempotency:    deps.IdempotencyStore,
		dlqHandler:     deps.DLQHandler,
		emailSender:    deps.EmailSender,
		whatsAppSender: deps.WhatsAppSender,
		bus:            deps.EventBus,
		metrics:        deps.Metrics,
		config:         config,
		logger:         OrNop(deps.Logger),
	}, nil
}

func (s *service) SendEmail(ctx context.Context, msg EmailMessage, opts SendOptions) (SendResult, error) {
	return s.send(ctx, ChannelEmail, opts,
		func(ctx context.Context) (SendResult, error) { return s.emailSender.Send(ctx, msg) },
		func(expiresAt time.Time) DLQMessage {
			return DLQMessage{Channel: ChannelEmail, Email: &msg, ExpiresAt: expiresAt}
		},
	)
}

func (s *service) SendWhatsApp(ctx context.Context, msg WhatsAppMessage, opts SendOptions) (SendResult, error) {
	return s.send(ctx, ChannelWhatsApp, opts,
		func(ctx context.Context) (SendResult, error) { return s.whatsAppSender.Send(ctx, msg) },
		func(expiresAt time.Time) DLQMessage {
			return DLQMessage{Channel: ChannelWhatsApp, WhatsApp: &msg, ExpiresAt: expiresAt}
		},
	)
}

// send is the shared SendEmail/SendWhatsApp pipeline:
//
//	IdempotencyKey required -> idempotency dedup check (fail closed on a
//	store error) -> inline-retried send -> permanent-vs-transient failure
//	classification -> DLQ publish (transient failures only) -> lifecycle
//	event -> mark idempotency key processed.
//
// buildDLQMessage is deferred until a failure is known, so the DLQMessage
// literal only gets built on the failure path that actually needs it.
func (s *service) send(ctx context.Context, channel Channel, opts SendOptions,
	sendFn func(context.Context) (SendResult, error), buildDLQMessage func(expiresAt time.Time) DLQMessage,
) (SendResult, error) {
	if s.closed.Load() {
		return SendResult{}, ErrClosed
	}
	if opts.IdempotencyKey == "" {
		return SendResult{}, ErrIdempotencyKeyRequired
	}

	// Fail closed: IdempotencyKey exists specifically to prevent a
	// caller-visible double-send (docs.go: retries are at-least-once, not
	// exactly-once), so a broken idempotency backend must block the send
	// rather than silently proceeding as if "not yet processed" — the
	// failure mode this check exists to prevent is exactly what
	// proceeding-on-error would risk.
	processed, err := s.idempotency.IsProcessed(ctx, opts.IdempotencyKey)
	if err != nil {
		return SendResult{}, fmt.Errorf("grpop: idempotency check: %w", err)
	}
	if processed {
		s.logger.Warn("grpop: duplicate send suppressed", "channel", channel, "idempotency_key", opts.IdempotencyKey)
		if s.metrics != nil {
			s.metrics.IncIdempotencyDedupHit(channel)
		}
		return SendResult{Channel: channel, Status: SendStatusSent, SentAt: time.Now().UTC(), Duplicate: true}, nil
	}

	result, sendErr := s.sendWithInlineRetry(ctx, channel, sendFn)

	if sendErr != nil && isPermanentSendError(sendErr) {
		// A caller bug (malformed message/misconfigured dispatcher), not a
		// delivery failure — retrying it (inline or via the DLQ) can never
		// succeed, so it gets no DLQ entry, no lifecycle event, and no
		// idempotency mark: the caller should fix the request and may
		// retry with the SAME key once they do, without a false
		// "duplicate" short-circuit.
		return result, sendErr
	}

	if sendErr != nil {
		s.publishToDLQ(ctx, channel, opts, buildDLQMessage, sendErr)
	}
	s.publishLifecycleEvent(ctx, channel, opts.IdempotencyKey, result, sendErr)

	ttl := opts.IdempotencyTTL
	if ttl <= 0 {
		ttl = s.config.DefaultIdempotencyTTL
	}
	if err := s.idempotency.MarkProcessed(ctx, opts.IdempotencyKey, ttl); err != nil {
		s.logger.Warn("grpop: failed to mark idempotency key processed", "idempotency_key", opts.IdempotencyKey, "error", err)
	}

	// Matches grnoti's own processEvent precedent: a delivery-level failure
	// is communicated through SendResult.Status, not the returned error —
	// the Go error return is reserved for pipeline-level failures (a
	// missing IdempotencyKey, a broken idempotency store, a permanent
	// validation error above), all of which return earlier than this
	// point.
	return result, nil
}

// sendWithInlineRetry retries a transient send failure up to
// config.MaxInlineRetries additional times, stopping early (without
// consuming further retries) the moment a permanent, non-retryable error is
// seen — see isPermanentSendError.
func (s *service) sendWithInlineRetry(ctx context.Context, channel Channel, sendFn func(context.Context) (SendResult, error)) (SendResult, error) {
	var result SendResult
	var err error

	maxAttempts := s.config.MaxInlineRetries + 1
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := FullJitterBackoff(s.config.InlineRetryBaseDelay, s.config.InlineRetryMaxDelay, attempt-1)
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(delay):
			}
		}

		result, err = sendFn(ctx)
		if err == nil {
			return result, nil
		}
		if isPermanentSendError(err) {
			s.logger.Debug("grpop: send failed with a permanent (non-retryable) error", "channel", channel, "error", err)
			return result, err
		}
		s.logger.Warn("grpop: send attempt failed", "channel", channel, "attempt", attempt+1, "max_attempts", maxAttempts, "error", err)
	}
	return result, err
}

// isPermanentSendError reports whether err is one of grpop's own
// construction/validation sentinels — a malformed message or misconfigured
// dispatcher that will fail identically on every retry, as opposed to a
// transient vendor/network failure that's actually worth retrying.
func isPermanentSendError(err error) bool {
	for _, sentinel := range permanentSendErrorSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

var permanentSendErrorSentinels = []error{
	ErrRecipientRequired,
	ErrNoContentModeSet,
	ErrMultipleContentModesSet,
	ErrEmailFromRequired,
	ErrEmailTemplateEngineRequired,
	ErrWhatsAppTemplateNameRequired,
	ErrWhatsAppLanguageCodeRequired,
	ErrWhatsAppTemplateNotApproved,
	ErrWhatsAppTemplateArityMismatch,
}

// publishToDLQ records a transient (already retried) send failure for later
// retry. sendID is always opts.IdempotencyKey, never a generated ID — the
// caller's own key is the durable identifier a DLQ entry is looked up by.
func (s *service) publishToDLQ(ctx context.Context, channel Channel, opts SendOptions, buildDLQMessage func(time.Time) DLQMessage, sendErr error) {
	expiresAt := opts.RetryExpiresAt
	switch {
	case expiresAt.IsZero():
		expiresAt = time.Now().Add(s.config.DefaultMaxRetryAge)
	case !expiresAt.After(time.Now()):
		// Accepted, not rejected: the event is still durably recorded and
		// immediately surfaces as DLQStatusExpired on the next
		// ClaimRetryableEvents sweep, but this is almost always a caller-side
		// TTL-computation bug worth surfacing loudly.
		s.logger.Warn("grpop: RetryExpiresAt is already in the past; this event will be immediately expired",
			"channel", channel, "idempotency_key", opts.IdempotencyKey, "retry_expires_at", expiresAt)
	}

	dlqMsg := buildDLQMessage(expiresAt)
	if err := s.dlqHandler.PublishToDLQ(ctx, opts.IdempotencyKey, dlqMsg, sendErr.Error()); err != nil {
		s.logger.Error("grpop: publish to DLQ failed", "channel", channel, "idempotency_key", opts.IdempotencyKey, "error", err)
		return
	}
	if s.metrics != nil {
		s.metrics.IncDLQPublished(channel)
	}
}

// publishLifecycleEvent publishes TopicMessageSent/TopicMessageFailed per
// PublishMessageSent/PublishMessageFailed's own nil-bus/best-effort
// contract. Never called for a Duplicate result — a dedup short-circuit
// performed no new work, so it has nothing to report.
func (s *service) publishLifecycleEvent(ctx context.Context, channel Channel, sendID string, result SendResult, sendErr error) {
	if sendErr != nil {
		PublishMessageFailed(ctx, s.bus, s.logger, MessageFailedPayload{SendID: sendID, Channel: channel, Reason: sendErr.Error()})
		return
	}
	PublishMessageSent(ctx, s.bus, s.logger, MessageSentPayload{SendID: sendID, Channel: channel, ProviderMessageID: result.ProviderMessageID})
}

// Close marks the service closed; further SendEmail/SendWhatsApp calls
// return ErrClosed. It does NOT cascade-close EmailSender/WhatsAppSender/
// IdempotencyStore/DLQHandler — those were constructed (and are owned) by
// the caller, which may share them across more than one Service or use them
// directly after this Service is done with them.
func (s *service) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.logger.Info("grpop: service closed")
	})
	return nil
}
