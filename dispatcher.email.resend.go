// File: dispatcher.email.resend.go

package grpop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// defaultResendBaseURL is Resend's API origin.
const defaultResendBaseURL = "https://api.resend.com"

// resendMaxIdempotencyKey is the longest Idempotency-Key Resend takes (1 to
// 256 characters, kept 24 hours; Resend's API reference, read 2026-10-23).
// A longer EmailMessage.IdempotencyKey is sent as its SHA-256 hex.
const resendMaxIdempotencyKey = 256

// resendMaxErrorBody bounds how much of an error response is read.
const resendMaxErrorBody = 64 << 10

// ResendDispatcherDeps configures NewResendDispatcher.
type ResendDispatcherDeps struct {
	// APIKey is a Resend API key with sending access. Required. It goes in
	// the Authorization header and nowhere else: never a log line or an
	// error.
	APIKey string

	// BaseURL defaults to https://api.resend.com; a test points it at an
	// httptest.Server.
	BaseURL string

	// DefaultFrom is used when an individual EmailMessage.From is empty;
	// its domain must be verified in Resend. At least one of the two must
	// be set at Send time, or Send returns ErrEmailFromRequired.
	DefaultFrom string

	// Timeout bounds one API call, in addition to ctx's own deadline,
	// whichever is sooner. Default: 15s.
	Timeout time.Duration

	// HTTPClient is optional; nil uses a client of its own.
	HTTPClient *http.Client

	// TemplateEngine renders EmailMessage.TemplateName/InlineTemplate, as
	// on the SMTP sender (see SMTPDispatcherDeps.TemplateEngine).
	TemplateEngine EmailTemplateEngine

	// RateLimiter bounds the whole channel and RecipientRateLimiter each
	// recipient (see RateLimiter's doc comment); both optional. A refusal
	// is ErrRateLimited at once: Send never waits for a token.
	RateLimiter          RateLimiter
	RecipientRateLimiter RateLimiter
	CircuitBreaker       CircuitBreaker
	Metrics              Metrics
	Logger               Logger
}

// resendDispatcher implements EmailSender over Resend's HTTP API
// (POST /emails), hand-rolled on net/http like the Meta client: grpop
// takes no vendor SDK.
type resendDispatcher struct {
	apiKey         string
	endpoint       string
	defaultFrom    string
	timeout        time.Duration
	httpClient     *http.Client
	ownsClient     bool // httpClient was made here, so Close may release it
	templateEngine EmailTemplateEngine
	rateLimiter    RateLimiter
	recipientLimit RateLimiter
	circuitBreaker CircuitBreaker
	metrics        Metrics
	logger         Logger
}

var _ EmailSender = (*resendDispatcher)(nil)

// NewResendDispatcher constructs an EmailSender that delivers through
// Resend's API. EmailMessage.IdempotencyKey goes to Resend as the
// Idempotency-Key header, so a retried attempt at one message within 24
// hours is sent once.
//
// Parameters:
//   - deps: ResendDispatcherDeps — deps.APIKey is required
//
// Returns:
//   - EmailSender
//   - error: non-nil if deps.APIKey is empty, deps.BaseURL isn't an http(s)
//     URL, or deps.DefaultFrom is set but isn't an address
func NewResendDispatcher(deps ResendDispatcherDeps) (EmailSender, error) {
	if deps.APIKey == "" {
		return nil, errors.New("grpop/resend: ResendDispatcherDeps.APIKey is required")
	}
	base := deps.BaseURL
	if base == "" {
		base = defaultResendBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errors.New("grpop/resend: BaseURL must be an http(s) URL")
	}
	if deps.DefaultFrom != "" {
		if _, err := parseEmailAddress("DefaultFrom", deps.DefaultFrom); err != nil {
			return nil, fmt.Errorf("grpop/resend: %w", err)
		}
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	httpClient, ownsClient := deps.HTTPClient, false
	if httpClient == nil {
		httpClient, ownsClient = &http.Client{Timeout: timeout}, true
	}
	return &resendDispatcher{
		apiKey:         deps.APIKey,
		endpoint:       strings.TrimRight(base, "/") + "/emails",
		defaultFrom:    deps.DefaultFrom,
		timeout:        timeout,
		httpClient:     httpClient,
		ownsClient:     ownsClient,
		templateEngine: deps.TemplateEngine,
		rateLimiter:    deps.RateLimiter,
		recipientLimit: deps.RecipientRateLimiter,
		circuitBreaker: deps.CircuitBreaker,
		metrics:        deps.Metrics,
		logger:         OrNop(deps.Logger),
	}, nil
}

// resendEmailRequest is POST /emails's body: the fields grpop sends.
type resendEmailRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html,omitempty"`
	Text    string   `json:"text,omitempty"`
	ReplyTo string   `json:"reply_to,omitempty"`
}

// resendEmailResponse is POST /emails's success body.
type resendEmailResponse struct {
	ID string `json:"id"`
}

// resendErrorResponse is Resend's error body, as a request without a key
// showed on 2026-10-24: {"message", "name", "statusCode"}.
type resendErrorResponse struct {
	Message string `json:"message"`
	Name    string `json:"name"`
}

func (d *resendDispatcher) Send(ctx context.Context, msg EmailMessage) (SendResult, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	email, err := prepareEmail(msg, d.defaultFrom, d.templateEngine)
	if err != nil {
		return SendResult{}, err
	}
	if err := checkRateLimits(ctx, ChannelEmail, email.to.Address, d.rateLimiter, d.recipientLimit, d.metrics); err != nil {
		return SendResult{}, err
	}

	body := resendEmailRequest{
		From:    resendAddress(email.from),
		To:      []string{resendAddress(email.to)},
		Subject: email.subject,
		HTML:    email.htmlBody,
		Text:    email.textBody,
	}
	if email.replyTo != nil {
		body.ReplyTo = resendAddress(email.replyTo)
	}

	var id string
	sendFn := func() error {
		var callErr error
		id, callErr = d.post(ctx, body, email.idempotencyKey)
		return callErr
	}
	var sendErr error
	if d.circuitBreaker != nil {
		sendErr = d.circuitBreaker.Execute(ctx, sendFn)
	} else {
		sendErr = sendFn()
	}

	if d.metrics != nil {
		d.metrics.ObserveSendLatency(ChannelEmail, time.Since(start))
	}
	if sendErr != nil {
		d.logger.Error("grpop/resend: send failed", "to", maskEmail(email.to.Address),
			"error", redactAddresses(sendErr.Error(), email.to.Address, email.from.Address))
		if d.metrics != nil {
			d.metrics.IncSendResult(ChannelEmail, SendStatusFailed)
		}
		return SendResult{Channel: ChannelEmail, Status: SendStatusFailed, SentAt: time.Now().UTC()}, sendErr
	}

	d.logger.Info("grpop/resend: sent", "to", maskEmail(email.to.Address), "message_id", id)
	if d.metrics != nil {
		d.metrics.IncSendResult(ChannelEmail, SendStatusSent)
	}
	return SendResult{
		Channel:           ChannelEmail,
		ProviderMessageID: id,
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// post makes one POST /emails call and returns the id Resend gave the
// email. A refusal is classed by resendErrorIsTemporary; a network failure,
// a timeout or a success body without an id is temporary.
func (d *resendDispatcher) post(ctx context.Context, body resendEmailRequest, idempotencyKey string) (string, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", permanent(fmt.Errorf("grpop/resend: encode request: %w", err))
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", permanent(fmt.Errorf("grpop/resend: build request: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grpop/"+Version)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", resendIdempotencyKey(idempotencyKey))
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("grpop/resend: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, resendMaxErrorBody))
		var refusal resendErrorResponse
		var apiErr error
		if json.Unmarshal(raw, &refusal) == nil && refusal.Message != "" {
			apiErr = fmt.Errorf("grpop/resend: api error (status %d, %s): %s", resp.StatusCode, refusal.Name, refusal.Message)
		} else {
			apiErr = fmt.Errorf("grpop/resend: api error: status %d", resp.StatusCode)
		}
		if !resendErrorIsTemporary(resp.StatusCode, refusal.Name) {
			apiErr = permanent(apiErr)
		}
		return "", apiErr
	}

	var accepted resendEmailResponse
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		return "", fmt.Errorf("grpop/resend: decode response: %w", err)
	}
	if accepted.ID == "" {
		return "", errors.New("grpop/resend: response carried no email id")
	}
	return accepted.ID, nil
}

// resendErrorIsTemporary reports whether a Resend refusal is worth another
// attempt later (Resend's error reference, read 2026-10-23): a 429 (the
// per-second rate limit, and the daily and monthly quotas, which reset on
// their own), a 5xx, and a 409 that another request with the same key is
// still running (concurrent_idempotent_requests) or the resource is locked.
// Every other refusal is permanent: a 400 or 422 (an invalid field or key),
// a 401 or 403 (a missing, invalid, restricted or suspended key, an
// unverified sender domain), a 404, a 405, and a 409 that the same key came
// with a different body (invalid_idempotent_request, a caller bug).
func resendErrorIsTemporary(status int, name string) bool {
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		return true
	case status == http.StatusConflict:
		return name != "invalid_idempotent_request"
	default:
		return false
	}
}

// resendIdempotencyKey is key as Resend's Idempotency-Key header takes it:
// as is when it fits Resend's 256 characters, its SHA-256 hex otherwise
// (stable, so every attempt still sends the same header).
func resendIdempotencyKey(key string) string {
	if len(key) <= resendMaxIdempotencyKey {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// resendAddress formats a parsed address the way Resend's docs show one:
// the bare address, or "Name <address>" with the name as it is when it is
// plain (letters, digits, spaces and RFC 5322's atext punctuation), and
// quoted, with \ and " escaped, when it holds anything else (a comma, a
// period, angle brackets). A name isn't RFC 2047-encoded here: the body is
// JSON, and Resend writes the header.
func resendAddress(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	if resendPlainName(a.Name) {
		return a.Name + " <" + a.Address + ">"
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.Name)
	return `"` + escaped + `" <` + a.Address + ">"
}

// resendPlainName reports whether name needs no quoting in an address:
// every rune a letter, a digit, a space, or one of RFC 5322's atext
// punctuation characters.
func resendPlainName(name string) bool {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", r) {
			continue
		}
		return false
	}
	return true
}

// Close releases the idle connections of the HTTP client the sender made
// for itself; a client passed in through HTTPClient is the caller's to
// close.
func (d *resendDispatcher) Close() error {
	if d.ownsClient {
		d.httpClient.CloseIdleConnections()
	}
	return nil
}
