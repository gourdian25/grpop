// File: dryrun_test.go

package grpop

import (
	"context"
	"strings"
	"testing"
)

func TestDryRunEmailSender_TemplateName(t *testing.T) {
	sender := NewDryRunEmailSender(nil)
	result, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", TemplateName: "welcome", TemplateData: map[string]any{"Secret": "should-not-leak"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if !strings.HasPrefix(result.ProviderMessageID, "dryrun-") {
		t.Fatalf("ProviderMessageID = %q, want dryrun- prefix", result.ProviderMessageID)
	}
	if result.Channel != ChannelEmail {
		t.Fatalf("Channel = %v, want ChannelEmail", result.Channel)
	}
}

func TestDryRunEmailSender_Close(t *testing.T) {
	if err := NewDryRunEmailSender(nil).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDryRunEmailSender_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewDryRunEmailSender(nil).Send(ctx, EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}); err == nil {
		t.Fatal("Send() with canceled ctx err = nil, want non-nil")
	}
}

func TestEmailContentDescription(t *testing.T) {
	tests := []struct {
		name string
		msg  EmailMessage
		want string
	}{
		{"template name", EmailMessage{TemplateName: "welcome"}, "welcome"},
		{"inline template", EmailMessage{InlineTemplate: &EmailTemplate{SubjectTemplate: "s"}}, "inline-template"},
		{"literal", EmailMessage{Subject: "s", HTMLBody: "b"}, "literal-body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := emailContentDescription(tt.msg); got != tt.want {
				t.Fatalf("emailContentDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDryRunWhatsAppSender_Send(t *testing.T) {
	sender := NewDryRunWhatsAppSender(nil)
	result, err := sender.Send(context.Background(), WhatsAppMessage{
		To: "15551234567", TemplateName: "order_confirmation", LanguageCode: "en_US",
		TemplateVariables: map[string]string{"1": "should-not-leak"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if !strings.HasPrefix(result.ProviderMessageID, "dryrun-") {
		t.Fatalf("ProviderMessageID = %q, want dryrun- prefix", result.ProviderMessageID)
	}
	if result.Channel != ChannelWhatsApp {
		t.Fatalf("Channel = %v, want ChannelWhatsApp", result.Channel)
	}
}

func TestDryRunWhatsAppSender_Close(t *testing.T) {
	if err := NewDryRunWhatsAppSender(nil).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDryRunWhatsAppSender_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewDryRunWhatsAppSender(nil).Send(ctx, WhatsAppMessage{To: "1", TemplateName: "t", LanguageCode: "en_US"}); err == nil {
		t.Fatal("Send() with canceled ctx err = nil, want non-nil")
	}
}

func TestDryRunSenders_UniqueMessageIDs(t *testing.T) {
	sender := NewDryRunEmailSender(nil)
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		result, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if seen[result.ProviderMessageID] {
			t.Fatalf("duplicate ProviderMessageID %q", result.ProviderMessageID)
		}
		seen[result.ProviderMessageID] = true
	}
}
