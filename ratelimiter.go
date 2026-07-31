// File: ratelimiter.go

package grpop

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultRecipientCacheSize is used whenever NewLocalRateLimiter is given a
// recipientCacheSize <= 0. A non-positive size is never silently treated as
// "unbounded" — recipient cardinality is caller-controlled input (an
// attacker can trivially mint new "recipients" by targeting new addresses),
// so an unbounded map here would be a real memory-exhaustion vector.
const defaultRecipientCacheSize = 10000

// localRateLimiter is a per-process, two-tier token bucket — the
// default/dev RateLimiter. It limits calls made through this one instance
// in this one process only: running N replicas of a service using it gives
// N times the configured rate, not a shared global rate. See
// ratelimiter.redis.go (Stage 9) for the distributed variant.
//
// "Two-tier" per RateLimiter's own doc comment: one bucket per Channel
// (a small, fixed set — no eviction needed) AND one bucket per
// (channel, recipient) pair, held in a bounded, least-recently-used cache
// so unbounded recipient cardinality can't grow this structure without
// limit. Both buckets share the same requestsPerSecond/burstSize
// configuration; there is no separate tuning knob for the channel-level
// vs. recipient-level rate today (see NewLocalRateLimiter's doc comment
// if that turns out to matter later).
//
// A request is allowed only if BOTH tiers have capacity. Composing two
// independent token buckets into one atomic allow/reject decision is the
// one real subtlety here: golang.org/x/time/rate has no "peek without
// consuming" operation, so Allow uses Reserve (which always commits) and
// explicitly Cancels the channel-level reservation if the recipient-level
// check then fails — otherwise a request rejected on the recipient tier
// would still have silently spent a channel-level token, throttling
// unrelated recipients for no reason.
type localRateLimiter struct {
	requestsPerSec int
	burstSize      int

	mu              sync.Mutex
	channelLimiters map[Channel]*rate.Limiter

	recipientCapacity int
	recipientItems    map[string]*list.Element
	recipientOrder    *list.List // Front() = most recently used
}

// recipientLimiterEntry is one (channel, recipient) bucket plus its own
// stats — GetStats reports per-(channel,recipient), not process-wide, so
// stats live here rather than on localRateLimiter itself.
type recipientLimiterEntry struct {
	key     string
	limiter *rate.Limiter

	mu            sync.Mutex
	allowedCount  int64
	blockedCount  int64
	waitCount     int64
	lastAllowedAt time.Time
}

func (e *recipientLimiterEntry) recordAllowed() {
	e.mu.Lock()
	e.allowedCount++
	e.lastAllowedAt = time.Now()
	e.mu.Unlock()
}

func (e *recipientLimiterEntry) recordBlocked() {
	e.mu.Lock()
	e.blockedCount++
	e.mu.Unlock()
}

func (e *recipientLimiterEntry) recordWait() {
	e.mu.Lock()
	e.waitCount++
	e.mu.Unlock()
}

func (e *recipientLimiterEntry) stats(requestsPerSec, burstSize int) RateLimiterStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return RateLimiterStats{
		RequestsPerSecond: requestsPerSec,
		BurstSize:         burstSize,
		AllowedCount:      e.allowedCount,
		BlockedCount:      e.blockedCount,
		WaitCount:         e.waitCount,
		LastAllowedAt:     e.lastAllowedAt,
	}
}

var _ RateLimiter = (*localRateLimiter)(nil)

// NewLocalRateLimiter constructs a per-process, two-tier RateLimiter.
//
// Parameters:
//   - requestsPerSecond: int — must be > 0; applies to both the
//     per-channel and per-(channel,recipient) tiers
//   - burstSize: int — must be >= requestsPerSecond
//   - recipientCacheSize: int — bounds the number of distinct recipients
//     tracked at once; defaults to defaultRecipientCacheSize if <= 0. When
//     the bound is reached, the least-recently-used recipient's bucket is
//     evicted and starts fresh on its next request — a bounded loss of
//     rate-limit precision under high recipient cardinality, not a
//     correctness or security issue (the per-channel tier is unaffected
//     and keeps limiting the aggregate rate regardless).
//
// Returns:
//   - RateLimiter
//   - error: non-nil if either rate constraint is violated
func NewLocalRateLimiter(requestsPerSecond, burstSize, recipientCacheSize int) (RateLimiter, error) {
	if requestsPerSecond <= 0 {
		return nil, errors.New("grpop: requestsPerSecond must be > 0")
	}
	if burstSize < requestsPerSecond {
		return nil, errors.New("grpop: burstSize must be >= requestsPerSecond")
	}
	if recipientCacheSize <= 0 {
		recipientCacheSize = defaultRecipientCacheSize
	}
	return &localRateLimiter{
		requestsPerSec:    requestsPerSecond,
		burstSize:         burstSize,
		channelLimiters:   make(map[Channel]*rate.Limiter),
		recipientCapacity: recipientCacheSize,
		recipientItems:    make(map[string]*list.Element),
		recipientOrder:    list.New(),
	}, nil
}

// channelLimiter returns channel's bucket, creating it if necessary. Called
// with r.mu held.
func (r *localRateLimiter) channelLimiter(channel Channel) *rate.Limiter {
	lim, ok := r.channelLimiters[channel]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(r.requestsPerSec), r.burstSize)
		r.channelLimiters[channel] = lim
	}
	return lim
}

// getOrCreateRecipientEntry returns the (channel, recipient) bucket,
// creating it (and evicting the least-recently-used entry if the cache is
// now over capacity) if necessary. Marks the entry most-recently-used.
// Called with r.mu held.
func (r *localRateLimiter) getOrCreateRecipientEntry(channel Channel, recipient string) *recipientLimiterEntry {
	key := string(channel) + "|" + recipient
	if el, ok := r.recipientItems[key]; ok {
		r.recipientOrder.MoveToFront(el)
		return el.Value.(*recipientLimiterEntry)
	}

	entry := &recipientLimiterEntry{key: key, limiter: rate.NewLimiter(rate.Limit(r.requestsPerSec), r.burstSize)}
	el := r.recipientOrder.PushFront(entry)
	r.recipientItems[key] = el

	for r.recipientOrder.Len() > r.recipientCapacity {
		back := r.recipientOrder.Back()
		if back == nil {
			break
		}
		r.recipientOrder.Remove(back)
		delete(r.recipientItems, back.Value.(*recipientLimiterEntry).key)
	}
	return entry
}

// peekRecipientEntry returns the (channel, recipient) bucket without
// creating one or touching LRU order — used by GetStats so a mere
// inspection can't evict a legitimately-tracked recipient to make room.
// Called with r.mu held.
func (r *localRateLimiter) peekRecipientEntry(channel Channel, recipient string) (*recipientLimiterEntry, bool) {
	el, ok := r.recipientItems[string(channel)+"|"+recipient]
	if !ok {
		return nil, false
	}
	return el.Value.(*recipientLimiterEntry), true
}

func (r *localRateLimiter) Allow(ctx context.Context, channel Channel, recipient string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	r.mu.Lock()
	chLim := r.channelLimiter(channel)
	entry := r.getOrCreateRecipientEntry(channel, recipient)
	r.mu.Unlock()

	chRes := chLim.Reserve()
	if !chRes.OK() {
		entry.recordBlocked()
		return false, nil
	}
	if chRes.Delay() > 0 {
		chRes.Cancel()
		entry.recordBlocked()
		return false, nil
	}

	recRes := entry.limiter.Reserve()
	if !recRes.OK() || recRes.Delay() > 0 {
		if recRes.OK() {
			recRes.Cancel()
		}
		chRes.Cancel() // give back the channel-level token: this request is being rejected
		entry.recordBlocked()
		return false, nil
	}

	entry.recordAllowed()
	return true, nil
}

func (r *localRateLimiter) Wait(ctx context.Context, channel Channel, recipient string) error {
	r.mu.Lock()
	chLim := r.channelLimiter(channel)
	entry := r.getOrCreateRecipientEntry(channel, recipient)
	r.mu.Unlock()

	entry.recordWait()

	if err := chLim.Wait(ctx); err != nil {
		return fmt.Errorf("grpop: rate limiter wait failed: %w", err)
	}
	if err := entry.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("grpop: rate limiter wait failed: %w", err)
	}

	entry.recordAllowed()
	return nil
}

func (r *localRateLimiter) GetStats(_ context.Context, channel Channel, recipient string) (RateLimiterStats, error) {
	r.mu.Lock()
	entry, ok := r.peekRecipientEntry(channel, recipient)
	r.mu.Unlock()

	if !ok {
		return RateLimiterStats{RequestsPerSecond: r.requestsPerSec, BurstSize: r.burstSize}, nil
	}
	return entry.stats(r.requestsPerSec, r.burstSize), nil
}
