// File: templateengine.email_test.go

package grpop

import (
	"errors"
	"strings"
	"testing"
)

func TestEmailTemplateEngine_RegisterAndRender(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	err := e.RegisterTemplate("welcome", EmailTemplate{
		SubjectTemplate:  "Welcome, {{.Name}}!",
		HTMLBodyTemplate: "<p>Hi {{.Name}}, welcome aboard.</p>",
		TextBodyTemplate: "Hi {{.Name}}, welcome aboard.",
	})
	if err != nil {
		t.Fatalf("RegisterTemplate: %v", err)
	}

	subject, htmlBody, textBody, err := e.Render("welcome", map[string]any{"Name": "Ada"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if subject != "Welcome, Ada!" {
		t.Fatalf("subject = %q, want %q", subject, "Welcome, Ada!")
	}
	if htmlBody != "<p>Hi Ada, welcome aboard.</p>" {
		t.Fatalf("htmlBody = %q, want %q", htmlBody, "<p>Hi Ada, welcome aboard.</p>")
	}
	if textBody != "Hi Ada, welcome aboard." {
		t.Fatalf("textBody = %q, want %q", textBody, "Hi Ada, welcome aboard.")
	}
}

// TestEmailTemplateEngine_HTMLBodyAutoEscapes proves HTMLBodyTemplate uses
// html/template (context-aware auto-escaping), not text/template — a data
// value containing HTML-significant characters must come out escaped,
// unlike Subject/TextBody which use text/template and pass values through
// verbatim.
func TestEmailTemplateEngine_HTMLBodyAutoEscapes(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	_ = e.RegisterTemplate("t", EmailTemplate{
		SubjectTemplate:  "{{.Name}}",
		HTMLBodyTemplate: "<p>{{.Name}}</p>",
		TextBodyTemplate: "{{.Name}}",
	})

	malicious := `<script>alert(1)</script>`
	subject, htmlBody, textBody, err := e.Render("t", map[string]any{"Name": malicious})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(htmlBody, "<script>") {
		t.Fatalf("htmlBody = %q, want the script tag HTML-escaped", htmlBody)
	}
	// Subject/TextBody use text/template, which does NOT HTML-escape —
	// they are not rendered into an HTML document, so escaping would be
	// wrong here, not a missing protection.
	if subject != malicious {
		t.Fatalf("subject = %q, want verbatim (text/template, not HTML-escaped)", subject)
	}
	if textBody != malicious {
		t.Fatalf("textBody = %q, want verbatim (text/template, not HTML-escaped)", textBody)
	}
}

func TestEmailTemplateEngine_Render_NotFound(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	_, _, _, err := e.Render("never-registered", nil)
	if !errors.Is(err, ErrEmailTemplateNotFound) {
		t.Fatalf("Render(unregistered) error = %v, want ErrEmailTemplateNotFound", err)
	}
}

func TestEmailTemplateEngine_RegisterTemplate_InvalidSyntax(t *testing.T) {
	cases := []struct {
		name string
		tmpl EmailTemplate
	}{
		{"subject", EmailTemplate{SubjectTemplate: "{{.Unclosed"}},
		{"htmlBody", EmailTemplate{HTMLBodyTemplate: "{{.Unclosed"}},
		{"textBody", EmailTemplate{TextBodyTemplate: "{{.Unclosed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
			if err := e.RegisterTemplate("broken", tc.tmpl); err == nil {
				t.Fatal("RegisterTemplate(invalid template syntax) = nil error, want non-nil")
			}
		})
	}
}

func TestEmailTemplateEngine_Render_ExecutionError(t *testing.T) {
	cases := []struct {
		name string
		tmpl EmailTemplate
	}{
		{"subject", EmailTemplate{SubjectTemplate: "{{len .N}}"}},
		{"htmlBody", EmailTemplate{HTMLBodyTemplate: "{{len .N}}"}},
		{"textBody", EmailTemplate{TextBodyTemplate: "{{len .N}}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
			if err := e.RegisterTemplate("t", tc.tmpl); err != nil {
				t.Fatalf("RegisterTemplate: %v", err)
			}
			// len() of an int is a template execution-time error, not a
			// parse-time one — data is only known at Render/RenderInline time.
			_, _, _, err := e.Render("t", map[string]any{"N": 5})
			if err == nil {
				t.Fatal("Render(execution error) = nil error, want non-nil")
			}
		})
	}
}

func TestEmailTemplateEngine_RenderInline(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	tmpl := EmailTemplate{
		SubjectTemplate:  "Invite from {{.TenantName}}",
		HTMLBodyTemplate: "<p>Join {{.TenantName}}: {{.Link}}</p>",
		TextBodyTemplate: "Join {{.TenantName}}: {{.Link}}",
	}
	subject, htmlBody, textBody, err := e.RenderInline(tmpl, map[string]any{"TenantName": "Acme", "Link": "https://example.com/i/1"})
	if err != nil {
		t.Fatalf("RenderInline: %v", err)
	}
	if subject != "Invite from Acme" {
		t.Fatalf("subject = %q, want %q", subject, "Invite from Acme")
	}
	if !strings.Contains(htmlBody, "https://example.com/i/1") {
		t.Fatalf("htmlBody = %q, want it to contain the link", htmlBody)
	}
	if !strings.Contains(textBody, "https://example.com/i/1") {
		t.Fatalf("textBody = %q, want it to contain the link", textBody)
	}
}

func TestEmailTemplateEngine_RenderInline_NoPriorRegistrationNeeded(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	// No RegisterTemplate call at all — RenderInline must still work.
	_, _, _, err := e.RenderInline(EmailTemplate{SubjectTemplate: "hi"}, nil)
	if err != nil {
		t.Fatalf("RenderInline (no prior registration): %v", err)
	}
}

func TestEmailTemplateEngine_RenderInline_TooLarge(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{MaxInlineTemplateBytes: 10})
	tmpl := EmailTemplate{SubjectTemplate: "this subject template is definitely longer than 10 bytes"}
	_, _, _, err := e.RenderInline(tmpl, nil)
	if !errors.Is(err, ErrInlineTemplateTooLarge) {
		t.Fatalf("RenderInline(oversized) error = %v, want ErrInlineTemplateTooLarge", err)
	}
}

func TestEmailTemplateEngine_RenderInline_InvalidSyntax(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	_, _, _, err := e.RenderInline(EmailTemplate{SubjectTemplate: "{{.Unclosed"}, nil)
	if err == nil {
		t.Fatal("RenderInline(invalid template syntax) = nil error, want non-nil")
	}
}

func TestNewEmailTemplateEngine_DefaultsMaxInlineTemplateBytes(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{}).(*defaultEmailTemplateEngine)
	if e.config.MaxInlineTemplateBytes != defaultMaxInlineTemplateBytes {
		t.Fatalf("MaxInlineTemplateBytes = %d, want %d (the default)", e.config.MaxInlineTemplateBytes, defaultMaxInlineTemplateBytes)
	}
}

func TestEmailTemplateEngine_RegisterTemplate_OverwritesExisting(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	_ = e.RegisterTemplate("t", EmailTemplate{SubjectTemplate: "v1"})
	_ = e.RegisterTemplate("t", EmailTemplate{SubjectTemplate: "v2"})

	subject, _, _, err := e.Render("t", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if subject != "v2" {
		t.Fatalf("subject = %q, want %q (re-registration should overwrite)", subject, "v2")
	}
}

func TestEmailTemplateEngine_EmptyTextBodyTemplateRendersEmpty(t *testing.T) {
	e := NewEmailTemplateEngine(EmailTemplateEngineConfig{})
	_ = e.RegisterTemplate("t", EmailTemplate{SubjectTemplate: "hi", HTMLBodyTemplate: "<p>hi</p>"})
	_, _, textBody, err := e.Render("t", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if textBody != "" {
		t.Fatalf("textBody = %q, want empty (TextBodyTemplate was never set)", textBody)
	}
}
