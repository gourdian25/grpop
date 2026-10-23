// File: payloadvalidator_test.go

package grpop

import (
	"errors"
	"strings"
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

// TestParseEmailAddress: display names parse; a line break or anything
// net/mail refuses is ErrInvalidAddress (permanent), naming the field and
// never the value.
func TestParseEmailAddress(t *testing.T) {
	addr, err := parseEmailAddress("To", "Ada Lovelace <ada@example.com>")
	if err != nil || addr.Address != "ada@example.com" || addr.Name != "Ada Lovelace" {
		t.Fatalf("parseEmailAddress(display name) = %+v, %v", addr, err)
	}
	for _, value := range []string{
		"r@example.com\r\nBcc: spy@evil.test",
		"r@example.com\nBcc: spy@evil.test",
		"not-an-address",
		"two@example.com, addresses@example.com",
	} {
		_, err := parseEmailAddress("ReplyTo", value)
		if !errors.Is(err, ErrInvalidAddress) || !errors.Is(err, ErrPermanent) {
			t.Errorf("parseEmailAddress(%q) = %v, want ErrInvalidAddress", value, err)
			continue
		}
		if !strings.Contains(err.Error(), "ReplyTo") {
			t.Errorf("parseEmailAddress(%q) error %q doesn't name the field", value, err)
		}
		for _, part := range []string{"spy", "evil", "example.com", "not-an-address", "addresses"} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("parseEmailAddress(%q) error %q repeats the value (%q)", value, err, part)
			}
		}
	}
}

// TestMaskEmail: the first character, ***, the domain; never a split rune.
func TestMaskEmail(t *testing.T) {
	cases := map[string]string{
		"ada@example.com": "a***@example.com",
		"a@x.test":        "a***@x.test",
		"é@x.test":        "é***@x.test",
		"@x.test":         "***",
		"no-at-sign":      "***",
		"":                "***",
	}
	for in, want := range cases {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
	if got := maskedRecipient("Ada <Ada@Example.com>"); got != "A***@Example.com" {
		t.Errorf("maskedRecipient(display name) = %q", got)
	}
	if got := maskedRecipient("not an address"); got != "***" {
		t.Errorf("maskedRecipient(unparseable) = %q, want ***", got)
	}
}

// TestRedactAddresses: an SMTP reply echoing an address, as given or
// lower-cased, is masked.
func TestRedactAddresses(t *testing.T) {
	got := redactAddresses("550 5.1.1 <ada@example.com>: unknown; from Sender@Example.com", "Ada@Example.com", "Sender@Example.com", "")
	if strings.Contains(got, "ada@example.com") || strings.Contains(got, "Sender@Example.com") {
		t.Fatalf("redactAddresses left an address: %q", got)
	}
	if !strings.Contains(got, "A***@Example.com") || !strings.Contains(got, "S***@Example.com") {
		t.Fatalf("redactAddresses = %q, want masked forms", got)
	}
}
