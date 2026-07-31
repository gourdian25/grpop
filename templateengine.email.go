// File: templateengine.email.go

package grpop

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"strings"
	"sync"
	texttemplate "text/template"
)

// defaultMaxInlineTemplateBytes is used whenever EmailTemplateEngineConfig
// is given a MaxInlineTemplateBytes <= 0. See EmailTemplate's own doc
// comment (types.go) for why this bound exists: RenderInline is never
// cached the way Render is, so an oversized/pathological inline template
// would otherwise cost unbounded parse/render CPU on every call.
const defaultMaxInlineTemplateBytes = 64 * 1024

// EmailTemplateEngineConfig tunes defaultEmailTemplateEngine.
type EmailTemplateEngineConfig struct {
	// MaxInlineTemplateBytes bounds RenderInline's combined
	// Subject+HTMLBody+TextBody template source size. Defaults to
	// defaultMaxInlineTemplateBytes if <= 0. Never applied to
	// RegisterTemplate/Render — a registered template is compiled once and
	// reused, so its one-time compile cost isn't the same unbounded-CPU-
	// per-call concern RenderInline has.
	MaxInlineTemplateBytes int
}

// compiledEmailTemplate holds one EmailTemplate's three parsed template
// sources. HTMLBodyTemplate specifically is html/template, not
// text/template — see EmailTemplateEngine's own doc comment for why.
type compiledEmailTemplate struct {
	subjectTmpl  *texttemplate.Template
	htmlBodyTmpl *htmltemplate.Template
	textBodyTmpl *texttemplate.Template
}

// compileEmailTemplate parses tmpl's three template sources. Shared by
// RegisterTemplate (compiled once, cached) and RenderInline (compiled
// fresh every call) so there is exactly one parsing/execution code path
// for both, not two independently-written copies that could drift.
func compileEmailTemplate(tmpl EmailTemplate) (*compiledEmailTemplate, error) {
	subjectTmpl, err := texttemplate.New("subject").Parse(tmpl.SubjectTemplate)
	if err != nil {
		return nil, fmt.Errorf("grpop: parsing subject template: %w", err)
	}
	htmlBodyTmpl, err := htmltemplate.New("htmlBody").Parse(tmpl.HTMLBodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("grpop: parsing HTML body template: %w", err)
	}
	textBodyTmpl, err := texttemplate.New("textBody").Parse(tmpl.TextBodyTemplate)
	if err != nil {
		return nil, fmt.Errorf("grpop: parsing text body template: %w", err)
	}
	return &compiledEmailTemplate{subjectTmpl: subjectTmpl, htmlBodyTmpl: htmlBodyTmpl, textBodyTmpl: textBodyTmpl}, nil
}

// executeEmailTemplate renders compiled against data. Subject and TextBody
// are whitespace-trimmed (template control structures like {{if}}/{{end}}
// commonly leave stray leading/trailing newlines, and both fields are
// meant to be plain, single-purpose text); HTMLBody is left exactly as
// rendered, since trimming isn't meaningful for a full HTML document
// fragment and could in principle affect content inside a preformatted
// element.
func executeEmailTemplate(compiled *compiledEmailTemplate, data map[string]any) (subject, htmlBody, textBody string, err error) {
	var subjectBuf, htmlBuf, textBuf bytes.Buffer
	if err := compiled.subjectTmpl.Execute(&subjectBuf, data); err != nil {
		return "", "", "", fmt.Errorf("grpop: rendering subject: %w", err)
	}
	if err := compiled.htmlBodyTmpl.Execute(&htmlBuf, data); err != nil {
		return "", "", "", fmt.Errorf("grpop: rendering HTML body: %w", err)
	}
	if err := compiled.textBodyTmpl.Execute(&textBuf, data); err != nil {
		return "", "", "", fmt.Errorf("grpop: rendering text body: %w", err)
	}
	return strings.TrimSpace(subjectBuf.String()), htmlBuf.String(), strings.TrimSpace(textBuf.String()), nil
}

type defaultEmailTemplateEngine struct {
	mu        sync.RWMutex
	templates map[string]*compiledEmailTemplate
	config    EmailTemplateEngineConfig
}

var _ EmailTemplateEngine = (*defaultEmailTemplateEngine)(nil)

// NewEmailTemplateEngine constructs an EmailTemplateEngine.
func NewEmailTemplateEngine(config EmailTemplateEngineConfig) EmailTemplateEngine {
	if config.MaxInlineTemplateBytes <= 0 {
		config.MaxInlineTemplateBytes = defaultMaxInlineTemplateBytes
	}
	return &defaultEmailTemplateEngine{
		templates: make(map[string]*compiledEmailTemplate),
		config:    config,
	}
}

func (e *defaultEmailTemplateEngine) RegisterTemplate(name string, tmpl EmailTemplate) error {
	compiled, err := compileEmailTemplate(tmpl)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.templates[name] = compiled
	return nil
}

func (e *defaultEmailTemplateEngine) Render(name string, data map[string]any) (subject, htmlBody, textBody string, err error) {
	e.mu.RLock()
	compiled, ok := e.templates[name]
	e.mu.RUnlock()
	if !ok {
		return "", "", "", fmt.Errorf("%w: %s", ErrEmailTemplateNotFound, name)
	}
	return executeEmailTemplate(compiled, data)
}

func (e *defaultEmailTemplateEngine) RenderInline(tmpl EmailTemplate, data map[string]any) (subject, htmlBody, textBody string, err error) {
	combined := len(tmpl.SubjectTemplate) + len(tmpl.HTMLBodyTemplate) + len(tmpl.TextBodyTemplate)
	if combined > e.config.MaxInlineTemplateBytes {
		return "", "", "", ErrInlineTemplateTooLarge
	}
	compiled, err := compileEmailTemplate(tmpl)
	if err != nil {
		return "", "", "", err
	}
	return executeEmailTemplate(compiled, data)
}
