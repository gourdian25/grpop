// File: memory_test.go

package grpop

import (
	"context"
	"testing"
)

func TestMemoryEmailSender_RecordsSentMessages(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx := context.Background()
	msg := EmailMessage{To: "a@example.com", Subject: "hi", HTMLBody: "<p>hi</p>"}

	result, err := s.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %s, want %s", result.Status, SendStatusSent)
	}
	if result.Channel != ChannelEmail {
		t.Fatalf("Channel = %s, want %s", result.Channel, ChannelEmail)
	}
	if result.ProviderMessageID == "" {
		t.Fatal("ProviderMessageID is empty, want a generated ID")
	}

	sent := s.Sent()
	if len(sent) != 1 || sent[0].To != "a@example.com" {
		t.Fatalf("Sent() = %v, want [msg]", sent)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMemoryEmailSender_Send_CanceledContext(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Send(ctx, EmailMessage{To: "a@example.com", Subject: "hi"}); err == nil {
		t.Fatal("Send(canceled ctx) = nil error, want non-nil")
	}
	if len(s.Sent()) != 0 {
		t.Fatal("Sent() is non-empty after a canceled-context Send, want no recording")
	}
}

func TestMemoryEmailSender_Sent_ReturnsCopyNotAlias(t *testing.T) {
	s := NewMemoryEmailSender()
	ctx := context.Background()
	_, _ = s.Send(ctx, EmailMessage{To: "a@example.com", Subject: "hi"})

	sent := s.Sent()
	sent[0].To = "mutated@example.com"

	sentAgain := s.Sent()
	if sentAgain[0].To != "a@example.com" {
		t.Fatalf("internal state mutated via Sent()'s returned slice: got %q, want %q", sentAgain[0].To, "a@example.com")
	}
}

func TestMemoryWhatsAppSender_RecordsSentMessages(t *testing.T) {
	s := NewMemoryWhatsAppSender()
	ctx := context.Background()
	msg := WhatsAppMessage{To: "+15550001111", TemplateName: "welcome", LanguageCode: "en_US"}

	result, err := s.Send(ctx, msg)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %s, want %s", result.Status, SendStatusSent)
	}
	if result.Channel != ChannelWhatsApp {
		t.Fatalf("Channel = %s, want %s", result.Channel, ChannelWhatsApp)
	}

	sent := s.Sent()
	if len(sent) != 1 || sent[0].To != "+15550001111" {
		t.Fatalf("Sent() = %v, want [msg]", sent)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestMemoryWhatsAppSender_Send_CanceledContext(t *testing.T) {
	s := NewMemoryWhatsAppSender()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := WhatsAppMessage{To: "+15550001111", TemplateName: "welcome", LanguageCode: "en_US"}
	if _, err := s.Send(ctx, msg); err == nil {
		t.Fatal("Send(canceled ctx) = nil error, want non-nil")
	}
	if len(s.Sent()) != 0 {
		t.Fatal("Sent() is non-empty after a canceled-context Send, want no recording")
	}
}

func TestGenerateMemoryID_Unique(t *testing.T) {
	a := generateMemoryID()
	b := generateMemoryID()
	if a == b {
		t.Fatalf("generateMemoryID() produced the same value twice: %q", a)
	}
	if a == "" {
		t.Fatal("generateMemoryID() = empty string")
	}
}
