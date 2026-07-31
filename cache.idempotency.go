// File: cache.idempotency.go

package grpop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gourdian25/grcache"
)

// cacheIdempotencyStore implements IdempotencyStore as a thin adapter over
// any grcache.Cache — Exists/Set with a TTL is all IsProcessed/
// MarkProcessed actually need. Backend choice becomes "which grcache.Cache
// the caller constructs and passes in" (grcache/redis, grcache/mongostore,
// grcache/memory, ...), not "which grpop store type to construct" —
// mirrors grnoti's own cache.idempotency.go exactly.
type cacheIdempotencyStore struct {
	cache     grcache.Cache
	keyPrefix string
}

var _ IdempotencyStore = (*cacheIdempotencyStore)(nil)

// NewCacheIdempotencyStore constructs an IdempotencyStore backed by cache.
//
// Parameters:
//   - cache: grcache.Cache — caller-owned; not closed by this store's
//     Close (see Close's doc comment)
func NewCacheIdempotencyStore(cache grcache.Cache) IdempotencyStore {
	return &cacheIdempotencyStore{cache: cache, keyPrefix: "grpop:idempotency:"}
}

func (s *cacheIdempotencyStore) IsProcessed(ctx context.Context, idempotencyKey string) (bool, error) {
	ok, err := s.cache.Exists(ctx, s.keyPrefix+idempotencyKey)
	if err != nil {
		return false, fmt.Errorf("grpop: idempotency check for %s: %w", idempotencyKey, errors.Join(err, ErrBackendUnavailable))
	}
	return ok, nil
}

func (s *cacheIdempotencyStore) MarkProcessed(ctx context.Context, idempotencyKey string, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	marker := []byte(time.Now().UTC().Format(time.RFC3339))
	if err := s.cache.Set(ctx, s.keyPrefix+idempotencyKey, marker, ttl); err != nil {
		return fmt.Errorf("grpop: mark processed for %s: %w", idempotencyKey, errors.Join(err, ErrBackendUnavailable))
	}
	return nil
}

// Close is a no-op: the underlying grcache.Cache is caller-owned (likely
// shared with other components in the caller's own process), so closing it
// here would break every other holder of the same *grcache.Cache handle.
// The caller is responsible for closing the cache it constructed.
func (s *cacheIdempotencyStore) Close() error { return nil }
