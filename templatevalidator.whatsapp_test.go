// File: templatevalidator.whatsapp_test.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNewMetaTemplateValidator_RequiresClient(t *testing.T) {
	if _, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{}); err == nil {
		t.Fatal("NewMetaTemplateValidator with nil Client = nil error, want non-nil")
	}
}

// newTestValidator is a validator over client with ttl, logging to logger.
func newTestValidator(t *testing.T, client WhatsAppCloudAPIClient, ttl time.Duration, logger Logger) *metaTemplateValidator {
	t.Helper()
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client, TTL: ttl, Logger: logger})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}
	return validator.(*metaTemplateValidator)
}

// positional is a positional template's body of n placeholders.
func positional(n int) []string {
	var names []string
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprint(i))
	}
	return names
}

// texts is n positional text values.
func texts(n int) []WhatsAppParameter {
	var values []WhatsAppParameter
	for i := 0; i < n; i++ {
		values = append(values, WhatsAppText("v"))
	}
	return values
}

func TestMetaTemplateValidator_Validate_ApprovedAndFits(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "order_confirmation", LanguageCode: "en_US", BodyParameters: positional(2)},
	}}
	validator := newTestValidator(t, client, 0, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "order_confirmation", LanguageCode: "en_US", Body: texts(2)}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "order_confirmation", LanguageCode: "en_US", Body: texts(1)})
	if !errors.Is(err, ErrWhatsAppTemplateArityMismatch) || !errors.Is(err, ErrPermanent) {
		t.Fatalf("Validate(one value of two) = %v, want ErrWhatsAppTemplateArityMismatch", err)
	}
}

func TestMetaTemplateValidator_Validate_NotApproved(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "other_template", LanguageCode: "en_US"},
		{Name: "order_confirmation", LanguageCode: "fr_FR"},
	}}
	validator := newTestValidator(t, client, 0, nil)
	for _, lang := range []string{"en_US", "de_DE"} {
		err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "order_confirmation", LanguageCode: lang})
		if !errors.Is(err, ErrWhatsAppTemplateNotApproved) || !strings.Contains(err.Error(), "order_confirmation ("+lang+")") {
			t.Fatalf("Validate(%s) = %v, want ErrWhatsAppTemplateNotApproved naming the template (approval is per language)", lang, err)
		}
	}
}

// TestTemplateFits: a message fits when it fills exactly what each part of
// the template takes, and otherwise the error says what differs.
func TestTemplateFits(t *testing.T) {
	otpBody, otpButtons := WhatsAppOTP("123456")
	image := WhatsAppImage("https://example.com/i.png")
	video := WhatsAppVideo("https://example.com/v.mp4")
	location := WhatsAppParameter{Location: &WhatsAppLocation{Latitude: 1, Longitude: 2}}
	named := func(names ...string) []WhatsAppParameter {
		var values []WhatsAppParameter
		for _, name := range names {
			values = append(values, WhatsAppNamedText(name, "v"))
		}
		return values
	}
	urlButton := WhatsAppTemplateButton{Index: 0, Type: "URL", RequiresValue: true}
	cases := []struct {
		name     string
		info     WhatsAppTemplateInfo
		msg      WhatsAppMessage
		mismatch string // "" when the message fits
	}{
		{"nothing to fill", WhatsAppTemplateInfo{HeaderFormat: "TEXT"}, WhatsAppMessage{}, ""},
		{"two positional values", WhatsAppTemplateInfo{BodyParameters: positional(2)}, WhatsAppMessage{Body: texts(2)}, ""},
		{"25 positional values", WhatsAppTemplateInfo{BodyParameters: templatePlaceholders(placeholdersUpTo(25))},
			WhatsAppMessage{Body: texts(25)}, ""},
		{"named values in any order", WhatsAppTemplateInfo{Named: true, BodyParameters: []string{"first_name", "order_id"}},
			WhatsAppMessage{Body: named("order_id", "first_name")}, ""},
		{"a text header's value", WhatsAppTemplateInfo{HeaderFormat: "TEXT", HeaderParameters: positional(1)},
			WhatsAppMessage{Header: &WhatsAppParameter{Text: "v"}}, ""},
		{"a named text header's value", WhatsAppTemplateInfo{Named: true, HeaderFormat: "TEXT", HeaderParameters: []string{"order_id"}},
			WhatsAppMessage{Header: &WhatsAppParameter{Name: "order_id", Text: "v"}}, ""},
		{"an image header's image", WhatsAppTemplateInfo{HeaderFormat: "IMAGE"}, WhatsAppMessage{Header: &image}, ""},
		{"a location header's location", WhatsAppTemplateInfo{HeaderFormat: "LOCATION"}, WhatsAppMessage{Header: &location}, ""},
		{"a header format grpop doesn't know", WhatsAppTemplateInfo{HeaderFormat: "GIF"}, WhatsAppMessage{Header: &video}, ""},
		{"a URL suffix", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{urlButton}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "x"}}}, ""},
		{"a one-time code on the URL button Meta lists", WhatsAppTemplateInfo{BodyParameters: positional(1), Buttons: []WhatsAppTemplateButton{urlButton}},
			WhatsAppMessage{Body: otpBody, Buttons: otpButtons}, ""},
		{"a one-time code on an OTP button", WhatsAppTemplateInfo{BodyParameters: positional(1), Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "OTP", RequiresValue: true}}},
			WhatsAppMessage{Body: otpBody, Buttons: otpButtons}, ""},
		{"a quick reply left out", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "QUICK_REPLY"}}}, WhatsAppMessage{}, ""},
		{"a quick reply's payload", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "QUICK_REPLY"}, {Index: 1, Type: "PHONE_NUMBER"}}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonQuickReply, Value: "p"}}}, ""},
		{"a coupon", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "COPY_CODE", RequiresValue: true}}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonCopyCode, Value: "SAVE"}}}, ""},

		{"too few values", WhatsAppTemplateInfo{BodyParameters: positional(2)}, WhatsAppMessage{Body: texts(1)},
			"the template's body takes 2 values, the message has 1"},
		{"a value the template doesn't take", WhatsAppTemplateInfo{}, WhatsAppMessage{Body: texts(1)},
			"the template's body takes 0 values, the message has 1"},
		{"positional values for a named template", WhatsAppTemplateInfo{Named: true, BodyParameters: []string{"first_name"}},
			WhatsAppMessage{Body: texts(1)}, "the template's values are named, the message's positional"},
		{"named values for a positional template", WhatsAppTemplateInfo{BodyParameters: positional(1)},
			WhatsAppMessage{Body: named("first_name")}, "the template's values are positional, the message's named"},
		{"a named value missing", WhatsAppTemplateInfo{Named: true, BodyParameters: []string{"order_id", "first_name"}},
			WhatsAppMessage{Body: named("first_name")}, "the template's body takes {{first_name}}, {{order_id}}, the message fills {{first_name}}"},
		{"a named value the template lacks", WhatsAppTemplateInfo{Named: true},
			WhatsAppMessage{Body: named("first_name")}, "the template's body takes nothing, the message fills {{first_name}}"},
		{"a text header's value missing", WhatsAppTemplateInfo{HeaderFormat: "TEXT", HeaderParameters: positional(1)},
			WhatsAppMessage{}, "the template's header takes a text value"},
		{"media for a text header", WhatsAppTemplateInfo{HeaderFormat: "TEXT", HeaderParameters: positional(1)},
			WhatsAppMessage{Header: &image}, "the template's header takes a text value"},
		{"a header value for a fixed header", WhatsAppTemplateInfo{HeaderFormat: "TEXT"},
			WhatsAppMessage{Header: &WhatsAppParameter{Text: "v"}}, "the template's header takes no value, the message has one"},
		{"a header value with no header", WhatsAppTemplateInfo{}, WhatsAppMessage{Header: &image}, "the template's header takes no value"},
		{"a named header's wrong name", WhatsAppTemplateInfo{Named: true, HeaderFormat: "TEXT", HeaderParameters: []string{"order_id"}},
			WhatsAppMessage{Header: &WhatsAppParameter{Name: "order", Text: "v"}}, "the template's header takes {{order_id}}"},
		{"a video for an image header", WhatsAppTemplateInfo{HeaderFormat: "IMAGE"}, WhatsAppMessage{Header: &video}, "the template's header takes image media"},
		{"no image for an image header", WhatsAppTemplateInfo{HeaderFormat: "IMAGE"}, WhatsAppMessage{}, "the template's header takes image media"},
		{"no location for a location header", WhatsAppTemplateInfo{HeaderFormat: "LOCATION"}, WhatsAppMessage{Header: &image}, "the template's header takes a location"},
		{"a URL suffix missing", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "QUICK_REPLY"}, {Index: 1, Type: "URL", RequiresValue: true}}},
			WhatsAppMessage{}, "the template's button 1 (URL) requires a value, the message has none"},
		{"a one-time code missing", WhatsAppTemplateInfo{BodyParameters: positional(1), Buttons: []WhatsAppTemplateButton{urlButton}},
			WhatsAppMessage{Body: otpBody}, "the template's button 0 (URL) requires a value"},
		{"a button the template lacks", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{urlButton}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "x"}, {Index: 3, Kind: WhatsAppButtonURL, Value: "x"}}},
			"Buttons[1]: the template has no button 3"},
		{"a URL suffix on a quick reply", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "QUICK_REPLY"}}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "x"}}}, "Buttons[0]: the template's button 0 is QUICK_REPLY, which takes no url value"},
		{"a URL suffix on a fixed URL", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "URL"}}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonURL, Value: "x"}}}, "the template's button 0 is URL, which takes no url value"},
		{"a coupon on a URL button", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{urlButton}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonCopyCode, Value: "x"}}}, "which takes no copy_code value"},
		{"a payload on a phone button", WhatsAppTemplateInfo{Buttons: []WhatsAppTemplateButton{{Index: 0, Type: "PHONE_NUMBER"}}},
			WhatsAppMessage{Buttons: []WhatsAppButton{{Index: 0, Kind: WhatsAppButtonQuickReply, Value: "x"}}}, "PHONE_NUMBER, which takes no quick_reply value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := templateFits(tc.info, tc.msg)
			if tc.mismatch == "" {
				if err != nil {
					t.Fatalf("templateFits = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrWhatsAppTemplateArityMismatch) || !strings.Contains(err.Error(), tc.mismatch) {
				t.Fatalf("templateFits = %v, want ErrWhatsAppTemplateArityMismatch saying %q", err, tc.mismatch)
			}
		})
	}
}

// placeholdersUpTo is a body text with {{1}} to {{n}}.
func placeholdersUpTo(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "{{%d}} ", i)
	}
	return b.String()
}

func TestMetaTemplateValidator_Validate_FetchesOnceThenCaches(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US"}}}
	validator := newTestValidator(t, client, time.Hour, nil)
	for i := 0; i < 3; i++ {
		if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
			t.Fatalf("Validate() call %d: %v", i, err)
		}
	}
	if n := client.listings(); n != 1 {
		t.Fatalf("%d listings, want 1 (the list is fresh for the TTL)", n)
	}
}

func TestMetaTemplateValidator_Validate_RefetchesAfterTTLExpires(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US"}}}
	validator := newTestValidator(t, client, 10*time.Millisecond, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	client.setTemplates(nil, nil) // no longer approved as of the next listing
	time.Sleep(20 * time.Millisecond)

	err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"})
	if !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() after TTL expiry err = %v, want ErrWhatsAppTemplateNotApproved", err)
	}
}

func TestMetaTemplateValidator_Refresh_ForcesImmediateRefetch(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{}
	validator := newTestValidator(t, client, time.Hour, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() before approval err = %v, want ErrWhatsAppTemplateNotApproved", err)
	}
	client.setTemplates([]WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US"}}, nil)
	if err := validator.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("Validate() after Refresh: %v", err)
	}
}

// TestMetaTemplateValidator_NoListLetsSendsThrough: with no list at all
// (Meta's listing fails), Validate lets the send through with a warning,
// and lists again only after minInterval, not on every send. Refresh still
// reports the failure.
func TestMetaTemplateValidator_NoListLetsSendsThrough(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templatesErr: errors.New("graph api unavailable")}
	logger := &argsLogger{}
	validator := newTestValidator(t, client, time.Hour, logger)
	for i := 0; i < 3; i++ {
		if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
			t.Fatalf("Validate() %d with no list = %v, want nil (Meta decides)", i, err)
		}
	}
	if n := client.listings(); n != 1 {
		t.Fatalf("%d listings, want 1 (a failed listing waits minInterval)", n)
	}
	if !strings.Contains(logger.all(), "letting the send through") {
		t.Fatalf("no warning logged: %s", logger.all())
	}
	if err := validator.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() err = nil, want the listing's failure")
	}

	validator.minInterval = 0
	client.setTemplates([]WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US", BodyParameters: positional(1)}}, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); !errors.Is(err, ErrWhatsAppTemplateArityMismatch) {
		t.Fatalf("Validate() once Meta answers = %v, want the list checked", err)
	}
}

// TestMetaTemplateValidator_FailedListingKeepsTheLastList: when a listing
// fails, Validate checks against the last list that succeeded.
func TestMetaTemplateValidator_FailedListingKeepsTheLastList(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US"}}}
	validator := newTestValidator(t, client, 10*time.Millisecond, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	client.setTemplates(nil, errors.New("graph api unavailable"))
	time.Sleep(20 * time.Millisecond)

	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("Validate(an approved template) on the last list = %v, want nil", err)
	}
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "t", LanguageCode: "en_US", Body: texts(1)}); !errors.Is(err, ErrWhatsAppTemplateArityMismatch) {
		t.Fatalf("Validate(a misfit) on the last list = %v, want ErrWhatsAppTemplateArityMismatch", err)
	}
}

// TestMetaTemplateValidator_RefusalLooksAgain: a refusal from a list older
// than minInterval is checked against a new listing first, so a template
// approved since passes; within minInterval it is refused without listing.
func TestMetaTemplateValidator_RefusalLooksAgain(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{{Name: "old", LanguageCode: "en_US"}}}
	validator := newTestValidator(t, client, time.Hour, nil)
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "old", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	client.setTemplates([]WhatsAppTemplateInfo{{Name: "old", LanguageCode: "en_US"}, {Name: "new", LanguageCode: "en_US"}}, nil)

	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "new", LanguageCode: "en_US"}); !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate(new) within minInterval = %v, want ErrWhatsAppTemplateNotApproved", err)
	}
	if n := client.listings(); n != 1 {
		t.Fatalf("%d listings within minInterval, want 1", n)
	}

	validator.minInterval = 0
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "new", LanguageCode: "en_US"}); err != nil {
		t.Fatalf("Validate(new) after minInterval = %v, want nil (approved since the last listing)", err)
	}
	if n := client.listings(); n != 2 {
		t.Fatalf("%d listings, want 2", n)
	}
	if err := validator.Validate(context.Background(), WhatsAppMessage{TemplateName: "gone", LanguageCode: "en_US"}); !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate(gone) = %v, want ErrWhatsAppTemplateNotApproved after looking again", err)
	}
}

func TestTemplateKey(t *testing.T) {
	if got := templateKey("welcome", "en_US"); got != "welcome|en_US" {
		t.Fatalf("templateKey = %q, want welcome|en_US", got)
	}
}
