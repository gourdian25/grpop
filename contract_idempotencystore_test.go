// File: contract_idempotencystore_test.go

package grpop

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gourdian25/grcache"
)

const (
	testRedisAddr     = "localhost:6379"
	testRedisPassword = "redis_password"
)

// testIdempotencyStoreContract is the shared behavioral contract every
// grcache.Cache backend must satisfy identically once wrapped by
// cacheIdempotencyStore — cacheIdempotencyStore itself has only one
// implementation (it's a thin adapter, see cache.idempotency.go), so what
// actually varies here is which grcache.Cache backend backs it.
func testIdempotencyStoreContract(t *testing.T, newCache func(t *testing.T) grcache.Cache) {
	t.Helper()
	ctx := context.Background()
	// A nonce beyond t.Name() is required for any backend that persists
	// across separate `go test` invocations (Redis, unlike grcache/memory)
	// — a name-only key would collide with leftover state from a prior run
	// within the same TTL window.
	key := func(suffix string) string {
		return fmt.Sprintf("contract-%s-%s-%d", t.Name(), suffix, time.Now().UnixNano())
	}

	t.Run("UnmarkedIsNotProcessed", func(t *testing.T) {
		store := NewCacheIdempotencyStore(newCache(t))
		if processed, err := store.IsProcessed(ctx, key("k1")); err != nil || processed {
			t.Fatalf("IsProcessed(unmarked) = (%v, %v), want (false, nil)", processed, err)
		}
	})

	t.Run("MarkThenIsProcessed", func(t *testing.T) {
		store := NewCacheIdempotencyStore(newCache(t))
		k := key("k2")
		if err := store.MarkProcessed(ctx, k, time.Minute); err != nil {
			t.Fatalf("MarkProcessed: %v", err)
		}
		if processed, err := store.IsProcessed(ctx, k); err != nil || !processed {
			t.Fatalf("IsProcessed(marked) = (%v, %v), want (true, nil)", processed, err)
		}
	})

	t.Run("MarkProcessedTwiceIsIdempotent", func(t *testing.T) {
		store := NewCacheIdempotencyStore(newCache(t))
		k := key("k3")
		if err := store.MarkProcessed(ctx, k, time.Minute); err != nil {
			t.Fatalf("MarkProcessed (first): %v", err)
		}
		if err := store.MarkProcessed(ctx, k, time.Minute); err != nil {
			t.Fatalf("MarkProcessed (second): %v", err)
		}
	})

	t.Run("Expiry", func(t *testing.T) {
		store := NewCacheIdempotencyStore(newCache(t))
		k := key("k4")
		if err := store.MarkProcessed(ctx, k, 50*time.Millisecond); err != nil {
			t.Fatalf("MarkProcessed: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
		if processed, err := store.IsProcessed(ctx, k); err != nil || processed {
			t.Fatalf("IsProcessed(expired) = (%v, %v), want (false, nil)", processed, err)
		}
	})

	t.Run("DistinctKeysAreIndependent", func(t *testing.T) {
		store := NewCacheIdempotencyStore(newCache(t))
		k1, k2 := key("k5a"), key("k5b")
		if err := store.MarkProcessed(ctx, k1, time.Minute); err != nil {
			t.Fatalf("MarkProcessed: %v", err)
		}
		if processed, err := store.IsProcessed(ctx, k2); err != nil || processed {
			t.Fatalf("IsProcessed(distinct, unmarked key) = (%v, %v), want (false, nil)", processed, err)
		}
	})
}

func TestIdempotencyStore_Contract(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		testIdempotencyStoreContract(t, func(t *testing.T) grcache.Cache {
			t.Helper()
			cache, err := grcache.NewMemoryCache()
			if err != nil {
				t.Fatalf("grcache.NewMemoryCache: %v", err)
			}
			t.Cleanup(func() { _ = cache.Close() })
			return cache
		})
	})
	t.Run("Redis", func(t *testing.T) {
		testIdempotencyStoreContract(t, func(t *testing.T) grcache.Cache {
			t.Helper()
			cache, err := grcache.NewRedisCache(grcache.RedisConfig{Addr: testRedisAddr, Password: testRedisPassword})
			if err != nil {
				t.Skipf("Redis not available at %s, skipping: %v", testRedisAddr, err)
			}
			t.Cleanup(func() { _ = cache.Close() })
			return cache
		})
	})
}
