// File: errors_test.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"testing"
)

// TestValidationSentinelsArePermanent: every failure a retry can't fix
// matches ErrPermanent, on its own and wrapped, and stays itself.
func TestValidationSentinelsArePermanent(t *testing.T) {
	sentinels := []error{
		ErrRecipientRequired, ErrNoContentModeSet, ErrMultipleContentModesSet,
		ErrEmailTemplateNotFound, ErrInlineTemplateTooLarge,
		ErrWhatsAppTemplateNameRequired, ErrWhatsAppLanguageCodeRequired,
		ErrWhatsAppTemplateNotApproved, ErrWhatsAppTemplateArityMismatch,
		ErrEmailFromRequired, ErrEmailTemplateEngineRequired, ErrInvalidAddress,
	}
	for _, sentinel := range sentinels {
		if !errors.Is(sentinel, ErrPermanent) {
			t.Errorf("%v does not match ErrPermanent", sentinel)
		}
		wrapped := fmt.Errorf("context: %w", sentinel)
		if !errors.Is(wrapped, ErrPermanent) || !errors.Is(wrapped, sentinel) {
			t.Errorf("%v wrapped lost ErrPermanent or itself", sentinel)
		}
	}
	if errors.Is(ErrRecipientRequired, ErrNoContentModeSet) {
		t.Error("two distinct sentinels match each other")
	}
}

// TestTemporaryErrorsAreNotPermanent: what is worth another attempt doesn't
// match ErrPermanent.
func TestTemporaryErrorsAreNotPermanent(t *testing.T) {
	for _, err := range []error{ErrRateLimited, ErrCircuitOpen, ErrTooManyRequests, context.Canceled, context.DeadlineExceeded, errors.New("connection refused")} {
		if errors.Is(err, ErrPermanent) {
			t.Errorf("%v matches ErrPermanent", err)
		}
	}
}

// TestPermanent_KeepsMessageAndChain: permanent adds ErrPermanent and
// changes nothing else.
func TestPermanent_KeepsMessageAndChain(t *testing.T) {
	reply := &textproto.Error{Code: 550, Msg: "no such user"}
	inner := fmt.Errorf("grpop/smtp: rcpt to: %w", reply)
	err := permanent(inner)
	if err.Error() != inner.Error() {
		t.Fatalf("Error() = %q, want %q", err.Error(), inner.Error())
	}
	if !errors.Is(err, ErrPermanent) || !errors.Is(err, inner) {
		t.Fatal("permanent(err) lost ErrPermanent or err")
	}
	var got *textproto.Error
	if !errors.As(err, &got) || got.Code != 550 {
		t.Fatalf("errors.As through permanent = %v, want the 550 reply", got)
	}
	if permanent(nil) != nil {
		t.Fatal("permanent(nil) != nil")
	}
}
