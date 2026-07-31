// File: dryrun.go

package grpop

import (
	"context"
	"time"
)

// dryRunEmailSender implements EmailSender by never contacting a real SMTP
// relay — see NewDryRunEmailSender.
type dryRunEmailSender struct {
	logger Logger
}

var _ EmailSender = (*dryRunEmailSender)(nil)

// NewDryRunEmailSender returns an EmailSender that never contacts a real
// vendor: it logs recipient, channel, and template name (or "literal-body"/
// "inline-template" if no TemplateName was set) at Info level, and returns
// a synthetic SendResult{Status: SendStatusSent, ProviderMessageID:
// "dryrun-<generated-id>"}.
//
// It deliberately does NOT log the rendered Subject/HTMLBody/TextBody or
// TemplateData — staging environments still write to shared log
// aggregation, and grpop's actual payloads are security-sensitive
// (password-reset links, invite tokens); logging a fully-rendered dry-run
// message would leak exactly the secret grpop exists to deliver into every
// staging log for however long retention lasts.
//
// A construction-time swap (pick this instead of NewSMTPDispatcher when
// building a staging ServiceDeps), not a runtime toggle inside the real
// dispatcher — kept deliberately simple.
//
// Distinct from MemoryEmailSender (memory.go): that one exists for
// contract/unit tests and deliberately DOES record full sends for
// assertion (test-only, never wired into a shared log sink); this one
// exists for a staging/pre-prod deployment that should never actually
// deliver mail but should otherwise exercise the real Service pipeline.
func NewDryRunEmailSender(logger Logger) EmailSender {
	return &dryRunEmailSender{logger: OrNop(logger)}
}

func (s *dryRunEmailSender) Send(ctx context.Context, msg EmailMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.logger.Info("grpop/dryrun: email not sent (dry run)", "to", msg.To, "content", emailContentDescription(msg))
	return SendResult{
		Channel:           ChannelEmail,
		ProviderMessageID: "dryrun-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

func (s *dryRunEmailSender) Close() error { return nil }

// emailContentDescription summarizes msg's active content mode for
// dry-run logging, without ever including the rendered/literal content
// itself.
func emailContentDescription(msg EmailMessage) string {
	switch {
	case msg.TemplateName != "":
		return msg.TemplateName
	case msg.InlineTemplate != nil:
		return "inline-template"
	default:
		return "literal-body"
	}
}

// dryRunWhatsAppSender implements WhatsAppSender by never contacting Meta's
// Graph API — see NewDryRunWhatsAppSender.
type dryRunWhatsAppSender struct {
	logger Logger
}

var _ WhatsAppSender = (*dryRunWhatsAppSender)(nil)

// NewDryRunWhatsAppSender returns a WhatsAppSender that never contacts
// Meta's Graph API. See NewDryRunEmailSender's doc comment for the full
// rationale (redaction, construction-time-swap convention) — identical
// here, substituting TemplateName (WhatsApp has no rendered body at all,
// see WhatsAppMessage's own doc comment) for email's content-mode
// description, and masking the recipient phone number the same way
// dispatcher.whatsapp.metacloud.go's own logging does.
func NewDryRunWhatsAppSender(logger Logger) WhatsAppSender {
	return &dryRunWhatsAppSender{logger: OrNop(logger)}
}

func (s *dryRunWhatsAppSender) Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.logger.Info("grpop/dryrun: whatsapp not sent (dry run)", "to", maskPhoneNumber(msg.To), "template", msg.TemplateName)
	return SendResult{
		Channel:           ChannelWhatsApp,
		ProviderMessageID: "dryrun-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

func (s *dryRunWhatsAppSender) Close() error { return nil }
