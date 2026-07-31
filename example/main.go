// File: example/main.go

// Command example sends grpop's four grounding ERP use cases
// (grpop-plan.md §2: auth.ForgotPassword, provider.IssueInvite,
// admin.IssueInvite, a tenant's first-admin invite) through a real
// grpop.Service — idempotency, inline retry, DLQ-on-failure, all wired up —
// so the whole pipeline can be exercised end to end before being wired into
// a real application.
//
// Backends used here are deliberately the zero-external-dependency ones:
// grcache.NewMemoryCache (idempotency) and grpop.NewMemoryDLQHandler (DLQ)
// — swap these for NewCacheIdempotencyStore(a real Redis/Mongo grcache.Cache)
// and NewPostgresDLQHandler/NewMongoDLQHandler respectively once you're
// ready to run this for real; nothing about the Service/EmailMessage/
// SendOptions call sites below changes.
//
// WhatsApp is wired to NewDryRunWhatsAppSender (Service requires a
// WhatsAppSender even if this example only exercises email) — swap for
// NewMetaCloudWhatsAppDispatcher once real Meta credentials are available.
//
// Email, by default, targets a local Mailpit SMTP server (localhost:1025,
// no TLS, no auth) — the same one Stage 10's integration tests use — so it
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

	"github.com/gourdian25/grcache"
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

	idemCache, err := grcache.NewMemoryCache()
	if err != nil {
		log.Fatalf("construct memory cache: %v", err)
	}

	// Every constructor above can fail, so all deferred cleanup is
	// registered together only once construction has fully succeeded —
	// avoids a defer being silently skipped by an earlier log.Fatalf exit.
	defer func() { _ = emailSender.Close() }()
	defer func() { _ = idemCache.Close() }()

	// Service requires a WhatsAppSender too, even in an email-only demo —
	// dry run never contacts Meta, so no credentials are needed to run this.
	whatsAppSender := grpop.NewDryRunWhatsAppSender(nil)
	idempotencyStore := grpop.NewCacheIdempotencyStore(idemCache)
	dlqHandler := grpop.NewMemoryDLQHandler(5, time.Second, 30*time.Second, 20)
	defer func() { _ = dlqHandler.Close() }()

	svc, err := grpop.NewService(grpop.ServiceDeps{
		IdempotencyStore: idempotencyStore,
		DLQHandler:       dlqHandler,
		EmailSender:      emailSender,
		WhatsAppSender:   whatsAppSender,
		Config:           grpop.DefaultServiceConfig(),
	})
	if err != nil {
		//nolint:gocritic // NewService necessarily depends on the already-constructed,
		// already-deferred resources above; a short-lived CLI exiting here loses nothing
		// real (the OS reclaims everything on process exit)
		log.Fatalf("construct service: %v", err)
	}
	defer func() { _ = svc.Close() }()

	ctx := context.Background()
	const appName = "Skipp"
	const tenantName = "Acme Corp"
	now := time.Now()

	send(ctx, svc, "forgot password", grpop.EmailMessage{
		To:           "user@example.com",
		TemplateName: EmailTemplateForgotPassword,
		TemplateData: map[string]any{
			"AppName":          appName,
			"RecipientName":    "Ada",
			"ResetLink":        "https://app.skipp.co.in/reset-password?token=example-reset-token",
			"ExpiresInMinutes": 30,
		},
	}, grpop.SendOptions{
		IdempotencyKey: "demo-forgot-password",
		// A real caller should set this to the reset token's own real
		// expiry, not rely on ServiceConfig.DefaultMaxRetryAge (24h) —
		// see plan §9 item 12.
		RetryExpiresAt: now.Add(30 * time.Minute),
	})

	send(ctx, svc, "provider invite", grpop.EmailMessage{
		To:           "new-provider@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForProviderInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-provider-invite-token",
			"ExpiresInHours": 72,
		},
	}, grpop.SendOptions{IdempotencyKey: "demo-provider-invite", RetryExpiresAt: now.Add(72 * time.Hour)})

	send(ctx, svc, "admin invite", grpop.EmailMessage{
		To:           "new-admin@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForAdminInvite, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-admin-invite-token",
			"ExpiresInHours": 72,
		},
	}, grpop.SendOptions{IdempotencyKey: "demo-admin-invite", RetryExpiresAt: now.Add(72 * time.Hour)})

	send(ctx, svc, "tenant first-admin invite", grpop.EmailMessage{
		To:           "first-admin@example.com",
		TemplateName: EmailTemplateInvite,
		TemplateData: map[string]any{
			"AppName":        appName,
			"TenantName":     tenantName,
			"IntroLine":      fmt.Sprintf(inviteIntroLineForTenantFirstAdmin, tenantName),
			"InviteLink":     "https://app.skipp.co.in/invite/accept?token=example-first-admin-invite-token",
			"ExpiresInHours": 72,
		},
	}, grpop.SendOptions{IdempotencyKey: "demo-first-admin-invite", RetryExpiresAt: now.Add(72 * time.Hour)})

	// Calling the same key again demonstrates the idempotency short-circuit
	// — no second email is actually sent.
	result, err := svc.SendEmail(ctx, grpop.EmailMessage{
		To: "user@example.com", TemplateName: EmailTemplateForgotPassword,
		TemplateData: map[string]any{"AppName": appName, "ResetLink": "unused", "ExpiresInMinutes": 30},
	}, grpop.SendOptions{IdempotencyKey: "demo-forgot-password", RetryExpiresAt: now.Add(30 * time.Minute)})
	if err != nil {
		log.Fatalf("[forgot password, repeated] send failed: %v", err)
	}
	fmt.Printf("[forgot password, repeated] -> duplicate=%v (no second email sent)\n", result.Duplicate)

	fmt.Println("\nDone — check http://localhost:8025 (Mailpit's web UI) if using the default local target.")
}

func send(ctx context.Context, svc grpop.Service, label string, msg grpop.EmailMessage, opts grpop.SendOptions) {
	result, err := svc.SendEmail(ctx, msg, opts)
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
