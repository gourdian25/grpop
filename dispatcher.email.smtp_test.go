// File: dispatcher.email.smtp_test.go

package grpop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/smtp"
	"net/textproto"
	"regexp"
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
	rateLimitRejected   int
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
func (m *fakeMetrics) IncRateLimitRejected(_ Channel) {
	m.mu.Lock()
	m.rateLimitRejected++
	m.mu.Unlock()
}
func (m *fakeMetrics) IncCircuitBreakerStateChange(_ Channel, _ string) {}
func (m *fakeMetrics) ObserveSMTPResponseCode(code int) {
	m.mu.Lock()
	m.responseCodes = append(m.responseCodes, code)
	m.mu.Unlock()
}

// fakeRateLimiter is a RateLimiter (grlimiter.Limiter's shape) that
// allows or refuses every key, or fails, and records the keys it was asked
// about.
type fakeRateLimiter struct {
	refuse bool
	err    error

	mu   sync.Mutex
	keys []string
}

func (l *fakeRateLimiter) Allow(_ context.Context, key string) (bool, error) {
	l.mu.Lock()
	l.keys = append(l.keys, key)
	l.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	return !l.refuse, nil
}

func (l *fakeRateLimiter) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
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
	limiter := &fakeRateLimiter{refuse: true}
	client := &fakeSMTPClient{}
	sender, dialer := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.RateLimiter = limiter })

	_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Send() err = %v, want to wrap ErrRateLimited", err)
	}
	if got := limiter.asked(); len(got) != 1 || got[0] != "email" {
		t.Fatalf("limiter asked about %v, want [email]", got)
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

// argsLogger records every log line with its key/values flattened into one
// string, so a test can look for an address anywhere in what was logged.
type argsLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *argsLogger) log(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprint(append([]any{msg}, args...)...))
}
func (l *argsLogger) Debug(msg string, args ...any) { l.log(msg, args...) }
func (l *argsLogger) Info(msg string, args ...any)  { l.log(msg, args...) }
func (l *argsLogger) Warn(msg string, args ...any)  { l.log(msg, args...) }
func (l *argsLogger) Error(msg string, args ...any) { l.log(msg, args...) }

func (l *argsLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestSMTPDispatcher_Send_ClassifiesFailures: a 5xx reply is permanent
// (a missing mailbox, a refused sender, bad credentials); a 4xx reply, a dial
// failure and a write failure are not.
func TestSMTPDispatcher_Send_ClassifiesFailures(t *testing.T) {
	cases := []struct {
		name      string
		client    *fakeSMTPClient
		auth      bool
		permanent bool
	}{
		{"550 at RCPT", &fakeSMTPClient{rcptErr: &textproto.Error{Code: 550, Msg: "no such user"}}, false, true},
		{"553 at MAIL", &fakeSMTPClient{mailErr: &textproto.Error{Code: 553, Msg: "sender refused"}}, false, true},
		{"535 at AUTH", &fakeSMTPClient{authErr: &textproto.Error{Code: 535, Msg: "bad credentials"}}, true, true},
		{"421 at RCPT", &fakeSMTPClient{rcptErr: &textproto.Error{Code: 421, Msg: "try later"}}, false, false},
		{"450 at RCPT", &fakeSMTPClient{rcptErr: &textproto.Error{Code: 450, Msg: "mailbox busy"}}, false, false},
		{"a write failure", &fakeSMTPClient{writeErr: errors.New("connection reset")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender, _ := newTestSMTPDispatcher(t, tc.client, func(d *SMTPDispatcherDeps) {
				if tc.auth {
					d.Auth = smtp.PlainAuth("", "u", "p", "localhost")
					d.AllowInsecureAuth = true
				}
			})
			_, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"})
			if err == nil {
				t.Fatal("Send() err = nil, want a failure")
			}
			if got := errors.Is(err, ErrPermanent); got != tc.permanent {
				t.Fatalf("errors.Is(%v, ErrPermanent) = %v, want %v", err, got, tc.permanent)
			}
		})
	}

	dialer := &fakeSMTPDialer{err: errors.New("connection refused")}
	sender, err := NewSMTPDispatcher(SMTPDispatcherDeps{Addr: "localhost:1025", DefaultFrom: "s@example.com", TLSMode: SMTPTLSInsecureNoTLS, Dialer: dialer})
	if err != nil {
		t.Fatalf("NewSMTPDispatcher: %v", err)
	}
	if _, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b"}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("a dial failure = %v, want a temporary failure", err)
	}
}

// TestSMTPDispatcher_Send_RefusesBadAddresses: a line break in Reply-To, To
// or From, or an address net/mail refuses, is ErrInvalidAddress before
// anything is dialled, and the error never repeats the value.
func TestSMTPDispatcher_Send_RefusesBadAddresses(t *testing.T) {
	cases := []struct {
		name string
		msg  EmailMessage
	}{
		{"Reply-To with a Bcc line", EmailMessage{To: "a@b.com", ReplyTo: "r@b.com\r\nBcc: spy@evil.test", Subject: "s", TextBody: "b"}},
		{"To with a Bcc line", EmailMessage{To: "a@b.com\r\nBcc: spy@evil.test", Subject: "s", TextBody: "b"}},
		{"From with a Bcc line", EmailMessage{To: "a@b.com", From: "f@b.com\nBcc: spy@evil.test", Subject: "s", TextBody: "b"}},
		{"an unparseable To", EmailMessage{To: "spy-at-evil.test", Subject: "s", TextBody: "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeSMTPClient{}
			sender, dialer := newTestSMTPDispatcher(t, client, nil)
			_, err := sender.Send(context.Background(), tc.msg)
			if !errors.Is(err, ErrInvalidAddress) || !errors.Is(err, ErrPermanent) {
				t.Fatalf("Send() err = %v, want ErrInvalidAddress", err)
			}
			if strings.Contains(err.Error(), "evil.test") {
				t.Fatalf("the error repeats the value: %q", err)
			}
			if dialer.calls() != 0 || len(client.dataWritten) != 0 {
				t.Fatal("a bad address reached the relay")
			}
		})
	}
}

// TestNewSMTPDispatcher_RejectsInvalidDefaultFrom: a DefaultFrom that isn't
// an address fails at construction, not on the first send.
func TestNewSMTPDispatcher_RejectsInvalidDefaultFrom(t *testing.T) {
	_, err := NewSMTPDispatcher(SMTPDispatcherDeps{Addr: "localhost:1025", DefaultFrom: "no-reply\r\nBcc: spy@evil.test", TLSMode: SMTPTLSInsecureNoTLS})
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("NewSMTPDispatcher(bad DefaultFrom) = %v, want ErrInvalidAddress", err)
	}
}

// TestSMTPDispatcher_Send_DisplayNames: MAIL FROM and RCPT TO take the bare
// addresses, the headers the parsed ones, the Message-ID the sender's domain,
// and the recipient's rate-limit bucket is the same with or without a name.
func TestSMTPDispatcher_Send_DisplayNames(t *testing.T) {
	client := &fakeSMTPClient{}
	limiter := &fakeRateLimiter{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.RecipientRateLimiter = limiter })
	_, err := sender.Send(context.Background(), EmailMessage{
		To: "Ada Lovelace <Ada@Example.com>", From: "Acme <no-reply@acme.test>", ReplyTo: "Support <help@acme.test>",
		Subject: "s", TextBody: "b",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if client.mailFrom != "no-reply@acme.test" || client.rcptTo != "Ada@Example.com" {
		t.Fatalf("MAIL FROM %q, RCPT TO %q, want the bare addresses", client.mailFrom, client.rcptTo)
	}
	raw := string(client.dataWritten)
	for _, header := range []string{
		"From: \"Acme\" <no-reply@acme.test>\r\n",
		"To: \"Ada Lovelace\" <Ada@Example.com>\r\n",
		"Reply-To: \"Support\" <help@acme.test>\r\n",
	} {
		if !strings.Contains(raw, header) {
			t.Errorf("message lacks %q:\n%s", header, raw)
		}
	}
	if !regexp.MustCompile(`\r\nMessage-ID: <[0-9a-f]+@acme\.test>\r\n`).MatchString(raw) {
		t.Errorf("Message-ID doesn't carry the sender's bare domain:\n%s", raw)
	}
	if got := limiter.asked(); len(got) != 1 || got[0] != recipientRateLimitKey(ChannelEmail, "ada@example.com") {
		t.Fatalf("recipient key %v, want the bare address's", got)
	}
}

// TestSMTPDispatcher_LogsMaskedAddresses: no log line carries a full
// address, even when the relay's reply echoes it.
func TestSMTPDispatcher_LogsMaskedAddresses(t *testing.T) {
	logger := &argsLogger{}
	client := &fakeSMTPClient{}
	sender, _ := newTestSMTPDispatcher(t, client, func(d *SMTPDispatcherDeps) { d.Logger = logger })
	if _, err := sender.Send(context.Background(), EmailMessage{To: "ada@example.com", Subject: "s", TextBody: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	failing := &fakeSMTPClient{rcptErr: &textproto.Error{Code: 550, Msg: "5.1.1 <ada@example.com>: unknown user; sender sender@example.com"}}
	sender, _ = newTestSMTPDispatcher(t, failing, func(d *SMTPDispatcherDeps) { d.Logger = logger })
	if _, err := sender.Send(context.Background(), EmailMessage{To: "ada@example.com", Subject: "s", TextBody: "b"}); err == nil {
		t.Fatal("Send() err = nil, want the 550")
	}
	logged := logger.all()
	if strings.Contains(logged, "ada@example.com") || strings.Contains(logged, "sender@example.com") {
		t.Fatalf("a log line carries a full address:\n%s", logged)
	}
	if !strings.Contains(logged, "a***@example.com") {
		t.Fatalf("the logs don't name the masked recipient:\n%s", logged)
	}
}

// TestSMTPDispatcher_Send_IdempotencyKeyFixesMessageID: every attempt at one
// message (one key) carries the same Message-ID, the SHA-256 of the key at
// the sender's domain, and the key itself never appears in the message;
// another key, or none, gives another.
func TestSMTPDispatcher_Send_IdempotencyKeyFixesMessageID(t *testing.T) {
	send := func(key string) (string, string) {
		t.Helper()
		client := &fakeSMTPClient{}
		sender, _ := newTestSMTPDispatcher(t, client, nil)
		result, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", Subject: "s", TextBody: "b", IdempotencyKey: key})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		return result.ProviderMessageID, string(client.dataWritten)
	}

	first, raw := send("outbox-row-42")
	again, _ := send("outbox-row-42")
	if first != again {
		t.Fatalf("one key gave two Message-IDs: %q, %q", first, again)
	}
	sum := sha256.Sum256([]byte("outbox-row-42"))
	if want := hex.EncodeToString(sum[:]) + "@example.com"; first != want {
		t.Fatalf("Message-ID = %q, want %q", first, want)
	}
	if !strings.Contains(raw, "Message-ID: <"+first+">\r\n") || strings.Contains(raw, "outbox-row-42") {
		t.Fatalf("the header doesn't carry the derived id, or the key leaked:\n%s", raw)
	}
	if other, _ := send("outbox-row-43"); other == first {
		t.Fatal("two keys gave one Message-ID")
	}
	none1, _ := send("")
	none2, _ := send("")
	if none1 == none2 || none1 == first {
		t.Fatalf("without a key the Message-ID should be random: %q, %q", none1, none2)
	}
}
