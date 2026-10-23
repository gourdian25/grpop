// File: payloadvalidator_test.go

package grpop

import (
	"errors"
	"math"
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

// TestValidateWhatsAppMessage_PhoneNumber: international form only, an
// optional "+" then 7 to 15 digits with no leading 0; anything else is
// ErrInvalidPhoneNumber (permanent), which never repeats the number.
func TestValidateWhatsAppMessage_PhoneNumber(t *testing.T) {
	for _, to := range []string{"+15551234567", "15551234567", "+6831234", "123456789012345", "+447911123456"} {
		if err := validateWhatsAppMessage(WhatsAppMessage{To: to, TemplateName: "t", LanguageCode: "en_US"}); err != nil {
			t.Errorf("validateWhatsAppMessage(To %q) = %v, want nil", to, err)
		}
	}
	for _, to := range []string{
		"+", "123456", "1234567890123456", "+0123456789", "0123456789",
		"+1 555 123 4567", "+1-555-123-4567", "(555)1234567", "abc", "+15551234567\n", "++15551234567",
	} {
		err := validateWhatsAppMessage(WhatsAppMessage{To: to, TemplateName: "t", LanguageCode: "en_US"})
		if !errors.Is(err, ErrInvalidPhoneNumber) || !errors.Is(err, ErrPermanent) {
			t.Errorf("validateWhatsAppMessage(To %q) = %v, want ErrInvalidPhoneNumber", to, err)
			continue
		}
		if len(to) > 4 && strings.Contains(err.Error(), to[1:5]) {
			t.Errorf("validateWhatsAppMessage(To %q) error %q repeats the number", to, err)
		}
	}
}

// whatsAppMsg is a well-formed message to the one recipient, filled with
// mutate's values.
func whatsAppMsg(mutate func(*WhatsAppMessage)) WhatsAppMessage {
	msg := WhatsAppMessage{To: "+15551234567", TemplateName: "t", LanguageCode: "en_US"}
	mutate(&msg)
	return msg
}

// TestValidateWhatsAppMessage_AcceptsWhatMetaTakes: every value kind, at
// each limit, named or positional, passes.
func TestValidateWhatsAppMessage_AcceptsWhatMetaTakes(t *testing.T) {
	otpBody, otpButtons := WhatsAppOTP("123456789012345")
	cases := map[string]func(*WhatsAppMessage){
		"nothing to fill":       func(*WhatsAppMessage) {},
		"four spaces in a row":  func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{WhatsAppText("a    b")} },
		"a 60-character header": func(m *WhatsAppMessage) { m.Header = &WhatsAppParameter{Text: strings.Repeat("h", 60)} },
		"60 two-byte characters": func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Text: strings.Repeat("é", 60)}
		},
		"a 15-character one-time code": func(m *WhatsAppMessage) { m.Body, m.Buttons = otpBody, otpButtons },
		"a 20-character coupon": func(m *WhatsAppMessage) {
			m.Buttons = []WhatsAppButton{{Index: 9, Kind: WhatsAppButtonCopyCode, Value: strings.Repeat("C", 20)}}
		},
		"every positional kind": func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Text: "Order 42"}
			m.Body = []WhatsAppParameter{
				WhatsAppText("Ada"),
				{Currency: &WhatsAppCurrency{FallbackValue: "$100.99", Code: "USD", Amount1000: 100990}},
				{DateTime: &WhatsAppDateTime{FallbackValue: "Feb 25, 1977"}},
			}
			m.Buttons = []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "track/42"}, {Index: 1, Kind: WhatsAppButtonQuickReply, Value: "stop"}}
		},
		"every named kind": func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Name: "order_id", Text: "42"}
			m.Body = []WhatsAppParameter{
				WhatsAppNamedText("first_name", "Ada"),
				{Name: "total", Currency: &WhatsAppCurrency{FallbackValue: "$1", Code: "USD", Amount1000: 1000}},
				{Name: "when_2", DateTime: &WhatsAppDateTime{FallbackValue: "today"}},
			}
			m.Buttons = []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "x", Name: "customer_name"}}
		},
		"a media header with positional values": func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, ID: "1234"}}
			m.Body = []WhatsAppParameter{WhatsAppText("Ada")}
		},
		"a media header with named values": func(m *WhatsAppMessage) {
			h := WhatsAppDocument("https://example.com/invoice.pdf", "invoice.pdf")
			m.Header = &h
			m.Body = []WhatsAppParameter{WhatsAppNamedText("first_name", "Ada")}
		},
		"a video header":  func(m *WhatsAppMessage) { h := WhatsAppVideo("https://example.com/v.mp4"); m.Header = &h },
		"an image header": func(m *WhatsAppMessage) { h := WhatsAppImage("https://example.com/i.png"); m.Header = &h },
		"a location header": func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Location: &WhatsAppLocation{Latitude: -90, Longitude: 180, Name: "n", Address: "a"}}
		},
	}
	for name, mutate := range cases {
		if err := validateWhatsAppMessage(whatsAppMsg(mutate)); err != nil {
			t.Errorf("%s: validateWhatsAppMessage = %v, want nil", name, err)
		}
	}
}

// TestValidateWhatsAppMessage_RefusesWhatMetaRefuses: each is
// ErrWhatsAppParameterInvalid (permanent), naming the field and never the
// value (every value here holds SECRET).
func TestValidateWhatsAppMessage_RefusesWhatMetaRefuses(t *testing.T) {
	text := func(s string) []WhatsAppParameter { return []WhatsAppParameter{WhatsAppText(s)} }
	header := func(p WhatsAppParameter) *WhatsAppParameter { return &p }
	button := func(b WhatsAppButton) []WhatsAppButton { return []WhatsAppButton{b} }
	cases := []struct {
		name   string
		mutate func(*WhatsAppMessage)
		field  string
	}{
		{"an empty value", func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{{}} }, "Body[0]"},
		{"two values in one", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{WhatsAppText("SECRET"), {Text: "SECRET", DateTime: &WhatsAppDateTime{FallbackValue: "SECRET"}}}
		}, "Body[1]"},
		{"a line feed", func(m *WhatsAppMessage) { m.Body = text("SECRET\nSECRET") }, "Body[0]"},
		{"a carriage return", func(m *WhatsAppMessage) { m.Body = text("SECRET\rSECRET") }, "Body[0]"},
		{"a tab", func(m *WhatsAppMessage) { m.Body = text("SECRET\tSECRET") }, "Body[0]"},
		{"five spaces in a row", func(m *WhatsAppMessage) { m.Body = text("SECRET     SECRET") }, "Body[0]"},
		{"a line break in a header", func(m *WhatsAppMessage) { m.Header = header(WhatsAppText("SECRET\nSECRET")) }, "Header"},
		{"a 61-character header", func(m *WhatsAppMessage) { m.Header = header(WhatsAppText("SECRET" + strings.Repeat("h", 55))) }, "Header"},
		{"a currency with no fallback", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{Currency: &WhatsAppCurrency{Code: "USD", Amount1000: 1}}}
		}, "Body[0]"},
		{"a lower-case currency code", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{Currency: &WhatsAppCurrency{FallbackValue: "SECRET", Code: "usd"}}}
		}, "Body[0]"},
		{"a four-letter currency code", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{Currency: &WhatsAppCurrency{FallbackValue: "SECRET", Code: "USDX"}}}
		}, "Body[0]"},
		{"a tab in a currency's fallback", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{Currency: &WhatsAppCurrency{FallbackValue: "SECRET\tSECRET", Code: "USD"}}}
		}, "Body[0]"},
		{"a date with no fallback", func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{{DateTime: &WhatsAppDateTime{}}} }, "Body[0]"},
		{"a line break in a date", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{DateTime: &WhatsAppDateTime{FallbackValue: "SECRET\nSECRET"}}}
		}, "Body[0]"},
		{"media in the body", func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{WhatsAppImage("https://SECRET.test/i.png")} }, "Body[0]"},
		{"a location in the body", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{{Location: &WhatsAppLocation{Name: "SECRET"}}}
		}, "Body[0]"},
		{"media with neither ID nor Link", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage}}
		}, "Header"},
		{"media with both ID and Link", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, ID: "SECRET", Link: "https://SECRET.test"}}
		}, "Header"},
		{"media of an unknown kind", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Media: &WhatsAppMedia{Kind: "audio", Link: "https://SECRET.test"}}
		}, "Header"},
		{"a filename on an image", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, Link: "https://SECRET.test", Filename: "SECRET.png"}}
		}, "Header"},
		{"a named media header", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Name: "logo", Media: &WhatsAppMedia{Kind: WhatsAppMediaImage, ID: "SECRET"}}
		}, "Header"},
		{"a latitude past 90", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Location: &WhatsAppLocation{Latitude: 90.5, Name: "SECRET"}}
		}, "Header"},
		{"a longitude past -180", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Location: &WhatsAppLocation{Longitude: -180.5, Name: "SECRET"}}
		}, "Header"},
		{"a latitude that isn't a number", func(m *WhatsAppMessage) {
			m.Header = &WhatsAppParameter{Location: &WhatsAppLocation{Latitude: math.NaN(), Name: "SECRET"}}
		}, "Header"},
		{"named and positional body values", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{WhatsAppNamedText("first_name", "SECRET"), WhatsAppText("SECRET")}
		}, "Header and Body"},
		{"a named header and a positional body", func(m *WhatsAppMessage) {
			m.Header = header(WhatsAppNamedText("order_id", "SECRET"))
			m.Body = text("SECRET")
		}, "Header and Body"},
		{"a name with capitals", func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{WhatsAppNamedText("First_Name", "SECRET")} }, "Body[0]"},
		{"a name with braces", func(m *WhatsAppMessage) { m.Body = []WhatsAppParameter{WhatsAppNamedText("{{name}}", "SECRET")} }, "Body[0]"},
		{"a name used twice", func(m *WhatsAppMessage) {
			m.Body = []WhatsAppParameter{WhatsAppNamedText("first_name", "SECRET"), WhatsAppNamedText("first_name", "SECRET")}
		}, "Body[1]"},
		{"a button index of 10", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Index: 10, Kind: WhatsAppButtonURL, Value: "SECRET"})
		}, "Buttons[0]"},
		{"a negative button index", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Index: -1, Kind: WhatsAppButtonURL, Value: "SECRET"})
		}, "Buttons[0]"},
		{"a button index used twice", func(m *WhatsAppMessage) {
			m.Buttons = []WhatsAppButton{{Index: 1, Kind: WhatsAppButtonURL, Value: "SECRET"}, {Index: 1, Kind: WhatsAppButtonQuickReply, Value: "SECRET"}}
		}, "Buttons[1]"},
		{"a button with no value", func(m *WhatsAppMessage) { m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonQuickReply}) }, "Buttons[0]"},
		{"a line break in a button's value", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonURL, Value: "SECRET\nSECRET"})
		}, "Buttons[0]"},
		{"a button of an unknown kind", func(m *WhatsAppMessage) { m.Buttons = button(WhatsAppButton{Kind: "call", Value: "SECRET"}) }, "Buttons[0]"},
		{"a name on a quick reply", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonQuickReply, Value: "SECRET", Name: "payload"})
		}, "Buttons[0]"},
		{"a URL button name with capitals", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonURL, Value: "SECRET", Name: "Customer"})
		}, "Buttons[0]"},
		{"a 21-character coupon", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonCopyCode, Value: "SECRET" + strings.Repeat("C", 15)})
		}, "Buttons[0]"},
		{"a 16-character one-time code", func(m *WhatsAppMessage) { m.Body, m.Buttons = WhatsAppOTP("SECRET" + strings.Repeat("9", 10)) }, "Buttons[0]"},
		{"a one-time code the body doesn't carry", func(m *WhatsAppMessage) {
			m.Body = text("SECRET1")
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonOTP, Value: "SECRET2"})
		}, "Buttons[0]"},
		{"a one-time code with no body", func(m *WhatsAppMessage) {
			m.Buttons = button(WhatsAppButton{Kind: WhatsAppButtonOTP, Value: "SECRET"})
		}, "Buttons[0]"},
		{"a one-time code beside a second body value", func(m *WhatsAppMessage) {
			m.Body, m.Buttons = WhatsAppOTP("SECRET")
			m.Body = append(m.Body, WhatsAppText("SECRET"))
		}, "Buttons[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWhatsAppMessage(whatsAppMsg(tc.mutate))
			if !errors.Is(err, ErrWhatsAppParameterInvalid) || !errors.Is(err, ErrPermanent) {
				t.Fatalf("validateWhatsAppMessage = %v, want ErrWhatsAppParameterInvalid (permanent)", err)
			}
			if !strings.Contains(err.Error(), ": "+tc.field+" ") {
				t.Fatalf("error %q doesn't name %s", err, tc.field)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error %q repeats the value", err)
			}
		})
	}
}
