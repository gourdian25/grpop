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

// RateLimiter reports whether a send identified by key may go now. It is
// grsentry's pkg/grlimiter.Limiter's method set, exactly, so a caller passes
// its own limiter in (grsentry passes grlimiter's local or Redis one) with no
// adapter and no import in either direction; grpop ships no implementation
// of its own. A sender asks it once per send attempt and never waits: a
// refusal is ErrRateLimited, a temporary failure for the caller's own retry
// to space out.
//
// A sender takes two, both optional: RateLimiter, keyed by the channel alone
// ("email", "whatsapp"), bounds the whole channel, and RecipientRateLimiter,
// keyed by the channel and a SHA-256 of the recipient (never the address or
// number itself), stops one recipient being flooded. Either alone misses
// what the other catches: a sweep across many recipients, or many callers
// aiming at one.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (bool, error)
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
// currently approved on Meta's side AND that the message's Header, Body
// and Buttons fill what the approved template takes —
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
// calling Meta, a failed listing lets a send through rather than block it,
// and a refusal is checked once more against a new listing first (at most
// one listing per 30 seconds outside the TTL), so a template approved or
// edited since the last listing is refused only for that long.
type TemplateValidator interface {
	// Validate returns nil if msg's TemplateName+LanguageCode is approved
	// and its Header, Body and Buttons fill what the template takes, or if
	// there is no template list to check against;
	// ErrWhatsAppTemplateNotApproved if not approved;
	// ErrWhatsAppTemplateArityMismatch, saying what differs, if approved
	// but msg doesn't fit it.
	Validate(ctx context.Context, msg WhatsAppMessage) error

	// Refresh forces an immediate re-fetch of the approved-template list,
	// bypassing the internal cache — e.g. call this right after
	// registering a new template with Meta, instead of waiting out the
	// TTL.
	Refresh(ctx context.Context) error
}

// Metrics is the optional observability surface every sender accepts. All
// methods are fire-and-forget from the caller's perspective — a nil Metrics is a silent no-op (OrNop-equivalent), and a
// real implementation should never block or error out the operation it's
// instrumenting.
type Metrics interface {
	ObserveSendLatency(channel Channel, duration time.Duration)
	IncSendResult(channel Channel, status SendStatus)
	IncRateLimitRejected(channel Channel)
	IncCircuitBreakerStateChange(channel Channel, newState string)

	// ObserveSMTPResponseCode records the raw SMTP reply code (2xx/4xx/5xx)
	// from every send attempt, even though grpop itself takes no action on
	// the code beyond classifying the failure. A rising rate of 5xx (or a
	// creeping 4xx rate) on an otherwise-succeeding send path is an early
	// signal of sender-reputation damage on the configured relay — visible
	// here well before it would show up as a drop in actual delivered
	// mail, which grpop has no way to observe at all (no vendor-side
	// bounce/complaint feedback).
	ObserveSMTPResponseCode(code int)
}
