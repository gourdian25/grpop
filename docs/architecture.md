# grpop architecture

This document records how grpop is put together and *why*, with particular
attention to decisions made or revised during implementation that the
original plan didn't fully anticipate (see
[docs/plan/grpop-plan.md](plan/grpop-plan.md) for the stage-by-stage
account). It is a design reference, not a tutorial — see the
[README](../README.md) for a quickstart and
[example/main.go](../example/main.go) for a runnable, narrated walkthrough.

## 0. Delivery only (0.2.0, 2026-10-23)

grpop sends one message once and reports what the vendor said; it keeps
nothing and retries nothing. Up to 0.1.0 it also had an orchestrating
`Service` (an idempotency check, an inline retry, a dead-letter retry table
in memory, Postgres or Mongo, an optional payload encryptor, lifecycle
events on grevents) and its own two-tier rate limiters (in-process, and a
Lua-scripted Redis one). Those were removed for grsentry's email work
([plan/2026-10-23-grpop-delivery-only.md](plan/2026-10-23-grpop-delivery-only.md)):

- **Retry, durability and duplicates belong to the caller.** Only the
  caller knows the transaction a message belongs to and how long its link
  stays valid, and only a store written in that transaction (an outbox) can
  promise a committed message is never lost. A dead-letter table that only
  catches a send after its in-memory retries failed can't: a crash during
  those retries lost the message. Code review on 2026-10-23 also found the
  removed layer could lose a message outright (a failed dead-letter write
  still marked the key processed), left a crashed worker's claimed row
  `retrying` forever (no lease), raced its duplicate check, and refused an
  email-only setup.
- **No rate limiter of grpop's own.** `RateLimiter` is
  `Allow(ctx, key) (bool, error)`, grsentry's `pkg/grlimiter.Limiter`'s
  method set, so that limiter plugs in without an import either way (grpop
  importing grsentry would make the two modules require each other).

`FullJitterBackoff` stays, exported, for the caller's retry loop. The
sections below describe what is left; the removed parts' design is in git
history and in [plan/grpop-plan.md](plan/grpop-plan.md).

## 1. Package shape

grpop's public API is a single flat package with no subpackages and no
third-party dependencies:

```
interfaces.go / types.go / errors.go / logger.go / docs.go
retrystrategy.go            circuitbreaker.go           payloadvalidator.go
ratelimiter.go (the shared two-limiter check)
templateengine.email.go
dispatcher.email.smtp.go    dispatcher.whatsapp.metacloud.go
templatevalidator.whatsapp.go
dryrun.go                   memory.go (in-memory senders)
example/
```

File naming: `dispatcher.<channel>.<vendor>.go` for the senders — a third
segment, since grpop's dispatch layer has a genuinely two-dimensional axis
(channel × vendor) that `grnoti`'s single-channel, single-vendor dispatch
layer didn't need. Email goes over stdlib `net/smtp`; WhatsApp goes over a
hand-rolled `net/http` client against Meta's Graph API (no official Go SDK
exists). See `docs.go`'s package-level doc comment.

## 2. Interfaces and implementations

| Interface | Implementations |
|---|---|
| `EmailSender` | `smtpDispatcher` (stdlib SMTP, any vendor); `dryRunEmailSender` (staging); `MemoryEmailSender` (tests) |
| `WhatsAppSender` | `metaCloudDispatcher` (Meta Graph API, direct); `dryRunWhatsAppSender` (staging); `MemoryWhatsAppSender` (tests) |
| `RateLimiter` | none: the caller's (grsentry's `pkg/grlimiter` fits as it is) |
| `CircuitBreaker` | `standardCircuitBreaker` — the one implementation, returned as the interface type |
| `EmailTemplateEngine` | `defaultEmailTemplateEngine` (`html/template` + `text/template`) |
| `TemplateValidator` | `metaTemplateValidator` — TTL-cached over `WhatsAppCloudAPIClient.GetApprovedTemplates` |
| `WhatsAppCloudAPIClient` | `metaCloudAPIClient` (real, `net/http`); fake in tests — Meta's Graph API has no local emulator (§5) |

## 3. Key design decisions

### 3.1 Two rate limiters, both the caller's: per channel and per recipient

A sender takes two optional `RateLimiter`s: `RateLimiter`, keyed by the
channel alone, and `RecipientRateLimiter`, keyed by the channel and a
SHA-256 of the recipient (so the limiter's backend never stores an address
or number). A per-recipient limit alone can't stop one actor from
triggering many distinct recipients' sends in a burst (a scripted
forgot-password sweep across a whole user list); a per-channel limit alone
can't stop one recipient from being spammed by many distinct callers.
`checkRateLimits` asks the recipient's first, so a send one recipient's
bucket refuses spends nothing from the channel's. A refusal is
`ErrRateLimited` at once (a sender never waits for a token: the caller's
retry spaces it out), and a limiter that errors stops the send (fail
closed).

### 3.2 Rate limiting and template rendering live on the senders

Both gate or shape every individual `Send`: the limiters and the circuit
breaker on `SMTPDispatcherDeps`/`MetaCloudDispatcherDeps`, and template
rendering on `SMTPDispatcherDeps.TemplateEngine`, since content-mode
resolution (`TemplateName`/`InlineTemplate`/literal) has to happen
immediately before the MIME message is built.

### 3.3 A failure is the returned error

`Send` returns `(SendResult{Status: SendStatusFailed}, err)` for a failed
attempt and `(result, nil)` for an accepted one; there is no second channel
for "failed but handled". Validation failures (`ErrRecipientRequired`,
`ErrNoContentModeSet`, `ErrEmailFromRequired`,
`ErrWhatsAppTemplateNotApproved`, ...) fail identically on every attempt,
so a caller's retry should stop on them.

### 3.4 The Meta Cloud API client: hand-rolled, narrow, fake-tested

`WhatsAppCloudAPIClient` is grpop's own thin interface over two Graph API
endpoints (`SendTemplateMessage`, `GetApprovedTemplates`) — no official Go
SDK exists for WhatsApp regardless of vendor, so a hand-rolled `net/http` +
`encoding/json` client (`metaCloudAPIClient`) was always required. Sending
uses `PhoneNumberID`; listing approved templates (which backs
`TemplateValidator`, §3.5) needs the WhatsApp Business Account ID instead —
a separate ID Meta assigns, added to `MetaCloudDispatcherDeps` as
`BusinessAccountID` during implementation once this distinction became
concrete (the original plan draft didn't separate the two IDs). This is
the **one deliberate exception** to grpop's real-local-backend testing
policy: Meta's Graph API has no local emulator, so `metaCloudDispatcher`'s
own rate-limiter/circuit-breaker wiring is tested against a fake
`WhatsAppCloudAPIClient`, and the real HTTP client's own request-building/
auth-header/error-envelope-parsing logic is separately tested against a
real local `httptest.Server` (not Meta itself, but a real HTTP round trip).

### 3.5 `TemplateValidator`: advisory, TTL-cached, never a false negative

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

### 3.6 SMTP: zero dependencies, a narrow fakeable client, timeout-by-goroutine-race

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

Up to 0.1.0, two classes of bug in the since-removed storage layer were
caught by empirical verification against real backends: ciphertext written
straight to a Postgres JSONB column (fixed by base64-wrapping), and five
points of an external review of the Redis rate limiter (a client-supplied
clock, unhashed recipient keys, an unjittered poll, an undocumented
fail-open/fail-closed policy, an unchecked type assertion). The hashed
recipient key and the fail-closed policy carry over to `checkRateLimits`
(§3.1).

## 5. Testing philosophy and coverage

The SMTP sender is tested against a **real local Mailpit** (`t.Skip`-ing
when it isn't reachable) as well as a fake `SMTPClient`; `-race` is
mandatory. The Meta Cloud API client is the one documented exception
(§3.4): a fake client for the sender's wiring, an `httptest.Server` for the
real HTTP client. `make coverage-check` gates the package at 95%.
