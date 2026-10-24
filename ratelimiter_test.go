// File: ratelimiter_test.go

package grpop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCheckRateLimits_NilLimitersCheckNothing: a sender with no limiters
// sends.
func TestCheckRateLimits_NilLimitersCheckNothing(t *testing.T) {
	if err := checkRateLimits(context.Background(), ChannelEmail, "a@b.com", nil, nil, nil); err != nil {
		t.Fatalf("checkRateLimits with no limiters = %v, want nil", err)
	}
}

// TestCheckRateLimits_AsksRecipientThenChannel: both allow, so both are
// asked, the recipient's bucket first, then the channel's.
func TestCheckRateLimits_AsksRecipientThenChannel(t *testing.T) {
	var order []string
	perChannel := &orderedLimiter{name: "channel", order: &order}
	perRecipient := &orderedLimiter{name: "recipient", order: &order}
	if err := checkRateLimits(context.Background(), ChannelWhatsApp, "+15551234567", perChannel, perRecipient, nil); err != nil {
		t.Fatalf("checkRateLimits = %v, want nil", err)
	}
	if strings.Join(order, ",") != "recipient,channel" {
		t.Fatalf("asked %v, want recipient then channel", order)
	}
}

// TestCheckRateLimits_RecipientRefusalSpendsNothingOnTheChannel: the
// recipient's refusal stops the check before the channel's bucket is asked.
func TestCheckRateLimits_RecipientRefusalSpendsNothingOnTheChannel(t *testing.T) {
	perChannel := &fakeRateLimiter{}
	perRecipient := &fakeRateLimiter{refuse: true}
	metrics := &fakeMetrics{}
	err := checkRateLimits(context.Background(), ChannelEmail, "a@b.com", perChannel, perRecipient, metrics)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("checkRateLimits = %v, want ErrRateLimited", err)
	}
	if got := perChannel.asked(); len(got) != 0 {
		t.Fatalf("the channel limiter was asked about %v, want nothing", got)
	}
	if metrics.rateLimitRejected != 1 {
		t.Fatalf("rateLimitRejected = %d, want 1", metrics.rateLimitRejected)
	}
}

// TestCheckRateLimits_ChannelRefusal: the channel's bucket refuses, keyed
// by the channel alone.
func TestCheckRateLimits_ChannelRefusal(t *testing.T) {
	perChannel := &fakeRateLimiter{refuse: true}
	err := checkRateLimits(context.Background(), ChannelEmail, "a@b.com", perChannel, nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("checkRateLimits = %v, want ErrRateLimited", err)
	}
	if got := perChannel.asked(); len(got) != 1 || got[0] != "email" {
		t.Fatalf("the channel limiter was asked about %v, want [email]", got)
	}
}

// TestCheckRateLimits_LimiterErrorStopsTheSend: a failing limiter refuses
// the send (fail closed) with its own error, not ErrRateLimited, and counts
// no rejection.
func TestCheckRateLimits_LimiterErrorStopsTheSend(t *testing.T) {
	backend := errors.New("redis down")
	metrics := &fakeMetrics{}
	err := checkRateLimits(context.Background(), ChannelEmail, "a@b.com", &fakeRateLimiter{err: backend}, nil, metrics)
	if !errors.Is(err, backend) || errors.Is(err, ErrRateLimited) {
		t.Fatalf("checkRateLimits = %v, want the limiter's error and not ErrRateLimited", err)
	}
	if metrics.rateLimitRejected != 0 {
		t.Fatalf("rateLimitRejected = %d, want 0", metrics.rateLimitRejected)
	}
}

// TestRecipientRateLimitKey_NeverHoldsTheAddress: the key is the channel and
// a hash, the same for any case or surrounding space, different per channel
// and per recipient.
func TestRecipientRateLimitKey_NeverHoldsTheAddress(t *testing.T) {
	key := recipientRateLimitKey(ChannelEmail, "Ada@Example.com")
	if strings.Contains(strings.ToLower(key), "ada") || strings.Contains(key, "example") {
		t.Fatalf("key %q carries the address", key)
	}
	if !strings.HasPrefix(key, "email:") || len(key) != len("email:")+64 {
		t.Fatalf("key %q, want email: and a SHA-256 hex", key)
	}
	if recipientRateLimitKey(ChannelEmail, "  ada@example.com ") != key {
		t.Fatal("the same address in another case or with spaces got another key")
	}
	if recipientRateLimitKey(ChannelEmail, "bob@example.com") == key {
		t.Fatal("two addresses share a key")
	}
	if recipientRateLimitKey(ChannelWhatsApp, "Ada@Example.com") == key {
		t.Fatal("two channels share a key")
	}
}

// orderedLimiter allows everything and appends its name to order when asked.
type orderedLimiter struct {
	name  string
	order *[]string
}

func (l *orderedLimiter) Allow(context.Context, string) (bool, error) {
	*l.order = append(*l.order, l.name)
	return true, nil
}
