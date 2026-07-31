// File: payloadvalidator_test.go

package grpop

import (
	"errors"
	"testing"
)

func TestValidateEmailMessage_RecipientRequired(t *testing.T) {
	err := validateEmailMessage(EmailMessage{Subject: "hi", HTMLBody: "<p>hi</p>"})
	if !errors.Is(err, ErrRecipientRequired) {
		t.Fatalf("validateEmailMessage(no To) = %v, want ErrRecipientRequired", err)
	}
}

func TestValidateEmailMessage_NoContentMode(t *testing.T) {
	err := validateEmailMessage(EmailMessage{To: "a@example.com"})
	if !errors.Is(err, ErrNoContentModeSet) {
		t.Fatalf("validateEmailMessage(no content mode) = %v, want ErrNoContentModeSet", err)
	}
}

func TestValidateEmailMessage_MultipleContentModes(t *testing.T) {
	tests := []EmailMessage{
		{To: "a@example.com", TemplateName: "welcome", Subject: "hi"},
		{To: "a@example.com", TemplateName: "welcome", InlineTemplate: &EmailTemplate{SubjectTemplate: "hi"}},
		{To: "a@example.com", InlineTemplate: &EmailTemplate{SubjectTemplate: "hi"}, HTMLBody: "<p>hi</p>"},
	}
	for i, msg := range tests {
		if err := validateEmailMessage(msg); !errors.Is(err, ErrMultipleContentModesSet) {
			t.Fatalf("case %d: validateEmailMessage(multiple modes) = %v, want ErrMultipleContentModesSet", i, err)
		}
	}
}

func TestValidateEmailMessage_ValidModes(t *testing.T) {
	tests := []EmailMessage{
		{To: "a@example.com", TemplateName: "welcome"},
		{To: "a@example.com", InlineTemplate: &EmailTemplate{SubjectTemplate: "hi"}},
		{To: "a@example.com", Subject: "hi"},
		{To: "a@example.com", HTMLBody: "<p>hi</p>"},
		{To: "a@example.com", TextBody: "hi"},
	}
	for i, msg := range tests {
		if err := validateEmailMessage(msg); err != nil {
			t.Fatalf("case %d: validateEmailMessage(valid single mode) = %v, want nil", i, err)
		}
	}
}

func TestValidateWhatsAppMessage_RecipientRequired(t *testing.T) {
	err := validateWhatsAppMessage(WhatsAppMessage{TemplateName: "welcome", LanguageCode: "en_US"})
	if !errors.Is(err, ErrRecipientRequired) {
		t.Fatalf("validateWhatsAppMessage(no To) = %v, want ErrRecipientRequired", err)
	}
}

func TestValidateWhatsAppMessage_TemplateNameRequired(t *testing.T) {
	err := validateWhatsAppMessage(WhatsAppMessage{To: "+15550001111", LanguageCode: "en_US"})
	if !errors.Is(err, ErrWhatsAppTemplateNameRequired) {
		t.Fatalf("validateWhatsAppMessage(no TemplateName) = %v, want ErrWhatsAppTemplateNameRequired", err)
	}
}

func TestValidateWhatsAppMessage_LanguageCodeRequired(t *testing.T) {
	err := validateWhatsAppMessage(WhatsAppMessage{To: "+15550001111", TemplateName: "welcome"})
	if !errors.Is(err, ErrWhatsAppLanguageCodeRequired) {
		t.Fatalf("validateWhatsAppMessage(no LanguageCode) = %v, want ErrWhatsAppLanguageCodeRequired", err)
	}
}

func TestValidateWhatsAppMessage_Valid(t *testing.T) {
	msg := WhatsAppMessage{To: "+15550001111", TemplateName: "welcome", LanguageCode: "en_US"}
	if err := validateWhatsAppMessage(msg); err != nil {
		t.Fatalf("validateWhatsAppMessage(valid) = %v, want nil", err)
	}
}
