// File: payloadvalidator.go

package grpop

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
