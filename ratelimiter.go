// File: ratelimiter.go

package grpop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// checkRateLimits asks a sender's two optional limiters whether one send to
// recipient may go now: perRecipient first (keyed by recipientRateLimitKey),
// then perChannel (keyed by the channel alone), so a send one recipient's
// bucket refuses spends nothing from the channel's. A refusal is
// ErrRateLimited and is counted in metrics; a limiter that fails is an error
// too, so a broken limiter stops sends rather than letting them all through.
// Both are temporary failures. Nil limiters check nothing.
func checkRateLimits(ctx context.Context, channel Channel, recipient string, perChannel, perRecipient RateLimiter, metrics Metrics) error {
	for _, check := range []struct {
		limiter RateLimiter
		key     string
	}{
		{perRecipient, recipientRateLimitKey(channel, recipient)},
		{perChannel, string(channel)},
	} {
		if check.limiter == nil {
			continue
		}
		allowed, err := check.limiter.Allow(ctx, check.key)
		if err != nil {
			return fmt.Errorf("grpop: rate limiter: %w", err)
		}
		if !allowed {
			if metrics != nil {
				metrics.IncRateLimitRejected(channel)
			}
			return ErrRateLimited
		}
	}
	return nil
}

// recipientRateLimitKey is the per-recipient bucket's key: the channel and a
// SHA-256 of the recipient, trimmed and lower-cased (an email address's case
// doesn't change its mailbox in practice, and a phone number has none), so
// the limiter's backend never stores an address or number.
func recipientRateLimitKey(channel Channel, recipient string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(recipient))))
	return string(channel) + ":" + hex.EncodeToString(sum[:])
}
