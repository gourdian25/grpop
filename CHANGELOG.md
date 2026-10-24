# Changelog

All notable changes to this project are documented in this file.

## [0.2.0] - 2026-10-24

**Breaking: grpop is delivery only.** A sender sends one message once and
reports what the vendor said; grpop keeps nothing and retries nothing.
Retrying, keeping a message durably and suppressing duplicates are the
caller's (decided 2026-10-23 for grsentry's email work, which does all three
with an outbox; see `docs/plan/2026-10-23-grpop-delivery-only.md`). An
earlier draft of this version, never tagged, moved the Mongo dead-letter
backend to `mongo-driver/v2`; that backend is now removed.

### Added

- `ErrPermanent`, `ErrInvalidAddress`, `MetaCloudDispatcherDeps.GraphAPIVersion`,
  `EmailMessage.IdempotencyKey`, `NewResendDispatcher`/`ResendDispatcherDeps`.
- Typed WhatsApp values: `WhatsAppMessage.Header`, `Body` and `Buttons`;
  `WhatsAppParameter`, `WhatsAppCurrency`, `WhatsAppDateTime`,
  `WhatsAppMedia` (`WhatsAppMediaKind`), `WhatsAppLocation`, `WhatsAppButton`
  (`WhatsAppButtonKind`: URL, quick reply, copy code, one-time code); the
  constructors `WhatsAppText`, `WhatsAppNamedText`, `WhatsAppImage`,
  `WhatsAppVideo`, `WhatsAppDocument` and `WhatsAppOTP`.
- `ErrInvalidPhoneNumber` and `ErrWhatsAppParameterInvalid`, both permanent.
- `NewMetaCloudAPIClient(MetaCloudAPIClientConfig)`: the real Graph API
  client, exported so one client serves a sender and a `TemplateValidator`
  (before, a caller had no way to build a validator against Meta).
- `WhatsAppTemplateInfo.Named`, `HeaderFormat`, `HeaderParameters`,
  `BodyParameters` and `Buttons` (`WhatsAppTemplateButton`).

### Removed

- `WhatsAppMessage.TemplateVariables` and
  `WhatsAppCloudAPIRequest.TemplateVariables`: `Body` replaces them.
- `WhatsAppTemplateInfo.ParameterCount`: `BodyParameters` replaces it.
- `Service`, `NewService`, `ServiceDeps`, `ServiceConfig`,
  `DefaultServiceConfig`, `SendOptions` and `SendResult.Duplicate`.
- `IdempotencyStore` and `NewCacheIdempotencyStore`.
- `DLQHandler` and its three backends (`NewMemoryDLQHandler`,
  `NewPostgresDLQHandler`, `NewMongoDLQHandler`), the `grpop_dlq` schema,
  `PostgresConfig`, the `DLQ*` types, and `MessageEncryptor`.
- The lifecycle events (`TopicMessageSent`, `TopicMessageFailed`,
  `PublishMessageSent`, `PublishMessageFailed`).
- grpop's own rate limiters (`NewLocalRateLimiter`, `NewRedisRateLimiter`)
  and `RateLimiterStats`.
- `ErrClosed`, `ErrBackendUnavailable`, `ErrIdempotencyKeyRequired`,
  `ErrDLQEventNotFound`, `ErrDLQEventNotClaimed`; `Metrics.IncDLQPublished`
  and `Metrics.IncIdempotencyDedupHit`.
- Every third-party dependency: pgx, mongo-driver, go-redis, grcache,
  grevents and `golang.org/x/time`.

### Changed

- `RateLimiter` is now `Allow(ctx context.Context, key string) (bool, error)`,
  the method set of grsentry's `pkg/grlimiter.Limiter`, so that limiter
  plugs in as it is. Each sender's deps take two, both optional:
  `RateLimiter` (key: the channel) and `RecipientRateLimiter` (key: the
  channel and a SHA-256 of the recipient). A refused send returns
  `ErrRateLimited` at once instead of waiting for a token; a limiter error
  stops the send. `Metrics.IncRateLimitRejected` now counts refusals.
- `make docker-up`/`docker-down` start and stop Mailpit only.
- **`ErrPermanent`:** every failure that will recur on every attempt
  matches it (the validation sentinels, a template that won't render, an
  SMTP `5xx` reply, a Meta refusal outside its retry-later codes, `429` and
  `5xx`); everything else is temporary. Before, an SMTP `550` looked like a
  network blip.
- **Addresses:** `From`, `To` and `Reply-To` are parsed with `net/mail`; an
  unparseable one or one carrying a CR or LF is `ErrInvalidAddress` (a
  `Reply-To` with a line break used to add a header, such as a `Bcc`).
  Display names are supported; `MAIL FROM`/`RCPT TO` take the bare address.
  An invalid `SMTPDispatcherDeps.DefaultFrom` fails at construction.
- **Logs:** the SMTP and dry-run senders log a recipient masked
  (`a***@example.com`), also inside an SMTP reply that echoes it.
- **A Resend sender:** `NewResendDispatcher(ResendDispatcherDeps)` sends
  through Resend's HTTP API (hand-rolled, no SDK). `EmailMessage.IdempotencyKey`
  goes as Resend's `Idempotency-Key`, so a retried message is sent once
  within 24 hours; refusals are classed from Resend's error reference.
- **One email preparation step:** every email sender checks, renders and
  parses a message the same way (`prepareEmail`), so a second email vendor
  sends exactly what the SMTP sender would.
- **`EmailMessage.IdempotencyKey`:** optional, the same on every attempt at
  one message. The SMTP sender makes the `Message-ID` its SHA-256, so a
  retried message carries the same `Message-ID`; without a key it stays
  random.
- **WhatsApp messages carry every value kind Meta takes.** Before, only
  positional body text could be sent, so no authentication (one-time code)
  template could, and a gap in `TemplateVariables`' keys silently cut the
  list. `WhatsAppMessage.To` must be in international form, and every value
  is checked against Meta's send-time rules before the call.
- **`TemplateValidator.Validate(ctx, msg WhatsAppMessage)`** takes the
  message (was name, language and the variables map).
- **The template validator is usable and keeps its promise:**
  - the listing reads every page (it read one, so a template past it read
    as unapproved), asks for `status=APPROVED`, and never sends the token
    to a `paging.next` URL;
  - placeholders are counted without the `{{20}}` cap, named ones included;
  - a message is checked against the header, body and buttons
    (`ErrWhatsAppTemplateArityMismatch` now says what differs);
  - a failed listing no longer blocks sends: the last list is used, or the
    send goes through;
  - a refusal is checked against one new listing first (at most one per 30
    seconds).
- **The circuit breaker counts only temporary failures**, in every sender:
  a permanent one is recorded as the vendor answering, so a run of bad
  addresses no longer opens it. The SMTP sender classes a `5xx` inside the
  breaker for this.
- **The WhatsApp dry run** also logs whether there is a header and how many
  body values and buttons there are, never a value.
- **Meta Graph API version:** `MetaCloudDispatcherDeps.GraphAPIVersion`,
  default `v26.0`. The hard-coded `v20.0` stopped being served on
  2026-09-24.
- `example/` sends through the SMTP sender with a caller-side retry loop on
  `FullJitterBackoff`.

## [0.1.0] - 2026-07-31

### Added

- Core contracts: `EmailMessage`, `WhatsAppMessage`, `SendResult`,
  `SendOptions`, `DLQMessage`/`DLQEvent`, `RateLimiterStats`,
  `CircuitBreakerStats`, and the full set of storage-/vendor-agnostic
  interfaces (`EmailSender`, `WhatsAppSender`, `Service`,
  `IdempotencyStore`, `DLQHandler`, `RateLimiter`, `CircuitBreaker`,
  `EmailTemplateEngine`, `TemplateValidator`, `MessageEncryptor`,
  `Metrics`).
- Pure in-process logic: `FullJitterBackoff`, `standardCircuitBreaker`,
  per-channel payload validation (`validateEmailMessage`/
  `validateWhatsAppMessage`).
- `memory.go`: in-memory `DLQHandler` (with the full `ExpiresAt`/
  `DLQStatusExpired` retry-deadline state machine, built here first and
  repeated in SQL/Mongo afterward) and fake `EmailSender`/`WhatsAppSender`
  for tests/local dev.
- `grcache`-backed `IdempotencyStore` adapter — works with any
  `grcache.Cache` backend (Redis, Mongo, in-memory).
- Two-tier (per-channel AND per-(channel,recipient)) `RateLimiter`:
  `NewLocalRateLimiter` (per-process, bounded-LRU recipient cache) and
  `NewRedisRateLimiter` (distributed, single atomic Lua script for
  refill-and-consume across replicas).
- `NewEmailTemplateEngine`: `html/template`-based rendering for
  `HTMLBody` (auto-escaped) and `text/template` for `Subject`/`TextBody`,
  with both a registered-by-name path (`RegisterTemplate`/`Render`) and an
  ad-hoc inline path (`RenderInline`, size-capped via
  `MaxInlineTemplateBytes`) for content not known ahead of registration
  time (e.g. per-tenant custom branding).
- PostgreSQL (pgx/v5 + sqlc) and MongoDB `DLQHandler` backends: atomic
  claim via `SELECT ... FOR UPDATE SKIP LOCKED` / `findOneAndUpdate`,
  attempt-history capping (`MaxAttemptHistoryEntries`), and an optional
  `MessageEncryptor` seam for at-rest encryption of `message_data`.
  `PostgresConfig.Pool` allows sharing one `*pgxpool.Pool` across stores
  instead of each dialing its own.
- Cross-backend contract tests (`contract_*_test.go`): every
  `IdempotencyStore`/`DLQHandler`/`RateLimiter` implementation runs the
  same behavioral contract suite against real local Postgres/Redis/
  MongoDB containers.
- `dispatcher.email.smtp.go`: `EmailSender` over stdlib `net/smtp` — zero
  third-party dependencies, works against any SMTP-speaking vendor (SES,
  SendGrid, Postmark, Mailgun, a bare relay) via config alone.
  Construction-time TLS-mode guard (`SMTPTLSStartTLS` default;
  `SMTPTLSInsecureNoTLS` requires explicit `AllowInsecureAuth` if `Auth`
  is also set), `ConnectTimeout`/`SendTimeout` enforcement, hand-rolled
  MIME building (`multipart/alternative` when both HTML and text bodies
  are set). The first vendor-facing dispatcher in this ecosystem tested
  against a real local double (Mailpit) rather than a fake.
- `dispatcher.whatsapp.metacloud.go`: `WhatsAppSender` over Meta's
  WhatsApp Business Cloud API, direct — a hand-rolled `net/http` client
  (`WhatsAppCloudAPIClient`), since no official Go SDK exists for this
  regardless of vendor. The one deliberate exception to this repo's
  real-local-backend testing policy: Meta's Graph API has no local
  emulator, so it's tested against a fake client plus a real
  `httptest.Server` for the HTTP client's own request/response handling.
- `templatevalidator.whatsapp.go`: `NewMetaTemplateValidator` — a
  TTL-cached (default 5 min) pre-check against Meta's approved-template
  list, catching both "not approved" and "variable count doesn't match
  the approved template's expected parameter count" before a live send is
  attempted. Advisory only; the vendor call itself remains the ultimate
  source of truth.
- `dryrun.go`: `NewDryRunEmailSender`/`NewDryRunWhatsAppSender` — logging-
  only senders for staging, deliberately never logging rendered
  content/template data (grpop's payloads are frequently security-
  sensitive).
- `events.go`: best-effort, nil-safe `message.sent`/`message.failed`
  lifecycle events via an injected `grevents.Bus`, carrying only
  `SendID`/`Channel` — never the recipient address/number or message
  content.
- `NewService`: the orchestrator tying every interface above together —
  idempotency dedup (with a `Duplicate` result field and logged/metered
  dedup hits), inline Full-Jitter retry, permanent-vs-transient failure
  classification (a malformed message gets no retry/DLQ entry and no
  idempotency mark; a transient failure does), DLQ publication, and
  lifecycle events. Rate limiting and template rendering are deliberately
  dispatcher-level concerns, not `Service`-level — see
  `docs/architecture.md` §3.3 for why.
- `example/main.go` + `example/templates_auth.go`: a complete, runnable
  walkthrough (`go run ./example`) covering all four ERP use cases this
  design is grounded against (forgot-password + three invite variants),
  against a real local Mailpit container by default.
- `docs/architecture.md`: interface/backend matrix and the reasoning
  behind every major design decision, including where implementation
  diverged from the original plan and why.
- `docs/plan/grpop-plan.md`: the full research/design/build-plan
  document this repo was built from, kept in sync with implementation-
  time revisions throughout.
- `CLAUDE.md`, `Makefile` (`precommit`/`prerelease`/`coverage-check`/
  `docker-up`/`docker-down` targets), `.goreleaser.yaml`.

### Fixed

Found via this repo's real-local-services testing policy (Docker
containers, not mocks) rather than by code review alone:

- Postgres's JSONB `message_data` column rejected `MessageEncryptor`-
  produced ciphertext outright (`invalid byte sequence for encoding
  "UTF8"` — arbitrary ciphertext bytes aren't valid UTF8 in general).
  Fixed by base64-wrapping ciphertext as a JSON string scalar before
  writing it; Mongo's BSON binary type needed no equivalent change.
- `grpop_dlq.expires_at` started `NOT NULL`, rejecting `memory.go`'s own
  "zero `ExpiresAt` = no deadline" contract. Made nullable, with the
  claim query's `NULL`-handling made explicit (`expires_at IS NULL OR
  expires_at > $1` — `NULL > $1` alone evaluates to `NULL`, not `true`,
  in SQL).
- A pasted external code review of `ratelimiter.redis.go` was
  independently verified point-by-point rather than accepted on faith —
  all five findings confirmed real and fixed: a client-supplied timestamp
  inside the Lua refill script (replaced with `redis.call("TIME")`,
  verified empirically against real Redis), unhashed recipient
  identifiers in Redis key names (SHA-256'd), `Wait`'s unjittered fixed
  poll interval (jittered), an undocumented fail-open/fail-closed policy
  on a Redis outage (documented, and ultimately resolved at the
  dispatcher layer rather than left to `RateLimiter` itself), and an
  unchecked type assertion on the Lua script's return value (made safe).

### Testing

- 260+ tests, `-race` mandatory, 95.7% coverage on the root package
  (enforced by a 95% gate, `make coverage-check`) — verified against real
  local PostgreSQL, MongoDB (replica set + auth), Redis, and Mailpit
  containers. `golangci-lint run` reports 0 issues.
- A small number of defensive branches remain deliberately uncovered
  (e.g. a `golang.org/x/time/rate.Reservation.OK()` false case that
  `NewLocalRateLimiter`'s own validation makes structurally unreachable)
  — documented in `docs/architecture.md` §5 as accepted gaps rather than
  chased with contrived tests.

### Repository scaffolding

- `go.mod`, lint/release config, `Makefile`, `LICENSE`.
