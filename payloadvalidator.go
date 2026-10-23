// File: payloadvalidator.go

package grpop

import (
	"fmt"
	"net/mail"
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

// validateWhatsAppMessage checks msg's required fields before a vendor
// call is attempted. Template approval and variable-arity checking against
// what Meta actually has on file is TemplateValidator's job (§4.10), not
// this shape-only check — a template name/language can be well-formed here
// and still be rejected by Meta as unapproved.
func validateWhatsAppMessage(msg WhatsAppMessage) error {
	if msg.To == "" {
		return ErrRecipientRequired
	}
	if msg.TemplateName == "" {
		return ErrWhatsAppTemplateNameRequired
	}
	if msg.LanguageCode == "" {
		return ErrWhatsAppLanguageCodeRequired
	}
	return nil
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
