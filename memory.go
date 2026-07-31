// File: memory.go

package grpop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// generateMemoryID returns a random hex identifier, used only by memory.go's
// fake senders to synthesize a ProviderMessageID — stdlib crypto/rand only,
// matching grpop's zero-third-party-dependency dispatch layer even in its
// test doubles.
func generateMemoryID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read on the platforms Go supports never returns an error
	return hex.EncodeToString(b[:])
}

// memoryDLQHandlerConfig holds memoryDLQHandler's retry-backoff and
// history-cap tuning.
type memoryDLQHandlerConfig struct {
	maxRetries        int
	retryDelay        time.Duration
	maxRetryDelay     time.Duration
	maxAttemptHistory int
}

// memoryDLQHandler is an in-memory DLQHandler for tests and single-instance
// local dev — not for production use (no persistence across restarts). Its
// ClaimRetryableEvents implements the same atomic-claim contract every
// backend must (see DLQHandler's doc comment): the expiry sweep, the claim
// scan, and the pending->retrying status transition all happen under one
// mutex hold, so two concurrent callers on the same instance can never both
// claim the same event.
//
// This is the first implementation of grpop's ExpiresAt/DLQStatusExpired
// retry-deadline state machine — dlq.postgres.go and dlq.mongo.go repeat
// the same contract in SQL/Mongo, so bugs in the state machine itself
// should surface and get fixed here first.
type memoryDLQHandler struct {
	mu     sync.Mutex
	events map[string]*DLQEvent
	config memoryDLQHandlerConfig
}

var _ DLQHandler = (*memoryDLQHandler)(nil)

// NewMemoryDLQHandler constructs an in-memory DLQHandler.
//
// Parameters:
//   - maxRetries: int — defaults to 3 if <= 0
//   - retryDelay, maxRetryDelay: time.Duration — passed to
//     FullJitterBackoff for computing each event's NextRetryAt; unlike
//     maxRetries, 0 is a valid, deliberate choice here (immediate
//     retry-eligibility, useful for tests), not silently replaced with a
//     default
//   - maxAttemptHistory: int — caps DLQEvent.AttemptHistory, oldest entry
//     dropped first once exceeded; defaults to 20 if <= 0
func NewMemoryDLQHandler(maxRetries int, retryDelay, maxRetryDelay time.Duration, maxAttemptHistory int) DLQHandler {
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if maxAttemptHistory <= 0 {
		maxAttemptHistory = 20
	}
	return &memoryDLQHandler{
		events: make(map[string]*DLQEvent),
		config: memoryDLQHandlerConfig{
			maxRetries:        maxRetries,
			retryDelay:        retryDelay,
			maxRetryDelay:     maxRetryDelay,
			maxAttemptHistory: maxAttemptHistory,
		},
	}
}

func (h *memoryDLQHandler) PublishToDLQ(_ context.Context, sendID string, msg DLQMessage, failureReason string) error {
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.events[sendID]; ok {
		existing.MessageData = msg
		existing.FailureReason = failureReason
		existing.LastAttemptAt = now
		existing.UpdatedAt = now
		existing.AttemptHistory = appendAttemptCapped(existing.AttemptHistory, DLQRetryAttempt{
			AttemptNumber: existing.RetryCount,
			AttemptedAt:   now,
			Success:       false,
			ErrorMessage:  failureReason,
		}, h.config.maxAttemptHistory)
		return nil
	}

	h.events[sendID] = &DLQEvent{
		SendID:         sendID,
		MessageData:    msg,
		FailureReason:  failureReason,
		MaxRetries:     h.config.maxRetries,
		FirstFailureAt: now,
		LastAttemptAt:  now,
		NextRetryAt:    now.Add(h.config.retryDelay),
		Status:         DLQStatusPending,
		AttemptHistory: []DLQRetryAttempt{{AttemptNumber: 0, AttemptedAt: now, Success: false, ErrorMessage: failureReason}},
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	return nil
}

func (h *memoryDLQHandler) ClaimRetryableEvents(_ context.Context, limit int) ([]*DLQEvent, error) {
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()

	// Step 1: proactively expire any Pending event whose deadline has
	// passed, independent of the atomic-claim step below (see DLQHandler's
	// doc comment) — a zero ExpiresAt (only reachable by constructing a
	// DLQMessage directly, bypassing Service) is treated as "no deadline,"
	// never eligible for this sweep.
	for _, e := range h.events {
		if e.Status == DLQStatusPending && !e.MessageData.ExpiresAt.IsZero() && !e.MessageData.ExpiresAt.After(now) {
			e.Status = DLQStatusExpired
			e.UpdatedAt = now
		}
	}

	// Step 2: atomically claim whatever remains eligible.
	var candidates []*DLQEvent
	for _, e := range h.events {
		if e.Status != DLQStatusPending || e.NextRetryAt.After(now) {
			continue
		}
		if !e.MessageData.ExpiresAt.IsZero() && !e.MessageData.ExpiresAt.After(now) {
			continue
		}
		candidates = append(candidates, e)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].NextRetryAt.Before(candidates[j].NextRetryAt) })

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	claimed := make([]*DLQEvent, 0, len(candidates))
	for _, e := range candidates {
		e.Status = DLQStatusRetrying
		e.UpdatedAt = now
		copied := *e
		claimed = append(claimed, &copied)
	}
	return claimed, nil
}

func (h *memoryDLQHandler) MarkRetried(_ context.Context, sendID string, success bool, attemptErr error) error {
	now := time.Now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()

	e, ok := h.events[sendID]
	if !ok {
		return ErrDLQEventNotFound
	}
	if e.Status != DLQStatusRetrying {
		return ErrDLQEventNotClaimed
	}

	newRetryCount := e.RetryCount + 1
	errMsg := ""
	if attemptErr != nil {
		errMsg = attemptErr.Error()
	}

	switch {
	case success:
		e.Status = DLQStatusResolved
	default:
		nextRetryAt := now.Add(FullJitterBackoff(h.config.retryDelay, h.config.maxRetryDelay, newRetryCount))
		expiresAt := e.MessageData.ExpiresAt
		switch {
		// Checked before the MaxRetries case below: a message that expires
		// with retry budget still unused is reported as Expired, not
		// Exhausted (DLQHandler's doc comment).
		case !expiresAt.IsZero() && !nextRetryAt.Before(expiresAt):
			e.Status = DLQStatusExpired
		case newRetryCount >= e.MaxRetries:
			e.Status = DLQStatusExhausted
		default:
			e.Status = DLQStatusPending
			e.NextRetryAt = nextRetryAt
		}
	}

	e.RetryCount = newRetryCount
	e.LastAttemptAt = now
	e.UpdatedAt = now
	e.AttemptHistory = appendAttemptCapped(e.AttemptHistory, DLQRetryAttempt{
		AttemptNumber: newRetryCount,
		AttemptedAt:   now,
		Success:       success,
		ErrorMessage:  errMsg,
	}, h.config.maxAttemptHistory)
	return nil
}

// appendAttemptCapped appends attempt to history, dropping the oldest
// entries (FIFO) once max is exceeded. max<=0 disables the cap.
func appendAttemptCapped(history []DLQRetryAttempt, attempt DLQRetryAttempt, max int) []DLQRetryAttempt {
	history = append(history, attempt)
	if max > 0 && len(history) > max {
		history = history[len(history)-max:]
	}
	return history
}

func (h *memoryDLQHandler) GetEventByID(_ context.Context, sendID string) (*DLQEvent, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.events[sendID]
	if !ok {
		return nil, ErrDLQEventNotFound
	}
	copied := *e
	return &copied, nil
}

func (h *memoryDLQHandler) PurgeExpiredEvents(_ context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge)
	h.mu.Lock()
	defer h.mu.Unlock()
	var purged int64
	for id, e := range h.events {
		terminal := e.Status == DLQStatusResolved || e.Status == DLQStatusExhausted || e.Status == DLQStatusExpired
		if terminal || e.CreatedAt.Before(cutoff) {
			delete(h.events, id)
			purged++
		}
	}
	return purged, nil
}

func (h *memoryDLQHandler) Close() error { return nil }

// MemoryEmailSender is an in-memory EmailSender for tests and local dev —
// it never contacts a real vendor (no SMTP connection is made at all), and
// records every message passed to Send for later assertion via Sent().
//
// Distinct from NewDryRunEmailSender (dryrun.go): that one exists for a
// staging/pre-prod deployment and deliberately does NOT retain rendered
// message content (grpop's payloads are security-sensitive — see
// dryrun.go's own doc comment), logging only non-sensitive metadata.
// MemoryEmailSender is test-only scaffolding, never wired into a shared
// log sink, so it has no such reason to redact — recording full sends is
// the point.
type MemoryEmailSender struct {
	mu   sync.Mutex
	sent []EmailMessage
}

var _ EmailSender = (*MemoryEmailSender)(nil)

// NewMemoryEmailSender constructs a MemoryEmailSender.
func NewMemoryEmailSender() *MemoryEmailSender {
	return &MemoryEmailSender{}
}

func (s *MemoryEmailSender) Send(ctx context.Context, msg EmailMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return SendResult{
		Channel:           ChannelEmail,
		ProviderMessageID: "memory-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// Sent returns every EmailMessage passed to Send so far, in call order.
func (s *MemoryEmailSender) Sent() []EmailMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EmailMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *MemoryEmailSender) Close() error { return nil }

// MemoryWhatsAppSender is an in-memory WhatsAppSender for tests and local
// dev — it never contacts Meta's Graph API, and records every message
// passed to Send for later assertion via Sent(). See MemoryEmailSender's
// doc comment for why this differs from NewDryRunWhatsAppSender.
type MemoryWhatsAppSender struct {
	mu   sync.Mutex
	sent []WhatsAppMessage
}

var _ WhatsAppSender = (*MemoryWhatsAppSender)(nil)

// NewMemoryWhatsAppSender constructs a MemoryWhatsAppSender.
func NewMemoryWhatsAppSender() *MemoryWhatsAppSender {
	return &MemoryWhatsAppSender{}
}

func (s *MemoryWhatsAppSender) Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return SendResult{
		Channel:           ChannelWhatsApp,
		ProviderMessageID: "memory-" + generateMemoryID(),
		Status:            SendStatusSent,
		SentAt:            time.Now().UTC(),
	}, nil
}

// Sent returns every WhatsAppMessage passed to Send so far, in call order.
func (s *MemoryWhatsAppSender) Sent() []WhatsAppMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WhatsAppMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *MemoryWhatsAppSender) Close() error { return nil }
