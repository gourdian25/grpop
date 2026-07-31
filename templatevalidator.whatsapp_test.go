// File: templatevalidator.whatsapp_test.go

package grpop

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewMetaTemplateValidator_RequiresClient(t *testing.T) {
	if _, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{}); err == nil {
		t.Fatal("NewMetaTemplateValidator with nil Client = nil error, want non-nil")
	}
}

func TestMetaTemplateValidator_Validate_ApprovedAndArityMatches(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "order_confirmation", LanguageCode: "en_US", ParameterCount: 2},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	err = validator.Validate(context.Background(), "order_confirmation", "en_US", map[string]string{"1": "a", "2": "b"})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestMetaTemplateValidator_Validate_NotApproved(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "other_template", LanguageCode: "en_US", ParameterCount: 1},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	err = validator.Validate(context.Background(), "order_confirmation", "en_US", nil)
	if !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() err = %v, want ErrWhatsAppTemplateNotApproved", err)
	}
}

func TestMetaTemplateValidator_Validate_WrongLanguageIsNotApproved(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "order_confirmation", LanguageCode: "fr_FR", ParameterCount: 1},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	err = validator.Validate(context.Background(), "order_confirmation", "en_US", map[string]string{"1": "a"})
	if !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() err = %v, want ErrWhatsAppTemplateNotApproved (approval is per-language)", err)
	}
}

func TestMetaTemplateValidator_Validate_ArityMismatch(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "order_confirmation", LanguageCode: "en_US", ParameterCount: 2},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	err = validator.Validate(context.Background(), "order_confirmation", "en_US", map[string]string{"1": "only-one"})
	if !errors.Is(err, ErrWhatsAppTemplateArityMismatch) {
		t.Fatalf("Validate() err = %v, want ErrWhatsAppTemplateArityMismatch", err)
	}
}

func TestMetaTemplateValidator_Validate_FetchesOnceThenCaches(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "t", LanguageCode: "en_US", ParameterCount: 0},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client, TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := validator.Validate(context.Background(), "t", "en_US", nil); err != nil {
			t.Fatalf("Validate() call %d: %v", i, err)
		}
	}
	// GetApprovedTemplates has no exported call counter on
	// fakeWhatsAppCloudAPIClient, so assert indirectly: mutate the fake's
	// backing list after the first Validate and confirm a *still-cached*
	// validator does not observe the change until TTL elapses or Refresh is
	// called explicitly.
	client.templates = nil
	if err := validator.Validate(context.Background(), "t", "en_US", nil); err != nil {
		t.Fatalf("Validate() after backing list changed (should still be cache-fresh): %v", err)
	}
}

func TestMetaTemplateValidator_Validate_RefetchesAfterTTLExpires(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: []WhatsAppTemplateInfo{
		{Name: "t", LanguageCode: "en_US", ParameterCount: 0},
	}}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client, TTL: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	if err := validator.Validate(context.Background(), "t", "en_US", nil); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	client.templates = nil // template no longer approved as of the next fetch
	time.Sleep(20 * time.Millisecond)

	err = validator.Validate(context.Background(), "t", "en_US", nil)
	if !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() after TTL expiry err = %v, want ErrWhatsAppTemplateNotApproved", err)
	}
}

func TestMetaTemplateValidator_Refresh_ForcesImmediateRefetch(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templates: nil}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client, TTL: time.Hour})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	if err := validator.Validate(context.Background(), "t", "en_US", nil); !errors.Is(err, ErrWhatsAppTemplateNotApproved) {
		t.Fatalf("Validate() before approval err = %v, want ErrWhatsAppTemplateNotApproved", err)
	}

	client.templates = []WhatsAppTemplateInfo{{Name: "t", LanguageCode: "en_US", ParameterCount: 0}}
	if err := validator.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if err := validator.Validate(context.Background(), "t", "en_US", nil); err != nil {
		t.Fatalf("Validate() after Refresh: %v", err)
	}
}

func TestMetaTemplateValidator_Refresh_PropagatesClientError(t *testing.T) {
	client := &fakeWhatsAppCloudAPIClient{templatesErr: errors.New("graph api unavailable")}
	validator, err := NewMetaTemplateValidator(MetaTemplateValidatorDeps{Client: client})
	if err != nil {
		t.Fatalf("NewMetaTemplateValidator: %v", err)
	}

	if err := validator.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() err = nil, want non-nil")
	}
	if err := validator.Validate(context.Background(), "t", "en_US", nil); err == nil {
		t.Fatal("Validate() err = nil, want non-nil (underlying fetch failed)")
	}
}

func TestTemplateKey(t *testing.T) {
	if got := templateKey("welcome", "en_US"); got != "welcome|en_US" {
		t.Fatalf("templateKey = %q, want welcome|en_US", got)
	}
}
