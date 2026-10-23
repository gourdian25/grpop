# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`grpop` (module `github.com/gourdian25/grpop`) is a Go library: send-only,
transactional-message delivery over email (stdlib `net/smtp`, any SMTP vendor)
and WhatsApp (Meta Cloud API, hand-rolled `net/http` client). It sends messages
and does not receive them (not POP3), handles no push notifications (that's
`grnoti`), and has no notion of users or preferences. The caller passes the
recipient on every send. It is one flat public package (`package grpop`, every
file at the root). The only subpackage is sqlc's unexported
`internal/postgresdb`. `example/` is a runnable demo (`go run ./example`, sends
to Mailpit by default).

**The dispatch layer has zero third-party dependencies, and keeping it that way
is the design goal.** The only third-party imports are the storage and
rate-limit backends (`pgx/v5`, `go-redis/v9`, `mongo-driver/v2`), the
root-interface packages of `grcache`/`grevents`, and `golang.org/x/time/rate`.
Never add a vendor SDK (AWS, SendGrid, Twilio, ...) or a message-broker client.
SMS and multi-recipient email were cut on purpose (`docs/plan/grpop-plan.md`).

Design references, which are worth reading before changing behaviour:
`docs/architecture.md` (the why behind every decision below, plus bugs found
along the way), `docs.go` (godoc only; its "Precise, non-aspirational claims"
section is the contract the README's Limitations list is drawn from, so keep the
two in sync), and `docs/plan/grpop-plan.md` (the original 16-stage plan, which
is history).

## Commands

```bash
go build ./... && go vet ./...
GOWORK=off go build ./...      # standalone check; ../go.work otherwise swaps in local grcache/grevents checkouts
make test                      # go test -count=1 -timeout=5m -cover ./...
make race                      # -race; mandatory before committing changes to ratelimiter*.go, dlq.*.go or any store
go test -count=1 -run 'TestDLQHandler_Contract/Memory' .   # one contract suite, one backend
go test -count=1 -run TestName -v .                        # one test
make lint                      # golangci-lint run ./... (.golangci.yml: standard + gosec, misspell, gocritic, revive)
make coverage-check            # root package only (go test -cover .), must be >= 95%
make coverage-summary          # per-function breakdown
make precommit                 # fmt + vet + lint + race + coverage-check, the gate before every commit
sqlc generate                  # regenerate internal/postgresdb from its schema.sql + queries/dlq.sql
bark tree                      # regenerate bark.txt after adding/renaming a file (never hand-edit it)
make prerelease                # precommit + goreleaser check + snapshot (writes the gitignored dist/)
make release VERSION=vX.Y.Z    # tags, pushes and publishes a GitHub release: outward-facing, run only when asked
```

## Test backends

Backend tests run against real local services, not mocks. Each one `t.Skip`s
when its service is unreachable, so **a run with backends down still passes but
falls below the 95% coverage gate**. Before trusting `make coverage-check`,
check the test output for skips. The connection settings are hard-coded
constants in the test files:

| Service | Address | Credentials / target | Constant |
|---|---|---|---|
| Postgres `gourdian-postgres` | `localhost:5432` | `postgres_user`/`postgres_password`, db **`grpop_test`** | `testPostgresDSN` (postgres_test.go) |
| Mongo `gourdian-mongo-auth` (replica set rs0, auth) | `localhost:27018`, `directConnection=true` | `root`/`mongo_password`, db `grpop_test` | `testMongoURI` (dlq.mongo_test.go) |
| Redis `gourdian-redis` | `localhost:6379` | password `redis_password` | `testRedisAddr` (contract_idempotencystore_test.go) |
| Mailpit `gourdian-mailpit` | SMTP `localhost:1025`, API `localhost:8025` | none | `testMailpitSMTPAddr` (dispatcher.email.smtp_mailpit_test.go) |

The containers are shared with every gourdian25 repo, and each repo gets its
own database. Only Mailpit is specific to grpop. `make docker-up` is
idempotent: it creates whatever container is missing, starts the rest, and
creates `grpop_test`. **A gotcha:** the shared containers are usually created by
grsentry's `docker-compose.yml`, whose Mongo runs `mongod --port 27018` *inside*
the container. Once that container exists, `docker-up` only starts it, then its
Mongo wait loop runs `mongosh` on the default port 27017 and never ends. If it
hangs there, interrupt it and finish by hand:

```bash
docker exec gourdian-postgres psql -U postgres_user -d postgres -c "CREATE DATABASE grpop_test"
docker inspect gourdian-mailpit >/dev/null 2>&1 || docker run -d --name gourdian-mailpit -p 1025:1025 -p 8025:8025 axllent/mailpit
docker start gourdian-mailpit
```

The Meta WhatsApp Cloud API is the one deliberate exception, since it has no
local emulator. The dispatcher's retry, rate-limit and breaker wiring is tested
against a fake `WhatsAppCloudAPIClient`, and the real HTTP client against an
`httptest.Server` (`dispatcher.whatsapp.metacloud_http_test.go`).

`contract_*_test.go` holds one behavioural suite per interface
(`DLQHandler`, `IdempotencyStore`, `RateLimiter`), run against every backend as
subtests. A new backend joins the existing suite rather than getting a parallel
one. Contract rows must use a `contract-` id prefix and Mongo collections come
from `contractCollectionName(t)`. The cleanup helpers delete only those, through
their own connections, because the store's connection is already closed by the
time `t.Cleanup` runs.

## Architecture

**`Service` (`service.go`) is a thin orchestrator. Its pipeline, in order:**
`IdempotencyKey` required → `IsProcessed` (a store error *fails closed* and
blocks the send) → dispatcher `Send` retried inline on the caller's goroutine
(`FullJitterBackoff`, `MaxInlineRetries`) → `isPermanentSendError`
classification → transient failures only: `PublishToDLQ` → best-effort
`message.sent`/`message.failed` event on an optional, nil-safe `grevents.Bus`
(`events.go`) → `MarkProcessed`. Easy to get backwards:

- A delivery failure, even one that ends in the DLQ, comes back as
  `SendResult.Status == SendStatusFailed` with a **nil error**. A non-nil error
  means only a pipeline failure: no key, a broken idempotency store, or a
  permanent validation error.
- A permanent error matches one of grpop's own validation sentinels via
  `errors.Is` (`ErrRecipientRequired`, `ErrNoContentModeSet`,
  `ErrWhatsAppTemplateNotApproved`, ...). It gets no retry, no DLQ entry, no
  event and **no idempotency mark**, so the caller can fix the message and
  resend with the same key. A new validation sentinel must be added to
  `permanentSendErrorSentinels` (`service.go`), or it will be retried and
  DLQ'd.
- Delivery is at-least-once. That's why `IdempotencyKey` is required.
  `SendStatusSent` means only that the vendor accepted the message.

**Rate limiting, circuit breaking and template rendering live on the
dispatchers, not on `Service`.** `ServiceDeps` deliberately has no
`RateLimiter` or `EmailTemplateEngine` field. `SMTPDispatcherDeps` and
`MetaCloudDispatcherDeps` carry the limiter and the breaker, which gate every
`Send`, including each inline retry. A second gate in `Service` would spend
tokens twice per logical send. Likewise, `Service` calls only
`Metrics.IncIdempotencyDedupHit` and `Metrics.IncDLQPublished`, and the
dispatchers record latency and results. Email content resolution (exactly one of
`TemplateName`, `InlineTemplate` or a literal body; `ErrNoContentModeSet` and
`ErrMultipleContentModesSet` cover the two ways that fails) happens in
`dispatcher.email.smtp.go`, right before the MIME message is built.

**Rate limiting is two-tier:** each request needs capacity in both a
per-channel bucket and a per-(channel, recipient) bucket.
`localRateLimiter` composes two `rate.Limiter`s with `Reserve` + `Cancel`,
because `rate` has no way to check a bucket without consuming from it. Without
the cancel, a request refused on the recipient tier still spends a channel
token. `redisRateLimiter` does both tiers in one Lua script, using
`redis.call("TIME")` (never a client clock) and SHA-256-hashed recipient keys.
A Redis error makes `Wait` fail, so the dispatcher refuses the send: it fails
closed.

**DLQ (`DLQHandler`: memory, `dlq.postgres.go`, `dlq.mongo.go`) is a pull-based
retry store, not a transport.** `Service` only publishes to it, and a
consumer's own worker drains it with `ClaimRetryableEvents`/`MarkRetried`.
Retries are bounded by a wall-clock `ExpiresAt` (the message's real expiry,
`SendOptions.RetryExpiresAt`) as well as by `MaxRetries`:

- `Claim` first bulk-moves overdue pending rows to `expired`, then claims
  atomically: `FOR UPDATE SKIP LOCKED` on Postgres, a `findOneAndUpdate` loop
  on Mongo, both with an `expires_at > now` predicate.
- `MarkRetried` checks **expiry before exhaustion**, so a message that runs
  out of time with budget left is `expired`, not `exhausted`.
- A zero `ExpiresAt` means "no deadline" (a NULL in Postgres).
- `PurgeExpiredEvents`' "expired" means "old enough to delete" and is unrelated
  to `DLQStatusExpired`.

The state machine was built first in `memory.go` and then repeated in SQL and
Mongo. A change goes into all three, and the contract suite proves they
agree. `attempt_history` is capped (`MaxAttemptHistoryEntries`) in every write
path.

An optional `MessageEncryptor` encrypts `message_data` at rest. Postgres
base64-wraps the ciphertext, because its JSONB column won't accept non-UTF-8
bytes. Mongo stores it as BSON binary. Without an encryptor, reset links and
invite tokens sit in the DLQ in the clear.

**Postgres schema:** `internal/postgresdb/schema.sql` is the sqlc input
*and* the runtime DDL (`//go:embed` in `postgres.go`). Every connect applies it
under `pg_advisory_lock(grpopSchemaLockKey)`, unless
`PostgresConfig.SkipSchemaEnsure` is set. That lock key must stay distinct from
grnoti's. The DDL is `CREATE ... IF NOT EXISTS` only, with no migrations or
versioning, so editing an existing column in `schema.sql` never changes an
existing table. Queries live in `queries/dlq.sql`; run `sqlc generate` after
changing either file, and never hand-edit the generated `*.go`. Exactly one of
`PostgresConfig.DSN` and `PostgresConfig.Pool` may be set. grpop closes only a
pool it dialed itself.

**Vendors:**

- **SMTP:** `net/smtp` can't be cancelled, so `transact` races the whole
  dial-to-close transaction against a timer and abandons the goroutine on
  timeout. This is documented and deliberate. `SMTPDialer.Dial` performs
  STARTTLS itself. Plain-text auth over `SMTPTLSInsecureNoTLS` requires
  `AllowInsecureAuth`.
- **Meta:** sends use `PhoneNumberID`, while template listing for
  `TemplateValidator` needs the separate `BusinessAccountID`.
  `TemplateValidator`'s TTL cache is advisory. It can let a since-revoked
  template through, but it never blocks a send Meta would accept, and a passing
  check never skips the real call. grpop never rotates a Meta access token.

## Conventions

- **File names:** `<concern>.<backend>.go` (`dlq.postgres.go`,
  `ratelimiter.redis.go`), `dispatcher.<channel>.<vendor>.go`, with tests
  alongside. Every `.go` and `.sql` file and the Makefile opens with a
  `// File: <path>` (or `-- File:`/`# File:`) header from `bark`.
- **Errors:** sentinels live in `errors.go` and are prefixed `"grpop: "`. A
  constructor's validation errors use `"grpop/<component>: "`. Give a distinct
  condition its own sentinel rather than reusing one. Match with `errors.Is`;
  there is no `IsX` helper. Translate a backend-native error (`pgx.ErrNoRows`,
  `redis.Nil`, `mongo.ErrNoDocuments`, a Graph API error) into a sentinel before
  it crosses an interface, usually with `errors.Join(err, ErrBackendUnavailable)`.
- **Logger:** the ecosystem-wide slog-shaped interface (`Debug/Info/Warn/Error(msg,
  args ...any)`), so `*slog.Logger` satisfies it. Constructors pass a nil logger
  through `OrNop`. New log lines take a flat `"grpop: "` prefix plus a
  `"component"` key/value; don't bake a `/backend` suffix into the message.
  Level rules are in `../CONVENTIONS.md`.
- **Close:** a component holding a connection or pool makes `Close()`
  idempotent with a `sync.Once` plus an `atomic.Bool` `closed` flag, and
  returns `ErrClosed` from every method called after it.
- **`docs.go`:** holds only the package godoc.
- **Mongo driver:** grpop is on `mongo-driver/v2` (0.2.0). This supersedes
  `../CONVENTIONS.md`'s "siblings stay on v1" line.
- **Releases:** bump `Version` in `version.go` to match the tag, and add a
  `CHANGELOG.md` entry. GoReleaser ships a source archive only, since there are
  no binaries (`.goreleaser.yaml`).
