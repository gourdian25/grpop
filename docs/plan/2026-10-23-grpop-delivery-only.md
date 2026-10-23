# grpop: delivery only, a Resend sender, then WhatsApp completed (staged plan)

Decided 2026-10-23, for grsentry's M1 (the real email consumer). One stage per "next"; each stage ends with the gate, a report and a commit message, and the user commits. grpop is a library, so there is no twin: the proof is the gate, the backend tests (Redis, Mailpit) and, from P2 on, a mutation run.

**Where grpop stands:** `v0.1.0` is the only tag. `version.go` says `v0.2.0` and `CHANGELOG.md` has a `[0.2.0]` entry (the Mongo driver v2 move), but `v0.2.0` was never tagged, and no repo but grpop's own `example/` imports grpop. Breaking changes therefore cost nothing. The working tree has the user's uncommitted `go.mod`/`go.sum` bumps (pgx 5.11, go-redis 9.22, mongo-driver 2.9.1); P1 removes all three.

## Status

| Stage | What | Status |
|---|---|---|
| P1 | Remove what isn't delivery: `Service`, the idempotency store, the DLQ (memory, Postgres, Mongo), `MessageEncryptor`, `Service`'s lifecycle events, and grpop's own rate limiters (`RateLimiter` becomes `grlimiter.Limiter`'s shape) | **Done** 2026-10-23 |
| P2 | Fix what is left: one permanent-vs-temporary error check callers use, safe addresses, masked emails in logs, the Meta API version as config | **Done** 2026-10-23 |
| P3 | One shared email preparation step (validate, render, addresses) for every email sender, and an optional idempotency key on `EmailMessage` | **Done** 2026-10-23 |
| P4 | The Resend sender over `net/http` (no SDK) | **Next** |
| P5 | Release `v0.2.0` | To do |
| P6 | WhatsApp completion: header and media parameters, buttons (URL, copy-code), named parameters, Meta's current API version, a live smoke test | To do, after grsentry's M1 (M1 needs email only) |

## The decisions (the user's, 2026-10-23)

- **grpop sends one message, once, and says exactly what happened:** accepted, a permanent failure, or a temporary one. Retrying, keeping a message durably and suppressing duplicates belong to the caller, which knows its own transaction and its message's real expiry. grsentry does all three with an outbox (its M1).
- **So everything that retried or stored goes,** not only the retry: `Service`'s inline retry and the DLQ that caught its failures, and with them `Service` itself, whose one remaining job, the duplicate check, an outbox does not need (its row records the send, and Resend's `Idempotency-Key` covers the vendor). `FullJitterBackoff` stays, exported, for a caller's own retry loop.
- **Order: remove, fix, refactor, add.** Four of the seven problems found on 2026-10-23 live in code P1 deletes, so they are deleted rather than fixed.
- **Resend over `net/http`, no SDK,** beside SMTP; the caller picks one. grpop's dispatch layer keeps zero vendor SDKs (the Meta client is hand-rolled the same way). Resend over SMTP (`smtp.resend.com`) also works with the SMTP sender as it is, with no code.
- **grpop stays a standalone library, WhatsApp included and completed (P6)** (the user, 2026-10-23, after weighing a move into grsentry's `pkg/` as `grmail`). With three vendor senders it is more than a wrapper, the vendors change on their own schedule (Meta retires a Graph API version about every two years), and its tests (a fake Meta server, Mailpit, a Resend live test behind a key) don't fit grsentry's offline-unit-tests rule. P6 comes after grsentry's M1: administrators have only an optional, unverified profile phone; tenant users have verified ones.
- **No rate limiter of grpop's own** (the user, 2026-10-23: reuse grsentry's `grlimiter`). Importing `grsentry/pkg/grlimiter` would make the two modules require each other and pull grsentry's dependencies (gin, through its middleware) into grpop. Instead `RateLimiter` takes `grlimiter.Limiter`'s exact shape, `Allow(ctx, key) (bool, error)`, so grsentry passes its limiter in with no adapter and no import either way. grpop's in-process and Redis limiters and their LRU go (P1). grpop then depends on the standard library alone.

**The problems found (2026-10-23, from the code; 1 to 4 proven by a throwaway test on a copy):**
1. A send whose DLQ write fails is still marked processed, so a retry with the same key answers `sent`, `Duplicate: true`, and the message is in no store. *P1 deletes it.*
2. `NewService` refuses an email-only setup (`ServiceDeps.WhatsAppSender is required`). *P1 deletes it.*
3. An SMTP `550` is not a permanent error (`isPermanentSendError` knows only grpop's own sentinels), so it is retried and DLQ'd. Meta's errors likewise. *P2.*
4. `Reply-To` goes into the headers unchecked: `r@x\r\nBcc: spy@evil` adds a real `Bcc` line. (`From` and `To` are safe only because `net/smtp` refuses CR/LF in `MAIL`/`RCPT`.) *P2.*
5. A claimed DLQ row has no lease: a worker that dies leaves it `retrying` forever. *P1 deletes it.*
6. `IsProcessed` then `MarkProcessed` is not atomic, so two sends with one key both go. *P1 deletes it.*
7. Every SMTP send logs the full recipient address at Info (the WhatsApp sender masks phone numbers). *P2.*

grpop builds and vets against grcache `v0.6.0` (grsentry's), checked on a copy; P1 drops grcache anyway.

## P1: remove what isn't delivery (done)

**Delete:**
- `service.go`, `cache.idempotency.go`, `events.go`, `postgres.go`, `dlq.postgres.go`, `dlq.mongo.go`, `internal/postgresdb/` (schema, queries, generated code), `sqlc.yaml`;
- their tests: `service_test.go`, `cache.idempotency_test.go`, `events_test.go`, `postgres_test.go`, `dlq.postgres_test.go`, `dlq.mongo_test.go`, `contract_dlqhandler_test.go`, `contract_idempotencystore_test.go`;
- from `memory.go`: the memory DLQ (`memoryDLQHandler`, its config, `appendAttemptCapped`); `MemoryEmailSender`, `MemoryWhatsAppSender` and `generateMemoryID` (the SMTP Message-ID uses it) stay;
- from `contract_helpers_test.go`: whatever only the deleted suites used.

**The rate limiters:**
- Delete `ratelimiter.go`, `ratelimiter.redis.go`, `lru.go` and their tests, `contract_ratelimiter_test.go`, and `contract_helpers_test.go` once nothing uses it.
- `RateLimiter` becomes `Allow(ctx context.Context, key string) (bool, error)`, `grlimiter.Limiter`'s method set. Each sender's deps take two, both optional: `RateLimiter` (the whole channel, key `email` or `whatsapp`) and `RecipientRateLimiter` (key the channel plus a SHA-256 of the normalised recipient, never the address itself), checked recipient first. A refusal is `ErrRateLimited`, temporary, at once: a sender never blocks waiting for a token, since the caller owns retry. `RateLimiterStats` goes with `GetStats`.

**Trim the API:**
- `interfaces.go`: `Service`, `IdempotencyStore`, `DLQHandler`, `MessageEncryptor` go. `Metrics` loses `IncDLQPublished` and `IncIdempotencyDedupHit`.
- `types.go`: `SendOptions`, `SendResult.Duplicate`, the `DLQStatus*` constants, `DLQMessage`, `DLQRetryAttempt`, `DLQEvent` go.
- `errors.go`: `ErrIdempotencyKeyRequired`, `ErrDLQEventNotFound`, `ErrDLQEventNotClaimed` go, and so do `ErrBackendUnavailable` and `ErrClosed`, which only the deleted stores and the Redis limiter used.
- `retrystrategy.go`: `FullJitterBackoff` stays; its doc says it is for a caller's own retry loop.
- `go.mod`: `go mod tidy` drops every requirement: pgx, mongo-driver, grcache, grevents, go-redis and `golang.org/x/time`.

**Docs and tooling:**
- `Makefile`: `docker-up` starts Mailpit only (no `grpop_test` database, no Mongo, no Redis); the race note names what is left that is concurrent (the circuit breaker, the template engine).
- `example/main.go`: sends through the SMTP sender directly, with a small caller-side retry loop on `FullJitterBackoff`, to show where retry now lives.
- `README.md`, `docs.go` (its "Precise, non-aspirational claims", which the README's Limitations list must match), `docs/architecture.md` (a dated note where the DLQ and idempotency sections were), `CLAUDE.md` (untracked; the user's to commit), `CHANGELOG.md` (the unreleased `[0.2.0]` entry is rewritten: the Mongo v2 move it describes went with the Mongo DLQ).
- `bark tree`.

**Proof:**
- the gate (below), with Mailpit up so no backend test skips;
- `go.mod` requires nothing; `go list -m all` names only grpop;
- `grep` finds no `DLQ`, `Idempotency`, `NewService`, `MessageEncryptor`, `NewLocalRateLimiter` or `NewRedisRateLimiter` outside the dated history (`docs/plan/grpop-plan.md`, the CHANGELOG);
- coverage still at or above 95% (`make coverage-check`);
- `bark check` clean (today it flags the four generated `internal/postgresdb` files as stale, and P1 deletes them).

*P1 as built (2026-10-23):*
- **Removed** as planned: 23 files plus `internal/postgresdb/` (about 7,900 lines); `go.mod` requires nothing and `go.sum` is empty.
- **`ratelimiter.go` now holds `checkRateLimits`,** the one check both senders call: the recipient's limiter first (key `<channel>:<sha256 of the trimmed, lower-cased recipient>`), then the channel's (key `<channel>`); a refusal is `ErrRateLimited` and counts `Metrics.IncRateLimitRejected` (which nothing called before); a limiter error stops the send. `SMTPDispatcherDeps` and `MetaCloudDispatcherDeps` gained `RecipientRateLimiter`.
- **`example/main.go`** sends through the SMTP sender with `sendWithRetry` (three attempts on `FullJitterBackoff`, never past the message's expiry). It still retries every error; P2's `ErrPermanent` lets it stop early.
- **Docs:** `docs.go` (a "Delivery only" section), `README.md`, `docs/architecture.md` (a dated section 0, sections renumbered), `CLAUDE.md`, the `[0.2.0]` CHANGELOG entry (unreleased), the Makefile (Mailpit only; `docker-down` no longer stops the shared Postgres, Redis and Mongo), `.golangci.yml`'s comment.
- **Gate:** all green: gofmt, build and vet (both ways), `go mod tidy -diff`, `make precommit` (lint 0 issues, `-race`, coverage 96.4%) with a throwaway Mailpit on 1025/8025 (no skips; removed with `docker rm -f -v` after), `bark check` (`bark tree` run).
- **Live:** `go run ./example` against that Mailpit sent all four emails.
- **Mutants:** 6 of 6 caught by unit tests, the control green: the channel asked before the recipient; a limiter error letting the send through; the raw address in the key; the rejection metric dropped; the SMTP sender skipping the check; the WhatsApp sender skipping the recipient limiter.

## P2: fix what is left (done)

- **One permanent-error check.** A new sentinel `ErrPermanent`; every error a retry can never fix wraps it (`errors.Join`), so a caller asks `errors.Is(err, grpop.ErrPermanent)` and needs no list (grpop's convention: sentinels and `errors.Is`, no `IsX` helpers). It covers:
  - every validation sentinel (`ErrRecipientRequired`, `ErrNoContentModeSet`, `ErrMultipleContentModesSet`, `ErrEmailFromRequired`, `ErrEmailTemplateEngineRequired`, `ErrEmailTemplateNotFound`, `ErrInlineTemplateTooLarge`, the WhatsApp ones, and P2's `ErrInvalidAddress`);
  - SMTP: a `5xx` reply (`*textproto.Error`), including `535` (bad credentials, a configuration error every send will hit). A `4xx` reply, a dial or TLS failure, a timeout and a cancelled context are temporary;
  - Meta: by HTTP status and Graph error code, the list checked against Meta's error-code reference when the stage starts (an invalid parameter, a template that doesn't exist or isn't approved, an expired token are permanent; Meta's rate limits and `5xx` are temporary).
  - `ErrRateLimited` (P1) and a breaker refusal (`ErrCircuitOpen`, `ErrTooManyRequests`) are temporary.
- **Safe addresses.** `From`, `To` and `ReplyTo` are parsed with `net/mail.ParseAddress`; a failure, or a CR or LF anywhere, is `ErrInvalidAddress` (permanent) before anything is built. `MAIL FROM`/`RCPT TO` get the bare address, the headers the parsed form (a display name is Q-encoded, like `Subject`).
- **Masked emails in logs:** `a***@example.com`, the email twin of `maskPhoneNumber`; no log line or `SendResult.Raw` carries a full address or a rendered body.
- **The Meta API version as config:** `MetaCloudDispatcherDeps.GraphAPIVersion`, defaulting to a current version (the hard-coded `v20.0`, May 2024, is at or past Meta's two-year window; checked when the stage starts).
- **Tests:** the throwaway proofs become real tests: a `550` is permanent and a `421` isn't; a `Reply-To` with CR/LF is refused and no `Bcc` line is written; a log line carries the masked address; each Meta class.
- **Mutants:** the 5xx rule dropped; the CR/LF check dropped; `ParseAddress` skipped on `ReplyTo`; the mask bypassed; a validation sentinel unwrapped from `ErrPermanent`.

*P2 as built (2026-10-23):*
- **`ErrPermanent`** (`errors.go`): `permanent(err)` wraps an error in `permanentError`, which keeps its message and chain and also matches `ErrPermanent`. All 11 validation sentinels and the new `ErrInvalidAddress` are built with it; so are template parse and render errors (the same data fails every time).
- **SMTP:** a `5xx` reply (`*textproto.Error`) becomes permanent after the breaker; `4xx`, dial, TLS, write failures and timeouts stay temporary. The breaker still counts every failure, a permanent one included (documented; see the open point below).
- **Meta:** `doJSON` classes a refusal temporary when its Graph code is one of `metaTemporaryErrorCodes` (2, 4, 80007, 130429, 131000, 131016, 131056, 133004, 2494100; Meta's error reference, read 2026-10-23), or the status is `429` or `5xx`; every other refusal is permanent. A network failure stays temporary.
- **The Graph API version:** `MetaCloudDispatcherDeps.GraphAPIVersion`, validated as `vN.N`, default `v26.0` (released 2026-07-29, Meta's changelog read 2026-10-23). **The hard-coded `v20.0` had stopped being served on 2026-09-24**, a month before this stage.
- **Addresses** (`parseEmailAddress`, `payloadvalidator.go`): `From`, `To`, `Reply-To` and `DefaultFrom` (at construction) go through `net/mail`, a CR or LF refused first. The error names the field and never the value: not even `net/mail`'s own message, which can quote the input back (found by mutant 2). Headers carry the parsed form, `MAIL FROM`/`RCPT TO` and the recipient's rate-limit key the bare address; the Message-ID takes the bare sender's domain (a display-name `From` used to give it `acme.test>`).
- **Logs:** `maskEmail` (`a***@example.com`) on the SMTP and dry-run senders' lines, and `redactAddresses` masks an address an SMTP reply echoes.
- **`example/`** stops retrying on `ErrPermanent`.
- **Tests:** `errors_test.go` (new: every sentinel permanent, temporary errors not, `permanent` keeps the chain); SMTP: the failure classes, bad addresses refused before dialling, display names, a bad `DefaultFrom`, no full address in any log line; Meta: 11 refusal classes and a network failure, the version in the URL, malformed versions refused; the template engine's errors permanent; the dry run's masked log; `parseEmailAddress`, `maskEmail`, `redactAddresses`.
- **Gate:** all green: gofmt, build and vet (both ways), `go mod tidy -diff`, `make precommit` (lint 0 issues, `-race`, coverage 97.1%) with a throwaway Mailpit (no skips; removed `-v`), `bark check` (`bark tree` for `errors_test.go`); `go run ./example` sent all four.
- **Mutants:** 11 of 11 caught, the control green: the 5xx rule dropped; the line-break check dropped; `Reply-To` unparsed; the mask bypassed; a sentinel not permanent; 130429 classed permanent; the default back at `v20.0`; a render error not permanent; the SMTP reply logged unredacted; every Meta 4xx temporary; `net/mail`'s message echoed in the error.
- **Open, for the user:** the circuit breaker counts a permanent failure like any other, so a burst of `550`s for bad addresses could open it for every recipient. Counting only temporary failures would be a one-line change in each sender; not done without a decision.

## P3: one email preparation step, and an idempotency key (done)

- `resolveContent` is a method of the SMTP sender today. It becomes a package function, `prepareEmail(msg, defaultFrom, engine) (preparedEmail, error)`: validate the message, parse the addresses (P2), resolve exactly one content mode, apply the default sender. The SMTP sender uses it; the Resend sender (P4) will, so both send exactly the same message for the same input.
- `EmailMessage.IdempotencyKey` (optional): the Resend sender sends it as `Idempotency-Key`. The SMTP sender derives the `Message-ID` from it (`<sha256-hex@domain>`), so a retried send carries the same `Message-ID`, which receiving systems may use to collapse a duplicate; without a key the `Message-ID` stays random.
- **Tests:** the SMTP sender's behaviour is unchanged for every existing case; the same key gives the same `Message-ID`.

*P3 as built (2026-10-23):*
- **`dispatcher.email.go`** (new): `prepareEmail(msg, defaultFrom, engine) (preparedEmail, error)` checks the message, renders its one content mode (`renderEmailContent`, the SMTP sender's old `resolveContent` as a package function), applies the default sender and parses the three addresses; `preparedEmail` holds the parsed addresses, the rendered content and the key. The SMTP sender's `Send` now starts from it and reads no `EmailMessage` field itself.
- **`EmailMessage.IdempotencyKey`** (optional). The SMTP sender's `smtpMessageID` makes the Message-ID the key's SHA-256 hex at the bare sender's domain (random without a key), so every attempt at one message carries the same Message-ID and the key itself never appears in the message.
- **`example/`** gives each of its four messages a key.
- **Docs:** README, `docs.go` (the at-least-once paragraph), `docs/architecture.md` (a new §3.4, sections renumbered), `CLAUDE.md`, the CHANGELOG.
- **Tests:** `dispatcher.email_test.go` (new): a literal message with the default sender and its key, both template modes, nine refusals each permanent; SMTP: one key gives one Message-ID (the key's SHA-256), another key or none another, and the key never in the message. Every existing SMTP test passes unchanged.
- **Gate:** all green: gofmt, build and vet (both ways), `go mod tidy -diff`, `make precommit` (lint 0 issues, `-race`, coverage 97.1%) with a throwaway Mailpit (no skips; removed `-v`), `bark check` (`bark tree` for the two new files). `go run ./example` sent all four, and Mailpit's Message-IDs were the keys' hashes.
- **Mutants:** 8 of 8 caught, the control green: the key ignored; the raw key as the Message-ID; `prepareEmail` dropping the key; the default sender ignored; the Reply-To dropped; an inline template rendered as a registered one; validation skipped; the SMTP sender preparing something other than the message.

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

## P6: WhatsApp completion (after grsentry's M1)
Checked against Meta's Cloud API reference when the stage starts.
- **Every component a template can take:** header parameters (text, image, document, video), body parameters, and buttons (a URL button's dynamic suffix, quick replies, and the copy-code button Meta's authentication templates require).
- **Named parameters** beside positional ones (`parameter_name`), with the template validator counting each kind.
- **The API version** (P2's config field) defaulting to Meta's current one.
- **A gap in positional values is an error** (`ErrWhatsAppTemplateArityMismatch`, permanent), not a silent stop at the gap as `positionalTemplateParameters` does today.
- **Tests:** the fake Meta server for every component; a live smoke test only with `GRPOP_TEST_META_*` set (it skips otherwise).
- **Then release** `v0.3.0` the same way as P5.

## Known and accepted

- **At-least-once over SMTP.** `net/smtp` can't be cancelled, so a timed-out send's goroutine is abandoned and may still deliver; a caller that retries can send twice. Resend's `Idempotency-Key` closes that for 24 hours; SMTP can't, beyond the stable `Message-ID` (P3).
- **`SendStatusSent` means the vendor accepted it,** never that it reached an inbox. Bounces and complaints are Resend's webhooks, the caller's to receive.
- **A rate-limited send fails at once** (`ErrRateLimited`, temporary) rather than waiting: the caller's retry spaces it out.
- **No rate limiter ships with grpop:** a caller passes its own (grsentry's `grlimiter` fits as it is).

## How to prove a stage

- **The gate:** `gofmt -l .`, `go build ./... && go vet ./...`, `GOWORK=off go build ./...`, `GOWORK=off go mod tidy -diff`, `make precommit` (fmt, vet, lint, race, coverage at or above 95%), `bark check` (`bark tree` when a file is added or removed). The Mailpit test skips when Mailpit is down, which drops coverage below the gate: start it first and read the output for skips.
- **Mutants** (P2 to P4): on a copy of the repo, never the working tree; an unmutated control first; read each failing run for which test caught it.
- **Never on the working tree** for a quick check: a throwaway test goes in a copy.
