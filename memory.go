// File: memory.go

package grpop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// generateMemoryID returns a random hex identifier, used by memory.go's and
// dryrun.go's fake senders to synthesize a ProviderMessageID and by the SMTP
// sender for its Message-ID — stdlib crypto/rand only,
// matching grpop's zero-third-party-dependency dispatch layer even in its
// test doubles.
func generateMemoryID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read on the platforms Go supports never returns an error
	return hex.EncodeToString(b[:])
}

// MemoryEmailSender is an in-memory EmailSender for tests and local dev —
// it never contacts a real vendor (no SMTP connection is made at all), and
// records every message passed to Send for later assertion via Sent().
//
// Distinct from NewDryRunEmailSender (dryrun.go): that one exists for a
// staging/pre-prod deployment and deliberately does NOT retain rendered
// message content (grpop's payloads are security-sensitive — see
// dryrun.go's own doc comment), logging only non-sensitive metadata.
// MemoryEmailSender is test-only scaffolding, never wired into a shared
// log sink, so it has no such reason to redact — recording full sends is
// the point.
type MemoryEmailSender struct {
	mu   sync.Mutex
	sent []EmailMessage
}

var _ EmailSender = (*MemoryEmailSender)(nil)

// NewMemoryEmailSender constructs a MemoryEmailSender.
func NewMemoryEmailSender() *MemoryEmailSender {
	return &MemoryEmailSender{}
}

func (s *MemoryEmailSender) Send(ctx context.Context, msg EmailMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return SendResult{
		Channel:           ChannelEmail,
		ProviderMessageID: "memory-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// Sent returns every EmailMessage passed to Send so far, in call order.
func (s *MemoryEmailSender) Sent() []EmailMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EmailMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *MemoryEmailSender) Close() error { return nil }

// MemoryWhatsAppSender is an in-memory WhatsAppSender for tests and local
// dev — it never contacts Meta's Graph API, and records every message
// passed to Send for later assertion via Sent(). See MemoryEmailSender's
// doc comment for why this differs from NewDryRunWhatsAppSender.
type MemoryWhatsAppSender struct {
	mu   sync.Mutex
	sent []WhatsAppMessage
}

var _ WhatsAppSender = (*MemoryWhatsAppSender)(nil)

// NewMemoryWhatsAppSender constructs a MemoryWhatsAppSender.
func NewMemoryWhatsAppSender() *MemoryWhatsAppSender {
	return &MemoryWhatsAppSender{}
}

func (s *MemoryWhatsAppSender) Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return SendResult{
		Channel:           ChannelWhatsApp,
		ProviderMessageID: "memory-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// Sent returns every WhatsAppMessage passed to Send so far, in call order.
func (s *MemoryWhatsAppSender) Sent() []WhatsAppMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WhatsAppMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *MemoryWhatsAppSender) Close() error { return nil }
