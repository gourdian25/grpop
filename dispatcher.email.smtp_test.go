// File: dispatcher.email.smtp_test.go

package grpop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/smtp"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPClient is a fake SMTPClient recording every call it received, for
// asserting exactly what smtpDispatcher sent without a real network hop.
type fakeSMTPClient struct {
	authErr, mailErr, rcptErr, dataErr, writeErr, closeErr error
	dataDelay                                              time.Duration

	authCalled  bool
	mailFrom    string
	rcptTo      string
	dataWritten []byte
	closed      bool
}

func (c *fakeSMTPClient) Auth(_ smtp.Auth) error {
	c.authCalled = true
	return c.authErr
}
func (c *fakeSMTPClient) Mail(from string) error { c.mailFrom = from; return c.mailErr }
func (c *fakeSMTPClient) Rcpt(to string) error   { c.rcptTo = to; return c.rcptErr }
func (c *fakeSMTPClient) Data() (io.WriteCloser, error) {
	if c.dataErr != nil {
		return nil, c.dataErr
	}
	return &fakeSMTPDataWriter{client: c}, nil
}
func (c *fakeSMTPClient) Close() error { c.closed = true; return c.closeErr }

type fakeSMTPDataWriter struct {
	client *fakeSMTPClient
	buf    bytes.Buffer
}

func (w *fakeSMTPDataWriter) Write(p []byte) (int, error) {
	if w.client.dataDelay > 0 {
		time.Sleep(w.client.dataDelay)
	}
	if w.client.writeErr != nil {
		return 0, w.client.writeErr
	}
	return w.buf.Write(p)
}
func (w *fakeSMTPDataWriter) Close() error {
	w.client.dataWritten = w.buf.Bytes()
	return nil
}

// fakeSMTPDialer returns a preconfigured SMTPClient (or error) and counts
// how many times Dial was called, so tests can assert whether a rate
// limiter/circuit breaker rejection actually prevented a dial.
type fakeSMTPDialer struct {
	mu         sync.Mutex
	client     SMTPClient
	err        error
	dialCount  int
	dialedAddr string
}

func (d *fakeSMTPDialer) Dial(addr string) (SMTPClient, error) {
	d.mu.Lock()
	d.dialCount++
	d.dialedAddr = addr
	d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return d.client, nil
}

func (d *fakeSMTPDialer) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialCount
}

// fakeMetrics is a fake Metrics recorder shared by the dispatcher test
// files (email + whatsapp).
type fakeMetrics struct {
	mu                  sync.Mutex
	sendResults         []SendStatus
	responseCodes       []int
	latencyObservations int
	dedupHits           int
}

func (m *fakeMetrics) ObserveSendLatency(_ Channel, _ time.Duration) {
	m.mu.Lock()
	m.latencyObservations++
	m.mu.Unlock()
}
func (m *fakeMetrics) IncSendResult(_ Channel, status SendStatus) {
	m.mu.Lock()
	m.sendResults = append(m.sendResults, status)
	m.mu.Unlock()
}
func (m *fakeMetrics) IncRateLimitRejected(_ Channel)                   {}
func (m *fakeMetrics) IncDLQPublished(_ Channel)                        {}
func (m *fakeMetrics) IncCircuitBreakerStateChange(_ Channel, _ string) {}
func (m *fakeMetrics) IncIdempotencyDedupHit(_ Channel) {
	m.mu.Lock()
	m.dedupHits++
	m.mu.Unlock()
}
func (m *fakeMetrics) ObserveSMTPResponseCode(code int) {
	m.mu.Lock()
	m.responseCodes = append(m.responseCodes, code)
	m.mu.Unlock()
}

// fakeChannelRateLimiter is a fake RateLimiter whose Wait can be forced to
// fail, for asserting Send never dials when the rate limiter rejects.
type fakeChannelRateLimiter struct {
	waitErr   error
	waitCalls int
}

func (l *fakeChannelRateLimiter) Allow(_ context.Context, _ Channel, _ string) (bool, error) {
	return l.waitErr == nil, l.waitErr
}
func (l *fakeChannelRateLimiter) Wait(_ context.Context, _ Channel, _ string) error {
	l.waitCalls++
	return l.waitErr
}
func (l *fakeChannelRateLimiter) GetStats(_ context.Context, _ Channel, _ string) (RateLimiterStats, error) {
	return RateLimiterStats{}, nil
}

func newTestSMTPDispatcher(t *testing.T, client SMTPClient, mutate func(*SMTPDispatcherDeps)) (EmailSender, *fakeSMTPDialer) {
	t.Helper()
	dialer := &fakeSMTPDialer{client: client}
	deps := SMTPDispatcherDeps{
		Addr:        "localhost:1025",
		DefaultFrom: "sender@example.com",
		TLSMode:     SMTPTLSInsecureNoTLS,
		Dialer:      dialer,
	}
	if mutate != nil {
		mutate(&deps)
	}
	sender, err := NewSMTPDispatcher(deps)
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	return sender, dialer
}

func TestNewSMTPDispatcher_RequiresAddr(t *testing.T) {
	if _, err := NewSMTPDispatcher(SMTPDispatcherDeps{}); err == nil {
		t.Fatal("NewSMTPDispatcher with empty Addr = nil error, want non-nil")
	}
}

func TestNewSMTPDispatcher_RequiresValidAddr(t *testing.T) {
	if _, err := NewSMTPDispatcher(SMTPDispatcherDeps{Addr: "not-a-host-port"}); err == nil {
		t.Fatal("NewSMTPDispatcher with unparseable Addr = nil error, want non-nil")
	}
}

func TestNewSMTPDispatcher_InsecureAuthWithoutOptInRejected(t *testing.T) {
	_, err := NewSMTPDispatcher(SMTPDispatcherDeps{
		Addr:    "localhost:1025",
		Auth:    smtp.PlainAuth("", "u", "p", "localhost"),
		TLSMode: SMTPTLSInsecureNoTLS,
	})
	if err == nil {
		t.Fatal("NewSMTPDispatcher with Auth+insecure_no_tls and no opt-in = nil error, want non-nil")
	}
}

func TestNewSMTPDispatcher_InsecureAuthWithOptInAllowed(t *testing.T) {
	_, err := NewSMTPDispatcher(SMTPDispatcherDeps{
		Addr:              "localhost:1025",
		Auth:              smtp.PlainAuth("", "u", "p", "localhost"),
		TLSMode:           SMTPTLSInsecureNoTLS,
		AllowInsecureAuth: true,
		Dialer:            &fakeSMTPDialer{client: &fakeSMTPClient{}},
	})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher with AllowInsecureAuth=true: %v", err)
	}
}

func TestSMTPDispatcher_Send_ValidatesMessage(t *testing.T) {
	sender, _ := newTestSMTPDispatcher(t, &fakeSMTPClient{}, nil)
	ctx := context.Background()

	tests := []struct {
		name    string
		msg     EmailMessage
		wantErr error
	}{
		{"no recipient", EmailMessage{Subject: "s", HTMLBody: "b"}, ErrRecipientRequired},
		{"no content mode", EmailMessage{To: "a@b.com"}, ErrNoContentModeSet},
		{"multiple content modes", EmailMessage{To: "a@b.com", TemplateName: "t", Subject: "s"}, ErrMultipleContentModesSet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sender.Send(ctx, tt.msg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Send() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSMTPDispatcher_Send_RequiresFromWhenNoDefault(t *testing.T) {
	dialer := &fakeSMTPDialer{client: &fakeSMTPClient{}}
	sender, err := NewSMTPDispatcher(SMTPDispatcherDeps{Addr: "localhost:1025", TLSMode: SMTPTLSInsecureNoTLS, Dialer: dialer})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	_, sendErr := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", HTMLBody: "b"})
	if !errors.Is(sendErr, ErrEmailFromRequired) {
		t.Fatalf("Send() err = %v, want ErrEmailFromRequired", sendErr)
	}
}

func TestSMTPDispatcher_Send_TemplateModeRequiresEngine(t *testing.T) {
	sender, _ := newTestSMTPDispatcher(t, &fakeSMTPClient{}, nil)
	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", TemplateName: "welcome"})
	if !errors.Is(err, ErrEmailTemplateEngineRequired) {
		t.Fatalf("Send() err = %v, want ErrEmailTemplateEngineRequired", err)
	}
}

func TestSMTPDispatcher_Send_UsesTemplateEngine(t *testing.T) {
	engine := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	if err := engine.RegisterTemplate("welcome", EmailTemplate{
		SubjectTemplate:  "Hi {{.Name}}",
		HTMLBodyTemplate: "<p>Welcome, {{.Name}}</p>",
	}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	client := &fakeSMTPClient{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.TemplateEngine = engine })

	_, err := sender.Send(context.Background(), EmailMessage{
		To:           "a@b.com",
		TemplateName: "welcome",
		TemplateData: map[string]any{"Name": "Ada"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Contains(client.dataWritten, []byte("Welcome, Ada")) {
		t.Fatalf("rendered body missing from DATA payload: %s", client.dataWritten)
	}
	if !bytes.Contains(client.dataWritten, []byte("Hi Ada")) {
		t.Fatalf("rendered subject missing from DATA payload: %s", client.dataWritten)
	}
}

func TestSMTPDispatcher_Send_Success(t *testing.T) {
	client := &fakeSMTPClient{}
	metrics := &fakeMetrics{}
	sender, dialer := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.Metrics = metrics })

	result, err := sender.Send(context.Background(), EmailMessage{
		To: "recipient@example.com", Subject: "hello", HTMLBody: "<b>hi</b>", TextBody: "hi",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent {
		t.Fatalf("Status = %v, want SendStatusSent", result.Status)
	}
	if result.ProviderMessageID == "" {
		t.Fatal("ProviderMessageID is empty, want a generated Message-ID")
	}
	if result.Channel != ChannelEmail {
		t.Fatalf("Channel = %v, want ChannelEmail", result.Channel)
	}
	if dialer.calls() != 1 {
		t.Fatalf("dial count = %d, want 1", dialer.calls())
	}
	if client.mailFrom != "sender@example.com" {
		t.Fatalf("MAIL FROM = %q, want sender@example.com", client.mailFrom)
	}
	if client.rcptTo != "recipient@example.com" {
		t.Fatalf("RCPT TO = %q, want recipient@example.com", client.rcptTo)
	}
	if !client.closed {
		t.Fatal("client was never closed")
	}
	if len(metrics.sendResults) != 1 || metrics.sendResults[0] != SendStatusSent {
		t.Fatalf("metrics.sendResults = %v, want [sent]", metrics.sendResults)
	}
}

func TestSMTPDispatcher_Send_DialError(t *testing.T) {
	dialer := &fakeSMTPDialer{err: errors.New("connection refused")}
	sender, err := NewSMTPDispatcher(SMTPDispatcherDeps{
		Addr: "localhost:1025", DefaultFrom: "sender@example.com", TLSMode: SMTPTLSInsecureNoTLS, Dialer: dialer,
	})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	result, sendErr := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if sendErr == nil {
		t.Fatal("Send() err = nil, want non-nil")
	}
	if result.Status != SendStatusFailed {
		t.Fatalf("Status = %v, want SendStatusFailed", result.Status)
	}
}

func TestSMTPDispatcher_Send_RcptErrorRecordsResponseCode(t *testing.T) {
	client := &fakeSMTPClient{rcptErr: &textproto.Error{Code: 550, Msg: "mailbox unavailable"}}
	metrics := &fakeMetrics{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.Metrics = metrics })

	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if err == nil {
		t.Fatal("Send() err = nil, want non-nil")
	}
	if len(metrics.responseCodes) != 1 || metrics.responseCodes[0] != 550 {
		t.Fatalf("metrics.responseCodes = %v, want [550]", metrics.responseCodes)
	}
	if len(metrics.sendResults) != 1 || metrics.sendResults[0] != SendStatusFailed {
		t.Fatalf("metrics.sendResults = %v, want [failed]", metrics.sendResults)
	}
}

func TestSMTPDispatcher_Send_RateLimiterBlocksBeforeDial(t *testing.T) {
	limiter := &fakeChannelRateLimiter{waitErr: ErrRateLimited}
	client := &fakeSMTPClient{}
	sender, dialer := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.RateLimiter = limiter })

	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Send() err = %v, want to wrap ErrRateLimited", err)
	}
	if limiter.waitCalls != 1 {
		t.Fatalf("waitCalls = %d, want 1", limiter.waitCalls)
	}
	if dialer.calls() != 0 {
		t.Fatalf("dial count = %d, want 0 (rate limiter should have blocked before dial)", dialer.calls())
	}
}

func TestSMTPDispatcher_Send_CircuitBreakerOpensAfterFailure(t *testing.T) {
	client := &fakeSMTPClient{mailErr: errors.New("boom")}
	cb, err := NewCircuitBreaker(1, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	sender, dialer := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.CircuitBreaker = cb })

	msg := EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}
	if _, err := sender.Send(context.Background(), msg); err == nil {
		t.Fatal("first Send() err = nil, want non-nil (fake client fails)")
	}
	if _, err := sender.Send(context.Background(), msg); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second Send() err = %v, want ErrCircuitOpen", err)
	}
	if dialer.calls() != 1 {
		t.Fatalf("dial count = %d, want 1 (circuit breaker should have short-circuited the second Send)", dialer.calls())
	}
}

func TestSMTPDispatcher_Send_TimesOut(t *testing.T) {
	client := &fakeSMTPClient{dataDelay: 300 * time.Millisecond}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.SendTimeout = 30 * time.Millisecond })

	start := time.Now()
	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Send() err = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Send() err = %v, want a timeout error", err)
	}
	if elapsed >= client.dataDelay {
		t.Fatalf("Send() took %s, want it to return before the simulated %s DATA delay", elapsed, client.dataDelay)
	}
}

func TestBuildMIMEMessage_MultipartAlternative(t *testing.T) {
	raw := buildMIMEMessage(mimeMessageInput{
		From: "f@example.com", To: "t@example.com", Subject: "Sub",
		HTMLBody: "<p>html</p>", TextBody: "plain text", MessageID: "abc123@example.com",
	})
	s := string(raw)
	if !strings.Contains(s, "multipart/alternative") {
		t.Fatal("expected multipart/alternative content type when both HTMLBody and TextBody are set")
	}
	if !strings.Contains(s, "<p>html</p>") || !strings.Contains(s, "plain text") {
		t.Fatal("expected both HTML and plain text parts present in output")
	}
	if !strings.Contains(s, "Message-ID: <abc123@example.com>") {
		t.Fatal("expected Message-ID header")
	}
}

func TestBuildMIMEMessage_HTMLOnly(t *testing.T) {
	raw := buildMIMEMessage(mimeMessageInput{From: "f@example.com", To: "t@example.com", Subject: "Sub", HTMLBody: "<p>hi</p>", MessageID: "id@example.com"})
	s := string(raw)
	if strings.Contains(s, "multipart/alternative") {
		t.Fatal("HTML-only message should not be multipart")
	}
	if !strings.Contains(s, "Content-Type: text/html") {
		t.Fatal("expected text/html content type")
	}
}

func TestBuildMIMEMessage_TextOnly(t *testing.T) {
	raw := buildMIMEMessage(mimeMessageInput{From: "f@example.com", To: "t@example.com", Subject: "Sub", TextBody: "hi", MessageID: "id@example.com"})
	s := string(raw)
	if !strings.Contains(s, "Content-Type: text/plain") {
		t.Fatal("expected text/plain content type")
	}
}

func TestSMTPResponseCode(t *testing.T) {
	if code := smtpResponseCode(nil); code != 0 {
		t.Fatalf("smtpResponseCode(nil) = %d, want 0", code)
	}
	if code := smtpResponseCode(errors.New("plain")); code != 0 {
		t.Fatalf("smtpResponseCode(plain error) = %d, want 0", code)
	}
	wrapped := &textproto.Error{Code: 421, Msg: "service not available"}
	if code := smtpResponseCode(wrapped); code != 421 {
		t.Fatalf("smtpResponseCode(*textproto.Error) = %d, want 421", code)
	}
}

func TestMessageIDDomain(t *testing.T) {
	if got := messageIDDomain("user@example.com"); got != "example.com" {
		t.Fatalf("messageIDDomain(user@example.com) = %q, want example.com", got)
	}
	if got := messageIDDomain("not-an-email"); got != "grpop.invalid" {
		t.Fatalf("messageIDDomain(not-an-email) = %q, want grpop.invalid", got)
	}
}

func TestSMTPDispatcher_Send_InlineTemplateRequiresEngine(t *testing.T) {
	sender, _ := newTestSMTPDispatcher(t, &fakeSMTPClient{}, nil)
	_, err := sender.Send(context.Background(), EmailMessage{
		To: "a@b.com", InlineTemplate: &EmailTemplate{SubjectTemplate: "s", HTMLBodyTemplate: "b"},
	})
	if !errors.Is(err, ErrEmailTemplateEngineRequired) {
		t.Fatalf("Send() err = %v, want ErrEmailTemplateEngineRequired", err)
	}
}

func TestSMTPDispatcher_Send_UsesInlineTemplate(t *testing.T) {
	engine := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	client := &fakeSMTPClient{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.TemplateEngine = engine })

	_, err := sender.Send(context.Background(), EmailMessage{
		To:             "a@b.com",
		InlineTemplate: &EmailTemplate{SubjectTemplate: "Hi {{.Name}}", HTMLBodyTemplate: "<p>Inline, {{.Name}}</p>"},
		TemplateData:   map[string]any{"Name": "Grace"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !bytes.Contains(client.dataWritten, []byte("Inline, Grace")) {
		t.Fatalf("rendered inline body missing from DATA payload: %s", client.dataWritten)
	}
}

func TestSMTPDispatcher_Send_AuthIsCalledWhenConfigured(t *testing.T) {
	client := &fakeSMTPClient{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) {
		d.Auth = smtp.PlainAuth("", "user", "pass", "localhost")
		d.AllowInsecureAuth = true
	})
	if _, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !client.authCalled {
		t.Fatal("client.Auth was never called despite Deps.Auth being configured")
	}
}

func TestSMTPDispatcher_Send_AuthError(t *testing.T) {
	client := &fakeSMTPClient{authErr: errors.New("auth rejected")}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) {
		d.Auth = smtp.PlainAuth("", "user", "pass", "localhost")
		d.AllowInsecureAuth = true
	})
	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if err == nil {
		t.Fatal("Send() err = nil, want non-nil (Auth failed)")
	}
}

func TestSMTPDispatcher_Send_DataError(t *testing.T) {
	client := &fakeSMTPClient{dataErr: errors.New("data command rejected")}
	sender, _ := newTestSMTPDispatcher(t, client, nil)
	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if err == nil {
		t.Fatal("Send() err = nil, want non-nil (Data failed)")
	}
}

func TestSMTPDispatcher_Send_WriteError(t *testing.T) {
	client := &fakeSMTPClient{writeErr: errors.New("connection reset mid-write")}
	sender, _ := newTestSMTPDispatcher(t, client, nil)
	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if err == nil {
		t.Fatal("Send() err = nil, want non-nil (write failed)")
	}
}

func TestRealSMTPDialer_DialError(t *testing.T) {
	dialer := &realSMTPDialer{serverName: "localhost", tlsMode: SMTPTLSInsecureNoTLS, connectTimeout: 200 * time.Millisecond}
	if _, err := dialer.Dial("localhost:1"); err == nil {
		t.Fatal("Dial(unreachable) err = nil, want non-nil")
	}
}
