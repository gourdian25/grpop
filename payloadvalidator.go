// File: payloadvalidator.go

package grpop

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"
)

// validateEmailMessage checks msg's required fields and content-mode
// exclusivity before a vendor call is attempted. Unlike grnoti's
// PayloadValidator (an FCM-specific byte-size check against a fixed
// vendor-documented limit), grpop's email/WhatsApp vendors have no single
// well-known payload-size constant to check against — SMTP message-size
// limits vary per relay, and WhatsApp template arity is TemplateValidator's
// job, not this one's. What both channels DO need checked up front is
// shape: required fields, and — for email specifically — exactly one of
// EmailMessage's three mutually-exclusive content modes being set.
func validateEmailMessage(msg EmailMessage) error {
	if msg.To == "" {
		return ErrRecipientRequired
	}

	modesSet := 0
	if msg.TemplateName != "" {
		modesSet++
	}
	if msg.InlineTemplate != nil {
		modesSet++
	}
	if msg.Subject != "" || msg.HTMLBody != "" || msg.TextBody != "" {
		modesSet++
	}
	switch {
	case modesSet == 0:
		return ErrNoContentModeSet
	case modesSet > 1:
		return ErrMultipleContentModesSet
	}

	return nil
}

// whatsAppPhoneNumberPattern is a phone number in international form, the
// way Meta's "to" field takes it: an optional "+", a country code (never
// starting with 0, which would be a national number missing its country),
// then the rest, 7 to 15 digits in all. E.164 allows at most 15; Niue's and
// Tokelau's shortest numbers make 7. Spaces, dashes and brackets are the
// caller's to strip.
var whatsAppPhoneNumberPattern = regexp.MustCompile(`^\+?[1-9][0-9]{6,14}$`)

// whatsAppParameterNamePattern is a named placeholder's name: Meta asks
// for lower-case letters and underscores; digits pass too.
var whatsAppParameterNamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// Meta's limits on a template send (its template components and
// authentication and coupon template pages, read 2026-10-24).
const (
	whatsAppHeaderTextMaxChars = 60
	whatsAppOTPMaxChars        = 15
	whatsAppCouponCodeMaxChars = 20
	whatsAppMaxButtonIndex     = 9 // at most 10 buttons
)

// validateWhatsAppMessage checks msg before a vendor call is attempted: the
// required fields, the recipient's phone number, and every Header, Body and
// Buttons value against what Meta refuses at send time. Template approval
// and arity against what Meta actually has on file is TemplateValidator's
// job, not this one: a message can be well formed here and still refer to
// a template Meta doesn't have.
//
// Every value holds exactly one thing; a text value (and a currency's or a
// date's fallback text, and a button's value) has no line break, tab or
// five spaces in a row (Meta's error 132018); a header's text is at most 60
// characters; media and location are a header's only, and media names
// exactly one of an ID or a Link; named and positional values aren't
// mixed, and no name repeats; a button's index is 0 to 9 and used once; a
// coupon code is at most 20 characters; a one-time code at most 15, and
// also the body's one value.
func validateWhatsAppMessage(msg WhatsAppMessage) error {
	if msg.To == "" {
		return ErrRecipientRequired
	}
	if !whatsAppPhoneNumberPattern.MatchString(msg.To) {
		return ErrInvalidPhoneNumber
	}
	if msg.TemplateName == "" {
		return ErrWhatsAppTemplateNameRequired
	}
	if msg.LanguageCode == "" {
		return ErrWhatsAppLanguageCodeRequired
	}

	// names counts the named values and the names they use; positional
	// counts the rest. A media or location header has no placeholder name
	// and counts as neither.
	names := make(map[string]bool)
	positional := 0
	countName := func(field string, p WhatsAppParameter) error {
		if p.Name == "" {
			positional++
			return nil
		}
		if !whatsAppParameterNamePattern.MatchString(p.Name) {
			return whatsAppParameterInvalid(field, "has a Name other than lower-case letters, digits and underscores")
		}
		if names[p.Name] {
			return whatsAppParameterInvalid(field, "has a Name another value already uses")
		}
		names[p.Name] = true
		return nil
	}

	if h := msg.Header; h != nil {
		if err := checkWhatsAppValue("Header", *h); err != nil {
			return err
		}
		if utf8.RuneCountInString(h.Text) > whatsAppHeaderTextMaxChars {
			return whatsAppParameterInvalid("Header", "is longer than 60 characters")
		}
		if h.Media != nil || h.Location != nil {
			if h.Name != "" {
				return whatsAppParameterInvalid("Header", "names a media or location value, which has no placeholder")
			}
		} else if err := countName("Header", *h); err != nil {
			return err
		}
	}
	for i, p := range msg.Body {
		field := fmt.Sprintf("Body[%d]", i)
		if err := checkWhatsAppValue(field, p); err != nil {
			return err
		}
		if p.Media != nil || p.Location != nil {
			return whatsAppParameterInvalid(field, "is a media or location value, which only a header takes")
		}
		if err := countName(field, p); err != nil {
			return err
		}
	}
	if len(names) > 0 && positional > 0 {
		return whatsAppParameterInvalid("Header and Body", "mix named and positional values")
	}

	indexes := make(map[int]bool)
	for i, b := range msg.Buttons {
		field := fmt.Sprintf("Buttons[%d]", i)
		if b.Index < 0 || b.Index > whatsAppMaxButtonIndex {
			return whatsAppParameterInvalid(field, "has an Index outside 0 to 9")
		}
		if indexes[b.Index] {
			return whatsAppParameterInvalid(field, "has an Index another button already uses")
		}
		indexes[b.Index] = true
		if b.Value == "" {
			return whatsAppParameterInvalid(field, "has no Value")
		}
		if err := checkWhatsAppText(field, b.Value); err != nil {
			return err
		}
		if b.Name != "" && (b.Kind != WhatsAppButtonURL || !whatsAppParameterNamePattern.MatchString(b.Name)) {
			return whatsAppParameterInvalid(field, "has a Name, which only a URL button takes, of lower-case letters, digits and underscores")
		}
		switch b.Kind {
		case WhatsAppButtonURL, WhatsAppButtonQuickReply:
		case WhatsAppButtonCopyCode:
			if utf8.RuneCountInString(b.Value) > whatsAppCouponCodeMaxChars {
				return whatsAppParameterInvalid(field, "has a coupon code longer than 20 characters")
			}
		case WhatsAppButtonOTP:
			if utf8.RuneCountInString(b.Value) > whatsAppOTPMaxChars {
				return whatsAppParameterInvalid(field, "has a one-time code longer than 15 characters")
			}
			if len(msg.Body) != 1 || msg.Body[0].Text != b.Value {
				return whatsAppParameterInvalid(field, "has a one-time code that isn't also the Body's one value")
			}
		default:
			return whatsAppParameterInvalid(field, "has an unknown Kind")
		}
	}
	return nil
}

// checkWhatsAppValue checks that p holds exactly one value and that the
// value is one Meta takes.
func checkWhatsAppValue(field string, p WhatsAppParameter) error {
	set := 0
	for _, isSet := range []bool{p.Text != "", p.Currency != nil, p.DateTime != nil, p.Media != nil, p.Location != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return whatsAppParameterInvalid(field, "doesn't hold exactly one of Text, Currency, DateTime, Media or Location")
	}

	switch {
	case p.Currency != nil:
		if p.Currency.FallbackValue == "" {
			return whatsAppParameterInvalid(field, "has a Currency with no FallbackValue")
		}
		if !isCurrencyCode(p.Currency.Code) {
			return whatsAppParameterInvalid(field, "has a Currency whose Code isn't three upper-case letters")
		}
		return checkWhatsAppText(field, p.Currency.FallbackValue)
	case p.DateTime != nil:
		if p.DateTime.FallbackValue == "" {
			return whatsAppParameterInvalid(field, "has a DateTime with no FallbackValue")
		}
		return checkWhatsAppText(field, p.DateTime.FallbackValue)
	case p.Media != nil:
		switch p.Media.Kind {
		case WhatsAppMediaImage, WhatsAppMediaVideo, WhatsAppMediaDocument:
		default:
			return whatsAppParameterInvalid(field, "has Media of an unknown Kind")
		}
		if (p.Media.ID == "") == (p.Media.Link == "") {
			return whatsAppParameterInvalid(field, "has Media that doesn't name exactly one of an ID or a Link")
		}
		if p.Media.Filename != "" && p.Media.Kind != WhatsAppMediaDocument {
			return whatsAppParameterInvalid(field, "has a Filename on Media that isn't a document")
		}
		return nil
	case p.Location != nil:
		if !(p.Location.Latitude >= -90 && p.Location.Latitude <= 90) || !(p.Location.Longitude >= -180 && p.Location.Longitude <= 180) {
			return whatsAppParameterInvalid(field, "has a Location outside -90 to 90 latitude or -180 to 180 longitude")
		}
		return nil
	default:
		return checkWhatsAppText(field, p.Text)
	}
}

// checkWhatsAppText refuses what Meta refuses in a template value: a line
// break, a tab, or five spaces in a row (its error 132018; four pass).
func checkWhatsAppText(field, text string) error {
	if strings.ContainsAny(text, "\r\n\t") || strings.Contains(text, "     ") {
		return whatsAppParameterInvalid(field, "has a line break, a tab or five spaces in a row")
	}
	return nil
}

// isCurrencyCode reports whether code is three upper-case ASCII letters,
// an ISO 4217 code's shape.
func isCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, c := range code {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// whatsAppParameterInvalid is ErrWhatsAppParameterInvalid naming field and
// what is wrong with it, never the value.
func whatsAppParameterInvalid(field, problem string) error {
	return fmt.Errorf("%w: %s %s", ErrWhatsAppParameterInvalid, field, problem)
}

// parseEmailAddress parses one email address field (From, To or Reply-To)
// with net/mail, so a display name is allowed ("Acme <no-reply@acme.test>")
// and anything net/mail refuses is ErrInvalidAddress, as is a CR or LF
// anywhere in it: written into a header unchecked, a line break would let
// the value add a header line of its own (a Bcc). The error names field,
// never value.
func parseEmailAddress(field, value string) (*mail.Address, error) {
	if strings.ContainsAny(value, "\r\n") {
		return nil, fmt.Errorf("%w: %s contains a line break", ErrInvalidAddress, field)
	}
	addr, err := mail.ParseAddress(value)
	if err != nil {
		// Not net/mail's own message: some of them quote the input back
		// ("expected single address, got ...").
		return nil, fmt.Errorf("%w: %s is not a single email address", ErrInvalidAddress, field)
	}
	return addr, nil
}

// maskEmail shortens address for a log line: its first character, "***",
// and its domain ("a***@example.com"), which is enough to tell sends apart
// without writing anyone's address into a shared log. Anything without a
// local part and a domain is "***".
func maskEmail(address string) string {
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return "***"
	}
	_, size := utf8.DecodeRuneInString(address)
	return address[:size] + "***" + address[at:]
}

// redactAddresses replaces every address in text (an SMTP reply often echoes
// the recipient) with its maskEmail form, matching the address as given and
// lower-cased.
func redactAddresses(text string, addresses ...string) string {
	for _, address := range addresses {
		if address == "" {
			continue
		}
		masked := maskEmail(address)
		text = strings.ReplaceAll(text, address, masked)
		text = strings.ReplaceAll(text, strings.ToLower(address), masked)
	}
	return text
}

// maskedRecipient is an email field as a log line shows it: the parsed
// address through maskEmail, or "***" for one that doesn't parse.
func maskedRecipient(value string) string {
	addr, err := parseEmailAddress("To", value)
	if err != nil {
		return "***"
	}
	return maskEmail(addr.Address)
}
