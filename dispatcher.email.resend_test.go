// File: dispatcher.email.resend_test.go

package grpop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testResendKey = "re_test_secret_key_123"

// fakeResend is a local stand-in for Resend's POST /emails: it records each
// request and answers with status and body.
type fakeResend struct {
	status int
	body   string
	delay  time.Duration

	mu       sync.Mutex
	requests []recordedResendRequest
}

type recordedResendRequest struct {
	method, path string
	header       http.Header
	body         resendEmailRequest
}

func (f *fakeResend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body resendEmailRequest
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedResendRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	status, answer := f.status, f.body
	if status == 0 {
		status, answer = http.StatusOK, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(answer))
}

func (f *fakeResend) received() []recordedResendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedResendRequest(nil), f.requests...)
}

// newTestResendDispatcher points a Resend sender at fake, with mutate's
// changes to its deps.
func newTestResendDispatcher(t *testing.T, fake *fakeResend, mutate func(*ResendDispatcherDeps)) EmailSender {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	deps := ResendDispatcherDeps{APIKey: testResendKey, BaseURL: server.URL, DefaultFrom: "Acme <no-reply@acme.test>"}
	if mutate != nil {
		mutate(&deps)
	}
	sender, err := NewResendDispatcher(deps)
	if err != nil {
		t.Fatalf("NewResendDispatcher: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	return sender
}

// TestNewResendDispatcher_Validation: no key, a base URL that isn't
// http(s), or a DefaultFrom that isn't an address fails at construction.
func TestNewResendDispatcher_Validation(t *testing.T) {
	for _, deps := range []ResendDispatcherDeps{
		{},
		{APIKey: "k", BaseURL: "ftp://api.resend.com"},
		{APIKey: "k", BaseURL: "not a url"},
		{APIKey: "k", DefaultFrom: "nope\r\nBcc: x@y.z"},
	} {
		if _, err := NewResendDispatcher(deps); err == nil {
			t.Errorf("NewResendDispatcher(%+v) = nil error", deps)
		}
	}
	sender, err := NewResendDispatcher(ResendDispatcherDeps{APIKey: "k"})
	if err != nil {
		t.Fatalf("NewResendDispatcher(key only): %v", err)
	}
	if got := sender.(*resendDispatcher).endpoint; got != "https://api.resend.com/emails" {
		t.Fatalf("default endpoint = %q", got)
	}
}

// TestResendDispatcher_Send_Request: one POST /emails with the key in the
// Authorization header alone, the prepared message as JSON, the idempotency
// key as a header, and Resend's id as the result.
func TestResendDispatcher_Send_Request(t *testing.T) {
	fake := &fakeResend{}
	metrics := &fakeMetrics{}
	sender := newTestResendDispatcher(t, fake, func(d *ResendDispatcherDeps) { d.Metrics = metrics })

	result, err := sender.Send(context.Background(), EmailMessage{
		To: "Ada Lovelace <ada@example.com>", ReplyTo: "help@acme.test",
		Subject: "Hi", HTMLBody: "<p>hi</p>", TextBody: "hi", IdempotencyKey: "outbox-row-7",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.Status != SendStatusSent || result.Channel != ChannelEmail || result.ProviderMessageID != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("result = %+v", result)
	}
	got := fake.received()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	req := got[0]
	if req.method != http.MethodPost || req.path != "/emails" {
		t.Fatalf("%s %s, want POST /emails", req.method, req.path)
	}
	if req.header.Get("Authorization") != "Bearer "+testResendKey || req.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", req.header)
	}
	if req.header.Get("Idempotency-Key") != "outbox-row-7" {
		t.Fatalf("Idempotency-Key = %q, want outbox-row-7", req.header.Get("Idempotency-Key"))
	}
	want := resendEmailRequest{
		From: "Acme <no-reply@acme.test>", To: []string{"Ada Lovelace <ada@example.com>"},
		Subject: "Hi", HTML: "<p>hi</p>", Text: "hi", ReplyTo: "help@acme.test",
	}
	if req.body.From != want.From || len(req.body.To) != 1 || req.body.To[0] != want.To[0] ||
		req.body.Subject != want.Subject || req.body.HTML != want.HTML || req.body.Text != want.Text || req.body.ReplyTo != want.ReplyTo {
		t.Fatalf("body = %+v, want %+v", req.body, want)
	}
	if len(metrics.sendResults) != 1 || metrics.sendResults[0] != SendStatusSent || metrics.latencyObservations != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestResendDispatcher_Send_IdempotencyKey: no key, no header; a key over
// Resend's 256 characters goes as its SHA-256 hex, the same every time.
func TestResendDispatcher_Send_IdempotencyKey(t *testing.T) {
	fake := &fakeResend{}
	sender := newTestResendDispatcher(t, fake, nil)
	long := strings.Repeat("k", 300)
	for _, key := range []string{"", long, long} {
		if _, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t", IdempotencyKey: key}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	got := fake.received()
	if _, present := got[0].header["Idempotency-Key"]; present {
		t.Fatal("a message without a key sent an Idempotency-Key header")
	}
	sum := sha256.Sum256([]byte(long))
	if h := got[1].header.Get("Idempotency-Key"); h != hex.EncodeToString(sum[:]) || got[2].header.Get("Idempotency-Key") != h {
		t.Fatalf("a long key sent %q then %q, want its SHA-256 hex both times", h, got[2].header.Get("Idempotency-Key"))
	}
}

// TestResendDispatcher_Send_ClassifiesRefusals: Resend's error reference,
// class by class.
func TestResendDispatcher_Send_ClassifiesRefusals(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		permanent bool
	}{
		{400, `{"statusCode":400,"name":"validation_error","message":"Invalid to field"}`, true},
		{401, `{"statusCode":401,"name":"missing_api_key","message":"Missing API Key"}`, true},
		{403, `{"statusCode":403,"name":"validation_error","message":"The acme.test domain is not verified"}`, true},
		{404, `{"statusCode":404,"name":"not_found","message":"Not found"}`, true},
		{422, `{"statusCode":422,"name":"missing_required_field","message":"Missing subject"}`, true},
		{409, `{"statusCode":409,"name":"invalid_idempotent_request","message":"Same key, different body"}`, true},
		{409, `{"statusCode":409,"name":"concurrent_idempotent_requests","message":"In progress"}`, false},
		{429, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests"}`, false},
		{429, `{"statusCode":429,"name":"daily_quota_exceeded","message":"Daily quota"}`, false},
		{500, `{"statusCode":500,"name":"application_error","message":"Oops"}`, false},
		{503, `not json`, false},
		{400, ``, true},
	}
	for _, tc := range cases {
		fake := &fakeResend{status: tc.status, body: tc.body}
		metrics := &fakeMetrics{}
		sender := newTestResendDispatcher(t, fake, func(d *ResendDispatcherDeps) { d.Metrics = metrics })
		result, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"})
		if err == nil {
			t.Fatalf("%d %s: Send() err = nil", tc.status, tc.body)
		}
		if got := errors.Is(err, ErrPermanent); got != tc.permanent {
			t.Errorf("%d %s: errors.Is(%v, ErrPermanent) = %v, want %v", tc.status, tc.body, err, got, tc.permanent)
		}
		if result.Status != SendStatusFailed || len(metrics.sendResults) != 1 || metrics.sendResults[0] != SendStatusFailed {
			t.Errorf("%d: result %+v, metrics %v", tc.status, result, metrics.sendResults)
		}
	}
}

// TestResendDispatcher_Send_TemporaryFailures: a timeout, a refused
// connection and a success body without an id are worth another attempt.
func TestResendDispatcher_Send_TemporaryFailures(t *testing.T) {
	slow := newTestResendDispatcher(t, &fakeResend{delay: 200 * time.Millisecond}, func(d *ResendDispatcherDeps) { d.Timeout = 20 * time.Millisecond })
	if _, err := slow.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("a timeout = %v, want a temporary failure", err)
	}

	server := httptest.NewServer(http.NotFoundHandler())
	closedURL := server.URL
	server.Close()
	gone, err := NewResendDispatcher(ResendDispatcherDeps{APIKey: testResendKey, BaseURL: closedURL, DefaultFrom: "f@b.com"})
	if err != nil {
		t.Fatalf("NewResendDispatcher: %v", err)
	}
	if _, err := gone.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("a refused connection = %v, want a temporary failure", err)
	}

	noID := newTestResendDispatcher(t, &fakeResend{status: 200, body: `{}`}, nil)
	if _, err := noID.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"}); err == nil || errors.Is(err, ErrPermanent) {
		t.Fatalf("a success without an id = %v, want a temporary failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeResend{}
	if _, err := newTestResendDispatcher(t, fake, nil).Send(ctx, EmailMessage{To: "a@b.com", TextBody: "t"}); !errors.Is(err, context.Canceled) || len(fake.received()) != 0 {
		t.Fatalf("a cancelled context = %v with %d requests, want context.Canceled and none", err, len(fake.received()))
	}
}

// TestResendDispatcher_Send_RefusesBeforeCalling: a message prepareEmail
// refuses, a rate-limited send and an open breaker never reach Resend.
func TestResendDispatcher_Send_RefusesBeforeCalling(t *testing.T) {
	fake := &fakeResend{}
	sender := newTestResendDispatcher(t, fake, nil)
	if _, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", ReplyTo: "r@b.com\r\nBcc: x@y.z", TextBody: "t"}); !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("a bad Reply-To = %v, want ErrInvalidAddress", err)
	}
	limited := newTestResendDispatcher(t, fake, func(d *ResendDispatcherDeps) { d.RecipientRateLimiter = &fakeRateLimiter{refuse: true} })
	if _, err := limited.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a refused limiter = %v, want ErrRateLimited", err)
	}
	if n := len(fake.received()); n != 0 {
		t.Fatalf("%d requests reached Resend, want 0", n)
	}

	failing := &fakeResend{status: 500, body: `{"name":"application_error","message":"Oops"}`}
	breaker, err := NewCircuitBreaker(1, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewCircuitBreaker: %v", err)
	}
	broken := newTestResendDispatcher(t, failing, func(d *ResendDispatcherDeps) { d.CircuitBreaker = breaker })
	_, _ = broken.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"})
	if _, err := broken.Send(context.Background(), EmailMessage{To: "a@b.com", TextBody: "t"}); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the second send = %v, want ErrCircuitOpen", err)
	}
	if n := len(failing.received()); n != 1 {
		t.Fatalf("%d requests reached Resend, want 1 (the breaker opened after it)", n)
	}
}

// TestResendDispatcher_Send_UsesTemplates: a template-mode message is
// rendered before it is sent, as on the SMTP sender.
func TestResendDispatcher_Send_UsesTemplates(t *testing.T) {
	engine := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	if err := engine.RegisterTemplate("reset", EmailTemplate{SubjectTemplate: "Reset for {{.Name}}", HTMLBodyTemplate: "<a href=\"{{.Link}}\">reset</a>"}); err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}
	fake := &fakeResend{}
	sender := newTestResendDispatcher(t, fake, func(d *ResendDispatcherDeps) { d.TemplateEngine = engine })
	if _, err := sender.Send(context.Background(), EmailMessage{To: "a@b.com", TemplateName: "reset", TemplateData: map[string]any{"Name": "Ada", "Link": "https://x.test/#t=1"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := fake.received()[0].body
	if body.Subject != "Reset for Ada" || body.HTML != `<a href="https://x.test/#t=1">reset</a>` || body.Text != "" {
		t.Fatalf("rendered body = %+v", body)
	}
}

// TestResendDispatcher_NeverLogsSecrets: no log line and no error carries
// the API key or a full address.
func TestResendDispatcher_NeverLogsSecrets(t *testing.T) {
	logger := &argsLogger{}
	ok := newTestResendDispatcher(t, &fakeResend{}, func(d *ResendDispatcherDeps) { d.Logger = logger })
	if _, err := ok.Send(context.Background(), EmailMessage{To: "ada@example.com", TextBody: "t"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	refused := newTestResendDispatcher(t, &fakeResend{status: 422, body: `{"name":"validation_error","message":"ada@example.com is not allowed"}`}, func(d *ResendDispatcherDeps) { d.Logger = logger })
	_, err := refused.Send(context.Background(), EmailMessage{To: "ada@example.com", TextBody: "t"})
	if err == nil || strings.Contains(err.Error(), testResendKey) {
		t.Fatalf("the refusal = %v, want an error without the key", err)
	}
	logged := logger.all()
	if strings.Contains(logged, testResendKey) || strings.Contains(logged, "ada@example.com") || strings.Contains(logged, "no-reply@acme.test") {
		t.Fatalf("a log line carries the key or a full address:\n%s", logged)
	}
	if !strings.Contains(logged, "a***@example.com") {
		t.Fatalf("the logs don't name the masked recipient:\n%s", logged)
	}
}

// TestResendAddress: a plain name as Resend's docs write it, anything else
// quoted and escaped, no name the bare address.
func TestResendAddress(t *testing.T) {
	cases := map[string]string{
		"no-reply@acme.test":                 "no-reply@acme.test",
		"Acme <no-reply@acme.test>":          "Acme <no-reply@acme.test>",
		"Zoë O'Brien <z@acme.test>":          "Zoë O'Brien <z@acme.test>",
		`"Acme, Inc." <no-reply@acme.test>`:  `"Acme, Inc." <no-reply@acme.test>`,
		`"Say \"hi\"" <no-reply@acme.test>`:  `"Say \"hi\"" <no-reply@acme.test>`,
		`"a\\b <evil>" <no-reply@acme.test>`: `"a\\b <evil>" <no-reply@acme.test>`,
	}
	for in, want := range cases {
		addr, err := mail.ParseAddress(in)
		if err != nil {
			t.Fatalf("ParseAddress(%q): %v", in, err)
		}
		if got := resendAddress(addr); got != want {
			t.Errorf("resendAddress(%q) = %q, want %q", in, got, want)
		}
		if back, err := mail.ParseAddress(resendAddress(addr)); err != nil || back.Address != addr.Address || back.Name != addr.Name {
			t.Errorf("resendAddress(%q) doesn't parse back to itself: %v, %v", in, back, err)
		}
	}
}

// TestResendDispatcher_Live sends one email through the real Resend API,
// only when GRPOP_TEST_RESEND_API_KEY is set (it skips otherwise, so it
// never runs in CI or the coverage gate): to Resend's test address
// delivered@resend.dev, from GRPOP_TEST_RESEND_FROM (default
// onboarding@resend.dev). It proves the request shape and the address
// format against the real API.
func TestResendDispatcher_Live(t *testing.T) {
	key := os.Getenv("GRPOP_TEST_RESEND_API_KEY")
	if key == "" {
		t.Skip("GRPOP_TEST_RESEND_API_KEY not set")
	}
	from := os.Getenv("GRPOP_TEST_RESEND_FROM")
	if from == "" {
		from = "grpop test <onboarding@resend.dev>"
	}
	sender, err := NewResendDispatcher(ResendDispatcherDeps{APIKey: key, DefaultFrom: from})
	if err != nil {
		t.Fatalf("NewResendDispatcher: %v", err)
	}
	result, err := sender.Send(context.Background(), EmailMessage{
		To: "grpop <delivered@resend.dev>", Subject: "grpop live test", TextBody: "sent by grpop's live Resend test",
		IdempotencyKey: "grpop-live-" + time.Now().UTC().Format("20060102T150405"),
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.ProviderMessageID == "" {
		t.Fatal("no Resend id")
	}
	t.Logf("sent, Resend id %s", result.ProviderMessageID)
}
