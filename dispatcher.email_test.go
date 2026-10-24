// File: dispatcher.email_test.go

package grpop

import (
	"errors"
	"testing"
)

// TestPrepareEmail_LiteralMessage: a literal message keeps its content, takes
// the default sender when it has none, and carries its key through.
func TestPrepareEmail_LiteralMessage(t *testing.T) {
	email, err := prepareEmail(EmailMessage{
		To: "Ada <ada@example.com>", Subject: "s", HTMLBody: "<p>h</p>", TextBody: "t", IdempotencyKey: "row-1",
	}, "Acme <no-reply@acme.test>", nil)
	if err != nil {
		t.Fatalf("prepareEmail: %v", err)
	}
	if email.from.Address != "no-reply@acme.test" || email.from.Name != "Acme" || email.to.Address != "ada@example.com" {
		t.Fatalf("addresses = %v, %v", email.from, email.to)
	}
	if email.replyTo != nil {
		t.Fatalf("replyTo = %v, want nil when the message has none", email.replyTo)
	}
	if email.subject != "s" || email.htmlBody != "<p>h</p>" || email.textBody != "t" || email.idempotencyKey != "row-1" {
		t.Fatalf("prepared = %+v", email)
	}

	email, err = prepareEmail(EmailMessage{To: "a@b.com", From: "f@b.com", ReplyTo: "r@b.com", TextBody: "t"}, "default@b.com", nil)
	if err != nil {
		t.Fatalf("prepareEmail: %v", err)
	}
	if email.from.Address != "f@b.com" || email.replyTo == nil || email.replyTo.Address != "r@b.com" {
		t.Fatalf("an explicit From and a Reply-To: %v, %v", email.from, email.replyTo)
	}
}

// TestPrepareEmail_RendersEitherTemplateMode: a registered template and an
// inline one both render through the engine.
func TestPrepareEmail_RendersEitherTemplateMode(t *testing.T) {
	engine := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	if err := engine.RegisterTemplate("hi", EmailTemplate{SubjectTemplate: "Hi {{.Name}}", TextBodyTemplate: "registered"}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	data := map[string]any{"Name": "Ada"}
	email, err := prepareEmail(EmailMessage{To: "a@b.com", TemplateName: "hi", TemplateData: data}, "f@b.com", engine)
	if err != nil || email.subject != "Hi Ada" || email.textBody != "registered" {
		t.Fatalf("registered template: %+v, %v", email, err)
	}
	inline := &EmailTemplate{SubjectTemplate: "Yo {{.Name}}", TextBodyTemplate: "inline"}
	email, err = prepareEmail(EmailMessage{To: "a@b.com", InlineTemplate: inline, TemplateData: data}, "f@b.com", engine)
	if err != nil || email.subject != "Yo Ada" || email.textBody != "inline" {
		t.Fatalf("inline template: %+v, %v", email, err)
	}
}

// TestPrepareEmail_Refusals: every way a message can't be sent is refused
// here, as a permanent error, before any vendor sees it.
func TestPrepareEmail_Refusals(t *testing.T) {
	cases := []struct {
		name        string
		msg         EmailMessage
		defaultFrom string
		want        error
	}{
		{"no recipient", EmailMessage{TextBody: "t"}, "f@b.com", ErrRecipientRequired},
		{"no content", EmailMessage{To: "a@b.com"}, "f@b.com", ErrNoContentModeSet},
		{"two content modes", EmailMessage{To: "a@b.com", TemplateName: "x", TextBody: "t"}, "f@b.com", ErrMultipleContentModesSet},
		{"a template and no engine", EmailMessage{To: "a@b.com", TemplateName: "x"}, "f@b.com", ErrEmailTemplateEngineRequired},
		{"an inline template and no engine", EmailMessage{To: "a@b.com", InlineTemplate: &EmailTemplate{}}, "f@b.com", ErrEmailTemplateEngineRequired},
		{"no sender at all", EmailMessage{To: "a@b.com", TextBody: "t"}, "", ErrEmailFromRequired},
		{"a bad From", EmailMessage{To: "a@b.com", From: "nope", TextBody: "t"}, "f@b.com", ErrInvalidAddress},
		{"a bad To", EmailMessage{To: "nope", TextBody: "t"}, "f@b.com", ErrInvalidAddress},
		{"a bad Reply-To", EmailMessage{To: "a@b.com", ReplyTo: "r@b.com\r\nBcc: x@y.z", TextBody: "t"}, "f@b.com", ErrInvalidAddress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareEmail(tc.msg, tc.defaultFrom, nil)
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrPermanent) {
				t.Fatalf("prepareEmail = %v, want %v (permanent)", err, tc.want)
			}
		})
	}
}
