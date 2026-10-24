// File: dispatcher.email.go

package grpop

import (
	"net/mail"
)

// preparedEmail is an EmailMessage made ready for a vendor: checked,
// rendered, and its addresses parsed. Every email sender builds its request
// from one (prepareEmail), so the SMTP sender and any other send exactly the
// same message for the same input and refuse exactly the same ones.
type preparedEmail struct {
	from    *mail.Address
	to      *mail.Address
	replyTo *mail.Address // nil when the message has none

	subject  string
	htmlBody string
	textBody string

	// idempotencyKey is EmailMessage.IdempotencyKey as given ("" for none).
	idempotencyKey string
}

// prepareEmail checks msg (validateEmailMessage), renders its one content
// mode through engine, applies defaultFrom when msg.From is empty, and
// parses From, To and Reply-To (parseEmailAddress), in that order. Every
// error it returns is permanent: the same message fails the same way every
// time.
func prepareEmail(msg EmailMessage, defaultFrom string, engine EmailTemplateEngine) (preparedEmail, error) {
	if err := validateEmailMessage(msg); err != nil {
		return preparedEmail{}, err
	}

	subject, htmlBody, textBody, err := renderEmailContent(msg, engine)
	if err != nil {
		return preparedEmail{}, err
	}

	from := msg.From
	if from == "" {
		from = defaultFrom
	}
	if from == "" {
		return preparedEmail{}, ErrEmailFromRequired
	}
	fromAddr, err := parseEmailAddress("From", from)
	if err != nil {
		return preparedEmail{}, err
	}
	toAddr, err := parseEmailAddress("To", msg.To)
	if err != nil {
		return preparedEmail{}, err
	}
	var replyToAddr *mail.Address
	if msg.ReplyTo != "" {
		if replyToAddr, err = parseEmailAddress("ReplyTo", msg.ReplyTo); err != nil {
			return preparedEmail{}, err
		}
	}

	return preparedEmail{
		from:           fromAddr,
		to:             toAddr,
		replyTo:        replyToAddr,
		subject:        subject,
		htmlBody:       htmlBody,
		textBody:       textBody,
		idempotencyKey: msg.IdempotencyKey,
	}, nil
}

// renderEmailContent turns msg's active content mode (already checked as
// exactly one by validateEmailMessage) into a literal subject, HTML body and
// text body: a registered template, an inline one, or the literal fields as
// they are.
func renderEmailContent(msg EmailMessage, engine EmailTemplateEngine) (subject, htmlBody, textBody string, err error) {
	switch {
	case msg.TemplateName != "":
		if engine == nil {
			return "", "", "", ErrEmailTemplateEngineRequired
		}
		return engine.Render(msg.TemplateName, msg.TemplateData)
	case msg.InlineTemplate != nil:
		if engine == nil {
			return "", "", "", ErrEmailTemplateEngineRequired
		}
		return engine.RenderInline(*msg.InlineTemplate, msg.TemplateData)
	default:
		return msg.Subject, msg.HTMLBody, msg.TextBody, nil
	}
}
