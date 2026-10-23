# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`grpop` (module `github.com/gourdian25/grpop`) is a Go library: send-only,
transactional-message delivery over email (stdlib `net/smtp`, any SMTP vendor)
and WhatsApp (Meta Cloud API, hand-rolled `net/http` client). It sends messages
and does not receive them (not POP3), handles no push notifications (that's
`grnoti`), and has no notion of users or preferences. The caller passes the
recipient on every send. It is one flat public package (`package grpop`, every
file at the root). `example/` is a runnable demo (`go run ./example`, sends to
Mailpit by default).

**grpop is delivery only (since 0.2.0, decided 2026-10-23): a sender sends one
message once and reports what the vendor said.** It keeps nothing and retries
nothing. Retrying, keeping a message durably and suppressing duplicates are the
caller's (grsentry does them with an outbox written in the change's own
transaction). The old orchestrating `Service`, its idempotency store, its
dead-letter retry table (memory/Postgres/Mongo), `MessageEncryptor`, its
grevents lifecycle events and grpop's own rate limiters were removed; don't
bring them back. `FullJitterBackoff` stays exported for a caller's retry loop.

**grpop has no third-party Go dependencies, and keeping it that way is the
design goal.** Never add a vendor SDK (AWS, SendGrid, Twilio, Resend, ...), a
message-broker client or a storage driver: a vendor is a hand-rolled
`net/http` client like the Meta one. SMS and multi-recipient email were cut on
purpose (`docs/plan/grpop-plan.md`).

The live plan is `docs/plan/2026-10-23-grpop-delivery-only.md` (P1 remove,
P2 fix, P3 shared email preparation, P4 a Resend sender, P5 release `v0.2.0`,
P6 WhatsApp completed after grsentry's M1): one stage per "next", the user
commits. Design references: `docs/architecture.md` (the why behind every
decision below), `docs.go` (godoc only; its "Precise, non-aspirational claims"
section is the contract the README's Limitations list is drawn from, so keep
the two in sync), and `docs/plan/grpop-plan.md` (the original 16-stage plan,
which is history).

## Commands

```bash
go build ./... && go vet ./...
GOWORK=off go build ./...      # standalone check; ../go.work otherwise resolves sibling modules locally
make test                      # go test -count=1 -timeout=5m -cover ./...
make race                      # -race; mandatory before committing a change to a sender, the breaker or the template engine
go test -count=1 -run TestName -v .                        # one test
make lint                      # golangci-lint run ./... (.golangci.yml: standard + gosec, misspell, gocritic, revive)
make coverage-check            # root package only (go test -cover .), must be >= 95%
make coverage-summary          # per-function breakdown
make precommit                 # fmt + vet + lint + race + coverage-check, the gate before every commit
bark tree                      # regenerate bark.txt after adding/renaming a file (never hand-edit it)
make prerelease                # precommit + goreleaser check + snapshot (writes the gitignored dist/)
make release VERSION=vX.Y.Z    # tags, pushes and publishes a GitHub release: outward-facing, run only when asked
```

## Test backends

The one backend is Mailpit (`gourdian-mailpit`: SMTP `localhost:1025`, API
`localhost:8025`, no credentials; `testMailpitSMTPAddr` in
`dispatcher.email.smtp_mailpit_test.go`). Its test `t.Skip`s when Mailpit is
unreachable, so **a run with it down still passes but falls below the 95%
coverage gate**: start it first (`make docker-up`, idempotent) and check the
test output for skips.

The Meta WhatsApp Cloud API is the one deliberate exception, since it has no
local emulator. The dispatcher's rate-limit and breaker wiring is tested
against a fake `WhatsAppCloudAPIClient`, and the real HTTP client against an
`httptest.Server` (`dispatcher.whatsapp.metacloud_http_test.go`).

## Architecture

- **A sender** (`EmailSender`, `WhatsAppSender`) validates the message,
  renders it (email: exactly one of `TemplateName`, `InlineTemplate` or a
  literal body; `ErrNoContentModeSet` and `ErrMultipleContentModesSet` cover
  the two ways that fails), checks its rate limiters, runs the vendor call
  inside its optional circuit breaker, and records metrics. A failed attempt
  is `(SendResult{Status: SendStatusFailed}, err)`; an accepted one is
  `(result, nil)`. `SendStatusSent` means only that the vendor accepted it.
- **Every email sender starts from `prepareEmail`** (`dispatcher.email.go`):
  it checks the message, renders its one content mode, applies the default
  sender and parses the addresses, so all email vendors send and refuse
  alike. A new email sender builds its request from the `preparedEmail` and
  never re-reads `EmailMessage` fields itself. `EmailMessage.IdempotencyKey`
  rides along: sent as is to a vendor with idempotency, turned into a stable
  `Message-ID` (its SHA-256) by the SMTP sender.
- **Rate limiting is the caller's limiter, on two keys** (`ratelimiter.go`).
  `RateLimiter` is `Allow(ctx, key) (bool, error)`, grsentry's
  `pkg/grlimiter.Limiter`'s method set, so it plugs in without an import
  either way (never import grsentry here: the modules would require each
  other). A sender takes `RateLimiter` (key: the channel) and
  `RecipientRateLimiter` (key: the channel and a SHA-256 of the trimmed,
  lower-cased recipient), both optional, asks the recipient's first, and
  answers a refusal with `ErrRateLimited` at once, never waiting; a limiter
  error stops the send (fail closed).
- **Circuit breaking and template rendering live on the senders** (their
  `Deps`), gating or shaping every individual `Send`.

**Vendors:**

- **SMTP:** `net/smtp` can't be cancelled, so `transact` races the whole
  dial-to-close transaction against a timer and abandons the goroutine on
  timeout, which may still deliver. This is documented and deliberate.
  `SMTPDialer.Dial` performs STARTTLS itself. Plain-text auth over
  `SMTPTLSInsecureNoTLS` requires `AllowInsecureAuth`.
- **Meta:** every call names `GraphAPIVersion` (default
  `defaultMetaGraphAPIVersion`, `v26.0`; Meta retires a version about two
  years after release, so bump the default before it does). Sends use
  `PhoneNumberID`, while template listing for
  `TemplateValidator` needs the separate `BusinessAccountID`.
  `TemplateValidator`'s TTL cache is advisory. It can let a since-revoked
  template through, but it never blocks a send Meta would accept, and a passing
  check never skips the real call. grpop never rotates a Meta access token.

## Conventions

- **File names:** `<concern>.go` (`ratelimiter.go`, `circuitbreaker.go`),
  `dispatcher.<channel>.<vendor>.go`, with tests alongside. Every `.go` file
  and the Makefile opens with a `// File: <path>` (or `# File:`) header from
  `bark`.
- **Errors:** sentinels live in `errors.go` and are prefixed `"grpop: "`. A
  constructor's validation errors use `"grpop/<component>: "`. Give a distinct
  condition its own sentinel rather than reusing one. Match with `errors.Is`;
  there is no `IsX` helper. A vendor's own error (a Graph API envelope, an
  SMTP reply) is wrapped with context before it leaves a sender.
- **`ErrPermanent` is the retry contract.** A new validation sentinel is
  built with `permanent(errors.New(...))` so it matches `ErrPermanent`; a
  sender wraps a vendor's final refusal with `permanent(err)` (SMTP `5xx`;
  Meta refusals outside `metaTemporaryErrorCodes`, a `429` and `5xx`).
  Anything worth another attempt stays plain. Getting this wrong either
  retries a hopeless send or drops a deliverable one.
- **Addresses and logs:** every email address goes through
  `parseEmailAddress` (net/mail, line breaks refused) before it reaches a
  header or the envelope; a log line carries `maskEmail`'s form (or
  `maskPhoneNumber`'s), never a full address or number, and never a
  rendered body.
- **Logger:** the ecosystem-wide slog-shaped interface (`Debug/Info/Warn/Error(msg,
  args ...any)`), so `*slog.Logger` satisfies it. Constructors pass a nil logger
  through `OrNop`. New log lines take a flat `"grpop: "` prefix plus a
  `"component"` key/value; don't bake a `/backend` suffix into the message.
  Level rules are in `../CONVENTIONS.md`.
- **`docs.go`:** holds only the package godoc.
- **Releases:** bump `Version` in `version.go` to match the tag, and add a
  `CHANGELOG.md` entry. GoReleaser ships a source archive only, since there are
  no binaries (`.goreleaser.yaml`).
