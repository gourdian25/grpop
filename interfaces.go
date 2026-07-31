// File: interfaces.go

package grpop

import (
	"context"
	"time"
)

// EmailSender dispatches a single EmailMessage. Kept separate from
// WhatsAppSender rather than unified behind one Send(ctx, msg Message)
// method — WhatsAppMessage's shape is not compatible with EmailMessage's
// without losing the compile-time template-vs-freeform-body distinction
// that is the whole point of the two message types.
type EmailSender interface {
	Send(ctx context.Context, msg EmailMessage) (SendResult, error)
	Close() error
}

// WhatsAppSender dispatches a single WhatsAppMessage. See EmailSender's
// doc comment for why this is a distinct interface, not a unified one.
type WhatsAppSender interface {
	Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error)
	Close() error
}

// Service is the top-level orchestrator: checks idempotency, gates through
// the rate limiter, dispatches via the channel-appropriate Sender directly
// (no queue/broker in between — see docs.go), publishes to DLQHandler on
// exhausted inline-retry failure, and best-effort-publishes a lifecycle
// event via grevents. Every SendX method is synchronous on the calling
// goroutine.
//
// SendEmail/SendWhatsApp return ErrIdempotencyKeyRequired immediately,
// before any rate-limit check or vendor call, if opts.IdempotencyKey is
// empty.
//
// When IdempotencyStore.IsProcessed reports an incoming IdempotencyKey as
// already processed, Service returns immediately with
// SendResult{Duplicate: true, ...} — no vendor call, no rate-limit
// consumption — but first logs a Warn (key + channel only, never message
// content) and increments Metrics.IncIdempotencyDedupHit. A dedup hit is
// the expected, correct outcome for a genuine retried request, but is
// otherwise indistinguishable from a real caller bug (the same key
// accidentally reused for two different message bodies) without this
// visibility.
type Service interface {
	SendEmail(ctx context.Context, msg EmailMessage, opts SendOptions) (SendResult, error)
	SendWhatsApp(ctx context.Context, msg WhatsAppMessage, opts SendOptions) (SendResult, error)
	Close() error
}

// IdempotencyStore records which IdempotencyKey values have already been
// processed, so Service can short-circuit a redelivered send instead of
// dispatching it twice. Backed by grcache.Cache.
type IdempotencyStore interface {
	IsProcessed(ctx context.Context, idempotencyKey string) (bool, error)
	MarkProcessed(ctx context.Context, idempotencyKey string, ttl time.Duration) error
	Close() error
}

// DLQHandler is the durable, pull-based retry store for sends whose inline
// retry was exhausted. There is no background reclaim loop inside grpop
// itself — ClaimRetryableEvents is a primitive a consuming application's
// own periodic worker/cron calls. This is the closest thing to a "queue"
// that exists in grpop: nothing pushes a claimed event anywhere, a
// caller's own process polls for work.
type DLQHandler interface {
	PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error

	// ClaimRetryableEvents does two things, in order, each call:
	//
	//  1. Proactively transitions any DLQStatusPending event whose
	//     msg.ExpiresAt has already passed to DLQStatusExpired — a plain
	//     bulk update, not part of the atomic-claim step below, since it
	//     needs no cross-replica coordination. Without this step, an event
	//     whose deadline passes while nothing calls ClaimRetryableEvents
	//     for it would sit invisibly in Pending forever instead of
	//     surfacing as a terminal, dashboard-visible state.
	//
	//  2. Atomically selects up to limit of the remaining events whose
	//     NextRetryAt has passed, Status is still DLQStatusPending, and
	//     ExpiresAt is still in the future, transitioning each to
	//     DLQStatusRetrying as part of the same operation — so N
	//     concurrent worker replicas each claim disjoint events.
	ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error)

	// MarkRetried records a retry attempt's outcome and transitions sendID
	// out of DLQStatusRetrying: to DLQStatusResolved on success; to
	// DLQStatusExpired if the recomputed NextRetryAt (on a failure with
	// retries remaining) would land at or past msg.ExpiresAt — this check
	// runs before the MaxRetries check, so a message that expires with
	// retry budget still unused is reported as Expired, not Exhausted; to
	// DLQStatusExhausted if RetryCount reaches MaxRetries (and ExpiresAt
	// hasn't passed); otherwise back to DLQStatusPending with the
	// recomputed NextRetryAt. Returns ErrDLQEventNotClaimed if sendID is
	// not currently DLQStatusRetrying.
	MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error

	GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error)

	// PurgeExpiredEvents deletes DLQStatusResolved/DLQStatusExhausted/
	// DLQStatusExpired events, and any event older than maxAge regardless
	// of status. Its name refers to a different sense of "expired" (old
	// enough to clean up) than DLQStatusExpired (gave up retrying) —
	// despite the naming overlap, the two are unrelated: an event can be
	// DLQStatusExpired for a long time before PurgeExpiredEvents(ctx,
	// maxAge) actually deletes its row.
	PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error)

	Close() error
}

// RateLimiter gates sends per (channel, recipient) AND per channel alone —
// a deliberate two-tier design, not a single global bucket: a per-
// recipient limit alone can't stop a single actor from triggering many
// distinct recipients' sends in a burst (e.g. a scripted forgot-password
// sweep across a whole user list), and a per-channel limit alone can't
// stop one recipient from being spammed by many distinct callers.
type RateLimiter interface {
	// Allow reports whether a request may proceed right now, without
	// blocking. Consumes a token if true.
	Allow(ctx context.Context, channel Channel, recipient string) (bool, error)

	// Wait blocks until a token is available or ctx is done.
	Wait(ctx context.Context, channel Channel, recipient string) error

	// GetStats returns a point-in-time snapshot of this (channel,
	// recipient) bucket's counters.
	GetStats(ctx context.Context, channel Channel, recipient string) (RateLimiterStats, error)
}

// CircuitBreaker wraps calls to an unreliable dependency (an SMTP relay,
// or Meta's Graph API) so persistent failures stop being retried
// immediately and instead fail fast for a cooldown period. One instance
// per dispatcher, not shared/centralized across replicas.
type CircuitBreaker interface {
	// Execute runs fn if the breaker's current state allows it.
	//
	// Returns:
	//   - error: ErrCircuitOpen if the breaker is open and its Timeout
	//     hasn't elapsed; ErrTooManyRequests if half-open and
	//     MaxHalfOpenRequests trial requests are already in flight;
	//     otherwise fn's own return value
	Execute(ctx context.Context, fn func() error) error

	State() CircuitState

	GetStats() CircuitBreakerStats

	// Reset forces the breaker back to CircuitStateClosed, for
	// administrative use.
	Reset()
}

// EmailTemplateEngine renders EmailMessage.Subject/HTMLBody/TextBody. Uses
// html/template (NOT text/template) for HTMLBody specifically, because
// HTMLBody renders into a real HTML document in the recipient's mail
// client. Subject and TextBody use text/template.
type EmailTemplateEngine interface {
	// RegisterTemplate compiles tmpl once, under name, for repeated cheap
	// reuse via Render — the path for templates known ahead of time.
	RegisterTemplate(name string, tmpl EmailTemplate) error

	Render(name string, data map[string]any) (subject, htmlBody, textBody string, err error)

	// RenderInline compiles and renders tmpl on the fly, with no prior
	// RegisterTemplate call — the "pass a custom template at send time"
	// path (EmailMessage.InlineTemplate), for content not known ahead of
	// RegisterTemplate time (e.g. per-tenant custom branding). Same
	// html/template-for-HTMLBody, text/template-for-Subject/TextBody
	// rendering rules as Render. Not cached against any name — a caller
	// sending the identical inline template repeatedly at high volume
	// should register it via RegisterTemplate instead for the
	// compile-once benefit; RenderInline compiles fresh every call.
	// Returns ErrInlineTemplateTooLarge if tmpl's combined source exceeds
	// the configured maximum size.
	RenderInline(tmpl EmailTemplate, data map[string]any) (subject, htmlBody, textBody string, err error)
}

// TemplateValidator confirms a WhatsApp template name+language is
// currently approved on Meta's side AND that the supplied variables'
// count matches the approved template's expected parameter count —
// catching the two most common vendor-rejection causes before a live send
// is attempted. Backed by Meta's own template-listing endpoint, with a
// short internal TTL cache (templates change rarely, so this is not
// re-fetched on every call).
//
// TemplateValidator is advisory, not mandatory: a WhatsApp dispatcher
// consults it only if one is configured, and its cache means "approved as
// of the last check," not a live guarantee — Meta can approve, reject, or
// delete a template between grpop's last cache refresh and a live send.
// The underlying vendor call remains the ultimate source of truth: a
// passed Validate check never causes a dispatcher to skip actually
// calling Meta.
type TemplateValidator interface {
	// Validate returns nil if templateName+languageCode is approved and
	// len(variables) matches its expected parameter count;
	// ErrWhatsAppTemplateNotApproved if not approved;
	// ErrWhatsAppTemplateArityMismatch if approved but the variable count
	// doesn't match.
	Validate(ctx context.Context, templateName, languageCode string, variables map[string]string) error

	// Refresh forces an immediate re-fetch of the approved-template list,
	// bypassing the internal cache — e.g. call this right after
	// registering a new template with Meta, instead of waiting out the
	// TTL.
	Refresh(ctx context.Context) error
}

// MessageEncryptor optionally encrypts a DLQMessage's serialized bytes
// before they're written to durable storage, and decrypts on read back —
// grpop provides only this seam, not a concrete implementation or any key
// management. A consumer wanting encryption at rest supplies their own
// (e.g. AES-GCM keyed from whatever secrets manager already backs their
// other signing keys). Absent one, DLQHandler's storage must be operated
// with the same access-control rigor as a credentials table — see docs.go.
type MessageEncryptor interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Metrics is the optional observability surface every dispatcher/Service
// component accepts. All methods are fire-and-forget from the caller's
// perspective — a nil Metrics is a silent no-op (OrNop-equivalent), and a
// real implementation should never block or error out the operation it's
// instrumenting.
type Metrics interface {
	ObserveSendLatency(channel Channel, duration time.Duration)
	IncSendResult(channel Channel, status SendStatus)
	IncRateLimitRejected(channel Channel)
	IncDLQPublished(channel Channel)
	IncCircuitBreakerStateChange(channel Channel, newState string)

	// IncIdempotencyDedupHit records every SendResult.Duplicate == true
	// outcome, alongside Service's own Warn-level log line, for a
	// caller-bug class (an IdempotencyKey reused across two genuinely
	// different message bodies) that would otherwise manifest only as
	// "the second message silently never went out," with nothing in any
	// log or metric to explain why.
	IncIdempotencyDedupHit(channel Channel)

	// ObserveSMTPResponseCode records the raw SMTP reply code (2xx/4xx/5xx)
	// from every send attempt, even though grpop itself takes no action on
	// the code beyond retry/DLQ classification. A rising rate of 5xx (or a
	// creeping 4xx rate) on an otherwise-succeeding send path is an early
	// signal of sender-reputation damage on the configured relay — visible
	// here well before it would show up as a drop in actual delivered
	// mail, which grpop has no way to observe at all (no vendor-side
	// bounce/complaint feedback).
	ObserveSMTPResponseCode(code int)
}
