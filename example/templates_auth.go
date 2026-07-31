// File: example/templates_auth.go

package main

import "github.com/gourdian25/grpop"

// Placeholder email templates covering every real-world call site
// grpop-plan.md §2 grounds this design against: auth.ForgotPassword, and
// the three platform.Invites issuers (provider.IssueInvite,
// admin.IssueInvite, a tenant's first-admin invite). Copy these into the
// consuming application and adjust branding/copy — nothing here is meant to
// ship as-is.
//
// The three invite call sites share one template rather than three nearly
// identical copies: they already share one platform.Invites primitive
// (Issue/Consume/Peek) with an identical data shape (an opaque bearer
// token), so the only real difference between them is the sentence
// explaining who's inviting whom — carried as TemplateData's IntroLine,
// not baked into three separate templates.
const (
	// EmailTemplateForgotPassword renders auth.ForgotPassword's reset
	// email. Required TemplateData keys:
	//   - AppName          string
	//   - RecipientName    string (optional — falls back to "there")
	//   - ResetLink        string
	//   - ExpiresInMinutes int
	EmailTemplateForgotPassword = "auth_forgot_password"

	// EmailTemplateInvite renders any of the three platform.Invites
	// issuers' invite email. Required TemplateData keys:
	//   - AppName        string
	//   - TenantName     string
	//   - IntroLine      string (see the three inviteIntroLineFor*
	//     constants below for a starting point per call site)
	//   - InviteLink     string
	//   - ExpiresInHours int
	EmailTemplateInvite = "auth_invite"
)

// Starting-point IntroLine text for each of the three invite call sites —
// adjust freely. Each is a fmt.Sprintf format string (one %s for
// TenantName), not a grpop template: IntroLine's rendered value is inserted
// as a literal string into EmailTemplateInvite's own {{.IntroLine}}
// placeholder, which does not recursively re-execute template syntax
// embedded inside it — the substitution has to happen before it becomes
// TemplateData, hence Sprintf here rather than {{.TenantName}}.
const (
	inviteIntroLineForProviderInvite   = "You've been invited to join %s as a provider."
	inviteIntroLineForAdminInvite      = "You've been invited to join %s as an administrator."
	inviteIntroLineForTenantFirstAdmin = "Your new %s workspace is ready — you've been set up as its first administrator."
)

// registerAuthEmailTemplates registers every template this example sends.
// Call once at startup, before any Send call references these names.
func registerAuthEmailTemplates(engine grpop.EmailTemplateEngine) error {
	if err := engine.RegisterTemplate(EmailTemplateForgotPassword, grpop.EmailTemplate{
		SubjectTemplate:  `Reset your {{.AppName}} password`,
		HTMLBodyTemplate: forgotPasswordHTML,
		TextBodyTemplate: forgotPasswordText,
	}); err != nil {
		return err
	}

	if err := engine.RegisterTemplate(EmailTemplateInvite, grpop.EmailTemplate{
		SubjectTemplate:  `You're invited to join {{.TenantName}} on {{.AppName}}`,
		HTMLBodyTemplate: inviteHTML,
		TextBodyTemplate: inviteText,
	}); err != nil {
		return err
	}

	return nil
}

//nolint:gosec // G101 false positive: this is email copy containing the word "password", not a credential
const forgotPasswordHTML = `<!doctype html>
<html>
<body style="font-family: sans-serif; background: #f4f4f5; padding: 24px;">
  <div style="max-width: 480px; margin: 0 auto; background: #ffffff; border-radius: 8px; padding: 32px;">
    <h2 style="margin-top: 0;">Reset your password</h2>
    <p>Hi {{if .RecipientName}}{{.RecipientName}}{{else}}there{{end}},</p>
    <p>We received a request to reset your {{.AppName}} password. Click the button below to choose a new one:</p>
    <p style="text-align: center; margin: 32px 0;">
      <a href="{{.ResetLink}}" style="background: #2563eb; color: #ffffff; padding: 12px 24px; border-radius: 6px; text-decoration: none; display: inline-block;">Reset password</a>
    </p>
    <p style="color: #6b7280; font-size: 14px;">This link expires in {{.ExpiresInMinutes}} minutes. If you didn't request this, you can safely ignore this email.</p>
  </div>
</body>
</html>`

//nolint:gosec // G101 false positive: this is email copy containing the word "password", not a credential
const forgotPasswordText = `Hi {{if .RecipientName}}{{.RecipientName}}{{else}}there{{end}},

We received a request to reset your {{.AppName}} password. Use the link below to choose a new one:

{{.ResetLink}}

This link expires in {{.ExpiresInMinutes}} minutes. If you didn't request this, you can safely ignore this email.`

const inviteHTML = `<!doctype html>
<html>
<body style="font-family: sans-serif; background: #f4f4f5; padding: 24px;">
  <div style="max-width: 480px; margin: 0 auto; background: #ffffff; border-radius: 8px; padding: 32px;">
    <h2 style="margin-top: 0;">You're invited</h2>
    <p>{{.IntroLine}}</p>
    <p style="text-align: center; margin: 32px 0;">
      <a href="{{.InviteLink}}" style="background: #2563eb; color: #ffffff; padding: 12px 24px; border-radius: 6px; text-decoration: none; display: inline-block;">Accept invite</a>
    </p>
    <p style="color: #6b7280; font-size: 14px;">This invite expires in {{.ExpiresInHours}} hours. If you weren't expecting this, you can safely ignore this email.</p>
  </div>
</body>
</html>`

const inviteText = `{{.IntroLine}}

Accept your invite:

{{.InviteLink}}

This invite expires in {{.ExpiresInHours}} hours. If you weren't expecting this, you can safely ignore this email.`
