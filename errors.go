// File: errors.go

package grpop

import "errors"

// Sentinel errors for use with errors.Is.
//
// There is deliberately no IsX(err error) bool helper: callers use
// errors.Is(err, grpop.ErrRateLimited) directly, consistent with every other
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
//
// A failure no retry can fix also matches ErrPermanent: every validation
// sentinel below (they are built with permanent, so
// errors.Is(ErrRecipientRequired, ErrPermanent) holds), and a vendor's own
// refusal a sender classifies as final. ErrRateLimited, ErrCircuitOpen,
// ErrTooManyRequests, a timeout, a cancelled context and a network error
// don't: those are worth another attempt later.
var (
	// ErrPermanent marks a send failure that will fail the same way on every
	// attempt: a malformed or unrenderable message, an address that can't
	// be parsed, an SMTP 5xx reply (a mailbox that doesn't exist, bad
	// credentials), a Meta refusal of the request, the account or the
	// template. A caller's retry stops on errors.Is(err, ErrPermanent);
	// everything else is temporary.
	ErrPermanent = errors.New("grpop: permanent failure")

	// ErrRecipientRequired indicates EmailMessage.To or WhatsAppMessage.To
	// was empty.
	ErrRecipientRequired = permanent(errors.New("grpop: recipient is required"))

	// ErrNoContentModeSet indicates an EmailMessage has none of
	// TemplateName, InlineTemplate, or a literal Subject/HTMLBody/TextBody
	// set — there is no content to send.
	ErrNoContentModeSet = permanent(errors.New("grpop: email message has no content mode set (TemplateName, InlineTemplate, or literal body)"))

	// ErrMultipleContentModesSet indicates an EmailMessage has more than
	// one of TemplateName, InlineTemplate, and a literal Subject/HTMLBody/
	// TextBody set — grpop does not guess which one wins, the caller must
	// pick exactly one.
	ErrMultipleContentModesSet = permanent(errors.New("grpop: email message has more than one content mode set (TemplateName, InlineTemplate, literal body are mutually exclusive)"))

	// ErrEmailTemplateNotFound indicates EmailTemplateEngine.Render was
	// called with a name no RegisterTemplate call has registered.
	ErrEmailTemplateNotFound = permanent(errors.New("grpop: email template not found"))

	// ErrInlineTemplateTooLarge indicates an EmailMessage.InlineTemplate's
	// combined template source exceeds
	// EmailTemplateEngineConfig.MaxInlineTemplateBytes. RenderInline has no
	// caching to amortize an oversized template's compile cost the way
	// Render does for a registered one, so this is enforced up front.
	ErrInlineTemplateTooLarge = permanent(errors.New("grpop: inline template exceeds maximum size"))

	// ErrWhatsAppTemplateNameRequired indicates WhatsAppMessage.TemplateName
	// was empty.
	ErrWhatsAppTemplateNameRequired = permanent(errors.New("grpop: whatsapp template name is required"))

	// ErrWhatsAppLanguageCodeRequired indicates WhatsAppMessage.LanguageCode
	// was empty.
	ErrWhatsAppLanguageCodeRequired = permanent(errors.New("grpop: whatsapp language code is required"))

	// ErrWhatsAppTemplateNotApproved indicates TemplateValidator.Validate
	// found no currently-approved Meta template matching the requested
	// name+language.
	ErrWhatsAppTemplateNotApproved = permanent(errors.New("grpop: whatsapp template is not approved"))

	// ErrWhatsAppTemplateArityMismatch indicates TemplateValidator.Validate
	// found an approved template matching name+language, but the message's
	// Header, Body or Buttons don't fill what it takes (a count, a name, a
	// kind). The error says what differs, never a value.
	ErrWhatsAppTemplateArityMismatch = permanent(errors.New("grpop: whatsapp message does not fit the approved template's parameters"))

	// ErrInvalidPhoneNumber indicates a WhatsAppMessage.To that isn't a
	// phone number in international form: an optional "+", a country code
	// that doesn't start with 0, 7 to 15 digits in all, nothing else. The
	// error never repeats the number.
	ErrInvalidPhoneNumber = permanent(errors.New("grpop: invalid whatsapp phone number"))

	// ErrWhatsAppParameterInvalid indicates a WhatsAppMessage Header, Body
	// or Buttons value Meta would refuse (validateWhatsAppMessage has the
	// list). The error names the field ("Body[1]"), never the value.
	ErrWhatsAppParameterInvalid = permanent(errors.New("grpop: invalid whatsapp template parameter"))

	// ErrRateLimited indicates a sender's RateLimiter or
	// RecipientRateLimiter refused the send. Temporary: the send was not
	// attempted, and the caller's own retry tries again later.
	ErrRateLimited = errors.New("grpop: rate limited")

	// ErrEmailFromRequired indicates both EmailMessage.From and the
	// dispatcher's configured DefaultFrom were empty — SMTP requires a MAIL
	// FROM address, and grpop does not invent one.
	ErrEmailFromRequired = permanent(errors.New("grpop: email From address is required (set EmailMessage.From or SMTPDispatcherDeps.DefaultFrom)"))

	// ErrEmailTemplateEngineRequired indicates an EmailMessage used
	// TemplateName or InlineTemplate but the dispatcher has no
	// EmailTemplateEngine configured to render it.
	ErrEmailTemplateEngineRequired = permanent(errors.New("grpop: email message uses TemplateName/InlineTemplate but no EmailTemplateEngine is configured"))

	// ErrInvalidAddress indicates an email From, To or Reply-To that
	// net/mail can't parse, or that carries a CR or LF (which would let it
	// write a header line of its own, such as a Bcc). The error names the
	// field, never the value.
	ErrInvalidAddress = permanent(errors.New("grpop: invalid email address"))
)

// permanentError is an error that also matches ErrPermanent, keeping err's
// own message and chain: every validation sentinel above is one, and so is a
// vendor's final refusal once a sender has classified it.
type permanentError struct{ err error }

// permanent marks err as one no retry can fix (see ErrPermanent). A nil err
// stays nil.
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

func (e *permanentError) Error() string        { return e.err.Error() }
func (e *permanentError) Unwrap() error        { return e.err }
func (e *permanentError) Is(target error) bool { return target == ErrPermanent }
