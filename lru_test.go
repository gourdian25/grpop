// File: lru_test.go

package grpop

import (
	"sync"
	"testing"
)

func TestStringKeyedLRU_GetOrCreate_CreatesOnce(t *testing.T) {
	c := newStringKeyedLRU[int](10)
	calls := 0
	create := func() int { calls++; return 42 }

	if got := c.getOrCreate("k", create); got != 42 {
		t.Fatalf("getOrCreate() = %d, want 42", got)
	}
	if got := c.getOrCreate("k", create); got != 42 {
		t.Fatalf("getOrCreate() (second call) = %d, want 42", got)
	}
	if calls != 1 {
		t.Fatalf("create called %d times, want 1 (second getOrCreate should hit the cache)", calls)
	}
}

func TestStringKeyedLRU_Peek_DoesNotCreate(t *testing.T) {
	c := newStringKeyedLRU[int](10)
	if _, ok := c.peek("missing"); ok {
		t.Fatal("peek(missing) = (_, true), want (_, false)")
	}
	if c.len() != 0 {
		t.Fatalf("len() = %d after a peek-only miss, want 0", c.len())
	}
}

func TestStringKeyedLRU_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newStringKeyedLRU[int](2)
	c.getOrCreate("a", func() int { return 1 })
	c.getOrCreate("b", func() int { return 2 })
	c.getOrCreate("c", func() int { return 3 }) // should evict "a"

	if c.len() != 2 {
		t.Fatalf("len() = %d, want 2 (capacity)", c.len())
	}
	if _, ok := c.peek("a"); ok {
		t.Fatal("peek(a) = (_, true), want evicted")
	}
	if _, ok := c.peek("c"); !ok {
		t.Fatal("peek(c) = (_, false), want present (most recently used)")
	}
}

func TestStringKeyedLRU_GetOrCreateTouchesRecency(t *testing.T) {
	c := newStringKeyedLRU[int](2)
	c.getOrCreate("a", func() int { return 1 })
	c.getOrCreate("b", func() int { return 2 })
	c.getOrCreate("a", func() int { return 1 }) // touch "a", making "b" the least recently used
	c.getOrCreate("c", func() int { return 3 }) // should evict "b", not "a"

	if _, ok := c.peek("a"); !ok {
		t.Fatal("peek(a) = (_, false), want present (recently touched)")
	}
	if _, ok := c.peek("b"); ok {
		t.Fatal("peek(b) = (_, true), want evicted (least recently used)")
	}
}

func TestStringKeyedLRU_ConcurrentAccess(t *testing.T) {
	c := newStringKeyedLRU[int](20)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%10))
			c.getOrCreate(key, func() int { return i })
			_, _ = c.peek(key)
			_ = c.len()
		}(i)
	}
	wg.Wait()
}
