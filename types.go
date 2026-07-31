// File: types.go

package grpop

import "time"

// Channel identifies which delivery channel a message/send/rate-limit
// bucket applies to.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelWhatsApp Channel = "whatsapp"
)

// EmailTemplate is the raw template content, usable two ways: registered
// once by name (EmailTemplateEngine.RegisterTemplate, compiled once, cheap
// to reuse) or passed inline per-send (EmailMessage.InlineTemplate,
// compiled fresh each call) — same fields, same rendering rules either way
// (see EmailTemplateEngine).
//
// SECURITY NOTE, InlineTemplate specifically: the plausible real source for
// an inline template is per-tenant custom branding — i.e. content that may
// trace back to a tenant admin's own input, not a trusted grpop operator.
// html/template auto-escapes DATA values safely, but the template
// STRUCTURE itself is not sandboxed against referencing EmailMessage's
// TemplateData fields the caller didn't intend to expose (e.g. an internal
// field accidentally present in the map) — InlineTemplate content must
// come from a trusted admin-configured path, never raw end-user input, and
// EmailTemplateEngine.RenderInline enforces a size cap (returns
// ErrInlineTemplateTooLarge) so an oversized/pathological template can't
// cost unbounded parse/render CPU per call — a real cost specifically
// because RenderInline, unlike Render, is never cached.
type EmailTemplate struct {
	SubjectTemplate  string // text/template
	HTMLBodyTemplate string // html/template — HTMLBody renders into a real HTML document in the
	// recipient's mail client, so it is escaped differently from Subject/TextBody
	TextBodyTemplate string // text/template, optional
}

// EmailMessage is an email with exactly one of three mutually-exclusive
// content modes set (see ErrNoContentModeSet, ErrMultipleContentModesSet):
//
//  1. TemplateName (+ TemplateData) — renders via a template already
//     registered with EmailTemplateEngine.RegisterTemplate.
//  2. InlineTemplate (+ TemplateData) — a caller-supplied custom template,
//     rendered on the fly via EmailTemplateEngine.RenderInline without
//     requiring prior registration. This is the "pass a custom template at
//     send time" path — for content not known ahead of RegisterTemplate
//     time, e.g. per-tenant custom branding on an invite email.
//  3. Literal Subject/HTMLBody/TextBody — no templating at all, used
//     verbatim.
type EmailMessage struct {
	To             string // single recipient address, required
	From           string // optional; empty uses the dispatcher's configured default sender
	ReplyTo        string // optional
	Subject        string
	HTMLBody       string
	TextBody       string // optional plain-text alternative part
	TemplateName   string
	InlineTemplate *EmailTemplate
	TemplateData   map[string]any
}

// WhatsAppMessage is structurally different from EmailMessage on purpose:
// WhatsApp requires a pre-approved message template for anything sent
// outside a user-initiated 24-hour session window, which every one of
// grpop's intended use cases falls into (system-initiated sends, never a
// reply to a live user session). There is deliberately no freeform Body
// field.
//
// "Custom template" for WhatsApp means something different from email's
// InlineTemplate: TemplateName is a free string, not restricted to a
// fixed/hardcoded set, so any template a caller has had approved by Meta
// (however new, however tenant/feature-specific) can be sent by name with
// no grpop-side registration step at all. What's NOT possible, as a hard
// vendor constraint rather than a grpop design gap: there is no ad-hoc/
// unregistered-with-Meta WhatsApp template — grpop cannot render or invent
// WhatsApp template content itself the way EmailTemplateEngine.RenderInline
// can for email, because Meta's own approval process is the source of
// truth for what content is allowed to go out. TemplateValidator only
// checks against whatever is already approved; it never submits new ones.
type WhatsAppMessage struct {
	To           string // E.164 phone number, required
	TemplateName string // required — the Meta-approved template's name; any approved name
	// works, nothing hardcoded/enumerated grpop-side
	TemplateVariables map[string]string // stringified positional keys ("1","2","3", in order),
	// per Meta Cloud API's template-parameter convention
	LanguageCode string // e.g. "en_US", required, must match the approved template's
	// registered language
}

// SendStatus is the result of a vendor call, not a delivery-receipt
// status. See docs.go's "precise, non-aspirational claims" section.
type SendStatus string

const (
	SendStatusSent   SendStatus = "sent"   // the vendor accepted the request
	SendStatusFailed SendStatus = "failed" // the vendor rejected the request, or the call itself errored
)

// SendResult is the delivery-status handle every Service.SendX call
// returns.
type SendResult struct {
	Channel           Channel
	ProviderMessageID string // vendor's own message/tracking ID (Meta's wamid, or the
	// generated SMTP Message-ID); empty if Status is SendStatusFailed
	Status SendStatus
	SentAt time.Time
	Raw    map[string]string // optional vendor-specific diagnostic fields — logging only

	// Duplicate is true if this call short-circuited on
	// IdempotencyStore.IsProcessed (the same IdempotencyKey was already
	// marked processed) rather than performing a new vendor call. A dedup
	// hit is not silent: Service also logs a Warn and increments
	// Metrics.IncIdempotencyDedupHit on every hit regardless of whether the
	// caller inspects this field — see Service's doc comment.
	Duplicate bool
}

// SendOptions carries per-send cross-cutting behavior.
type SendOptions struct {
	// IdempotencyKey is REQUIRED — Service.SendEmail/SendWhatsApp return
	// ErrIdempotencyKeyRequired if it's empty. grpop's inline retry-on-
	// transient-failure is at-least-once, not exactly-once (docs.go): a
	// network error after the vendor already accepted the message is
	// indistinguishable from one before, so a retry can double-send. The
	// guarantee against a caller-visible double-send comes entirely from
	// the caller supplying a stable key and IdempotencyStore catching the
	// redelivery.
	IdempotencyKey string
	IdempotencyTTL time.Duration // 0 uses the configured default
	SkipRateLimit  bool          // escape hatch, e.g. an admin-triggered manual resend

	// RetryExpiresAt is the hard wall-clock deadline after which grpop
	// stops retrying a failed send entirely, regardless of remaining
	// retry-count budget. Zero value uses the configured default max retry
	// age, measured from the first failure, not from this call. Callers
	// who know their own message's real expiry (a password-reset link's
	// TTL, an invite token's expiry) should set this explicitly rather
	// than relying on the library default.
	RetryExpiresAt time.Time
}

// DLQStatus is a DLQEvent's lifecycle state.
type DLQStatus string

const (
	// DLQStatusPending is newly-recorded or awaiting its next retry.
	DLQStatusPending DLQStatus = "pending"
	// DLQStatusRetrying means a worker currently holds an atomic claim on
	// this event (see DLQHandler.ClaimRetryableEvents) and is attempting
	// delivery — not a status any caller sets directly.
	DLQStatusRetrying DLQStatus = "retrying"
	// DLQStatusResolved means a retry eventually succeeded.
	DLQStatusResolved DLQStatus = "resolved"
	// DLQStatusExhausted means RetryCount reached MaxRetries before
	// ExpiresAt passed.
	DLQStatusExhausted DLQStatus = "exhausted"
	// DLQStatusExpired means ExpiresAt passed before a successful retry,
	// independent of remaining RetryCount budget. Unrelated to
	// DLQHandler.PurgeExpiredEvents' own, different sense of "expired"
	// (old enough to delete) — see that method's doc comment.
	DLQStatusExpired DLQStatus = "expired"
)

// DLQMessage is a channel-tagged envelope — exactly one of Email/WhatsApp
// is non-nil, matching Channel.
type DLQMessage struct {
	Channel  Channel
	Email    *EmailMessage
	WhatsApp *WhatsAppMessage

	// ExpiresAt is the hard wall-clock deadline after which this event
	// stops being retried at all, regardless of remaining RetryCount
	// budget — set by Service from SendOptions.RetryExpiresAt or the
	// configured default max retry age before PublishToDLQ is called.
	ExpiresAt time.Time
}

// DLQRetryAttempt records the outcome of one retry attempt for a DLQEvent.
type DLQRetryAttempt struct {
	AttemptNumber int
	AttemptedAt   time.Time
	Success       bool
	ErrorMessage  string
}

// DLQEvent is the durable record of one failed send, awaiting retry or
// already Resolved/Exhausted/Expired.
type DLQEvent struct {
	SendID         string
	MessageData    DLQMessage // carries Channel and ExpiresAt
	FailureReason  string
	RetryCount     int
	MaxRetries     int
	FirstFailureAt time.Time
	LastAttemptAt  time.Time
	NextRetryAt    time.Time
	Status         DLQStatus
	AttemptHistory []DLQRetryAttempt // capped at a configured maximum entry count, oldest dropped first
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// RateLimiterStats is a point-in-time snapshot of one (channel, recipient)
// bucket's counters.
type RateLimiterStats struct {
	RequestsPerSecond int
	BurstSize         int
	AllowedCount      int64
	BlockedCount      int64
	WaitCount         int64
	LastAllowedAt     time.Time
}

// CircuitState is a CircuitBreaker's current state.
type CircuitState string

const (
	// CircuitStateClosed is the normal state: requests pass through, and
	// consecutive failures are counted toward MaxFailures.
	CircuitStateClosed CircuitState = "closed"
	// CircuitStateOpen rejects every request immediately (without
	// attempting them) until Timeout elapses, then transitions to
	// CircuitStateHalfOpen.
	CircuitStateOpen CircuitState = "open"
	// CircuitStateHalfOpen allows up to MaxHalfOpenRequests trial requests
	// through to test whether the failing dependency has recovered: the
	// first trial failure trips it straight back to CircuitStateOpen, the
	// first trial success closes it.
	CircuitStateHalfOpen CircuitState = "half_open"
)

// CircuitBreakerConfig configures a CircuitBreaker.
type CircuitBreakerConfig struct {
	// MaxFailures is the number of consecutive failures that trips the
	// breaker from closed to open.
	MaxFailures int
	// Timeout is how long the breaker stays open before allowing a trial
	// request through (transitioning to half-open).
	Timeout time.Duration
	// ResetTimeout is how long a closed breaker must go without a failure
	// before its consecutive-failure counter resets to zero.
	ResetTimeout time.Duration
	// MaxHalfOpenRequests bounds concurrent trial requests while
	// half-open. Defaults to 1 if <= 0.
	MaxHalfOpenRequests int
	// Logger receives optional diagnostic messages for state transitions
	// (open/half-open/close). A nil Logger disables logging.
	Logger Logger
}

// CircuitBreakerStats is a point-in-time snapshot of a CircuitBreaker's
// counters.
type CircuitBreakerStats struct {
	State                CircuitState
	ConsecutiveFailures  int
	TotalSuccesses       int64
	TotalFailures        int64
	TotalRejections      int64
	LastFailureTime      time.Time
	LastStateChange      time.Time
	OpenedAt             time.Time
	TimeUntilNextAttempt time.Duration
}

// WhatsAppTemplateInfo describes one Meta-approved template, as returned
// by a WhatsApp dispatcher's vendor-narrow client.
type WhatsAppTemplateInfo struct {
	Name           string
	LanguageCode   string
	ParameterCount int // number of positional variables ("1","2","3", ...) the approved
	// template body actually expects, so TemplateValidator.Validate can
	// catch an arity mismatch, not just approval status
}
