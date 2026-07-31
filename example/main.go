// File: example/main.go

// Command example sends grpop's four grounding ERP use cases
// (grpop-plan.md §2: auth.ForgotPassword, provider.IssueInvite,
// admin.IssueInvite, a tenant's first-admin invite) as real emails over
// SMTP, so the templates + SMTP dispatcher can be exercised end to end
// before being wired into a real application. Service (idempotency/rate
// limiting/DLQ orchestration) isn't built yet, so this calls EmailSender.Send
// directly — a real app will eventually route these through
// Service.SendEmail instead, once that stage lands.
//
// By default this targets a local Mailpit SMTP server (localhost:1025, no
// TLS, no auth) — the same one Stage 10's integration tests use — so it
// runs with nothing but `docker run -d -p 1025:1025 -p 8025:8025
// axllent/mailpit` and no real credentials. Point it at a real relay via
// environment variables when you're ready to integrate for real:
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

	sender, err := grpop.NewSMTPDispatcher(smtpDispatcherDepsFromEnv(engine))
	if err != nil {
		log.Fatalf("construct SMTP dispatcher: %v", err)
	}
	defer func() { _ = sender.Close() }()

	ctx := context.Background()
	const appName = "Skipp"
	const tenantName = "Acme Corp"

	send(ctx, sender, "forgot password", grpop.EmailMessage{
		To:           "user@example.com",
		TemplateName: EmailTemplateForgotPassword,
		TemplateData: map[string]any{
			"AppName":          appName,
			"RecipientName":    "Ada",
			"ResetLink":        "https://app.skipp.co.in/reset-password?token=example-reset-token",
			"ExpiresInMinutes": 30,
		},
	})

	send(ctx, sender, "provider invite", grpop.EmailMessage{
		To:           "new-provider@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForProviderInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-provider-invite-token",
			"ExpiresInHours": 72,
		},
	})

	send(ctx, sender, "admin invite", grpop.EmailMessage{
		To:           "new-admin@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForAdminInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-admin-invite-token",
			"ExpiresInHours": 72,
		},
	})

	send(ctx, sender, "tenant first-admin invite", grpop.EmailMessage{
		To:           "first-admin@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForTenantFirstAdmin, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-first-admin-invite-token",
			"ExpiresInHours": 72,
		},
	})

	fmt.Println("\nDone — check http://localhost:8025 (Mailpit's web UI) if using the default local target.")
}

func send(ctx context.Context, sender grpop.EmailSender, label string, msg grpop.EmailMessage) {
	result, err := sender.Send(ctx, msg)
	if err != nil {
		log.Fatalf("[%s] send failed: %v", label, err)
	}
	fmt.Printf("[%s] -> to=%s status=%s message_id=%s\n", label, msg.To, result.Status, result.ProviderMessageID)
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
