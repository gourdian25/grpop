# grpop architecture

This document records how grpop is put together and *why*, with particular
attention to decisions made or revised during implementation that the
original plan didn't fully anticipate (see
[docs/plan/grpop-plan.md](plan/grpop-plan.md) for the stage-by-stage
account). It is a design reference, not a tutorial — see the
[README](../README.md) for a quickstart and
[example/main.go](../example/main.go) for a runnable, narrated walkthrough.

## 1. Package shape

grpop's public API is a single flat package with no subpackages — every
storage backend (PostgreSQL, MongoDB, Redis) and every vendor dispatcher
lives in this one module:

```
interfaces.go / types.go / errors.go / logger.go / docs.go
retrystrategy.go            circuitbreaker.go           payloadvalidator.go
templateengine.email.go
cache.idempotency.go        ratelimiter.go (local)       ratelimiter.redis.go
postgres.go                 dlq.postgres.go              dlq.mongo.go
dispatcher.email.smtp.go    dispatcher.whatsapp.metacloud.go
templatevalidator.whatsapp.go
dryrun.go                   events.go                    service.go
memory.go (in-memory variant of every store)
internal/postgresdb/        (sqlc-generated, unexported — see §5)
example/
```

File naming: `<concern>.<backend>.go` for storage/rate-limiting (matching
`grnoti`), `dispatcher.<channel>.<vendor>.go` for the two dispatch files —
a third segment, since grpop's dispatch layer has a genuinely
two-dimensional axis (channel × vendor) that `grnoti`'s single-channel,
single-vendor dispatch layer didn't need.

**This follows `grnoti`'s/`gourdiantoken`'s precedent, not
`grcache`'s/`graudit`'s subpackage-per-backend layout** (which exists
specifically to keep unused backend drivers out of a consumer's dependency
graph). grpop's version of that tradeoff is unusually cheap: the dispatch
layer itself — the part most consumers actually touch — has **zero
third-party dependencies**, full stop. Email goes over stdlib `net/smtp`;
WhatsApp goes over a hand-rolled `net/http` client against Meta's Graph API
(no official Go SDK exists regardless of channel scope). The only
third-party imports anywhere in the module are for storage/rate-limiting
(`pgx/v5`, `go-redis/v9`, optionally `mongo-driver`) and `grcache`'s/
`grevents`' own root-interface packages. See `docs.go`'s package-level doc
comment and `docs/plan/grpop-plan.md` §3 for the full reasoning.

## 2. Interfaces and backend matrix

| Interface | In-memory (`memory.go`) | Postgres | Mongo | Redis | Other |
|---|:---:|:---:|:---:|:---:|---|
| `EmailSender` | `MemoryEmailSender` | — | — | — | `smtpDispatcher` (stdlib SMTP, any vendor); `dryRunEmailSender` (staging) |
| `WhatsAppSender` | `MemoryWhatsAppSender` | — | — | — | `metaCloudDispatcher` (Meta Graph API, direct); `dryRunWhatsAppSender` (staging) |
| `IdempotencyStore` | — | — | — | — | `cacheIdempotencyStore` — thin adapter over any `grcache.Cache` (Redis, Mongo, in-memory, ...) |
| `DLQHandler` | `memoryDLQHandler` | `postgresDLQHandler` (`FOR UPDATE SKIP LOCKED`) | `mongoDLQHandler` (`findOneAndUpdate`) | — | |
| `RateLimiter` | — | — | — | `redisRateLimiter` (Lua-scripted, two-tier) | `localRateLimiter` (per-process, two-tier) |
| `CircuitBreaker` | — | — | — | — | `standardCircuitBreaker` — the one implementation, returned as the interface type |
| `EmailTemplateEngine` | — | — | — | — | `defaultEmailTemplateEngine` (`html/template` + `text/template`) |
| `TemplateValidator` | — | — | — | — | `metaTemplateValidator` — TTL-cached over `WhatsAppCloudAPIClient.GetApprovedTemplates` |
| `WhatsAppCloudAPIClient` | — | — | — | — | `metaCloudAPIClient` (real, `net/http`); fake in tests — Meta's Graph API has no local emulator (§5) |

`Service` (`service.go`) is the orchestrator composing `IdempotencyStore` +
`DLQHandler` + both `Sender`s via `ServiceDeps`; see §3.4 for what it
deliberately does *not* also hold.

## 3. Key design decisions

### 3.1 Two-tier rate limiting: per-channel AND per-(channel, recipient)

`RateLimiter.Allow`/`Wait` gate on **both** a per-`Channel` bucket and a
per-`(Channel, recipient)` bucket, requiring both to have capacity — a
deliberate divergence from `grnoti`'s single global bucket. A
per-recipient limit alone can't stop one actor from triggering many
distinct recipients' sends in a burst (a scripted forgot-password sweep
across a whole user list); a per-channel limit alone can't stop one
recipient from being spammed by many distinct callers. Composing two
independent `golang.org/x/time/rate.Limiter`s into one atomic decision is
the real subtlety in `localRateLimiter.Allow`: `rate.Limiter` has no "peek
without consuming" operation, so `Allow` uses `Reserve` (which always
commits) and explicitly `Cancel`s the channel-level reservation if the
recipient-level check then fails — otherwise a request rejected on the
recipient tier would still have silently spent a channel-level token.
`redisRateLimiter` gets the equivalent atomicity from a single Lua script
touching both `KEYS[]` entries in one round trip, computing `now` via
`redis.call("TIME")` server-side (not a client-supplied timestamp — see
§4 for why that mattered).

### 3.2 `ExpiresAt`/`DLQStatusExpired`: a retry deadline independent of `MaxRetries`

`DLQHandler` retries are bounded by a hard wall-clock deadline
(`DLQMessage.ExpiresAt`), not just a retry-count ceiling. A retry-count
ceiling alone assumes every failure is equally worth retrying no matter how
much wall-clock time has passed — true for a generic delivery failure, but
not for grpop's actual payloads: a password-reset link or invite token is
itself time-boxed, independent of grpop, so retrying a send for five more
hours past that point doesn't help the recipient, it just spends vendor
budget delivering a message that's already useless.

`ClaimRetryableEvents` runs two steps every call: (1) a plain bulk
transition of any `DLQStatusPending` event whose deadline has already
passed to `DLQStatusExpired` — no cross-replica coordination needed, since
a second attempt on an already-expired row matches zero rows; (2) the
actual atomic claim (`FOR UPDATE SKIP LOCKED` in Postgres, a
`findOneAndUpdate` loop in Mongo), with an added `expires_at > now()`
predicate so a row can never be claimed past its own deadline.
`MarkRetried` checks the expiry-vs-`MaxRetries` ordering deliberately:
**expiry is checked before exhaustion**, so a message that expires with
retry budget still unused is reported as `DLQStatusExpired`, not
`DLQStatusExhausted` — the two are different failure modes worth
distinguishing in monitoring (`Exhausted` suggests a vendor-side problem
worth alerting on; `Expired` is expected/benign).

This state machine was built first in `memoryDLQHandler` (Stage 3) and
repeated in SQL/Mongo afterward — two real cross-backend bugs surfaced and
got fixed doing so: Postgres's `expires_at` column started `NOT NULL`
(rejecting the "zero = no deadline" contract `memory.go` established) and
had to become nullable with explicit `IS NULL OR ... >` handling in the
claim query; the encrypted-payload path needed base64-wrapping before
Postgres's JSONB column would accept it, since raw ciphertext isn't valid
UTF8 (Mongo's BSON binary type has no such restriction, so its equivalent
path stores ciphertext directly).

### 3.3 Rate limiting and template rendering live at the dispatcher layer, not `Service`

`ServiceDeps` (`service.go`) has **no `RateLimiter` field and no
`EmailTemplateEngine` field** — both were considered for `Service` during
Stage 15 and rejected in favor of keeping them where Stage 10/11 had
already wired them:

- **RateLimiter**: `SMTPDispatcherDeps.RateLimiter`/
  `MetaCloudDispatcherDeps.RateLimiter` already gate every individual
  `Send` call. `Service`'s own inline retry (§3.4) calls a dispatcher's
  `Send` more than once per logical send — a second gate at the `Service`
  layer would silently consume two-to-four tokens for one logical send,
  with neither layer aware of the other's consumption. Rate limiting is
  dispatcher-layer-only, and this incidentally resolves
  `ratelimiter.redis.go`'s own fail-open/fail-closed question (deferred to
  "Service, Stage 15" in an earlier doc comment): the dispatcher's
  `Wait`-then-error-on-failure wiring already **is** that decision —
  fail-closed — made at construction time, not by `Service` at all.
- **EmailTemplateEngine**: template rendering was moved into
  `dispatcher.email.smtp.go` itself during Stage 10
  (`SMTPDispatcherDeps.TemplateEngine`), since content-mode resolution
  (`TemplateName`/`InlineTemplate`/literal) has to happen immediately
  before the MIME message is built — `Service` never sees a template name,
  only an `EmailSender` that already knows how to render one.

### 3.4 `Service`'s pipeline: idempotency → inline retry → permanent/transient classification → DLQ → lifecycle event

```
IdempotencyKey required
  → IsProcessed check (fail closed on a store error — see below)
  → inline-retried Send (FullJitterBackoff, docs.go: no broker, so this
     retry runs synchronously on the caller's own goroutine)
  → permanent vs. transient failure classification (isPermanentSendError)
  → [transient only] PublishToDLQ
  → best-effort lifecycle event (message.sent / message.failed)
  → MarkProcessed (unconditionally, once Service is done with this key)
```

Two behaviors worth calling out because they're easy to get backwards on a
re-read of `service.go`:

- **A delivery failure is reported via `SendResult.Status`, not the
  returned `error`.** `SendEmail`/`SendWhatsApp` return `(result, nil)` even
  after inline retries are exhausted and the event has been DLQ'd — mirrors
  `grnoti`'s own `processEvent` precedent exactly (a `dispatchErr` is
  logged, never propagated; the final return is unconditional). The `error`
  return is reserved for pipeline-level failures: a missing
  `IdempotencyKey`, a broken idempotency store, or a permanent validation
  error.
- **Permanent (validation-shaped) errors are classified before retry or
  DLQ**, via `errors.Is` against grpop's own construction/validation
  sentinels (`ErrRecipientRequired`, `ErrNoContentModeSet`, `ErrEmailFromRequired`,
  `ErrWhatsAppTemplateNotApproved`, ...). These get zero inline retries
  (retrying a malformed message is pointless — it fails identically every
  time), zero DLQ entries (a DLQ entry that can never succeed just wastes a
  dashboard-visible slot), zero lifecycle events, and critically **no
  idempotency mark** — the caller can fix their bug and resend with the
  exact same key rather than being silently blocked by a false "duplicate."
  Everything else (network errors, vendor 5xx, circuit-breaker rejections)
  is transient: retried inline, DLQ'd on exhaustion, idempotency key marked
  processed either way (a caller-side redelivery of the same key must not
  re-trigger a parallel attempt once the failure has been handed to the
  DLQ for its own retry cycle).

**`IsProcessed` failing fails closed, not open.** `SendOptions.IdempotencyKey`
exists specifically to prevent a caller-visible double-send; a broken
idempotency backend silently proceeding as "not yet processed" is exactly
the failure mode the key exists to prevent, so a store error blocks the
send entirely rather than risking it.

### 3.5 The Meta Cloud API client: hand-rolled, narrow, fake-tested

`WhatsAppCloudAPIClient` is grpop's own thin interface over two Graph API
endpoints (`SendTemplateMessage`, `GetApprovedTemplates`) — no official Go
SDK exists for WhatsApp regardless of vendor, so a hand-rolled `net/http` +
`encoding/json` client (`metaCloudAPIClient`) was always required. Sending
uses `PhoneNumberID`; listing approved templates (which backs
`TemplateValidator`, §3.6) needs the WhatsApp Business Account ID instead —
a separate ID Meta assigns, added to `MetaCloudDispatcherDeps` as
`BusinessAccountID` during implementation once this distinction became
concrete (the original plan draft didn't separate the two IDs). This is
the **one deliberate exception** to grpop's real-local-backend testing
policy: Meta's Graph API has no local emulator, so `metaCloudDispatcher`'s
own retry/rate-limiter/circuit-breaker wiring is tested against a fake
`WhatsAppCloudAPIClient`, and the real HTTP client's own request-building/
auth-header/error-envelope-parsing logic is separately tested against a
real local `httptest.Server` (not Meta itself, but a real HTTP round trip).

### 3.6 `TemplateValidator`: advisory, TTL-cached, never a false negative

Meta requires every WhatsApp template to go through approval before use;
`TemplateValidator.Validate` checks both approval status and that
`TemplateVariables`' count matches the approved template's expected
parameter count (`WhatsAppTemplateInfo.ParameterCount`, parsed from the
approved template body's `{{1}}`, `{{2}}`, ... placeholder count) —
catching the two most common vendor-rejection causes before a live send is
attempted. The cache (default 5 minute TTL, refreshed lazily on a stale
`Validate` call or eagerly via `Refresh`) means "approved as of the last
check," not a live guarantee — Meta can approve, reject, or delete a
template between grpop's last refresh and a live send. This is accepted
because the vendor call itself remains the ultimate source of truth: a
passed `Validate` check never causes `Send` to skip actually calling Meta,
so the cache can only ever produce a false *positive* window (a
since-revoked template briefly still looks approved), never a false
negative that blocks a send that would have succeeded.

### 3.7 SMTP: zero dependencies, a narrow fakeable client, timeout-by-goroutine-race

`SMTPClient` (`Auth`/`Mail`/`Rcpt`/`Data`/`Close`) is satisfied directly by
`*net/smtp.Client` — no adapter type needed. `SMTPDialer.Dial` performs the
full connect + (for `SMTPTLSStartTLS`) `StartTLS` handshake internally,
since the narrow `SMTPClient` interface has no room for `Hello`/`StartTLS`
themselves; `Mail`/`Auth` trigger an implicit `EHLO` internally via
`net/smtp`'s own behavior, so no explicit `Hello` call is needed either.

`net/smtp`'s API is fully blocking with no way to cancel an in-flight call,
so `ConnectTimeout`/`SendTimeout` enforcement races the whole
dial-through-close transaction against a timer on a background goroutine
(`transact`) rather than threading a context through `net/smtp` itself. A
timed-out call's goroutine is abandoned, not interrupted — it keeps running
to its own eventual completion or I/O error in the background. This is a
known, documented limitation (see `transact`'s own doc comment), not an
oversight: the alternative (no timeout at all) risks a hung vendor
connection stalling a caller's request indefinitely, which is strictly
worse.

## 4. Notable bugs caught before they shipped

Two classes of bug were caught by empirical verification against real
backends before being wired into Go code, consistent with this repo's
"verify against the real thing, not an assumption" practice:

- **Postgres JSONB + `MessageEncryptor`**: `MessageEncryptor`-produced
  ciphertext isn't valid UTF8; writing it directly to the JSONB
  `message_data` column failed with `invalid byte sequence for encoding
  "UTF8"`. Fixed by base64-wrapping ciphertext as a JSON string scalar.
- **A pasted 5-point external code review of `ratelimiter.redis.go`**,
  independently verified point-by-point rather than accepted blindly — all
  five were confirmed real and fixed: a client-supplied clock (replaced
  with `redis.call("TIME")`, verified empirically to work inside a Lua
  script against real Redis — Redis 5+'s effects-based replication removed
  the older restriction on non-deterministic commands like `TIME` inside
  `EVAL`), unhashed recipient identifiers in Redis key names (SHA-256'd to
  remove delimiter-injection risk), `Wait`'s unjittered fixed poll interval
  (jittered), an undocumented fail-open/fail-closed policy (documented, and
  ultimately resolved at the dispatcher layer — see §3.3), and an unchecked
  type assertion on the Lua script's return value (made safe).

## 5. Testing philosophy and coverage

Every storage backend is tested against a **real local Docker container** —
Postgres, MongoDB (replica set + auth), Redis, and Mailpit (SMTP) — never a
mock, via `contract_*_test.go` shared behavioral suites plus per-backend
test files, `t.Skip`-ing when the corresponding container isn't reachable.
`-race` is mandatory. The Meta Cloud API client is the one documented
exception (§3.5).

`internal/postgresdb` (sqlc-generated query code) is excluded from the
coverage target the same way `grnoti`'s does — it's exercised indirectly,
at high volume, by every `dlq.postgres_test.go`/contract test that runs
against a real Postgres container, but Go's default per-package coverage
accounting only attributes coverage to a package that has its own test
files, so its standalone number reads as near-zero regardless of how
thoroughly it's actually exercised. `make coverage-check` measures the root
package only (`go test -cover .`), which is where nearly all of grpop's own
logic — as opposed to sqlc's generated SQL-parameter marshaling — actually
lives.

A small number of defensive branches remain genuinely uncovered by design,
not oversight — e.g. `golang.org/x/time/rate.Reservation.OK()` returning
false is only possible when requesting more tokens than the limiter's
burst, which `NewLocalRateLimiter`'s own validation (`burstSize >=
requestsPerSecond > 0`) makes structurally unreachable given a
correctly-constructed limiter. These are kept as real checks rather than
assumed away — the same judgment call `grnoti`'s own codebase documents in
at least one place — not chased with contrived tests whose only purpose
would be moving a coverage percentage.
