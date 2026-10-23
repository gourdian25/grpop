// File: example/main.go

// Command example sends grpop's four grounding ERP use cases
// (grpop-plan.md §2: auth.ForgotPassword, provider.IssueInvite,
// admin.IssueInvite, a tenant's first-admin invite) through the SMTP sender,
// with the retry loop a caller now owns: grpop sends each message once and
// reports what happened, and retrying, keeping a message durably and
// suppressing duplicates are the caller's (grsentry does them with an
// outbox). sendWithRetry below is the smallest such loop: a few attempts,
// spaced with grpop.FullJitterBackoff, never past the message's own expiry.
//
// Email, by default, targets a local Mailpit SMTP server (localhost:1025,
// no TLS, no auth), the same one the integration test uses, so it runs with
// nothing but `docker run -d -p 1025:1025 -p 8025:8025 axllent/mailpit` and
// no real credentials. Point it at a real relay via environment variables
// when you're ready to integrate for real:
//
//	GRPOP_SMTP_ADDR      host:port, default "localhost:1025"
//	GRPOP_SMTP_FROM      default sender address, default "no-reply@example.com"
//	GRPOP_SMTP_TLS_MODE  "starttls" | "implicit" | "insecure_no_tls", default "insecure_no_tls"
//	GRPOP_SMTP_USER      optional; enables PLAIN auth if set alongside GRPOP_SMTP_PASS
//	GRPOP_SMTP_PASS      optional
//
// Run it with:
//
//	go run ./example
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"time"

	"github.com/gourdian25/grpop"
)

func main() {
	engine := grpop.NewEmailTemplateEngine(grpop.EmailTemplateEngineConfig{})
	if err := registerAuthEmailTemplates(engine); err != nil {
		log.Fatalf("register templates: %v", err)
	}

	emailSender, err := grpop.NewSMTPDispatcher(smtpDispatcherDepsFromEnv(engine))
	if err != nil {
		log.Fatalf("construct SMTP dispatcher: %v", err)
	}

	defer func() { _ = emailSender.Close() }()

	ctx := context.Background()
	const appName = "Skipp"
	const tenantName = "Acme Corp"
	now := time.Now()

	sendWithRetry(ctx, emailSender, "forgot password", grpop.EmailMessage{
		To:             "user@example.com",
		TemplateName:   EmailTemplateForgotPassword,
		IdempotencyKey: "demo-forgot-password",
		TemplateData: map[string]any{
			"AppName":          appName,
			"RecipientName":    "Ada",
			"ResetLink":        "https://app.skipp.co.in/reset-password?token=example-reset-token",
			"ExpiresInMinutes": 30,
		},
	}, now.Add(30*time.Minute)) // the reset link's own expiry: retrying past it delivers a dead link

	sendWithRetry(ctx, emailSender, "provider invite", grpop.EmailMessage{
		To:             "new-provider@example.com",
		TemplateName:   EmailTemplateInvite,
		IdempotencyKey: "demo-provider-invite",
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForProviderInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-provider-invite-token",
			"ExpiresInHours": 72,
		},
	}, now.Add(72*time.Hour))

	sendWithRetry(ctx, emailSender, "admin invite", grpop.EmailMessage{
		To:             "new-admin@example.com",
		TemplateName:   EmailTemplateInvite,
		IdempotencyKey: "demo-admin-invite",
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForAdminInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-admin-invite-token",
			"ExpiresInHours": 72,
		},
	}, now.Add(72*time.Hour))

	sendWithRetry(ctx, emailSender, "tenant first-admin invite", grpop.EmailMessage{
		To:             "first-admin@example.com",
		TemplateName:   EmailTemplateInvite,
		IdempotencyKey: "demo-first-admin-invite",
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForTenantFirstAdmin, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-first-admin-invite-token",
			"ExpiresInHours": 72,
		},
	}, now.Add(72*time.Hour))

	fmt.Println("\nDone — check http://localhost:8025 (Mailpit's web UI) if using the default local target.")
}

// sendWithRetry sends msg once and, on a temporary failure, tries again a
// few times, spaced by grpop.FullJitterBackoff, giving up at expiresAt (the
// link the message carries is dead by then). A failure matching
// grpop.ErrPermanent (a bad address, a mailbox that doesn't exist, a
// template that won't render) stops at once: it would fail the same way
// every time. Each message carries an IdempotencyKey, so every attempt at it
// has the same Message-ID. A real caller keeps the message durably between
// attempts so a crash doesn't lose it; this demo only loops.
func sendWithRetry(ctx context.Context, sender grpop.EmailSender, label string, msg grpop.EmailMessage, expiresAt time.Time) {
	const maxAttempts = 3
	for attempt := 0; ; attempt++ {
		result, err := sender.Send(ctx, msg)
		if err == nil {
			fmt.Printf("[%s] -> to=%s status=%s message_id=%s\n", label, msg.To, result.Status, result.ProviderMessageID)
			return
		}
		wait := grpop.FullJitterBackoff(200*time.Millisecond, 2*time.Second, attempt)
		if errors.Is(err, grpop.ErrPermanent) || attempt+1 >= maxAttempts || time.Now().Add(wait).After(expiresAt) {
			log.Fatalf("[%s] send failed after %d attempt(s): %v", label, attempt+1, err)
		}
		time.Sleep(wait)
	}
}

func smtpDispatcherDepsFromEnv(engine grpop.EmailTemplateEngine) grpop.SMTPDispatcherDeps {
	addr := envOrDefault("GRPOP_SMTP_ADDR", "localhost:1025")
	from := envOrDefault("GRPOP_SMTP_FROM", "no-reply@example.com")
	tlsMode := grpop.SMTPTLSMode(envOrDefault("GRPOP_SMTP_TLS_MODE", string(grpop.SMTPTLSInsecureNoTLS)))

	deps := grpop.SMTPDispatcherDeps{
		Addr:           addr,
		DefaultFrom:    from,
		TLSMode:        tlsMode,
		TemplateEngine: engine,
		ConnectTimeout: 5 * time.Second,
		SendTimeout:    15 * time.Second,
	}

	if user, pass := os.Getenv("GRPOP_SMTP_USER"), os.Getenv("GRPOP_SMTP_PASS"); user != "" && pass != "" {
		host, _, _ := net.SplitHostPort(addr)
		deps.Auth = smtp.PlainAuth("", user, pass, host)
	}

	return deps
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
