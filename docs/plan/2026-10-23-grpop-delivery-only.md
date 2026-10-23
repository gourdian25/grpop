# grpop: delivery only, then a Resend sender (staged plan)

Decided 2026-10-23, for grsentry's M1 (the real email consumer). One stage per "next"; each stage ends with the gate, a report and a commit message, and the user commits. grpop is a library, so there is no twin: the proof is the gate, the backend tests (Redis, Mailpit) and, from P2 on, a mutation run.

**Where grpop stands:** `v0.1.0` is the only tag. `version.go` says `v0.2.0` and `CHANGELOG.md` has a `[0.2.0]` entry (the Mongo driver v2 move), but `v0.2.0` was never tagged, and no repo but grpop's own `example/` imports grpop. Breaking changes therefore cost nothing. The working tree has the user's uncommitted `go.mod`/`go.sum` bumps (pgx 5.11, go-redis 9.22, mongo-driver 2.9.1); P1 removes two of those three.

## Status

| Stage | What | Status |
|---|---|---|
| P1 | Remove the retry and storage layer: `Service`, the idempotency store, the DLQ (memory, Postgres, Mongo), `MessageEncryptor`, `Service`'s lifecycle events | **Next** |
| P2 | Fix what is left: one permanent-vs-temporary error check callers use, safe addresses, masked emails in logs, the Meta API version as config | To do |
| P3 | One shared email preparation step (validate, render, addresses) for every email sender, and an optional idempotency key on `EmailMessage` | To do |
| P4 | The Resend sender over `net/http` (no SDK) | To do |
| P5 | Release `v0.2.0` | To do |
| *Later* | WhatsApp completion: header and media parameters, buttons, named parameters | Not scheduled (grsentry's M1 needs email only) |

## The decisions (the user's, 2026-10-23)

- **grpop sends one message, once, and says exactly what happened:** accepted, a permanent failure, or a temporary one. Retrying, keeping a message durably and suppressing duplicates belong to the caller, which knows its own transaction and its message's real expiry. grsentry does all three with an outbox (its M1).
- **So everything that retried or stored goes,** not only the retry: `Service`'s inline retry and the DLQ that caught its failures, and with them `Service` itself, whose one remaining job, the duplicate check, an outbox does not need (its row records the send, and Resend's `Idempotency-Key` covers the vendor). `FullJitterBackoff` stays, exported, for a caller's own retry loop.
- **Order: remove, fix, refactor, add.** Four of the seven problems found on 2026-10-23 live in code P1 deletes, so they are deleted rather than fixed.
- **Resend over `net/http`, no SDK,** beside SMTP; the caller picks one. grpop's dispatch layer keeps zero vendor SDKs (the Meta client is hand-rolled the same way). Resend over SMTP (`smtp.resend.com`) also works with the SMTP sender as it is, with no code.
- **WhatsApp is completed later.** grsentry's administrators have only an optional, unverified profile phone.

**The problems found (2026-10-23, from the code; 1 to 4 proven by a throwaway test on a copy):**
1. A send whose DLQ write fails is still marked processed, so a retry with the same key answers `sent`, `Duplicate: true`, and the message is in no store. *P1 deletes it.*
2. `NewService` refuses an email-only setup (`ServiceDeps.WhatsAppSender is required`). *P1 deletes it.*
3. An SMTP `550` is not a permanent error (`isPermanentSendError` knows only grpop's own sentinels), so it is retried and DLQ'd. Meta's errors likewise. *P2.*
4. `Reply-To` goes into the headers unchecked: `r@x\r\nBcc: spy@evil` adds a real `Bcc` line. (`From` and `To` are safe only because `net/smtp` refuses CR/LF in `MAIL`/`RCPT`.) *P2.*
5. A claimed DLQ row has no lease: a worker that dies leaves it `retrying` forever. *P1 deletes it.*
6. `IsProcessed` then `MarkProcessed` is not atomic, so two sends with one key both go. *P1 deletes it.*
7. Every SMTP send logs the full recipient address at Info (the WhatsApp sender masks phone numbers). *P2.*

grpop builds and vets against grcache `v0.6.0` (grsentry's), checked on a copy; P1 drops grcache anyway.

## P1: remove the retry and storage layer (next)

**Delete:**
- `service.go`, `cache.idempotency.go`, `events.go`, `postgres.go`, `dlq.postgres.go`, `dlq.mongo.go`, `internal/postgresdb/` (schema, queries, generated code), `sqlc.yaml`;
- their tests: `service_test.go`, `cache.idempotency_test.go`, `events_test.go`, `postgres_test.go`, `dlq.postgres_test.go`, `dlq.mongo_test.go`, `contract_dlqhandler_test.go`, `contract_idempotencystore_test.go`;
- from `memory.go`: the memory DLQ (`memoryDLQHandler`, its config, `appendAttemptCapped`); `MemoryEmailSender`, `MemoryWhatsAppSender` and `generateMemoryID` (the SMTP Message-ID uses it) stay;
- from `contract_helpers_test.go`: whatever only the deleted suites used.

**Trim the API:**
- `interfaces.go`: `Service`, `IdempotencyStore`, `DLQHandler`, `MessageEncryptor` go. `Metrics` loses `IncDLQPublished` and `IncIdempotencyDedupHit`.
- `types.go`: `SendOptions`, `SendResult.Duplicate`, the `DLQStatus*` constants, `DLQMessage`, `DLQRetryAttempt`, `DLQEvent` go.
- `errors.go`: `ErrIdempotencyKeyRequired`, `ErrDLQEventNotFound`, `ErrDLQEventNotClaimed` go. `ErrBackendUnavailable` and `ErrClosed` stay (the Redis limiter uses both).
- `retrystrategy.go`: `FullJitterBackoff` stays; its doc says it is for a caller's own retry loop.
- `go.mod`: `go mod tidy` drops pgx, mongo-driver, grcache and grevents. go-redis (the Redis limiter) and `golang.org/x/time` remain.

**Docs and tooling:**
- `Makefile`: `docker-up` starts Redis and Mailpit only (no `grpop_test` database, no Mongo); the race note names the rate limiters.
- `example/main.go`: sends through the SMTP sender directly, with a small caller-side retry loop on `FullJitterBackoff`, to show where retry now lives.
- `README.md`, `docs.go` (its "Precise, non-aspirational claims", which the README's Limitations list must match), `docs/architecture.md` (a dated note where the DLQ and idempotency sections were), `CLAUDE.md` (untracked; the user's to commit), `CHANGELOG.md` (the unreleased `[0.2.0]` entry is rewritten: the Mongo v2 move it describes went with the Mongo DLQ).
- `bark tree`.

**Proof:**
- the gate (below), with Redis and Mailpit up so no backend test skips;
- `go list -m all` names none of pgx, mongo-driver, grcache, grevents;
- `grep` finds no `DLQ`, `Idempotency`, `NewService` or `MessageEncryptor` outside the dated history (`docs/plan/grpop-plan.md`, the CHANGELOG);
- coverage still at or above 95% (`make coverage-check`);
- `bark check` clean (today it flags the four generated `internal/postgresdb` files as stale, and P1 deletes them).

## P2: fix what is left

- **One permanent-error check.** A new sentinel `ErrPermanent`; every error a retry can never fix wraps it (`errors.Join`), so a caller asks `errors.Is(err, grpop.ErrPermanent)` and needs no list (grpop's convention: sentinels and `errors.Is`, no `IsX` helpers). It covers:
  - every validation sentinel (`ErrRecipientRequired`, `ErrNoContentModeSet`, `ErrMultipleContentModesSet`, `ErrEmailFromRequired`, `ErrEmailTemplateEngineRequired`, `ErrEmailTemplateNotFound`, `ErrInlineTemplateTooLarge`, the WhatsApp ones, and P2's `ErrInvalidAddress`);
  - SMTP: a `5xx` reply (`*textproto.Error`), including `535` (bad credentials, a configuration error every send will hit). A `4xx` reply, a dial or TLS failure, a timeout and a cancelled context are temporary;
  - Meta: by HTTP status and Graph error code, the list checked against Meta's error-code reference when the stage starts (an invalid parameter, a template that doesn't exist or isn't approved, an expired token are permanent; Meta's rate limits and `5xx` are temporary).
  - `ErrRateLimited` and a breaker refusal (`ErrCircuitOpen`, `ErrTooManyRequests`) are temporary.
- **Safe addresses.** `From`, `To` and `ReplyTo` are parsed with `net/mail.ParseAddress`; a failure, or a CR or LF anywhere, is `ErrInvalidAddress` (permanent) before anything is built. `MAIL FROM`/`RCPT TO` get the bare address, the headers the parsed form (a display name is Q-encoded, like `Subject`).
- **Masked emails in logs:** `a***@example.com`, the email twin of `maskPhoneNumber`; no log line or `SendResult.Raw` carries a full address or a rendered body.
- **The Meta API version as config:** `MetaCloudDispatcherDeps.GraphAPIVersion`, defaulting to a current version (the hard-coded `v20.0`, May 2024, is at or past Meta's two-year window; checked when the stage starts).
- **Tests:** the throwaway proofs become real tests: a `550` is permanent and a `421` isn't; a `Reply-To` with CR/LF is refused and no `Bcc` line is written; a log line carries the masked address; each Meta class.
- **Mutants:** the 5xx rule dropped; the CR/LF check dropped; `ParseAddress` skipped on `ReplyTo`; the mask bypassed; a validation sentinel unwrapped from `ErrPermanent`.

## P3: one email preparation step, and an idempotency key

- `resolveContent` is a method of the SMTP sender today. It becomes a package function, `prepareEmail(msg, defaultFrom, engine) (preparedEmail, error)`: validate the message, parse the addresses (P2), resolve exactly one content mode, apply the default sender. The SMTP sender uses it; the Resend sender (P4) will, so both send exactly the same message for the same input.
- `EmailMessage.IdempotencyKey` (optional): the Resend sender sends it as `Idempotency-Key`. The SMTP sender derives the `Message-ID` from it (`<sha256-hex@domain>`), so a retried send carries the same `Message-ID`, which receiving systems may use to collapse a duplicate; without a key the `Message-ID` stays random.
- **Tests:** the SMTP sender's behaviour is unchanged for every existing case; the same key gives the same `Message-ID`.

## P4: the Resend sender

`dispatcher.email.resend.go`, `NewResendDispatcher(ResendDispatcherDeps) (EmailSender, error)`, a hand-rolled `net/http` client like the Meta one.

- **Deps:** `APIKey` (required), `BaseURL` (default `https://api.resend.com`), `DefaultFrom`, `Timeout` (default 15 s), `HTTPClient` (optional), `TemplateEngine`, `RateLimiter`, `CircuitBreaker`, `Metrics`, `Logger`. The key never appears in a log line or an error.
- **The call:** `POST /emails`, `Authorization: Bearer <key>`, JSON `{from, to: [address], subject, html, text, reply_to}` from `prepareEmail`; `Idempotency-Key` when the message has one (Resend takes 1 to 256 characters and keeps it 24 hours; a longer key is sent as its SHA-256 hex). `SendResult.ProviderMessageID` is the response's `id`, which Resend's webhooks name later.
- **Errors** (Resend's error reference, read 2026-10-23):
  - permanent: `400` (`validation_error`, `invalid_idempotency_key`), `401` (a missing or invalid key), `403` (an unverified `from` domain, a restricted or suspended key), `404`, `405`, `422`, and `409 invalid_idempotent_request` (the same key with a different body: a caller bug);
  - temporary: `409 concurrent_idempotent_requests`, `429` (`rate_limit_exceeded`, and the daily and monthly quotas, which reset on their own), `500`, `503`, network errors and timeouts.
  - The body's `name` and `message` go into the error; its exact JSON shape is confirmed when the stage starts with one unauthenticated request (`401 missing_api_key` needs no account).
- **Tests:** an `httptest` server, like `dispatcher.whatsapp.metacloud_http_test.go`: the request's method, path, headers and body; the idempotency key, long and short; every status class above; a timeout and a cancelled context; the limiter and breaker gating every send; nothing secret in a log. A live smoke test runs only with `GRPOP_TEST_RESEND_API_KEY` set (it skips otherwise), sending to Resend's test address `delivered@resend.dev` (`bounced@resend.dev` answers like a `550`).
- **Mutants:** the idempotency header dropped; a `422` classed temporary; a `429` classed permanent; the key logged; `prepareEmail` bypassed.

## P5: release v0.2.0

`CHANGELOG.md`'s `[0.2.0]` entry (dated, listing the breaking removals and the additions), `version.go`, the README, `make prerelease`. Tagging and publishing (`make release VERSION=v0.2.0`) is outward-facing: the user runs it or says to. grsentry then requires `github.com/gourdian25/grpop v0.2.0`.

## Known and accepted

- **At-least-once over SMTP.** `net/smtp` can't be cancelled, so a timed-out send's goroutine is abandoned and may still deliver; a caller that retries can send twice. Resend's `Idempotency-Key` closes that for 24 hours; SMTP can't, beyond the stable `Message-ID` (P3).
- **`SendStatusSent` means the vendor accepted it,** never that it reached an inbox. Bounces and complaints are Resend's webhooks, the caller's to receive.
- **A limiter's `Wait` blocks the caller** until a token is free: a worker passes a context with a deadline.

## How to prove a stage

- **The gate:** `gofmt -l .`, `go build ./... && go vet ./...`, `GOWORK=off go build ./...`, `GOWORK=off go mod tidy -diff`, `make precommit` (fmt, vet, lint, race, coverage at or above 95%), `bark check` (`bark tree` when a file is added or removed). Backend tests skip when their service is down, which drops coverage below the gate: run `make docker-up` first and read the output for skips.
- **Mutants** (P2 to P4): on a copy of the repo, never the working tree; an unmutated control first; read each failing run for which test caught it.
- **Never on the working tree** for a quick check: a throwaway test goes in a copy.
