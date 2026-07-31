// File: dlq.postgres.go

package grpop

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gourdian25/grpop/internal/postgresdb"
)

// PostgresDLQHandlerConfig configures a DLQHandler constructed by
// NewPostgresDLQHandler — the primary DLQ backend.
type PostgresDLQHandlerConfig struct {
	PostgresConfig
	MaxRetries    int           // defaults to 3
	RetryDelay    time.Duration // 0 is a valid, deliberate "immediately retry-eligible" choice — not defaulted
	MaxRetryDelay time.Duration // passed through to FullJitterBackoff as-is

	// MaxAttemptHistoryEntries caps DLQEvent.AttemptHistory, oldest entry
	// dropped first once exceeded. Defaults to 20 if <= 0. Enforced in SQL
	// (see internal/postgresdb/queries/dlq.sql's UpsertDLQEvent/
	// FinalizeRetryPending/FinalizeRetryTerminal), not in Go — unlike
	// memoryDLQHandler's appendAttemptCapped, which runs after a round trip
	// through Go anyway.
	MaxAttemptHistoryEntries int

	// Encryptor, if set, encrypts DLQMessage's serialized bytes before
	// they're written to message_data, and decrypts on read back. Nil (the
	// default) stores message_data in the clear — see docs.go's warning
	// about operating grpop_dlq with credentials-table-grade access
	// control in that case.
	Encryptor MessageEncryptor
}

type postgresDLQHandler struct {
	pool              *pgxpool.Pool
	queries           *postgresdb.Queries
	maxRetries        int
	retryDelay        time.Duration
	maxRetryDelay     time.Duration
	maxAttemptHistory int
	encryptor         MessageEncryptor
	logger            Logger
	ownsPool          bool

	closed    atomic.Bool
	closeOnce sync.Once
}

var _ DLQHandler = (*postgresDLQHandler)(nil)

// NewPostgresDLQHandler connects per cfg.
//
// Claim semantics: ClaimRetryableEvents runs an expiry sweep (a plain
// UPDATE) followed by a single UPDATE statement whose subquery uses
// SELECT ... FOR UPDATE SKIP LOCKED to let N concurrent callers each claim
// a disjoint batch of pending events without contention — see
// interfaces.go's DLQHandler doc comment and
// internal/postgresdb/queries/dlq.sql for the full two-statement sequence.
func NewPostgresDLQHandler(cfg PostgresDLQHandlerConfig) (DLQHandler, error) {
	pool, queries, ownsPool, err := connectPostgres(context.Background(), cfg.PostgresConfig, "DLQHandler")
	if err != nil {
		return nil, err
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	maxAttemptHistory := cfg.MaxAttemptHistoryEntries
	if maxAttemptHistory <= 0 {
		maxAttemptHistory = 20
	}
	logger := OrNop(cfg.Logger)
	logger.Info("grpop/postgres: dlq handler connected")
	return &postgresDLQHandler{
		pool: pool, queries: queries,
		maxRetries: maxRetries, retryDelay: cfg.RetryDelay, maxRetryDelay: cfg.MaxRetryDelay,
		maxAttemptHistory: maxAttemptHistory, encryptor: cfg.Encryptor,
		logger: logger, ownsPool: ownsPool,
	}, nil
}

// encodeMessageData serializes msg to JSON, then encrypts it if an
// Encryptor is configured.
//
// message_data is a JSONB column, which requires valid UTF8 JSON text —
// arbitrary ciphertext bytes are neither in general (a real AES-GCM
// Encryptor, or the fake XOR one in tests, both routinely produce bytes
// like 0x00 that Postgres rejects with "invalid byte sequence for
// encoding UTF8"). So the encrypted path base64-encodes the ciphertext and
// wraps it as a JSON string scalar (itself valid JSONB content) rather
// than writing the raw ciphertext bytes directly.
func (h *postgresDLQHandler) encodeMessageData(msg DLQMessage) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: encode message data: %w", err)
	}
	if h.encryptor == nil {
		return raw, nil
	}
	encrypted, err := h.encryptor.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: encrypt message data: %w", err)
	}
	wrapped, err := json.Marshal(base64.StdEncoding.EncodeToString(encrypted))
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: encode encrypted message data: %w", err)
	}
	return wrapped, nil
}

// decodeMessageData reverses encodeMessageData: unwraps the base64
// envelope and decrypts (if an Encryptor is configured), then unmarshals.
func (h *postgresDLQHandler) decodeMessageData(data []byte) (DLQMessage, error) {
	raw := data
	if h.encryptor != nil {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/postgres: decode encrypted message data envelope: %w", err)
		}
		encrypted, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/postgres: base64-decode encrypted message data: %w", err)
		}
		decrypted, err := h.encryptor.Decrypt(encrypted)
		if err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/postgres: decrypt message data: %w", err)
		}
		raw = decrypted
	}
	var msg DLQMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &msg); err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/postgres: decode message data: %w", err)
		}
	}
	return msg, nil
}

// rowToDomain converts a raw row to a *DLQEvent. Channel/ExpiresAt are
// taken from the row's own indexed columns, not the decoded message_data
// blob's embedded copies — the columns are the authoritative, queryable
// source of truth (see schema.sql), and always agree with the blob since
// both are written from the same DLQMessage at publish time, but preferring
// the columns costs nothing and removes any doubt.
func (h *postgresDLQHandler) rowToDomain(r postgresdb.GrpopDlq) (*DLQEvent, error) {
	msg, err := h.decodeMessageData(r.MessageData)
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: decode message data for %s: %w", r.SendID, err)
	}
	msg.Channel = Channel(r.Channel)
	msg.ExpiresAt = pgTime(r.ExpiresAt)

	var history []DLQRetryAttempt
	if len(r.AttemptHistory) > 0 {
		if err := json.Unmarshal(r.AttemptHistory, &history); err != nil {
			return nil, fmt.Errorf("grpop/postgres: decode attempt history for %s: %w", r.SendID, err)
		}
	}
	return &DLQEvent{
		SendID: r.SendID, MessageData: msg, FailureReason: r.FailureReason,
		RetryCount: int(r.RetryCount), MaxRetries: int(r.MaxRetries),
		FirstFailureAt: pgTime(r.FirstFailureAt), LastAttemptAt: pgTime(r.LastAttemptAt), NextRetryAt: pgTime(r.NextRetryAt),
		Status: DLQStatus(r.Status), AttemptHistory: history,
		CreatedAt: pgTime(r.CreatedAt), UpdatedAt: pgTime(r.UpdatedAt),
	}, nil
}

func (h *postgresDLQHandler) PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error {
	if h.closed.Load() {
		return ErrClosed
	}
	now := time.Now().UTC()

	messageData, err := h.encodeMessageData(msg)
	if err != nil {
		return err
	}
	attemptJSON, err := json.Marshal([]DLQRetryAttempt{{AttemptNumber: 0, AttemptedAt: now, Success: false, ErrorMessage: failureReason}})
	if err != nil {
		return fmt.Errorf("grpop/postgres: encode attempt for %s: %w", sendID, err)
	}

	err = h.queries.UpsertDLQEvent(ctx, postgresdb.UpsertDLQEventParams{
		SendID: sendID, Channel: string(msg.Channel), MessageData: messageData, FailureReason: failureReason,
		MaxRetries: pgInt32(h.maxRetries), FirstFailureAt: pgTimestamptz(now),
		NextRetryAt: pgTimestamptz(now.Add(h.retryDelay)), ExpiresAt: pgTimestamptz(msg.ExpiresAt),
		Status: string(DLQStatusPending), AttemptHistory: attemptJSON, Column11: pgInt32(h.maxAttemptHistory),
	})
	if err != nil {
		return fmt.Errorf("grpop/postgres: publish to dlq %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return nil
}

func (h *postgresDLQHandler) ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error) {
	if h.closed.Load() {
		return nil, ErrClosed
	}
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()

	if err := h.queries.ExpirePastDeadlineEvents(ctx, postgresdb.ExpirePastDeadlineEventsParams{
		ExpiresAt: pgTimestamptz(now), UpdatedAt: pgTimestamptz(now),
	}); err != nil {
		return nil, fmt.Errorf("grpop/postgres: expire past-deadline events: %w", errors.Join(err, ErrBackendUnavailable))
	}

	rows, err := h.queries.ClaimRetryableEvents(ctx, postgresdb.ClaimRetryableEventsParams{
		NextRetryAt: pgTimestamptz(now), UpdatedAt: pgTimestamptz(now), Limit: pgInt32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: claim retryable events: %w", errors.Join(err, ErrBackendUnavailable))
	}
	claimed := make([]*DLQEvent, 0, len(rows))
	for _, r := range rows {
		e, err := h.rowToDomain(r)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, e)
	}
	return claimed, nil
}

func (h *postgresDLQHandler) MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error {
	if h.closed.Load() {
		return ErrClosed
	}
	now := time.Now().UTC()

	// Step 1: atomically increment retry_count, scoped to the claimed
	// ("retrying") state — the WHERE clause is what makes this safe.
	row, err := h.queries.IncrementRetryCount(ctx, postgresdb.IncrementRetryCountParams{
		SendID: sendID, LastAttemptAt: pgTimestamptz(now),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, getErr := h.GetEventByID(ctx, sendID); errors.Is(getErr, ErrDLQEventNotFound) {
				return ErrDLQEventNotFound
			}
			return ErrDLQEventNotClaimed
		}
		return fmt.Errorf("grpop/postgres: mark retried %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}

	errMsg := ""
	if attemptErr != nil {
		errMsg = attemptErr.Error()
	}
	retryCount := int(row.RetryCount)
	attemptJSON, jsonErr := json.Marshal([]DLQRetryAttempt{{AttemptNumber: retryCount, AttemptedAt: now, Success: success, ErrorMessage: errMsg}})
	if jsonErr != nil {
		return fmt.Errorf("grpop/postgres: encode attempt for %s: %w", sendID, jsonErr)
	}

	// Step 2: finalize status/next_retry_at and record the attempt. The
	// expiry check runs before the MaxRetries check — a message that
	// expires with retry budget still unused is reported as Expired, not
	// Exhausted (see DLQHandler's doc comment).
	switch {
	case success:
		err = h.queries.FinalizeRetryTerminal(ctx, postgresdb.FinalizeRetryTerminalParams{
			SendID: sendID, Status: string(DLQStatusResolved), UpdatedAt: pgTimestamptz(now),
			AttemptHistory: attemptJSON, Column5: pgInt32(h.maxAttemptHistory),
		})
	default:
		nextRetryAt := now.Add(FullJitterBackoff(h.retryDelay, h.maxRetryDelay, retryCount))
		expiresAt := pgTime(row.ExpiresAt)
		switch {
		case !expiresAt.IsZero() && !nextRetryAt.Before(expiresAt):
			err = h.queries.FinalizeRetryTerminal(ctx, postgresdb.FinalizeRetryTerminalParams{
				SendID: sendID, Status: string(DLQStatusExpired), UpdatedAt: pgTimestamptz(now),
				AttemptHistory: attemptJSON, Column5: pgInt32(h.maxAttemptHistory),
			})
		case retryCount >= int(row.MaxRetries):
			err = h.queries.FinalizeRetryTerminal(ctx, postgresdb.FinalizeRetryTerminalParams{
				SendID: sendID, Status: string(DLQStatusExhausted), UpdatedAt: pgTimestamptz(now),
				AttemptHistory: attemptJSON, Column5: pgInt32(h.maxAttemptHistory),
			})
		default:
			err = h.queries.FinalizeRetryPending(ctx, postgresdb.FinalizeRetryPendingParams{
				SendID: sendID, NextRetryAt: pgTimestamptz(nextRetryAt), UpdatedAt: pgTimestamptz(now),
				AttemptHistory: attemptJSON, Column5: pgInt32(h.maxAttemptHistory),
			})
		}
	}
	if err != nil {
		return fmt.Errorf("grpop/postgres: mark retried %s (finalize): %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return nil
}

func (h *postgresDLQHandler) GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error) {
	if h.closed.Load() {
		return nil, ErrClosed
	}
	row, err := h.queries.GetDLQEventByID(ctx, sendID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDLQEventNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("grpop/postgres: get event %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return h.rowToDomain(row)
}

func (h *postgresDLQHandler) PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error) {
	if h.closed.Load() {
		return 0, ErrClosed
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	purged, err := h.queries.PurgeExpiredEvents(ctx, pgTimestamptz(cutoff))
	if err != nil {
		return 0, fmt.Errorf("grpop/postgres: purge expired events: %w", errors.Join(err, ErrBackendUnavailable))
	}
	return purged, nil
}

func (h *postgresDLQHandler) Close() error {
	h.closeOnce.Do(func() {
		h.closed.Store(true)
		if h.ownsPool {
			h.pool.Close()
		}
		h.logger.Info("grpop/postgres: dlq handler closed")
	})
	return nil
}
