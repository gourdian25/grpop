// File: dlq.mongo.go

package grpop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// DefaultDLQCollection is the collection name used when
// MongoDLQHandlerConfig.CollectionName is empty.
const DefaultDLQCollection = "grpop_dlq"

// mongoDLQDoc's expires_at is deliberately OMITTED from the document
// entirely (not stored as an explicit null) when a DLQMessage's ExpiresAt
// is the zero value ("no deadline" — see DLQMessage's doc comment). This
// is why the field has no `bson:"expires_at"` struct tag driving automatic
// marshaling for writes — ExpirePastDeadlineEvents/ClaimRetryableEvents
// build their filter documents by hand (see below) specifically so a
// missing expires_at field correctly fails a plain $lte/$gt comparison the
// same way SQL NULL does, rather than round-tripping through Go's zero
// time.Time (year 1), which — if written naively — would compare as
// "already expired" against $lte $now and break every no-deadline event.
type mongoDLQDoc struct {
	SendID         string            `bson:"send_id"`
	Channel        Channel           `bson:"channel"`
	MessageData    []byte            `bson:"message_data"`
	FailureReason  string            `bson:"failure_reason"`
	RetryCount     int               `bson:"retry_count"`
	MaxRetries     int               `bson:"max_retries"`
	FirstFailureAt time.Time         `bson:"first_failure_at"`
	LastAttemptAt  time.Time         `bson:"last_attempt_at"`
	NextRetryAt    time.Time         `bson:"next_retry_at"`
	ExpiresAt      time.Time         `bson:"expires_at,omitempty"`
	Status         DLQStatus         `bson:"status"`
	AttemptHistory []DLQRetryAttempt `bson:"attempt_history"`
	CreatedAt      time.Time         `bson:"created_at"`
	UpdatedAt      time.Time         `bson:"updated_at"`
}

// MongoDLQHandlerConfig configures a DLQHandler constructed by
// NewMongoDLQHandler.
type MongoDLQHandlerConfig struct {
	URI            string
	Database       string
	CollectionName string        // defaults to DefaultDLQCollection
	MaxRetries     int           // defaults to 3
	RetryDelay     time.Duration // passed through as-is; 0 means immediately retry-eligible
	MaxRetryDelay  time.Duration // passed through as-is to FullJitterBackoff

	// MaxAttemptHistoryEntries caps DLQEvent.AttemptHistory, oldest entry
	// dropped first once exceeded. Defaults to 20 if <= 0. Enforced via
	// $push's $slice modifier at write time (see PublishToDLQ/MarkRetried),
	// MongoDB's native equivalent of dlq.postgres.go's SQL-side cap.
	MaxAttemptHistoryEntries int

	// Encryptor, if set, encrypts DLQMessage's serialized bytes before
	// they're written to message_data, and decrypts on read back. Nil (the
	// default) stores message_data in the clear. Unlike Postgres's JSONB
	// column, Mongo's message_data field is stored as native BSON binary
	// ([]byte), so — unlike dlq.postgres.go — no base64 wrapping is needed
	// to make arbitrary ciphertext bytes storable.
	Encryptor MessageEncryptor

	Logger Logger
}

type mongoDLQHandler struct {
	client            *mongo.Client
	collection        *mongo.Collection
	maxRetries        int
	retryDelay        time.Duration
	maxRetryDelay     time.Duration
	maxAttemptHistory int
	encryptor         MessageEncryptor
	logger            Logger

	closed    atomic.Bool
	closeOnce sync.Once
}

var _ DLQHandler = (*mongoDLQHandler)(nil)

// NewMongoDLQHandler connects to MongoDB per cfg, ensures indexes
// (including a 7-day TTL index on created_at as a durable-retention
// backstop independent of PurgeExpiredEvents — relevant here specifically
// because grpop_dlq can hold sensitive payloads, see docs.go), and
// validates connectivity before returning.
//
// Claim semantics: unlike a naive read-then-write, every mutating
// operation here is scoped by an atomic MongoDB operation —
// ClaimRetryableEvents uses FindOneAndUpdate per document (atomic
// per-document claim, no transaction needed), and MarkRetried's
// retry_count increment is a $inc scoped to {send_id, status: "retrying"}
// rather than a Go-side read-then-set.
func NewMongoDLQHandler(cfg MongoDLQHandlerConfig) (DLQHandler, error) {
	if cfg.URI == "" {
		return nil, fmt.Errorf("grpop/mongo: MongoDLQHandlerConfig.URI is required")
	}
	if cfg.Database == "" {
		return nil, fmt.Errorf("grpop/mongo: MongoDLQHandlerConfig.Database is required")
	}
	collName := cfg.CollectionName
	if collName == "" {
		collName = DefaultDLQCollection
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	maxAttemptHistory := cfg.MaxAttemptHistoryEntries
	if maxAttemptHistory <= 0 {
		maxAttemptHistory = 20
	}
	// RetryDelay/MaxRetryDelay are passed through unchanged, including 0 —
	// unlike MaxRetries, 0 is a valid, deliberate choice here (immediate
	// retry-eligibility, useful for tests), not silently replaced with a
	// default. See NewMemoryDLQHandler's identical convention.
	retryDelay := cfg.RetryDelay
	maxRetryDelay := cfg.MaxRetryDelay
	logger := OrNop(cfg.Logger)

	// v2's mongo.Connect no longer takes a context or blocks on the network
	// itself, so the 10s timeout that previously bounded Connect now bounds
	// the Ping below instead -- Ping is the real connectivity check.
	connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.URI))
	if err != nil {
		return nil, fmt.Errorf("grpop/mongo: connect: %w", errors.Join(err, ErrBackendUnavailable))
	}
	if err := client.Ping(connectCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("grpop/mongo: ping: %w", errors.Join(err, ErrBackendUnavailable))
	}

	collection := client.Database(cfg.Database).Collection(collName)
	if _, err := collection.Indexes().CreateMany(connectCtx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "send_id", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "next_retry_at", Value: 1}}},
		{Keys: bson.D{{Key: "channel", Value: 1}, {Key: "status", Value: 1}}},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "expires_at", Value: 1}}},
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Options: options.Index().SetExpireAfterSeconds(7 * 24 * 60 * 60)},
	}); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("grpop/mongo: ensure indexes: %w", err)
	}

	logger.Info("grpop/mongo: dlq handler connected", "database", cfg.Database, "collection", collName)
	return &mongoDLQHandler{
		client: client, collection: collection,
		maxRetries: maxRetries, retryDelay: retryDelay, maxRetryDelay: maxRetryDelay,
		maxAttemptHistory: maxAttemptHistory, encryptor: cfg.Encryptor,
		logger: logger,
	}, nil
}

// encodeMessageData serializes msg to JSON, then encrypts it if an
// Encryptor is configured. See MongoDLQHandlerConfig.Encryptor's doc
// comment for why no base64 wrapping is needed here, unlike
// dlq.postgres.go's equivalent.
func (h *mongoDLQHandler) encodeMessageData(msg DLQMessage) ([]byte, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("grpop/mongo: encode message data: %w", err)
	}
	if h.encryptor == nil {
		return raw, nil
	}
	encrypted, err := h.encryptor.Encrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("grpop/mongo: encrypt message data: %w", err)
	}
	return encrypted, nil
}

// decodeMessageData reverses encodeMessageData: decrypts (if an Encryptor
// is configured), then unmarshals.
func (h *mongoDLQHandler) decodeMessageData(data []byte) (DLQMessage, error) {
	raw := data
	if h.encryptor != nil {
		decrypted, err := h.encryptor.Decrypt(data)
		if err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/mongo: decrypt message data: %w", err)
		}
		raw = decrypted
	}
	var msg DLQMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &msg); err != nil {
			return DLQMessage{}, fmt.Errorf("grpop/mongo: decode message data: %w", err)
		}
	}
	return msg, nil
}

// docToDLQEvent converts d to a *DLQEvent. Channel/ExpiresAt are taken
// from d's own top-level fields, not the decoded message_data blob's
// embedded copies — see dlq.postgres.go's rowToDomain for the identical
// reasoning.
func (h *mongoDLQHandler) docToDLQEvent(d mongoDLQDoc) (*DLQEvent, error) {
	msg, err := h.decodeMessageData(d.MessageData)
	if err != nil {
		return nil, fmt.Errorf("grpop/mongo: decode message data for %s: %w", d.SendID, err)
	}
	msg.Channel = d.Channel
	msg.ExpiresAt = d.ExpiresAt

	return &DLQEvent{
		SendID: d.SendID, MessageData: msg, FailureReason: d.FailureReason,
		RetryCount: d.RetryCount, MaxRetries: d.MaxRetries,
		FirstFailureAt: d.FirstFailureAt, LastAttemptAt: d.LastAttemptAt, NextRetryAt: d.NextRetryAt,
		Status: d.Status, AttemptHistory: d.AttemptHistory,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}, nil
}

func (h *mongoDLQHandler) PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error {
	if h.closed.Load() {
		return ErrClosed
	}
	now := time.Now().UTC()

	messageData, err := h.encodeMessageData(msg)
	if err != nil {
		return err
	}
	attempt := DLQRetryAttempt{AttemptNumber: 0, AttemptedAt: now, Success: false, ErrorMessage: failureReason}

	setOnInsert := bson.M{
		"send_id": sendID, "retry_count": 0, "max_retries": h.maxRetries,
		"first_failure_at": now, "next_retry_at": now.Add(h.retryDelay), "status": DLQStatusPending,
		"created_at": now,
	}
	// expires_at is only ever written via $setOnInsert (never updated on a
	// republish, matching dlq.postgres.go's UpsertDLQEvent), and only when
	// non-zero — see mongoDLQDoc's doc comment for why a zero value must
	// stay entirely absent from the document rather than being written as
	// the Go zero time.Time.
	if !msg.ExpiresAt.IsZero() {
		setOnInsert["expires_at"] = msg.ExpiresAt
	}

	// A single atomic upsert handles both "new failure" (insert) and
	// "another failure for a send_id already pending/retrying" (update) in
	// one write path. $slice: -h.maxAttemptHistory caps attempt_history to
	// its most recent entries, oldest dropped first — MongoDB's native
	// equivalent of dlq.postgres.go's SQL-side cap subquery.
	_, err = h.collection.UpdateOne(ctx,
		bson.M{"send_id": sendID},
		bson.M{
			"$push": bson.M{"attempt_history": bson.M{"$each": bson.A{attempt}, "$slice": -h.maxAttemptHistory}},
			"$set": bson.M{
				"channel": msg.Channel, "message_data": messageData,
				"failure_reason": failureReason, "last_attempt_at": now, "updated_at": now,
			},
			"$setOnInsert": setOnInsert,
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("grpop/mongo: publish to dlq %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return nil
}

// ClaimRetryableEvents does two things, in order, each call — see
// DLQHandler's doc comment for the full contract.
func (h *mongoDLQHandler) ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error) {
	if h.closed.Load() {
		return nil, ErrClosed
	}
	if limit <= 0 {
		limit = 10
	}
	now := time.Now().UTC()

	// Step 1: proactively expire any Pending document whose deadline has
	// passed. expires_at: {$lte: now} naturally excludes documents where
	// the field is entirely absent (a missing field never satisfies a
	// comparison operator in MongoDB) — see mongoDLQDoc's doc comment.
	if _, err := h.collection.UpdateMany(ctx,
		bson.M{"status": DLQStatusPending, "expires_at": bson.M{"$lte": now}},
		bson.M{"$set": bson.M{"status": DLQStatusExpired, "updated_at": now}},
	); err != nil {
		return nil, fmt.Errorf("grpop/mongo: expire past-deadline events: %w", errors.Join(err, ErrBackendUnavailable))
	}

	// Step 2: claim up to limit of what remains, one FindOneAndUpdate per
	// iteration (Mongo has no single-statement "claim up to N documents"
	// equivalent to Postgres's SKIP LOCKED query). The $or below is the
	// mirror image of step 1's plain $lte: a missing expires_at ("no
	// deadline") must be explicitly included, since it satisfies neither
	// $exists:false's absence check nor $gt on its own without the $or.
	claimFilter := bson.M{
		"status": DLQStatusPending, "next_retry_at": bson.M{"$lte": now},
		"$or": bson.A{
			bson.M{"expires_at": bson.M{"$exists": false}},
			bson.M{"expires_at": bson.M{"$gt": now}},
		},
	}

	claimed := make([]*DLQEvent, 0, limit)
	for i := 0; i < limit; i++ {
		var doc mongoDLQDoc
		err := h.collection.FindOneAndUpdate(ctx,
			claimFilter,
			bson.M{"$set": bson.M{"status": DLQStatusRetrying, "updated_at": now}},
			options.FindOneAndUpdate().SetSort(bson.D{{Key: "next_retry_at", Value: 1}}).SetReturnDocument(options.After),
		).Decode(&doc)
		if err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				break
			}
			return claimed, fmt.Errorf("grpop/mongo: claim retryable events: %w", errors.Join(err, ErrBackendUnavailable))
		}
		e, err := h.docToDLQEvent(doc)
		if err != nil {
			return claimed, err
		}
		claimed = append(claimed, e)
	}
	return claimed, nil
}

func (h *mongoDLQHandler) MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error {
	if h.closed.Load() {
		return ErrClosed
	}
	now := time.Now().UTC()

	// Step 1: atomically increment retry_count, scoped to the claimed
	// ("retrying") state — the filter is what makes this safe.
	var doc mongoDLQDoc
	err := h.collection.FindOneAndUpdate(ctx,
		bson.M{"send_id": sendID, "status": DLQStatusRetrying},
		bson.M{"$inc": bson.M{"retry_count": 1}, "$set": bson.M{"last_attempt_at": now, "updated_at": now}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			if _, getErr := h.GetEventByID(ctx, sendID); errors.Is(getErr, ErrDLQEventNotFound) {
				return ErrDLQEventNotFound
			}
			return ErrDLQEventNotClaimed
		}
		return fmt.Errorf("grpop/mongo: mark retried %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}

	errMsg := ""
	if attemptErr != nil {
		errMsg = attemptErr.Error()
	}
	setFields := bson.M{"updated_at": now}

	// Step 2: finalize status/next_retry_at. The expiry check runs before
	// the MaxRetries check — a message that expires with retry budget
	// still unused is reported as Expired, not Exhausted (see
	// DLQHandler's doc comment). doc.ExpiresAt is the Go zero value when
	// the field was absent in the document (no deadline).
	switch {
	case success:
		setFields["status"] = DLQStatusResolved
	default:
		nextRetryAt := now.Add(FullJitterBackoff(h.retryDelay, h.maxRetryDelay, doc.RetryCount))
		switch {
		case !doc.ExpiresAt.IsZero() && !nextRetryAt.Before(doc.ExpiresAt):
			setFields["status"] = DLQStatusExpired
		case doc.RetryCount >= doc.MaxRetries:
			setFields["status"] = DLQStatusExhausted
		default:
			setFields["status"] = DLQStatusPending
			setFields["next_retry_at"] = nextRetryAt
		}
	}

	attempt := DLQRetryAttempt{AttemptNumber: doc.RetryCount, AttemptedAt: now, Success: success, ErrorMessage: errMsg}
	if _, err := h.collection.UpdateOne(ctx,
		bson.M{"send_id": sendID},
		bson.M{
			"$set":  setFields,
			"$push": bson.M{"attempt_history": bson.M{"$each": bson.A{attempt}, "$slice": -h.maxAttemptHistory}},
		},
	); err != nil {
		return fmt.Errorf("grpop/mongo: mark retried %s (finalize): %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return nil
}

func (h *mongoDLQHandler) GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error) {
	if h.closed.Load() {
		return nil, ErrClosed
	}
	var doc mongoDLQDoc
	err := h.collection.FindOne(ctx, bson.M{"send_id": sendID}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrDLQEventNotFound
		}
		return nil, fmt.Errorf("grpop/mongo: get event %s: %w", sendID, errors.Join(err, ErrBackendUnavailable))
	}
	return h.docToDLQEvent(doc)
}

func (h *mongoDLQHandler) PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error) {
	if h.closed.Load() {
		return 0, ErrClosed
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	res, err := h.collection.DeleteMany(ctx, bson.M{"$or": []bson.M{
		{"status": DLQStatusResolved},
		{"status": DLQStatusExhausted},
		{"status": DLQStatusExpired},
		{"created_at": bson.M{"$lt": cutoff}},
	}})
	if err != nil {
		return 0, fmt.Errorf("grpop/mongo: purge expired events: %w", errors.Join(err, ErrBackendUnavailable))
	}
	return res.DeletedCount, nil
}

func (h *mongoDLQHandler) Close() error {
	var err error
	h.closeOnce.Do(func() {
		h.closed.Store(true)
		err = h.client.Disconnect(context.Background())
		h.logger.Info("grpop/mongo: dlq handler closed")
	})
	return err
}
