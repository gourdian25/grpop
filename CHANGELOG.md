# Changelog

All notable changes to this project are documented in this file.

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
