# grpop: delivery only, a Resend sender, WhatsApp completed, then the release (staged plan)

Decided 2026-10-23, for grsentry's M1 (the real email consumer). One stage per "next"; each stage ends with the gate, a report and a commit message, and the user commits. grpop is a library, so there is no twin: the proof is the gate, the backend tests (Redis, Mailpit) and, from P2 on, a mutation run.

**Where grpop stands:** `v0.1.0` is the only tag. `version.go` says `v0.2.0` and `CHANGELOG.md` has a `[0.2.0]` entry (the Mongo driver v2 move), but `v0.2.0` was never tagged, and no repo but grpop's own `example/` imports grpop. Breaking changes therefore cost nothing. The working tree has the user's uncommitted `go.mod`/`go.sum` bumps (pgx 5.11, go-redis 9.22, mongo-driver 2.9.1); P1 removes all three.

## Status

| Stage | What | Status |
|---|---|---|
| P1 | Remove what isn't delivery: `Service`, the idempotency store, the DLQ (memory, Postgres, Mongo), `MessageEncryptor`, `Service`'s lifecycle events, and grpop's own rate limiters (`RateLimiter` becomes `grlimiter.Limiter`'s shape) | **Done** 2026-10-23 |
| P2 | Fix what is left: one permanent-vs-temporary error check callers use, safe addresses, masked emails in logs, the Meta API version as config | **Done** 2026-10-23 |
| P3 | One shared email preparation step (validate, render, addresses) for every email sender, and an optional idempotency key on `EmailMessage` | **Done** 2026-10-23 |
| P4 | The Resend sender over `net/http` (no SDK) | **Done** 2026-10-24 |
| P5 | WhatsApp messages: typed components (header, body, buttons; named or positional), the OTP helper, phone and value checks, and the breaker counting only temporary failures | **Next** |
| P6 | The template validator, usable and truthful: an exported Meta client, paginated listing, component-aware counts, advisory when the listing fails; a live Meta smoke test | To do |
| *grsentry* | M1a–M1d on this untagged grpop: `../go.work` in workspace mode, a pseudo-version of a pushed `master` commit for `GOWORK=off` and CI. A change M1 needs here becomes a stage of its own | To do |
| P7 | Release `v0.2.0` once grsentry runs on it and grpop needs nothing more: the CHANGELOG dated, `make prerelease`, the user tags; grsentry moves from the pseudo-version to the tag | To do, last |

## The decisions (the user's, 2026-10-23)

- **grpop sends one message, once, and says exactly what happened:** accepted, a permanent failure, or a temporary one. Retrying, keeping a message durably and suppressing duplicates belong to the caller, which knows its own transaction and its message's real expiry. grsentry does all three with an outbox (its M1).
- **So everything that retried or stored goes,** not only the retry: `Service`'s inline retry and the DLQ that caught its failures, and with them `Service` itself, whose one remaining job, the duplicate check, an outbox does not need (its row records the send, and Resend's `Idempotency-Key` covers the vendor). `FullJitterBackoff` stays, exported, for a caller's own retry loop.
- **Order: remove, fix, refactor, add.** Four of the seven problems found on 2026-10-23 live in code P1 deletes, so they are deleted rather than fixed.
- **Resend over `net/http`, no SDK,** beside SMTP; the caller picks one. grpop's dispatch layer keeps zero vendor SDKs (the Meta client is hand-rolled the same way). Resend over SMTP (`smtp.resend.com`) also works with the SMTP sender as it is, with no code.
- **grpop stays a standalone library, WhatsApp included and completed (P5, P6)** (the user, 2026-10-23, after weighing a move into grsentry's `pkg/` as `grmail`). With three vendor senders it is more than a wrapper, the vendors change on their own schedule (Meta retires a Graph API version about every two years), and its tests (a fake Meta server, Mailpit, a Resend live test behind a key) don't fit grsentry's offline-unit-tests rule.
- **The order, revised (the user, 2026-10-24):** WhatsApp is completed now (P5, P6), not after grsentry's M1; grsentry then integrates over `../go.work` on the untagged grpop; the tag (P7) comes last, once grsentry runs on it and grpop needs no more changes.
- **Typed WhatsApp components replace `TemplateVariables`** (2026-10-24; breaking, but 0.2.0 is unreleased): one way to say what a template needs, with no positional-key gaps.
- **The template validator stays advisory** (2026-10-24): with no fresh list it uses the last one it had, or lets the send through with a warning; Meta decides.
- **grsentry builds on the untagged grpop through a pseudo-version** (2026-10-24): the user pushes grpop's `master` without a tag and grsentry's `go.mod` requires that commit (`go get github.com/gourdian25/grpop@<commit>`), so `GOWORK=off` builds, CI and `make e2e` get the same code the workspace uses from `../grpop`. P7's tag replaces it.
- **The circuit breaker counts only temporary failures** (2026-10-24, P2's open point): a permanent failure is about one message, not the vendor's health, and a burst of `550`s for bad addresses must not block every recipient.
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

## P4: the Resend sender (done)

`dispatcher.email.resend.go`, `NewResendDispatcher(ResendDispatcherDeps) (EmailSender, error)`, a hand-rolled `net/http` client like the Meta one.

- **Deps:** `APIKey` (required), `BaseURL` (default `https://api.resend.com`), `DefaultFrom`, `Timeout` (default 15 s), `HTTPClient` (optional), `TemplateEngine`, `RateLimiter`, `CircuitBreaker`, `Metrics`, `Logger`. The key never appears in a log line or an error.
- **The call:** `POST /emails`, `Authorization: Bearer <key>`, JSON `{from, to: [address], subject, html, text, reply_to}` from `prepareEmail`; `Idempotency-Key` when the message has one (Resend takes 1 to 256 characters and keeps it 24 hours; a longer key is sent as its SHA-256 hex). `SendResult.ProviderMessageID` is the response's `id`, which Resend's webhooks name later.
- **Errors** (Resend's error reference, read 2026-10-23):
  - permanent: `400` (`validation_error`, `invalid_idempotency_key`), `401` (a missing or invalid key), `403` (an unverified `from` domain, a restricted or suspended key), `404`, `405`, `422`, and `409 invalid_idempotent_request` (the same key with a different body: a caller bug);
  - temporary: `409 concurrent_idempotent_requests`, `429` (`rate_limit_exceeded`, and the daily and monthly quotas, which reset on their own), `500`, `503`, network errors and timeouts.
  - The body's `name` and `message` go into the error; its exact JSON shape is confirmed when the stage starts with one unauthenticated request (`401 missing_api_key` needs no account).
- **Tests:** an `httptest` server, like `dispatcher.whatsapp.metacloud_http_test.go`: the request's method, path, headers and body; the idempotency key, long and short; every status class above; a timeout and a cancelled context; the limiter and breaker gating every send; nothing secret in a log. A live smoke test runs only with `GRPOP_TEST_RESEND_API_KEY` set (it skips otherwise), sending to Resend's test address `delivered@resend.dev` (`bounced@resend.dev` answers like a `550`).
- **Mutants:** the idempotency header dropped; a `422` classed temporary; a `429` classed permanent; the key logged; `prepareEmail` bypassed.

*P4 as built (2026-10-24):*
- **Resend's error body, checked first:** one `POST https://api.resend.com/emails` with no key answered `401 {"message":"Missing API Key","name":"missing_api_key","statusCode":401}`, and one with a malformed key `401 {"message":"API key is invalid","name":"validation_error","statusCode":401}`. No account, no data sent.
- **`dispatcher.email.resend.go`** (new): `NewResendDispatcher(ResendDispatcherDeps)` with `APIKey` (required), `BaseURL`, `DefaultFrom`, `Timeout` (15 s), `HTTPClient`, `TemplateEngine`, both limiters, `CircuitBreaker`, `Metrics`, `Logger`; it refuses a missing key, a non-http(s) base URL and a bad `DefaultFrom`. `Send` starts from `prepareEmail`, checks the limiters, posts `{from, to, subject, html, text, reply_to}` inside the breaker, and returns Resend's `id`.
  - `Idempotency-Key` is the message's key, or its SHA-256 hex past Resend's 256 characters; no key, no header.
  - `resendErrorIsTemporary`: `429`, `5xx` and a `409` other than `invalid_idempotent_request` are temporary; every other refusal permanent. A network failure, a timeout and a success body without an `id` are temporary.
  - Addresses as Resend's docs write them (`resendAddress`): `Name <address>` for a plain name, quoted and escaped otherwise, never RFC 2047-encoded.
  - The API key only in the `Authorization` header; logs mask addresses and redact them inside Resend's message; `Close` releases only a client it made.
- **Docs:** README (the sender, its key and its error classes, the live test), `docs.go`, `docs/architecture.md` (a new §3.5, sections renumbered), `CLAUDE.md`, the CHANGELOG.
- **Tests** (`dispatcher.email.resend_test.go`, new, against an `httptest` fake): construction refusals; the request's method, path, headers and body; the idempotency header (none, long); 12 refusal classes; a timeout, a refused connection, a body without an id, a cancelled context; refusals before any call (a bad address, a limiter, an open breaker); templates rendered; neither the key nor a full address in a log line or the error; `resendAddress` round-trips through `net/mail`. `TestResendDispatcher_Live` sends to `delivered@resend.dev` only with `GRPOP_TEST_RESEND_API_KEY` set; it was not run (no key here).
- **Gate:** all green: gofmt, build and vet (both ways), `go mod tidy -diff`, `make precommit` (lint 0 issues, `-race`, coverage 97.1%) with a throwaway Mailpit (the one skip is the live Resend test; removed `-v`), `bark check` (`bark tree` for the two new files); `go run ./example` sent all four over SMTP.
- **Mutants:** 11 of 11 caught, each building, the control green: the idempotency header dropped; a `422` temporary; a `429` permanent; the key logged; `prepareEmail` bypassed; every `409` temporary; a long key sent raw; names never quoted; the breaker bypassed; the limiters skipped; Resend's message logged unredacted.
- **Not proven here:** that Resend accepts the request as built. The live test does that the first time it runs with a key; until then, the shape rests on Resend's API reference and the fake.

## What the WhatsApp code lacks (read 2026-10-24)

- **Only body text values, positional,** from `TemplateVariables map[string]string` keyed "1","2",…; a gap silently cuts the list (`positionalTemplateParameters`). No header (text, image, video, document, location), no buttons, no named parameters, no currency or date-time values. So no authentication (OTP) template can be sent: Meta's copy-code send puts the code in the body *and* as URL button `index "0"` (Meta's copy-code page, read 2026-10-24).
- **The template validator can't be wired to Meta by a caller:** it needs a `WhatsAppCloudAPIClient`, and the only real one (`newRealWhatsAppCloudAPIClient`) is unexported.
- **The validator can block a send Meta would accept, against its own doc:**
  - the listing reads one page (`limit=200`, no `paging`), so a template past it reads as not approved, a permanent error;
  - `countTemplateParameters` stops at `{{20}}` and counts named placeholders as 0;
  - a failed listing fails the send.
- **Nothing checks `To`:** any string goes to Meta.

Meta's reference, read 2026-10-24, confirms:
- the copy-code send shape (the code at most 15 characters);
- the URL button send with `parameter_name`;
- the location header;
- the component limits: a text header at most 60 characters, a body at most 1024, URL buttons at most 2, copy code at most 1, buttons at most 10.

Meta's docs have moved (`developers.facebook.com/documentation/business-messaging/whatsapp/`), and the pages read did not show the body, text-header and media-header parameter shapes or the quick-reply and coupon-code ones. P5 starts by confirming them, and the live test (P6) proves the payload against Meta.

## P5: WhatsApp messages (next)

- **`types.go`:** `WhatsAppMessage` keeps `To`, `TemplateName`, `LanguageCode` and replaces `TemplateVariables` with:
  - `Header *WhatsAppParameter`: a text, image, video, document or location value, one per template;
  - `Body []WhatsAppParameter`: in order for a positional template, each named (`Name`) for a named one, never mixed;
  - `Buttons []WhatsAppButton`: `{Index int, Kind (URL | QuickReply | CopyCode), Value string}`, where `Value` is the URL suffix, the payload or the coupon code.
- **`WhatsAppParameter`** holds exactly one value: `Text`; `Currency{FallbackValue, Code, Amount1000}`; `DateTime{FallbackValue}`; `Media{Kind image|video|document, ID or Link, Filename}`; or `Location{Latitude, Longitude, Name, Address}`. It also holds `Name` for a named template.
- **Constructors:** `WhatsAppText`, `WhatsAppNamedText`, `WhatsAppImage`, … and `WhatsAppOTP(code)`. The OTP one returns the body value and URL button 0 holding the code, Meta's authentication-template shape.
- **Checks** (`payloadvalidator.go`, all permanent, before any call; the limits re-read from Meta's reference first):
  - `To` is an optional `+` then 8–15 digits, otherwise the new `ErrInvalidPhoneNumber`;
  - each parameter holds exactly one value;
  - named and positional values aren't mixed;
  - a text header is at most 60 characters;
  - a body text value has no newline or tab and no run of more than four spaces;
  - an OTP code is at most 15 characters;
  - a button index is 0–9 and used once;
  - a media value names an `ID` or a `Link`.

  The parameter failures are the new `ErrWhatsAppParameterInvalid`, which names the field, never the value.
- **`dispatcher.whatsapp.metacloud.go`:**
  - `WhatsAppCloudAPIRequest` carries the components;
  - the real client builds Meta's `components` (`header`, `body`, `button` with `sub_type`, a string `index` and typed `parameters`);
  - `positionalTemplateParameters` goes;
  - `MemoryWhatsAppSender` and the dry run adapt (the dry run logs the template and the counts, never a value).
- **The circuit breaker counts only temporary failures,** in all three senders, through a small helper over `CircuitBreaker.Execute`. A permanent failure is returned unchanged and not counted.
- **Tests:**
  - the exact JSON for every component kind against the `httptest` server, copy-code OTP included (the code twice);
  - every check refused before a call;
  - named versus positional;
  - ten permanent failures leave the breaker closed, and temporary ones then open it.
- **Mutants** (on a copy, each built first):
  - the OTP button dropped;
  - the index sent as a number;
  - a newline allowed in a body value;
  - the phone check skipped;
  - a permanent failure counted by the breaker;
  - named and positional values mixed.

## P6: the template validator, usable and truthful

- **An exported client:** `NewMetaCloudAPIClient(MetaCloudAPIClientConfig{PhoneNumberID, BusinessAccountID, AccessToken, GraphAPIVersion, Timeout})`. The dispatcher uses it when `Client` is nil, and a caller hands the same client to `NewMetaTemplateValidator`.
- **The listing:**
  - `GetApprovedTemplates` follows `paging` to the end, with a hard page cap so a loop can't run away;
  - it asks for `status=APPROVED`;
  - it reads `parameter_format` when Meta returns it.
- **`WhatsAppTemplateInfo`** gains the header kind and whether it takes a value, the body's positional count or named set, and the URL buttons that take a value. Placeholders are counted with a regexp: no `{{20}}` cap, and named ones counted too. `Validate` compares the message's components with them; a mismatch is `ErrWhatsAppTemplateArityMismatch`, naming what differs.
- **Advisory when the listing fails:** `Validate` uses the last good list if it has one, and otherwise lets the send through. Either way it logs a warning and leaves Meta as the authority.
- **A live smoke test:** it runs only with `GRPOP_TEST_META_PHONE_NUMBER_ID`, `GRPOP_TEST_META_ACCESS_TOKEN` and `GRPOP_TEST_META_TO` set (and optionally `GRPOP_TEST_META_BUSINESS_ACCOUNT_ID`, to validate first), and skips otherwise. It sends Meta's `hello_world` sample template (`en_US`, no values).
- **Tests:**
  - three fake pages, with a template on the third approved;
  - named and positional counts, and `{{25}}`;
  - header and button arity;
  - a failed refresh with and without a cached list;
  - the exported client used by both the dispatcher and the validator.
- **Mutants:**
  - the listing stopping at page one;
  - the `{{20}}` cap back;
  - named placeholders uncounted;
  - a failed refresh blocking the send;
  - the approved filter dropped.

## P7: release v0.2.0 (last)

After grsentry's M1 runs on grpop and no more grpop change is needed:
- the CHANGELOG's `[0.2.0]` entry dated;
- `version.go`;
- the README's status line;
- `make prerelease`;
- the live Resend and Meta tests run with the user's keys, so the tag ships with both vendors proven.

Tagging and publishing (`make release VERSION=v0.2.0`) is outward-facing: the user runs it or says to. grsentry's `go.mod` then moves from the pseudo-version to `v0.2.0`.

## Known and accepted

- **At-least-once over SMTP.** `net/smtp` can't be cancelled, so a timed-out send's goroutine is abandoned and may still deliver; a caller that retries can send twice. Resend's `Idempotency-Key` closes that for 24 hours; SMTP can't, beyond the stable `Message-ID` (P3).
- **`SendStatusSent` means the vendor accepted it,** never that it reached an inbox. Bounces and complaints are Resend's webhooks, the caller's to receive.
- **A rate-limited send fails at once** (`ErrRateLimited`, temporary) rather than waiting: the caller's retry spaces it out.
- **No rate limiter ships with grpop:** a caller passes its own (grsentry's `grlimiter` fits as it is).

## How to prove a stage

- **The gate:** `gofmt -l .`, `go build ./... && go vet ./...`, `GOWORK=off go build ./...`, `GOWORK=off go mod tidy -diff`, `make precommit` (fmt, vet, lint, race, coverage at or above 95%), `bark check` (`bark tree` when a file is added or removed). The Mailpit test skips when Mailpit is down, which drops coverage below the gate: start it first and read the output for skips.
- **Mutants** (P2 to P6): on a copy of the repo, each confirmed to build first, never the working tree; an unmutated control first; read each failing run for which test caught it.
- **Never on the working tree** for a quick check: a throwaway test goes in a copy.
