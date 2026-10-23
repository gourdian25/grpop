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

	// IdempotencyKey is optional: the same value on every attempt at one
	// message (a grsentry outbox row's id, say), so a sender that can tell
	// the vendor makes a retry safe. The SMTP sender derives the
	// Message-ID from it (its SHA-256, so the key itself never appears in
	// the message), which receiving systems may use to collapse a
	// duplicate; it can do no more, since SMTP has no idempotency of its
	// own. Two different messages must never share one.
	IdempotencyKey string
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

// SendResult is what every sender's Send returns: what the vendor said,
// for the one attempt it made.
type SendResult struct {
	Channel           Channel
	ProviderMessageID string // vendor's own message/tracking ID (Meta's wamid, or the
	// generated SMTP Message-ID); empty if Status is SendStatusFailed
	Status SendStatus
	SentAt time.Time
	Raw    map[string]string // optional vendor-specific diagnostic fields — logging only
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
