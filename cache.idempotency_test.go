// File: cache.idempotency_test.go

package grpop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
)

func newTestCache(t *testing.T) grcache.Cache {
	t.Helper()
	cache, err := grcache.NewMemoryCache()
	if err != nil {
		t.Fatalf("grcache.NewMemoryCache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func TestCacheIdempotencyStore_MarkAndCheck(t *testing.T) {
	store := NewCacheIdempotencyStore(newTestCache(t))
	ctx := context.Background()

	if processed, err := store.IsProcessed(ctx, "key-1"); err != nil || processed {
		t.Fatalf("IsProcessed(unmarked) = (%v, %v), want (false, nil)", processed, err)
	}

	if err := store.MarkProcessed(ctx, "key-1", time.Hour); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	if processed, err := store.IsProcessed(ctx, "key-1"); err != nil || !processed {
		t.Fatalf("IsProcessed(marked) = (%v, %v), want (true, nil)", processed, err)
	}
}

func TestCacheIdempotencyStore_MarkProcessedTwiceIsIdempotent(t *testing.T) {
	store := NewCacheIdempotencyStore(newTestCache(t))
	ctx := context.Background()
	if err := store.MarkProcessed(ctx, "key-1", time.Hour); err != nil {
		t.Fatalf("MarkProcessed (first): %v", err)
	}
	if err := store.MarkProcessed(ctx, "key-1", time.Hour); err != nil {
		t.Fatalf("MarkProcessed (second): %v", err)
	}
}

func TestCacheIdempotencyStore_Expiry(t *testing.T) {
	store := NewCacheIdempotencyStore(newTestCache(t))
	ctx := context.Background()
	if err := store.MarkProcessed(ctx, "key-1", 50*time.Millisecond); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if processed, err := store.IsProcessed(ctx, "key-1"); err != nil || processed {
		t.Fatalf("IsProcessed(expired) = (%v, %v), want (false, nil)", processed, err)
	}
}

func TestCacheIdempotencyStore_NegativeTTLTreatedAsZero(t *testing.T) {
	store := NewCacheIdempotencyStore(newTestCache(t))
	ctx := context.Background()
	if err := store.MarkProcessed(ctx, "key-1", -time.Hour); err != nil {
		t.Fatalf("MarkProcessed(negative ttl): %v", err)
	}
}

func TestCacheIdempotencyStore_Close_DoesNotCloseSharedCache(t *testing.T) {
	cache := newTestCache(t)
	store := NewCacheIdempotencyStore(cache)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The shared cache must still be usable — Close on the adapter must not
	// have closed the caller-owned cache.
	if err := cache.Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("cache unusable after adapter Close: %v", err)
	}
}

func TestCacheIdempotencyStore_ClosedCacheErrors(t *testing.T) {
	cache := newTestCache(t)
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store := NewCacheIdempotencyStore(cache)
	ctx := context.Background()

	if _, err := store.IsProcessed(ctx, "key-1"); err == nil {
		t.Fatal("IsProcessed on a closed cache = nil error, want non-nil")
	} else if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("IsProcessed on a closed cache error = %v, want wrapping ErrBackendUnavailable", err)
	}
	if err := store.MarkProcessed(ctx, "key-1", time.Hour); err == nil {
		t.Fatal("MarkProcessed on a closed cache = nil error, want non-nil")
	} else if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("MarkProcessed on a closed cache error = %v, want wrapping ErrBackendUnavailable", err)
	}
}
