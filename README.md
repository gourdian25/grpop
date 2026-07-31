# grpop

[![Go Reference](https://pkg.go.dev/badge/github.com/gourdian25/grpop.svg)](https://pkg.go.dev/github.com/gourdian25/grpop)
[![Go Version](https://img.shields.io/badge/go-1.26.4+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Send-only, multi-channel transactional-messaging delivery library for the
gourdian ecosystem (`github.com/gourdian25/grpop`): email (stdlib SMTP) and
WhatsApp (Meta Cloud API, direct) dispatch, idempotent send processing, a
hard-deadline dead-letter retry queue, circuit breaking, and
per-channel-and-per-recipient rate limiting, behind a set of storage- and
vendor-agnostic interfaces.

grpop is **not** an email-retrieval/POP3 library despite the name, and
**not** a push-notification library — that's the sibling module
[grnoti](https://github.com/gourdian25/grnoti)'s job. grpop has no concept
of "user" or "preferences"; it's about delivery, not identity — the
recipient address/number is supplied per-send by the caller.

Status: feature-complete per the 16-stage build plan
([docs/plan/grpop-plan.md](docs/plan/grpop-plan.md)), pre-tagged-release.
`golangci-lint run` reports 0 issues; test coverage is 95.7%+ on the root
package, enforced by a 95% gate (`make coverage-check`), verified against
real local PostgreSQL/MongoDB/Redis/Mailpit instances (see
[CLAUDE.md](CLAUDE.md) for the docker setup) — Meta's WhatsApp Cloud API is
the one deliberate exception, tested against a fake client since Meta ships
no local emulator.

## Table of Contents

- [Part of the gourdian25 ecosystem](#part-of-the-gourdian25-ecosystem)
- [Install](#install)
- [Dependencies](#dependencies)
- [Quickstart](#quickstart)
- [An intermediate example: DLQ, circuit breaker, rate limiting](#an-intermediate-example-dlq-circuit-breaker-rate-limiting)
- [Configuration](#configuration)
- [Public API overview](#public-api-overview)
- [Why this shape](#why-this-shape)
- [Error handling](#error-handling)
- [Limitations / out of scope](#limitations--out-of-scope)
- [Testing](#testing)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## Part of the gourdian25 ecosystem

grpop is one of several small, independent Go libraries meant to be used
together:

- [grcache](https://github.com/gourdian25/grcache) — backend-agnostic
  caching abstraction; grpop's `NewCacheIdempotencyStore` wraps any
  `grcache.Cache` directly, no adapter needed.
- [grevents](https://github.com/gourdian25/grevents) — an in-process event
  bus; grpop optionally publishes `message.sent`/`message.failed` lifecycle
  events through it — best-effort, so a nil bus or a publish failure never
  affects the actual send it follows.
- [grnoti](https://github.com/gourdian25/grnoti) — push notifications (FCM);
  the sibling this module was deliberately named to avoid colliding with.
- [gourdiantoken](https://github.com/gourdian25/gourdiantoken) — JWT
  access/refresh token issuance; a natural source of the reset/invite tokens
  grpop delivers links for.
- [graudit](https://github.com/gourdian25/graudit) — an append-only,
  tamper-evident audit log, if a durable send-history record beyond the DLQ
  is ever needed.

## Install

```sh
go get github.com/gourdian25/grpop
```

## Dependencies

grpop is a single flat package with no subpackages of its own (see
[Why this shape](#why-this-shape)). Unlike `grnoti`, importing grpop pulls
in **zero vendor-messaging SDKs and zero message-broker client library**:
email goes over plain stdlib `net/smtp` (any SMTP-speaking vendor — SES,
SendGrid, Postmark, Mailgun, a bare relay — is just a config value, not a
Go dependency), and WhatsApp goes directly against Meta's Graph API over a
hand-rolled `net/http` client (Meta ships no official Go SDK regardless).
The only third-party imports anywhere in this module are for its own
storage/rate-limiting backends — `pgx/v5`, `go-redis/v9`, optionally
`mongo-driver` — and `grcache`'s/`grevents`' own lightweight root-interface
packages. A consumer using only the in-memory backends and a local rate
limiter pulls in none of even those.

## Quickstart

```go
engine := grpop.NewEmailTemplateEngine(grpop.EmailTemplateEngineConfig{})
engine.RegisterTemplate("password-reset", grpop.EmailTemplate{
    SubjectTemplate:  "Reset your password",
    HTMLBodyTemplate: `<p>Click <a href="{{.ResetLink}}">here</a> to reset your password.</p>`,
})

emailSender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
    Addr:           "email-smtp.us-east-1.amazonaws.com:587",
    Auth:           smtp.PlainAuth("", sesUser, sesPass, "email-smtp.us-east-1.amazonaws.com"),
    DefaultFrom:    "no-reply@example.com",
    TemplateEngine: engine,
})
if err != nil {
    log.Fatal(err)
}
defer emailSender.Close()

// WhatsAppSender is required by Service even if you only send email today
// — swap NewDryRunWhatsAppSender for grpop.NewMetaCloudWhatsAppDispatcher
// once you have real Meta credentials.
whatsAppSender := grpop.NewDryRunWhatsAppSender(nil)

cache, _ := grcache.NewMemoryCache() // swap for NewRedisCache/NewMongoCache in production
svc, err := grpop.NewService(grpop.ServiceDeps{
    IdempotencyStore: grpop.NewCacheIdempotencyStore(cache),
    DLQHandler:       grpop.NewMemoryDLQHandler(5, time.Second, 30*time.Second, 20),
    EmailSender:      emailSender,
    WhatsAppSender:   whatsAppSender,
    Config:           grpop.DefaultServiceConfig(),
})
if err != nil {
    log.Fatal(err)
}
defer svc.Close()

result, err := svc.SendEmail(ctx, grpop.EmailMessage{
    To:           "user@example.com",
    TemplateName: "password-reset",
    TemplateData: map[string]any{"ResetLink": link},
}, grpop.SendOptions{
    IdempotencyKey: hashOf(token), // required — see Configuration below
    RetryExpiresAt: tokenExpiresAt, // the reset token's own real expiry
})
```

See [example/main.go](example/main.go) and
[example/templates_auth.go](example/templates_auth.go) for a complete,
runnable walkthrough covering all four use cases this design is grounded
against (forgot-password + three invite variants) — `go run ./example`,
against a local [Mailpit](https://github.com/axllent/mailpit) container by
default, no real credentials required.

## An intermediate example: DLQ, circuit breaker, rate limiting

The Quickstart above already wires a `DLQHandler` (required by
`ServiceDeps`). A more realistic production wiring also protects the SMTP
dispatch path itself with a circuit breaker and a rate limiter — both are
**dispatcher-level** concerns in grpop, not `Service`-level (see
[Configuration](#configuration) for why):

```go
// After 5 consecutive SMTP failures, stop calling the relay for 30s and
// fail fast instead; a closed breaker's failure counter resets after 1
// minute with no failures.
breaker, err := grpop.NewCircuitBreaker(5, 30*time.Second, time.Minute)
if err != nil {
    log.Fatal(err)
}

// A local (per-process) rate limiter: at most 50 sends/sec per channel AND
// per (channel, recipient) — a scripted forgot-password sweep across many
// distinct recipients is still capped by the per-channel tier even though
// each individual recipient is well under its own limit. Swap for
// grpop.NewRedisRateLimiter(...) to share one limit across replicas.
limiter, err := grpop.NewLocalRateLimiter(50, 100, 0)
if err != nil {
    log.Fatal(err)
}

emailSender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
    Addr:           "email-smtp.us-east-1.amazonaws.com:587",
    Auth:           smtp.PlainAuth("", sesUser, sesPass, "email-smtp.us-east-1.amazonaws.com"),
    DefaultFrom:    "no-reply@example.com",
    TemplateEngine: engine,
    RateLimiter:    limiter,  // gates every Send attempt, including inline retries
    CircuitBreaker: breaker,  // wraps every vendor call
})
```

`DLQHandler.ClaimRetryableEvents` is never called by `Service` itself —
draining the queue is a separate, external retry-worker process's job,
polling periodically:

```go
events, err := dlqHandler.ClaimRetryableEvents(ctx, 50) // atomically claims up to 50
for _, ev := range events {
    var result grpop.SendResult
    var attemptErr error
    switch ev.MessageData.Channel {
    case grpop.ChannelEmail:
        result, attemptErr = emailSender.Send(ctx, *ev.MessageData.Email)
    case grpop.ChannelWhatsApp:
        result, attemptErr = whatsAppSender.Send(ctx, *ev.MessageData.WhatsApp)
    }
    _ = dlqHandler.MarkRetried(ctx, ev.SendID, attemptErr == nil, attemptErr)
}
```

## Configuration

### `NewService(ServiceDeps) (Service, error)`

| `ServiceDeps` field | Required? | Notes |
|---|:---:|---|
| `IdempotencyStore` | **required** | duplicate-send suppression |
| `DLQHandler` | **required** | receives sends whose inline retry is exhausted |
| `EmailSender` | **required** | e.g. `NewSMTPDispatcher` |
| `WhatsAppSender` | **required** | e.g. `NewMetaCloudWhatsAppDispatcher` |
| `EventBus` (`grevents.Bus`) | optional | receives `message.sent`/`message.failed` lifecycle events — nil-safe, always best-effort |
| `Metrics` | optional | only `IncIdempotencyDedupHit`/`IncDLQPublished` — `Service` deliberately never calls `ObserveSendLatency`/`IncSendResult` itself, since the dispatcher already does when the same `Metrics` is wired into its own `Deps` |
| `Config` (`ServiceConfig`) | optional | see below; zero value is upgraded to `DefaultServiceConfig()`'s values field-by-field |

**`ServiceDeps` deliberately has no `RateLimiter` or `EmailTemplateEngine`
field.** Rate limiting lives on `SMTPDispatcherDeps`/`MetaCloudDispatcherDeps`
instead — `Service`'s own inline retry calls a dispatcher's `Send` more than
once per logical send, so a second gate at the `Service` layer would
silently double-consume tokens. Template rendering lives on
`SMTPDispatcherDeps.TemplateEngine` — `Service` never sees a `TemplateName`,
only the already-template-aware `EmailSender`.

### `ServiceConfig` (via `DefaultServiceConfig()`)

| Field | Default | Effect |
|---|---|---|
| `DefaultIdempotencyTTL` | `24h` | Used when `SendOptions.IdempotencyTTL` is zero |
| `DefaultMaxRetryAge` | `24h` | Used when `SendOptions.RetryExpiresAt` is zero — a genuine guess, not derived from any real token TTL; **set `RetryExpiresAt` explicitly** to your message's real expiry (a reset link's TTL, an invite token's expiry) rather than relying on this |
| `MaxInlineRetries` | `2` | Total attempts = `MaxInlineRetries + 1`, run inline on the caller's own goroutine before falling through to the DLQ |
| `InlineRetryBaseDelay` / `InlineRetryMaxDelay` | `200ms` / `2s` | Feed `FullJitterBackoff` between inline retry attempts |

### `SendOptions`

| Field | Required? | Notes |
|---|:---:|---|
| `IdempotencyKey` | **required** | `Service.SendEmail`/`SendWhatsApp` return `ErrIdempotencyKeyRequired` if empty — grpop's retry is at-least-once, not exactly-once, so this is the only guarantee against a caller-visible double-send |
| `IdempotencyTTL` | optional | 0 uses `ServiceConfig.DefaultIdempotencyTTL` |
| `SkipRateLimit` | optional | escape hatch, e.g. an admin-triggered manual resend (dispatcher-level, only honored if the dispatcher itself checks it) |
| `RetryExpiresAt` | optional | 0 uses `ServiceConfig.DefaultMaxRetryAge` measured from the first failure — set this to your message's real expiry when you know it |

A delivery failure — even after inline retries are exhausted and the event
has been DLQ'd — is reported via `SendResult.Status == SendStatusFailed`,
**not** a non-nil `error` return. The `error` return is reserved for
pipeline-level failures: a missing `IdempotencyKey`, a broken idempotency
backend, or a permanent validation error (bad message shape) that would
fail identically on every retry and therefore gets no DLQ entry, no
lifecycle event, and no idempotency mark — you can fix the bug and resend
with the exact same key.

## Public API overview

**EmailSender**

| Backend | Constructor |
|---|---|
| SMTP (any vendor) | `NewSMTPDispatcher(SMTPDispatcherDeps)` |
| In-memory (tests/dev) | `NewMemoryEmailSender()` |
| Dry run (staging, redacted logging) | `NewDryRunEmailSender(Logger)` |

**WhatsAppSender**

| Backend | Constructor |
|---|---|
| Meta WhatsApp Business Cloud API | `NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps)` |
| In-memory (tests/dev) | `NewMemoryWhatsAppSender()` |
| Dry run (staging, redacted logging) | `NewDryRunWhatsAppSender(Logger)` |

**IdempotencyStore**

| Backend | Constructor |
|---|---|
| Any `grcache.Cache` (Redis, Mongo, in-memory, ...) | `NewCacheIdempotencyStore(grcache.Cache)` |

**DLQHandler**

| Backend | Constructor |
|---|---|
| In-memory | `NewMemoryDLQHandler(maxRetries, retryDelay, maxRetryDelay, maxAttemptHistory)` |
| PostgreSQL (`FOR UPDATE SKIP LOCKED`) | `NewPostgresDLQHandler(PostgresDLQHandlerConfig)` |
| MongoDB (`findOneAndUpdate`) | `NewMongoDLQHandler(MongoDLQHandlerConfig)` |

**RateLimiter** (two-tier: per-channel AND per-(channel,recipient))

| Variant | Constructor |
|---|---|
| Local (per-process) | `NewLocalRateLimiter(requestsPerSecond, burstSize, recipientCacheSize int)` |
| Redis (distributed, Lua-scripted) | `NewRedisRateLimiter(RedisRateLimiterConfig)` |

**CircuitBreaker**

| Variant | Constructor |
|---|---|
| The one implementation, returned as the `CircuitBreaker` interface | `NewCircuitBreaker(maxFailures, timeout, resetTimeout)` or `NewCircuitBreakerWithConfig(CircuitBreakerConfig)` |

**EmailTemplateEngine**

| Variant | Constructor |
|---|---|
| `html/template`/`text/template`-based | `NewEmailTemplateEngine(EmailTemplateEngineConfig)` |

**TemplateValidator** (WhatsApp template-approval pre-check)

| Variant | Constructor |
|---|---|
| Meta-backed, TTL-cached | `NewMetaTemplateValidator(MetaTemplateValidatorDeps)` |

## Why this shape

grpop is a single flat package, no subpackages — matching `grnoti`'s and
`gourdiantoken`'s precedent rather than `grcache`'s/`graudit`'s
subpackage-per-backend layout. That layout exists specifically to keep
unused backend drivers out of a consumer's dependency graph; grpop's
version of that tradeoff is unusually cheap, since the dispatch layer
itself (the part most consumers actually exercise) has zero third-party
dependencies to begin with — see [Dependencies](#dependencies). The only
real multi-backend surface left is storage (Postgres/Mongo for the DLQ,
Redis for the distributed rate limiter), a smaller version of the same
tradeoff every sibling already accepts. See
[docs/architecture.md](docs/architecture.md) for the full design rationale,
and [docs/plan/grpop-plan.md](docs/plan/grpop-plan.md) for the original
research and stage-by-stage build log.

## Error handling

Sentinel errors (`errors.go`), matched with `errors.Is` — there is
deliberately no `IsX(err) bool` helper, consistent with every other
`gourdian25` repo. A backend-native error (`pgx.ErrNoRows`,
`redis.Nil`, `mongo.ErrNoDocuments`, a Graph API error envelope) is always
translated into one of these before crossing a grpop interface boundary,
never leaked unwrapped:

`ErrClosed`, `ErrBackendUnavailable`, `ErrIdempotencyKeyRequired`,
`ErrRecipientRequired`, `ErrNoContentModeSet`, `ErrMultipleContentModesSet`,
`ErrEmailTemplateNotFound`, `ErrInlineTemplateTooLarge`,
`ErrEmailFromRequired`, `ErrEmailTemplateEngineRequired`,
`ErrWhatsAppTemplateNameRequired`, `ErrWhatsAppLanguageCodeRequired`,
`ErrWhatsAppTemplateNotApproved`, `ErrWhatsAppTemplateArityMismatch`,
`ErrRateLimited`, `ErrDLQEventNotFound`, `ErrDLQEventNotClaimed`,
`ErrCircuitOpen`, `ErrTooManyRequests`.

## Limitations / out of scope

- **SendStatusSent means "the vendor accepted the request," never
  "arrived" or "was read."** grpop has no vendor-side bounce/complaint/
  delivery-receipt feedback for either channel.
- **Retries are at-least-once, not exactly-once.** A network error after
  the vendor already accepted a send is indistinguishable from one before —
  this is why `SendOptions.IdempotencyKey` is required, not optional.
- **No message broker or queue anywhere in this package.** Every `Send`
  call is direct and synchronous on the caller's goroutine; `DLQHandler`'s
  table is a durable, pull-based retry store a consumer's own worker polls,
  never a transport a send passes through.
- **SMS is not supported, and never was in scope for this design** — see
  `docs/plan/grpop-plan.md`'s revision note for the reasoning (dependency
  minimization; WhatsApp/email cover the four grounding use cases).
  Multi-recipient email (`EmailMessage.To []string`) is also cut: a real
  SMTP transaction can partially fail per-recipient in a way `SendResult`
  has no room to represent, so send once per recipient instead.
- **WhatsApp has no local test emulator.** Unlike Postgres/Mongo/Redis/
  Mailpit, which are all tested against real local instances, the Meta
  Cloud API client is tested against a hand-rolled fake `WhatsAppCloudAPIClient`
  — there is no equivalent "run it locally" option.
- **`grpop_dlq` stores full send payloads — including secrets like reset
  links or invite tokens — in the clear unless a `MessageEncryptor` is
  configured.** Absent one, operate it with credentials-table-grade access
  control.
- **grpop never refreshes or rotates a Meta access token.** Token lifecycle
  is entirely the operator's responsibility.
- **`CircuitBreaker` state is per-process, deliberately not centralized** —
  avoids a synchronized thundering-herd retry the moment a relay recovers.
- **Postgres schema management is additive only** — every `New*Postgres*`
  constructor applies `CREATE TABLE/INDEX IF NOT EXISTS` on connect; no
  down-migration, no versioning. Set `PostgresConfig.SkipSchemaEnsure: true`
  once you own the schema via your own migration tool.

See [docs.go](docs.go)'s "Precise, non-aspirational claims" section for the
complete list this draws from.

## Testing

Real local Docker containers for every storage backend — no mocking. See
[CLAUDE.md](CLAUDE.md) for exact `docker run` commands, or just:

```sh
make docker-up   # Postgres, Redis, Mongo (replica set + auth), Mailpit
make test        # or `make race` for the race detector
make docker-down # stop them when done (state preserved for a fast restart)
```

## Development

```sh
make docker-up   # start the shared test containers
make precommit   # fmt + vet + lint + race + coverage-check
make docker-down # stop them when you're done
```

These containers are shared with `graudit`, `grcache`, `grnoti`, and
`gourdiantoken` (each gets its own database/keyspace) — Mailpit is
grpop-specific, no sibling repo needs it.

## Contributing

Issues and PRs are welcome at
[github.com/gourdian25/grpop](https://github.com/gourdian25/grpop). Please
run `make precommit` (fmt + vet + lint + race + coverage-check) before
submitting.

## License

MIT — see [LICENSE](LICENSE).
