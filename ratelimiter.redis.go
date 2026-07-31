// File: ratelimiter.redis.go

package grpop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisRateLimiterDialTimeout  = 5 * time.Second
	redisRateLimiterReadTimeout  = 3 * time.Second
	redisRateLimiterWriteTimeout = 3 * time.Second
	redisRateLimiterPingTimeout  = 5 * time.Second
	redisRateLimiterPoolSize     = 100

	// redisRateLimiterKeyTTL bounds how long an idle bucket's Redis hash
	// survives. Set well above any plausible refill window so a bucket
	// under active use never expires mid-burst; a bucket that goes idle
	// for this long simply starts full again next time, which is correct
	// token-bucket behavior anyway.
	redisRateLimiterKeyTTL = 10 * time.Minute

	// redisRateLimiterWaitPollInterval is the base interval Wait retries
	// Allow at while blocked. go-redis has no server-side blocking
	// primitive for a Lua script the way a plain BLPOP would give one, so
	// Wait polls. redisRateLimiterWaitPollJitter is added on top,
	// randomized fresh each iteration (see Wait), so that many goroutines
	// or replicas simultaneously Wait-blocked on the same exhausted bucket
	// don't all wake and hit Redis on the exact same tick cadence — a
	// self-synchronized retry storm against Redis at precisely the moment
	// the system is already rate-limited, i.e. already under pressure.
	redisRateLimiterWaitPollInterval = 20 * time.Millisecond
	redisRateLimiterWaitPollJitter   = 10 * time.Millisecond
)

// twoTierTokenBucketScript atomically evaluates and updates TWO token
// buckets — one per-channel, one per-(channel,recipient) — stored as Redis
// hashes {tokens, updated_at}, and consumes from both or neither: a
// request is allowed only if BOTH buckets currently have capacity.
//
// Unlike localRateLimiter's Go-side equivalent (ratelimiter.go's Allow,
// which has to use golang.org/x/time/rate's Reserve+Cancel dance because
// two separate in-process calls aren't atomic with each other), a single
// Lua script IS atomic end to end — so this script can just check both
// buckets' available tokens before consuming either, with no equivalent
// "give back a token" step needed.
//
// "now" is read once server-side via redis.call("TIME"), NOT passed in as
// a client-supplied argument — deliberately: this script is evaluated by
// every replica of a multi-process deployment (the entire point of this
// backend over localRateLimiter), and those replicas' wall clocks are not
// guaranteed to agree. A replica with a fast clock computing its own
// elapsed-time-since-last-refill would grant itself a larger refill than a
// replica with an accurate clock, silently corrupting the shared bucket —
// exactly the correctness property a distributed rate limiter exists to
// provide. Reading Redis's own clock makes every caller agree on "now" by
// construction, at the cost of one extra redis.call inside the script
// (still one network round trip total — TIME runs server-side).
//
// KEYS[1] = channel-level bucket key
// KEYS[2] = per-(channel,recipient)-level bucket key
// ARGV[1] = capacity (burst size), shared by both tiers
// ARGV[2] = refill rate, tokens/second, shared by both tiers
// ARGV[3] = key TTL, seconds
//
// Returns 1 if a token was consumed from both buckets (allowed), 0
// otherwise (neither bucket is touched beyond recording the refill that
// would have happened anyway).
var twoTierTokenBucketScript = redis.NewScript(`
local function refill(key, capacity, refillRate, now)
    local bucket = redis.call("HMGET", key, "tokens", "updated_at")
    local tokens = tonumber(bucket[1])
    local updatedAt = tonumber(bucket[2])
    if tokens == nil then
        tokens = capacity
        updatedAt = now
    end
    local elapsed = now - updatedAt
    if elapsed < 0 then
        elapsed = 0
    end
    tokens = math.min(capacity, tokens + elapsed * refillRate)
    return tokens
end

local capacity = tonumber(ARGV[1])
local refillRate = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])

local t = redis.call("TIME")
local now = tonumber(t[1]) + tonumber(t[2]) / 1e6

local chTokens = refill(KEYS[1], capacity, refillRate, now)
local recTokens = refill(KEYS[2], capacity, refillRate, now)

local allowed = 0
if chTokens >= 1 and recTokens >= 1 then
    chTokens = chTokens - 1
    recTokens = recTokens - 1
    allowed = 1
end

redis.call("HMSET", KEYS[1], "tokens", tostring(chTokens), "updated_at", tostring(now))
redis.call("EXPIRE", KEYS[1], ttl)
redis.call("HMSET", KEYS[2], "tokens", tostring(recTokens), "updated_at", tostring(now))
redis.call("EXPIRE", KEYS[2], ttl)

return allowed
`)

// RedisRateLimiterConfig configures a redisRateLimiter constructed by
// NewRedisRateLimiter. Zero-valued connection fields fall back to the same
// defaults as grcache/redis's RedisConfig; RequestsPerSecond and BurstSize
// have no sensible zero value and must be set explicitly.
type RedisRateLimiterConfig struct {
	// Addr is the Redis server address, e.g. "localhost:6379". Required.
	Addr string
	// Password authenticates with the server. Empty means no auth.
	Password string
	// DB selects the Redis logical database.
	DB int
	// PoolSize is the maximum number of connections in the pool. Defaults to 100.
	PoolSize int
	// DialTimeout bounds how long connecting to Redis may take. Defaults to 5s.
	DialTimeout time.Duration
	// ReadTimeout bounds how long a read may take. Defaults to 3s.
	ReadTimeout time.Duration
	// WriteTimeout bounds how long a write may take. Defaults to 3s.
	WriteTimeout time.Duration

	// RequestsPerSecond is each bucket's steady-state refill rate, shared
	// across every process using the same KeyPrefix. Required, must be > 0.
	// Applies to both the per-channel and per-(channel,recipient) tiers —
	// same single-knob tradeoff as NewLocalRateLimiter.
	RequestsPerSecond int
	// BurstSize is each bucket's capacity. Required, must be >= RequestsPerSecond.
	BurstSize int
	// KeyPrefix namespaces every Redis key this limiter touches. All
	// processes that should share one distributed quota must use the same
	// KeyPrefix. Defaults to "grpop:ratelimit". Unlike
	// NewLocalRateLimiter's in-process recipient cache, there is no
	// eviction/cardinality bound to configure here — Redis itself is the
	// shared store, and each bucket key expires on its own via
	// redisRateLimiterKeyTTL when idle.
	KeyPrefix string

	// Logger receives optional diagnostic messages. A nil Logger disables logging.
	Logger Logger
}

func (cfg RedisRateLimiterConfig) withDefaults() RedisRateLimiterConfig {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = redisRateLimiterPoolSize
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = redisRateLimiterDialTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = redisRateLimiterReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = redisRateLimiterWriteTimeout
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "grpop:ratelimit"
	}
	return cfg
}

// redisRecipientStats is one (channel, recipient) pair's LOCAL (this
// instance only) counters — see redisRateLimiter's doc comment for why
// these are necessarily imprecise across replicas.
type redisRecipientStats struct {
	mu            sync.Mutex
	allowedCount  int64
	blockedCount  int64
	waitCount     int64
	lastAllowedAt time.Time
}

func (s *redisRecipientStats) recordAllowed() {
	s.mu.Lock()
	s.allowedCount++
	s.lastAllowedAt = time.Now()
	s.mu.Unlock()
}

func (s *redisRecipientStats) recordBlocked() {
	s.mu.Lock()
	s.blockedCount++
	s.mu.Unlock()
}

func (s *redisRecipientStats) recordWait() {
	s.mu.Lock()
	s.waitCount++
	s.mu.Unlock()
}

func (s *redisRecipientStats) stats(requestsPerSec, burstSize int) RateLimiterStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RateLimiterStats{
		RequestsPerSecond: requestsPerSec,
		BurstSize:         burstSize,
		AllowedCount:      s.allowedCount,
		BlockedCount:      s.blockedCount,
		WaitCount:         s.waitCount,
		LastAllowedAt:     s.lastAllowedAt,
	}
}

// redisRateLimiter is a distributed, two-tier token bucket backed by a raw
// *redis.Client — the RateLimiter a multi-replica deployment should
// actually run with, since localRateLimiter enforces its quota per-process
// only (N replicas each independently enforcing the full rate).
//
// GetStats reports counters observed by THIS instance only: Allow/Wait
// calls made through other processes sharing the same KeyPrefix are
// invisible to it, since the authoritative token counts live in Redis, not
// locally — this instance only tracks its own local view via a bounded
// stringKeyedLRU (lru.go), the same structure localRateLimiter uses for
// its actual token buckets, reused here purely for local observability
// bookkeeping since recipient identifiers are still caller-controlled,
// unbounded-cardinality input.
//
// Fail-open vs. fail-closed on a Redis outage is deliberately NOT decided
// here: Allow/Wait return the backend error (wrapped in ErrBackendUnavailable)
// unchanged, the same way every other backend interface in this module
// (IdempotencyStore, DLQHandler) surfaces its own errors rather than
// silently picking a default. Whether "Redis is down" should mean "block
// every send" or "let everything through unlimited" is Service's call to
// make explicitly (Stage 15), not something a rate-limiter backend should
// decide unilaterally on its callers' behalf.
type redisRateLimiter struct {
	client    *redis.Client
	logger    Logger
	keyPrefix string

	mu             sync.RWMutex
	requestsPerSec int
	burstSize      int

	recipientStats *stringKeyedLRU[*redisRecipientStats]

	closed    atomic.Bool
	closeOnce sync.Once
}

var _ RateLimiter = (*redisRateLimiter)(nil)

// NewRedisRateLimiter builds its own *redis.Client from cfg and validates
// connectivity with a Ping before returning, mirroring grcache/redis's
// constructor-time validation.
//
// Parameters:
//   - cfg: RedisRateLimiterConfig — Addr, RequestsPerSecond, and BurstSize
//     are required; other fields default (see field docs)
//
// Returns:
//   - RateLimiter: ready to use, shared across every process using the
//     same cfg.Addr/cfg.KeyPrefix pair
//   - error: non-nil if a required field is invalid or the connection fails
func NewRedisRateLimiter(cfg RedisRateLimiterConfig) (RateLimiter, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("grpop: RedisRateLimiterConfig.Addr is required")
	}
	if cfg.RequestsPerSecond <= 0 {
		return nil, fmt.Errorf("grpop: RequestsPerSecond must be > 0")
	}
	if cfg.BurstSize < cfg.RequestsPerSecond {
		return nil, fmt.Errorf("grpop: BurstSize must be >= RequestsPerSecond")
	}
	cfg = cfg.withDefaults()
	logger := OrNop(cfg.Logger)

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), redisRateLimiterPingTimeout)
	defer cancel()
	if _, err := client.Ping(ctx).Result(); err != nil {
		_ = client.Close()
		logger.Error("grpop: redis rate limiter connect failed", "addr", cfg.Addr, "error", err)
		return nil, fmt.Errorf("grpop: connect %s: %w", cfg.Addr, ErrBackendUnavailable)
	}

	logger.Info("grpop: redis rate limiter connected", "addr", cfg.Addr, "key_prefix", cfg.KeyPrefix)
	return &redisRateLimiter{
		client:         client,
		logger:         logger,
		keyPrefix:      cfg.KeyPrefix,
		requestsPerSec: cfg.RequestsPerSecond,
		burstSize:      cfg.BurstSize,
		recipientStats: newStringKeyedLRU[*redisRecipientStats](defaultRecipientCacheSize),
	}, nil
}

func (r *redisRateLimiter) channelKey(channel Channel) string {
	return r.keyPrefix + ":channel:" + string(channel)
}

// recipientRedisKey hashes recipient (SHA-256, hex) into the key rather
// than concatenating it raw. recipient is caller-controlled input
// (EmailMessage.To/WhatsAppMessage.To — validated only for non-emptiness,
// not character content) — hashing removes any possibility of two
// genuinely different (channel, recipient) pairs colliding onto the same
// Redis key regardless of what characters recipient contains, rather than
// relying on "email addresses and phone numbers don't contain the ':'
// delimiter in practice."
func (r *redisRateLimiter) recipientRedisKey(channel Channel, recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return r.keyPrefix + ":recipient:" + string(channel) + ":" + hex.EncodeToString(sum[:])
}

func (r *redisRateLimiter) Allow(ctx context.Context, channel Channel, recipient string) (bool, error) {
	if r.closed.Load() {
		return false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	r.mu.RLock()
	rate, burst := r.requestsPerSec, r.burstSize
	r.mu.RUnlock()

	stats := r.recipientStats.getOrCreate(recipientKey(channel, recipient), func() *redisRecipientStats { return &redisRecipientStats{} })

	keys := []string{r.channelKey(channel), r.recipientRedisKey(channel, recipient)}
	res, err := twoTierTokenBucketScript.Run(ctx, r.client, keys, burst, rate, int(redisRateLimiterKeyTTL.Seconds())).Result()
	if err != nil {
		return false, fmt.Errorf("grpop: redis rate limiter eval: %w", ErrBackendUnavailable)
	}

	allowedN, ok := res.(int64)
	if !ok {
		return false, fmt.Errorf("grpop: redis rate limiter: unexpected script result type %T", res)
	}

	allowed := allowedN == 1
	if allowed {
		stats.recordAllowed()
	} else {
		stats.recordBlocked()
	}
	return allowed, nil
}

// Wait polls Allow, at redisRateLimiterWaitPollInterval plus a fresh random
// jitter each iteration, until a token is available or ctx is done. Redis
// has no server-side blocking primitive for a Lua-scripted token bucket the
// way BLPOP gives a plain list, so unlike localRateLimiter's Wait (which
// defers to golang.org/x/time/rate's own timer-based Wait), this one polls.
func (r *redisRateLimiter) Wait(ctx context.Context, channel Channel, recipient string) error {
	if r.closed.Load() {
		return ErrClosed
	}
	stats := r.recipientStats.getOrCreate(recipientKey(channel, recipient), func() *redisRecipientStats { return &redisRecipientStats{} })
	stats.recordWait()

	for {
		allowed, err := r.Allow(ctx, channel, recipient)
		if err != nil {
			return err
		}
		if allowed {
			return nil
		}

		//nolint:gosec // poll-interval jitter has no cryptographic requirement
		jitter := time.Duration(rand.Int63n(int64(redisRateLimiterWaitPollJitter)))
		timer := time.NewTimer(redisRateLimiterWaitPollInterval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *redisRateLimiter) GetStats(_ context.Context, channel Channel, recipient string) (RateLimiterStats, error) {
	if r.closed.Load() {
		return RateLimiterStats{}, ErrClosed
	}
	r.mu.RLock()
	rate, burst := r.requestsPerSec, r.burstSize
	r.mu.RUnlock()

	stats, ok := r.recipientStats.peek(recipientKey(channel, recipient))
	if !ok {
		return RateLimiterStats{RequestsPerSecond: rate, BurstSize: burst}, nil
	}
	return stats.stats(rate, burst), nil
}

// UpdateLimit adjusts the shared bucket's rate/capacity at runtime. Since
// capacity/rate are passed as script arguments on every call rather than
// stored server-side, this takes effect for this process's subsequent
// calls immediately; other processes sharing the same KeyPrefix keep using
// whatever they were last configured with until they call UpdateLimit too
// — there is no cross-process config propagation.
func (r *redisRateLimiter) UpdateLimit(requestsPerSecond, burstSize int) error {
	if r.closed.Load() {
		return ErrClosed
	}
	if requestsPerSecond <= 0 {
		return fmt.Errorf("grpop: requestsPerSecond must be > 0")
	}
	if burstSize < requestsPerSecond {
		return fmt.Errorf("grpop: burstSize must be >= requestsPerSecond")
	}
	r.mu.Lock()
	r.requestsPerSec = requestsPerSecond
	r.burstSize = burstSize
	r.mu.Unlock()
	return nil
}

// Close closes the underlying *redis.Client, guarded by sync.Once since
// go-redis errors on a double Close.
func (r *redisRateLimiter) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		err = r.client.Close()
		r.logger.Info("grpop: redis rate limiter closed")
	})
	return err
}
