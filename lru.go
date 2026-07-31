// File: lru.go

package grpop

import (
	"container/list"
	"sync"
)

// stringKeyedLRU is a size-bounded, least-recently-used cache keyed by an
// opaque string — shared by localRateLimiter's per-(channel,recipient)
// token buckets (ratelimiter.go) and redisRateLimiter's per-(channel,
// recipient) local stats counters (ratelimiter.redis.go), both of which
// need the exact same bounded-cardinality-under-caller-controlled-input
// structure (recipient identifiers are external input; an unbounded map
// keyed by them is a real memory-exhaustion vector) but store different
// value types. Mirrors grpolicy's own policyCache (container/list + map),
// generalized over V. All mutation is behind a single mutex — no
// lock-free cleverness — matching policyCache's own stated preference for
// the simplest thing to reason about.
type stringKeyedLRU[V any] struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	order    *list.List // Front() = most recently used
}

type lruEntry[V any] struct {
	key   string
	value V
}

// newStringKeyedLRU constructs a stringKeyedLRU bounded at capacity, which
// must be > 0 (callers are expected to have already applied their own
// default in place of a <= 0 value, the same way NewLocalRateLimiter does).
func newStringKeyedLRU[V any](capacity int) *stringKeyedLRU[V] {
	return &stringKeyedLRU[V]{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// getOrCreate returns the value for key, creating it via create (and
// evicting the least-recently-used entry if now over capacity) if
// necessary. Marks the entry most-recently-used either way.
func (c *stringKeyedLRU[V]) getOrCreate(key string, create func() V) V {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*lruEntry[V]).value
	}

	v := create()
	el := c.order.PushFront(&lruEntry[V]{key: key, value: v})
	c.items[key] = el

	for c.order.Len() > c.capacity {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.items, back.Value.(*lruEntry[V]).key)
	}
	return v
}

// peek returns key's value without creating one or touching LRU order —
// for read paths (like GetStats) where a mere inspection must not evict a
// legitimately-tracked entry to make room.
func (c *stringKeyedLRU[V]) peek(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	return el.Value.(*lruEntry[V]).value, true
}

// len reports the current number of tracked entries.
func (c *stringKeyedLRU[V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
