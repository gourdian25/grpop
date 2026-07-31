// File: dispatcher.email.smtp.go

package grpop

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// SMTPTLSMode controls how dispatcher.email.smtp.go secures its connection
// to SMTPDispatcherDeps.Addr. Mandatory-by-default, not implicit: unlike a
// vendor SDK (SES/SendGrid) that handles transport security internally,
// grpop owns the raw connection here, so it must get this right itself.
type SMTPTLSMode string

const (
	// SMTPTLSStartTLS is the default: a plaintext connection is upgraded via
	// STARTTLS before AUTH/MAIL is attempted.
	SMTPTLSStartTLS SMTPTLSMode = "starttls"
	// SMTPTLSImplicit dials with TLS from the first byte (e.g. port 465).
	SMTPTLSImplicit SMTPTLSMode = "implicit"
	// SMTPTLSInsecureNoTLS is plaintext, no TLS at any point — deliberately
	// loud name to discourage accidental production use. Intended only for
	// a local Mailpit/MailHog test container.
	SMTPTLSInsecureNoTLS SMTPTLSMode = "insecure_no_tls"
)

// SMTPDialer opens a connection to addr and returns it ready for an
// Auth/Mail/Rcpt/Data transaction — any TLS handshake (implicit or
// STARTTLS) and the initial EHLO happen inside Dial, since SMTPClient's own
// method set has no room for them. A real implementation is a thin wrapper
// over net/smtp.Dial/NewClient/StartTLS; nil in SMTPDispatcherDeps uses one
// backed by the real network.
type SMTPDialer interface {
	Dial(addr string) (SMTPClient, error)
}

// SMTPClient is the subset of *net/smtp.Client's method set that Send
// actually drives — narrow enough to fake in unit tests, and satisfied by
// *smtp.Client itself with no adapter needed. Unlike a vendor-SDK-shaped
// narrow interface, this one's real backend can also be exercised against a
// real local Mailpit/MailHog SMTP server in integration tests — it is a
// fakeability seam for unit tests, not a documented "no real backend
// available" exception.
type SMTPClient interface {
	Auth(a smtp.Auth) error
	Mail(from string) error
	Rcpt(to string) error
	Data() (io.WriteCloser, error)
	Close() error
}

// SMTPDispatcherDeps configures NewSMTPDispatcher.
type SMTPDispatcherDeps struct {
	// Addr is the SMTP relay's host:port, e.g.
	// "email-smtp.us-east-1.amazonaws.com:587". SES/SendGrid/Postmark/
	// Mailgun/a bare relay are all just different values here, never a
	// different Go dependency. Required.
	Addr string
	Auth smtp.Auth
	// DefaultFrom is used when an individual EmailMessage.From is empty. At
	// least one of the two must be set at Send time, or Send returns
	// ErrEmailFromRequired.
	DefaultFrom string

	// TLSMode defaults to SMTPTLSStartTLS if unset (the zero value maps to
	// the secure default, not to SMTPTLSInsecureNoTLS). Constructing with
	// Auth set and TLSMode == SMTPTLSInsecureNoTLS is a construction-time
	// error (credentials over plaintext) unless AllowInsecureAuth is also
	// explicitly set.
	TLSMode           SMTPTLSMode
	AllowInsecureAuth bool

	// ConnectTimeout/SendTimeout bound one Send call end-to-end so a hanging
	// vendor connection can't stall the caller's own request indefinitely —
	// every Send is synchronous on the caller's own goroutine. Both apply as
	// a ceiling in addition to, never instead of, ctx's own deadline if the
	// caller supplied one. Defaults: ConnectTimeout 5s, SendTimeout 15s.
	ConnectTimeout time.Duration
	SendTimeout    time.Duration

	// Dialer is optional; nil uses a real net/smtp-backed dialer.
	Dialer SMTPDialer

	// TemplateEngine renders EmailMessage.TemplateName/InlineTemplate into a
	// literal Subject/HTMLBody/TextBody before the MIME message is built.
	// Not part of the original plan's Deps field list — added during
	// implementation because content-mode resolution has to happen
	// somewhere before the vendor call, and no earlier stage owns it yet
	// (see docs/plan/grpop-plan.md's Stage 10 revision note). Required only
	// if a Send call actually uses TemplateName/InlineTemplate; a literal
	// EmailMessage never touches it. A nil TemplateEngine with a
	// template-mode message returns ErrEmailTemplateEngineRequired.
	TemplateEngine EmailTemplateEngine

	RateLimiter    RateLimiter
	CircuitBreaker CircuitBreaker
	Metrics        Metrics
	Logger         Logger
}

// smtpDispatcher implements EmailSender over SMTP. It holds no persistent
// connection — Send dials fresh and closes when done — so, unlike a
// backend-holding type, it needs no sync.Once/atomic.Bool-guarded Close:
// there is no live resource for a second Close to double-release.
type smtpDispatcher struct {
	addr           string
	auth           smtp.Auth
	defaultFrom    string
	sendTimeout    time.Duration
	dialer         SMTPDialer
	templateEngine EmailTemplateEngine
	rateLimiter    RateLimiter
	circuitBreaker CircuitBreaker
	metrics        Metrics
	logger         Logger
}

var _ EmailSender = (*smtpDispatcher)(nil)

// NewSMTPDispatcher constructs an EmailSender that delivers over SMTP.
//
// Parameters:
//   - deps: SMTPDispatcherDeps — deps.Addr is required
//
// Returns:
//   - EmailSender
//   - error: non-nil if deps.Addr is empty/unparseable, or if deps.Auth is
//     set alongside TLSMode == SMTPTLSInsecureNoTLS without
//     AllowInsecureAuth
func NewSMTPDispatcher(deps SMTPDispatcherDeps) (EmailSender, error) {
	if deps.Addr == "" {
		return nil, errors.New("grpop/smtp: SMTPDispatcherDeps.Addr is required")
	}
	host, _, err := net.SplitHostPort(deps.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpop/smtp: invalid Addr: %w", err)
	}
	if deps.TLSMode == "" {
		deps.TLSMode = SMTPTLSStartTLS
	}
	if deps.TLSMode == SMTPTLSInsecureNoTLS && deps.Auth != nil && !deps.AllowInsecureAuth {
		return nil, errors.New("grpop/smtp: Auth is set with TLSMode=insecure_no_tls (credentials over plaintext); set AllowInsecureAuth to confirm this is intentional")
	}
	if deps.ConnectTimeout <= 0 {
		deps.ConnectTimeout = 5 * time.Second
	}
	if deps.SendTimeout <= 0 {
		deps.SendTimeout = 15 * time.Second
	}

	dialer := deps.Dialer
	if dialer == nil {
		dialer = &realSMTPDialer{
			serverName:     host,
			tlsMode:        deps.TLSMode,
			connectTimeout: deps.ConnectTimeout,
		}
	}

	return &smtpDispatcher{
		addr:           deps.Addr,
		auth:           deps.Auth,
		defaultFrom:    deps.DefaultFrom,
		sendTimeout:    deps.SendTimeout,
		dialer:         dialer,
		templateEngine: deps.TemplateEngine,
		rateLimiter:    deps.RateLimiter,
		circuitBreaker: deps.CircuitBreaker,
		metrics:        deps.Metrics,
		logger:         OrNop(deps.Logger),
	}, nil
}

func (d *smtpDispatcher) Send(ctx context.Context, msg EmailMessage) (SendResult, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	if err := validateEmailMessage(msg); err != nil {
		return SendResult{}, err
	}

	subject, htmlBody, textBody, err := d.resolveContent(msg)
	if err != nil {
		return SendResult{}, err
	}

	from := msg.From
	if from == "" {
		from = d.defaultFrom
	}
	if from == "" {
		return SendResult{}, ErrEmailFromRequired
	}

	if d.rateLimiter != nil {
		if err := d.rateLimiter.Wait(ctx, ChannelEmail, msg.To); err != nil {
			return SendResult{}, fmt.Errorf("grpop/smtp: rate limiter wait: %w", err)
		}
	}

	messageID := fmt.Sprintf("%s@%s", generateMemoryID(), messageIDDomain(from))
	raw := buildMIMEMessage(mimeMessageInput{
		From:      from,
		To:        msg.To,
		ReplyTo:   msg.ReplyTo,
		Subject:   subject,
		HTMLBody:  htmlBody,
		TextBody:  textBody,
		MessageID: messageID,
	})

	sendFn := func() error { return d.transact(ctx, from, msg.To, raw) }

	var sendErr error
	if d.circuitBreaker != nil {
		sendErr = d.circuitBreaker.Execute(ctx, sendFn)
	} else {
		sendErr = sendFn()
	}

	if d.metrics != nil {
		d.metrics.ObserveSendLatency(ChannelEmail, time.Since(start))
		if code := smtpResponseCode(sendErr); code != 0 {
			d.metrics.ObserveSMTPResponseCode(code)
		}
	}

	if sendErr != nil {
		d.logger.Error("grpop/smtp: send failed", "to", msg.To, "error", sendErr)
		if d.metrics != nil {
			d.metrics.IncSendResult(ChannelEmail, SendStatusFailed)
		}
		return SendResult{Channel: ChannelEmail, Status: SendStatusFailed, SentAt: time.Now().UTC()}, sendErr
	}

	d.logger.Info("grpop/smtp: sent", "to", msg.To, "message_id", messageID)
	if d.metrics != nil {
		d.metrics.IncSendResult(ChannelEmail, SendStatusSent)
	}
	return SendResult{
		Channel:           ChannelEmail,
		ProviderMessageID: messageID,
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// resolveContent turns msg's active content mode (already validated as
// exactly one by validateEmailMessage) into a literal subject/htmlBody/
// textBody triple.
func (d *smtpDispatcher) resolveContent(msg EmailMessage) (subject, htmlBody, textBody string, err error) {
	switch {
	case msg.TemplateName != "":
		if d.templateEngine == nil {
			return "", "", "", ErrEmailTemplateEngineRequired
		}
		return d.templateEngine.Render(msg.TemplateName, msg.TemplateData)
	case msg.InlineTemplate != nil:
		if d.templateEngine == nil {
			return "", "", "", ErrEmailTemplateEngineRequired
		}
		return d.templateEngine.RenderInline(*msg.InlineTemplate, msg.TemplateData)
	default:
		return msg.Subject, msg.HTMLBody, msg.TextBody, nil
	}
}

// transact runs the dial-through-close SMTP transaction on a background
// goroutine and races it against d.sendTimeout/ctx, so a hung dial or a
// stalled DATA write can't block Send indefinitely. net/smtp's blocking API
// gives no way to cancel an in-flight call, so on timeout the goroutine is
// abandoned rather than interrupted — it will still run to completion (or
// its own eventual I/O error) in the background; this is a known,
// documented limitation, not an oversight.
func (d *smtpDispatcher) transact(ctx context.Context, from, to string, raw []byte) error {
	resultCh := make(chan error, 1)
	go func() { resultCh <- d.doTransact(from, to, raw) }()

	timer := time.NewTimer(d.sendTimeout)
	defer timer.Stop()
	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		return fmt.Errorf("grpop/smtp: send timed out after %s", d.sendTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *smtpDispatcher) doTransact(from, to string, raw []byte) error {
	client, err := d.dialer.Dial(d.addr)
	if err != nil {
		return fmt.Errorf("grpop/smtp: dial: %w", err)
	}
	defer func() { _ = client.Close() }()

	if d.auth != nil {
		if err := client.Auth(d.auth); err != nil {
			return fmt.Errorf("grpop/smtp: auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("grpop/smtp: mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("grpop/smtp: rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("grpop/smtp: data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("grpop/smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("grpop/smtp: close data: %w", err)
	}
	return nil
}

func (d *smtpDispatcher) Close() error { return nil }

// smtpResponseCode extracts the raw SMTP reply code from err's chain, if
// it wraps a *textproto.Error (every error net/smtp returns for a rejected
// command does) — used only for Metrics.ObserveSMTPResponseCode, a
// deliverability-reputation canary; grpop takes no other action on the code.
func smtpResponseCode(err error) int {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return protoErr.Code
	}
	return 0
}

// messageIDDomain extracts the domain part of a From address for use in a
// generated Message-ID, falling back to a fixed placeholder for a
// malformed/domainless address rather than failing the send over a cosmetic
// header value.
func messageIDDomain(from string) string {
	if idx := strings.LastIndex(from, "@"); idx != -1 && idx < len(from)-1 {
		return from[idx+1:]
	}
	return "grpop.invalid"
}

// mimeMessageInput is buildMIMEMessage's parameter struct.
type mimeMessageInput struct {
	From, To, ReplyTo           string
	Subject, HTMLBody, TextBody string
	MessageID                   string
}

// buildMIMEMessage renders in as a complete RFC 5322 message ready for
// SMTPClient.Data's writer: multipart/alternative when both HTMLBody and
// TextBody are set, a single text/html or text/plain part otherwise.
func buildMIMEMessage(in mimeMessageInput) []byte {
	var headers bytes.Buffer
	writeHeader := func(k, v string) { fmt.Fprintf(&headers, "%s: %s\r\n", k, v) }
	writeHeader("From", in.From)
	writeHeader("To", in.To)
	if in.ReplyTo != "" {
		writeHeader("Reply-To", in.ReplyTo)
	}
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", in.Subject))
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader("Message-ID", "<"+in.MessageID+">")
	writeHeader("MIME-Version", "1.0")

	var body bytes.Buffer
	switch {
	case in.HTMLBody != "" && in.TextBody != "":
		mw := multipart.NewWriter(&body)
		writeHeader("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
		headers.WriteString("\r\n")
		if textPart, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}}); err == nil {
			_, _ = textPart.Write([]byte(in.TextBody))
		}
		if htmlPart, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/html; charset=utf-8"}}); err == nil {
			_, _ = htmlPart.Write([]byte(in.HTMLBody))
		}
		_ = mw.Close()
	case in.HTMLBody != "":
		writeHeader("Content-Type", "text/html; charset=utf-8")
		headers.WriteString("\r\n")
		body.WriteString(in.HTMLBody)
	default:
		writeHeader("Content-Type", "text/plain; charset=utf-8")
		headers.WriteString("\r\n")
		body.WriteString(in.TextBody)
	}

	var out bytes.Buffer
	out.Write(headers.Bytes())
	out.Write(body.Bytes())
	return out.Bytes()
}

// realSMTPDialer is the real, network-backed SMTPDialer NewSMTPDispatcher
// uses when SMTPDispatcherDeps.Dialer is nil.
type realSMTPDialer struct {
	serverName     string
	tlsMode        SMTPTLSMode
	connectTimeout time.Duration
}

var _ SMTPDialer = (*realSMTPDialer)(nil)

func (d *realSMTPDialer) Dial(addr string) (SMTPClient, error) {
	netDialer := &net.Dialer{Timeout: d.connectTimeout}

	var conn net.Conn
	var err error
	if d.tlsMode == SMTPTLSImplicit {
		conn, err = tls.DialWithDialer(netDialer, "tcp", addr, &tls.Config{ServerName: d.serverName, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = netDialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	client, err := smtp.NewClient(conn, d.serverName)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if d.tlsMode == SMTPTLSStartTLS {
		if err := client.StartTLS(&tls.Config{ServerName: d.serverName, MinVersion: tls.VersionTLS12}); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("grpop/smtp: starttls: %w", err)
		}
	}

	// *smtp.Client already satisfies SMTPClient (Auth/Mail/Rcpt/Data/Close
	// all match exactly) — no adapter type needed.
	return client, nil
}
