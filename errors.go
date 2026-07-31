// File: errors.go

package grpop

import "errors"

// Sentinel errors for use with errors.Is. Backend implementations translate
// their own native errors (pgx.ErrNoRows, redis.Nil, mongo.ErrNoDocuments,
// a vendor SDK's own error type, ...) into these sentinels before wrapping
// with fmt.Errorf("...: %w", ...) — a backend-native error must never leak
// through a grpop interface unwrapped, matching grcache's, graudit's, and
// grnoti's own documented rule.
//
// There is deliberately no IsX(err error) bool helper: callers use
// errors.Is(err, grpop.ErrClosed) directly, consistent with every other
// gourdian repo's sentinel-error convention.
//
// Distinct conditions get distinct sentinels rather than being reused
// across unrelated cases — see docs/plan/grpop-plan.md §2 (of grnoti's own
// plan, the precedent this follows) for the class of bug that reusing one
// sentinel for two different meanings causes. Two examples here:
// ErrNoContentModeSet and ErrMultipleContentModesSet are separate sentinels
// for opposite conditions on EmailMessage's three mutually-exclusive
// content modes, not one generic "invalid content mode" error; likewise
// ErrWhatsAppTemplateNotApproved and ErrWhatsAppTemplateArityMismatch are
// the two distinct reasons TemplateValidator.Validate can fail.
var (
	// ErrClosed indicates a method was called after Close.
	ErrClosed = errors.New("grpop: closed")

	// ErrBackendUnavailable indicates a storage backend or vendor
	// connection could not be reached (connection failure, timeout, etc.).
	ErrBackendUnavailable = errors.New("grpop: backend unavailable")

	// ErrIdempotencyKeyRequired indicates SendOptions.IdempotencyKey was
	// empty. Required, not optional (docs.go, plan §9 item 8): grpop's
	// inline retry is at-least-once, not exactly-once, so the guarantee
	// against a caller-visible double-send comes entirely from a caller-
	// supplied stable key and IdempotencyStore catching the redelivery.
	ErrIdempotencyKeyRequired = errors.New("grpop: idempotency key is required")

	// ErrRecipientRequired indicates EmailMessage.To or WhatsAppMessage.To
	// was empty.
	ErrRecipientRequired = errors.New("grpop: recipient is required")

	// ErrNoContentModeSet indicates an EmailMessage has none of
	// TemplateName, InlineTemplate, or a literal Subject/HTMLBody/TextBody
	// set — there is no content to send.
	ErrNoContentModeSet = errors.New("grpop: email message has no content mode set (TemplateName, InlineTemplate, or literal body)")

	// ErrMultipleContentModesSet indicates an EmailMessage has more than
	// one of TemplateName, InlineTemplate, and a literal Subject/HTMLBody/
	// TextBody set — grpop does not guess which one wins, the caller must
	// pick exactly one.
	ErrMultipleContentModesSet = errors.New("grpop: email message has more than one content mode set (TemplateName, InlineTemplate, literal body are mutually exclusive)")

	// ErrInlineTemplateTooLarge indicates an EmailMessage.InlineTemplate's
	// combined template source exceeds
	// EmailTemplateEngineConfig.MaxInlineTemplateBytes. RenderInline has no
	// caching to amortize an oversized template's compile cost the way
	// Render does for a registered one, so this is enforced up front.
	ErrInlineTemplateTooLarge = errors.New("grpop: inline template exceeds maximum size")

	// ErrWhatsAppTemplateNameRequired indicates WhatsAppMessage.TemplateName
	// was empty.
	ErrWhatsAppTemplateNameRequired = errors.New("grpop: whatsapp template name is required")

	// ErrWhatsAppLanguageCodeRequired indicates WhatsAppMessage.LanguageCode
	// was empty.
	ErrWhatsAppLanguageCodeRequired = errors.New("grpop: whatsapp language code is required")

	// ErrWhatsAppTemplateNotApproved indicates TemplateValidator.Validate
	// found no currently-approved Meta template matching the requested
	// name+language.
	ErrWhatsAppTemplateNotApproved = errors.New("grpop: whatsapp template is not approved")

	// ErrWhatsAppTemplateArityMismatch indicates TemplateValidator.Validate
	// found an approved template matching name+language, but
	// len(TemplateVariables) does not match its expected parameter count.
	ErrWhatsAppTemplateArityMismatch = errors.New("grpop: whatsapp template variable count does not match the approved template's expected parameter count")

	// ErrRateLimited indicates RateLimiter.Allow reported no token
	// available (per-channel or per-recipient bucket exhausted) for a
	// Send call that did not opt into SendOptions.SkipRateLimit.
	ErrRateLimited = errors.New("grpop: rate limited")

	// ErrDLQEventNotFound indicates a DLQHandler lookup found no DLQEvent
	// for the requested send ID.
	ErrDLQEventNotFound = errors.New("grpop: dead-letter event not found")

	// ErrDLQEventNotClaimed indicates MarkRetried was called for an event
	// that is not currently in the "retrying" (claimed) state — either it
	// was never claimed via ClaimRetryableEvents, already resolved/
	// exhausted/expired, or claimed by a concurrent caller.
	ErrDLQEventNotClaimed = errors.New("grpop: dead-letter event is not in a claimed (retrying) state")
)
