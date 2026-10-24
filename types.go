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
//
// Header, Body and Buttons fill the template's placeholders, each value
// typed the way Meta's send API takes it; a template with none leaves all
// three empty. An authentication (one-time code) template is
// WhatsAppOTP's Body and Buttons.
type WhatsAppMessage struct {
	To string // required: international form, an optional "+" then 7 to 15 digits
	// with no spaces or dashes ("+15551234567"); anything else is ErrInvalidPhoneNumber
	TemplateName string // required — the Meta-approved template's name; any approved name
	// works, nothing hardcoded/enumerated grpop-side
	LanguageCode string // e.g. "en_US", required, must match the approved template's
	// registered language

	// Header fills the template's header: a text value for a text header
	// with a placeholder, or the image, video, document or location a
	// media or location header shows. nil when the header has nothing to
	// fill.
	Header *WhatsAppParameter

	// Body fills the body's placeholders: in placeholder order ({{1}},
	// {{2}}, ...) for a positional template, or each with its Name
	// ({{first_name}}) in any order for a named one, never a mix of the
	// two. Text, currency and date-and-time values only.
	Body []WhatsAppParameter

	// Buttons fills the buttons that take a value at send time, each by
	// its position among the template's buttons.
	Buttons []WhatsAppButton
}

// WhatsAppParameter is one template value. It holds exactly one of Text,
// Currency, DateTime, Media or Location. Name is set only in a named
// template, and only on a text, currency or date-and-time value.
type WhatsAppParameter struct {
	// Name is the placeholder's name in a named template ("first_name" for
	// {{first_name}}): lower-case letters, digits and underscores. Empty in
	// a positional template.
	Name string

	// Text is a plain value. Meta refuses one with a line break, a tab or
	// five spaces in a row; a header's is at most 60 characters.
	Text     string
	Currency *WhatsAppCurrency
	DateTime *WhatsAppDateTime
	Media    *WhatsAppMedia    // a header's only
	Location *WhatsAppLocation // a header's only
}

// WhatsAppCurrency is an amount of money.
type WhatsAppCurrency struct {
	FallbackValue string // the text shown when WhatsApp can't format the amount ("$100.99")
	Code          string // ISO 4217, upper case ("USD")
	Amount1000    int64  // the amount times 1000 (100990 for 100.99)
}

// WhatsAppDateTime is a date and time, sent as the text WhatsApp shows.
type WhatsAppDateTime struct {
	FallbackValue string
}

// WhatsAppMediaKind is a media header's kind.
type WhatsAppMediaKind string

const (
	WhatsAppMediaImage    WhatsAppMediaKind = "image"
	WhatsAppMediaVideo    WhatsAppMediaKind = "video"
	WhatsAppMediaDocument WhatsAppMediaKind = "document"
)

// WhatsAppMedia is a media header's file: exactly one of ID (from Meta's
// media upload) or Link (a public HTTPS URL Meta fetches).
type WhatsAppMedia struct {
	Kind     WhatsAppMediaKind
	ID       string
	Link     string
	Filename string // the name a document shows; documents only
}

// WhatsAppLocation is a location header's place.
type WhatsAppLocation struct {
	Latitude  float64 // -90 to 90
	Longitude float64 // -180 to 180
	Name      string  // optional
	Address   string  // optional
}

// WhatsAppButtonKind is what a button's Value is.
type WhatsAppButtonKind string

const (
	// WhatsAppButtonURL: Value is the suffix appended to the button's URL.
	WhatsAppButtonURL WhatsAppButtonKind = "url"
	// WhatsAppButtonQuickReply: Value is the payload Meta's webhook returns
	// when the recipient taps it.
	WhatsAppButtonQuickReply WhatsAppButtonKind = "quick_reply"
	// WhatsAppButtonCopyCode: Value is a marketing template's coupon code,
	// at most 20 characters.
	WhatsAppButtonCopyCode WhatsAppButtonKind = "copy_code"
	// WhatsAppButtonOTP: Value is an authentication template's one-time
	// code, at most 15 characters, which must also be the Body's one value
	// (Meta takes the code twice). WhatsAppOTP builds both.
	WhatsAppButtonOTP WhatsAppButtonKind = "otp"
)

// WhatsAppButton fills one button of the template.
type WhatsAppButton struct {
	Index int // the button's position among the template's buttons, 0 to 9
	Kind  WhatsAppButtonKind
	Value string
	// Name is a URL button's placeholder name in a named template; empty
	// otherwise.
	Name string
}

// WhatsAppText is a text value for a positional template.
func WhatsAppText(text string) WhatsAppParameter {
	return WhatsAppParameter{Text: text}
}

// WhatsAppNamedText is a text value for a named template's {{name}}.
func WhatsAppNamedText(name, text string) WhatsAppParameter {
	return WhatsAppParameter{Name: name, Text: text}
}

// WhatsAppImage is an image header fetched from link.
func WhatsAppImage(link string) WhatsAppParameter {
	return WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, Link: link}}
}

// WhatsAppVideo is a video header fetched from link.
func WhatsAppVideo(link string) WhatsAppParameter {
	return WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaVideo, Link: link}}
}

// WhatsAppDocument is a document header fetched from link, shown as
// filename.
func WhatsAppDocument(link, filename string) WhatsAppParameter {
	return WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaDocument, Link: link, Filename: filename}}
}

// WhatsAppOTP is the Body and Buttons of an authentication template's send
// (Meta's copy-code and one-tap templates): the code as the body's one
// value and again as button 0's, the two places Meta requires it.
//
//	msg := grpop.WhatsAppMessage{To: to, TemplateName: "login_code", LanguageCode: "en_US"}
//	msg.Body, msg.Buttons = grpop.WhatsAppOTP(code)
func WhatsAppOTP(code string) ([]WhatsAppParameter, []WhatsAppButton) {
	return []WhatsAppParameter{{Text: code}}, []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonOTP, Value: code}}
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

// WhatsAppTemplateInfo describes one Meta-approved template: what each part
// of it takes at send time, read from Meta's template listing
// (WhatsAppCloudAPIClient.GetApprovedTemplates), so TemplateValidator can
// check a WhatsAppMessage's Header, Body and Buttons against it.
type WhatsAppTemplateInfo struct {
	Name         string
	LanguageCode string

	// Named is a template whose placeholders are names ({{first_name}},
	// Meta's NAMED parameter format); otherwise they are positional
	// ({{1}}, {{2}}, ...).
	Named bool

	// HeaderFormat is Meta's header format: "TEXT", "IMAGE", "VIDEO",
	// "DOCUMENT" or "LOCATION" ("" for no header). A media or location
	// header takes its value at every send; a text one only when it has a
	// placeholder.
	HeaderFormat string

	// HeaderParameters and BodyParameters are the placeholders of a text
	// header and of the body, each once, in order of first appearance:
	// "1", "2", ... or the names.
	HeaderParameters []string
	BodyParameters   []string

	// Buttons are the template's buttons, in order.
	Buttons []WhatsAppTemplateButton
}

// WhatsAppTemplateButton is one button of an approved template.
type WhatsAppTemplateButton struct {
	Index int    // its position among the template's buttons
	Type  string // Meta's button type: "URL", "QUICK_REPLY", "COPY_CODE", "OTP", "PHONE_NUMBER", ...
	// RequiresValue is a button every send must fill: a URL with a
	// placeholder (a one-time code's button is one: Meta lists a created
	// authentication button as a URL), a coupon code, a one-time code. A
	// quick reply's payload is optional.
	RequiresValue bool
}
