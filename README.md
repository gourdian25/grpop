# grpop

[![Go Reference](https://pkg.go.dev/badge/github.com/gourdian25/grpop.svg)](https://pkg.go.dev/github.com/gourdian25/grpop)
[![Go Version](https://img.shields.io/badge/go-1.26.4+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Send-only, multi-channel transactional-messaging delivery library for the
gourdian ecosystem (`github.com/gourdian25/grpop`): email (stdlib SMTP, or
Resend's API) and WhatsApp (Meta Cloud API, direct) senders that each **send one message once
and report what the vendor said**, with templates, optional circuit breaking
and optional per-channel and per-recipient rate limiting through a limiter
you supply. No third-party Go dependencies.

grpop is **not** an email-retrieval/POP3 library despite the name, and
**not** a push-notification library — that's the sibling module
[grnoti](https://github.com/gourdian25/grnoti)'s job. grpop has no concept
of "user" or "preferences"; it's about delivery, not identity — the
recipient address/number is supplied per-send by the caller.

Status: `v0.2.0` (2026-10-24), delivery only: an SMTP sender, a Resend
sender and a WhatsApp (Meta Cloud API) sender, each sending one message once
and saying whether it was accepted, refused for good or failed for now
([docs/plan/2026-10-23-grpop-delivery-only.md](docs/plan/2026-10-23-grpop-delivery-only.md);
the changes are in [CHANGELOG.md](CHANGELOG.md)). grsentry's email outbox
sends through it. `v0.1.0` had an orchestrating `Service` with an
idempotency store and a dead-letter retry table; they are gone (see
[Delivery only](#delivery-only)).

## Table of Contents

- [Delivery only](#delivery-only)
- [Install](#install)
- [Dependencies](#dependencies)
- [Quickstart](#quickstart)
- [Circuit breaking and rate limiting](#circuit-breaking-and-rate-limiting)
- [Public API overview](#public-api-overview)
- [Error handling](#error-handling)
- [Limitations / out of scope](#limitations--out-of-scope)
- [Testing](#testing)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## Delivery only

grpop keeps nothing and retries nothing. Retrying a failed send, keeping a
message durably until it goes, and suppressing a duplicate are the
caller's: only the caller knows the transaction a message belongs to and
how long its link stays valid, and only a store written in that transaction
(an outbox) can promise a committed message is never lost. grsentry does
all three with an outbox and calls a grpop sender from its worker.
`FullJitterBackoff` is exported for your retry loop; see
[example/main.go](example/main.go)'s `sendWithRetry` for the smallest one.

## Install

```sh
go get github.com/gourdian25/grpop
```

## Dependencies

None outside the standard library. Email goes over `net/smtp`, so any
SMTP-speaking vendor (SES, SendGrid, Postmark, Mailgun, Resend's
`smtp.resend.com`, a bare relay) is a config value, not a Go dependency, or
over Resend's HTTP API through a hand-rolled `net/http` client (no Resend
SDK). WhatsApp goes directly against Meta's Graph API the same way (Meta
ships no official Go SDK). There is no rate-limiter
implementation to pull in: `RateLimiter` is one method, and you pass your
own.

## Quickstart

```go
engine := grpop.NewEmailTemplateEngine(grpop.EmailTemplateEngineConfig{})
engine.RegisterTemplate("password-reset", grpop.EmailTemplate{
    SubjectTemplate:  "Reset your password",
    HTMLBodyTemplate: `<p>Click <a href="{{.ResetLink}}">here</a> to reset your password.</p>`,
})

sender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
    Addr:           "email-smtp.us-east-1.amazonaws.com:587",
    Auth:           smtp.PlainAuth("", sesUser, sesPass, "email-smtp.us-east-1.amazonaws.com"),
    DefaultFrom:    "no-reply@example.com",
    TemplateEngine: engine,
})
if err != nil {
    log.Fatal(err)
}
defer sender.Close()

result, err := sender.Send(ctx, grpop.EmailMessage{
    To:           "user@example.com",
    TemplateName: "password-reset",
    TemplateData: map[string]any{"ResetLink": link},
})
switch {
case err == nil:
    // accepted by the vendor
case errors.Is(err, grpop.ErrPermanent):
    // it will fail the same way every time: stop
default:
    // temporary: your retry tries again later
}
```

The Resend sender takes the same `EmailMessage` and renders it the same way;
pick it instead of SMTP to get Resend's idempotency (below) and its email id:

```go
sender, err := grpop.NewResendDispatcher(grpop.ResendDispatcherDeps{
    APIKey:         os.Getenv("RESEND_API_KEY"),
    DefaultFrom:    "Acme <no-reply@acme.test>", // a domain verified in Resend
    TemplateEngine: engine,
})
```

A WhatsApp message names a Meta-approved template and fills its
placeholders with typed values: `Header` (text, image, video, document or
location), `Body` (text, currency or date and time, positional or named) and
`Buttons` (a URL suffix, a quick-reply payload, a coupon code or a one-time
code). An authentication template's code goes in twice, which
`WhatsAppOTP` does:

```go
sender, err := grpop.NewMetaCloudWhatsAppDispatcher(grpop.MetaCloudDispatcherDeps{
    PhoneNumberID: os.Getenv("META_PHONE_NUMBER_ID"),
    AccessToken:   os.Getenv("META_ACCESS_TOKEN"),
})

msg := grpop.WhatsAppMessage{To: "+15551234567", TemplateName: "login_code", LanguageCode: "en_US"}
msg.Body, msg.Buttons = grpop.WhatsAppOTP(code)
result, err := sender.Send(ctx, msg)

// a positional template: {{1}} and {{2}}, in order
order := grpop.WhatsAppMessage{To: "+15551234567", TemplateName: "order_shipped", LanguageCode: "en_US",
    Body:    []grpop.WhatsAppParameter{grpop.WhatsAppText("Ada"), grpop.WhatsAppText("#42")},
    Buttons: []grpop.WhatsAppButton{{Index: 0, Kind: grpop.WhatsAppButtonURL, Value: "track/42"}},
}
```

Before calling Meta, the sender refuses a number that isn't in international
form (`ErrInvalidPhoneNumber`) and any value Meta's send-time rules refuse
(`ErrWhatsAppParameterInvalid`, naming the field). Examples: a line break, a
tab or five spaces in a row in a value; a header over 60 characters; a
one-time code over 15; named and positional values mixed.

See [example/main.go](example/main.go) and
[example/templates_auth.go](example/templates_auth.go) for a complete,
runnable walkthrough covering all four use cases this design is grounded
against (forgot-password + three invite variants) — `go run ./example`,
against a local [Mailpit](https://github.com/axllent/mailpit) container by
default, no real credentials required.

## Circuit breaking and rate limiting

Both are optional and live on the sender's deps, gating every `Send`:

```go
// After 5 consecutive SMTP failures, stop calling the relay for 30s and
// fail fast instead; a closed breaker's failure counter resets after 1
// minute with no failures.
breaker, err := grpop.NewCircuitBreaker(5, 30*time.Second, time.Minute)
if err != nil {
    log.Fatal(err)
}

sender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
    // ...
    RateLimiter:          channelLimiter,   // key "email": the whole channel
    RecipientRateLimiter: recipientLimiter, // key "email:<sha256 of the address>"
    CircuitBreaker:       breaker,
})
```

The breaker counts only temporary failures (a timeout, a `4xx` reply, a
vendor `5xx`). A permanent one (`ErrPermanent`: a mailbox that doesn't exist,
a template Meta refuses) concerns that message, not the vendor's health, and
the vendor did answer, so the breaker records it as a success. A run of bad
addresses never opens the breaker on the good messages behind them.

`RateLimiter` is `Allow(ctx context.Context, key string) (bool, error)`,
exactly grsentry's `pkg/grlimiter.Limiter`, so its local or Redis limiter
plugs in as it is. The two tiers catch different abuse: a sweep across many
recipients, and many callers aiming at one. A refused send returns
`ErrRateLimited` at once; a sender never waits for a token.

## Public API overview

**EmailSender**

| Backend | Constructor |
|---|---|
| SMTP (any vendor) | `NewSMTPDispatcher(SMTPDispatcherDeps)` |
| Resend's HTTP API | `NewResendDispatcher(ResendDispatcherDeps)` |
| In-memory (tests/dev) | `NewMemoryEmailSender()` |
| Dry run (staging, redacted logging) | `NewDryRunEmailSender(Logger)` |

**WhatsAppSender**

| Backend | Constructor |
|---|---|
| Meta WhatsApp Business Cloud API | `NewMetaCloudWhatsAppDispatcher(MetaCloudDispatcherDeps)` |
| In-memory (tests/dev) | `NewMemoryWhatsAppSender()` |
| Dry run (staging, redacted logging) | `NewDryRunWhatsAppSender(Logger)` |

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

**WhatsAppCloudAPIClient** (the Graph API client both use)

| Variant | Constructor |
|---|---|
| Meta's Graph API, `net/http` | `NewMetaCloudAPIClient(MetaCloudAPIClientConfig)` |

To check templates before sending, build the client once and give it to
both:

```go
client, err := grpop.NewMetaCloudAPIClient(grpop.MetaCloudAPIClientConfig{
    PhoneNumberID:     os.Getenv("META_PHONE_NUMBER_ID"),     // to send
    BusinessAccountID: os.Getenv("META_BUSINESS_ACCOUNT_ID"), // to list templates
    AccessToken:       os.Getenv("META_ACCESS_TOKEN"),
})
validator, err := grpop.NewMetaTemplateValidator(grpop.MetaTemplateValidatorDeps{Client: client})
sender, err := grpop.NewMetaCloudWhatsAppDispatcher(grpop.MetaCloudDispatcherDeps{
    Client:            client,
    TemplateValidator: validator,
})
```

The validator lists the account's approved templates, every page, and keeps
the list for its TTL (5 minutes). For each send it checks that the
template is approved in that language and that the message fills what the
template takes:
- the header: text, media of the right kind, or a location;
- the body: its count of values, or its names;
- the buttons: each button that requires a value has one.

A misfit is `ErrWhatsAppTemplateArityMismatch` saying what differs. The
validator is advisory, and Meta decides:
- when a listing fails, it checks against the last list it had, or lets the
  send through when it has none;
- a refusal is checked once more against a new listing (at most one listing
  per 30 seconds), so a template approved a moment ago isn't refused for
  long.

**Helpers:** `FullJitterBackoff(base, max, attempt)` for a caller's retry
loop.

## Error handling

Sentinel errors (`errors.go`), matched with `errors.Is` — there is
deliberately no `IsX(err) bool` helper, consistent with every other
`gourdian25` repo.

**`ErrPermanent`** is the one a retry loop needs: every failure that will
fail the same way on every attempt matches it, and nothing else does.

- **Permanent:** the validation sentinels (`ErrRecipientRequired`,
  `ErrNoContentModeSet`, `ErrMultipleContentModesSet`,
  `ErrEmailTemplateNotFound`, `ErrInlineTemplateTooLarge`,
  `ErrEmailFromRequired`, `ErrEmailTemplateEngineRequired`,
  `ErrInvalidAddress`, `ErrWhatsAppTemplateNameRequired`,
  `ErrWhatsAppLanguageCodeRequired`, `ErrWhatsAppTemplateNotApproved`,
  `ErrWhatsAppTemplateArityMismatch`, `ErrInvalidPhoneNumber`,
  `ErrWhatsAppParameterInvalid`), a template that won't render, an SMTP
  `5xx` reply (a mailbox that doesn't exist, a refused sender, bad
  credentials), and a Meta refusal of the request, the account or the
  template.
- **Temporary:** `ErrRateLimited`, `ErrCircuitOpen`, `ErrTooManyRequests`,
  an SMTP `4xx` reply, a dial, TLS or network failure, a timeout, a
  cancelled context, a Meta `429` or `5xx`, and Meta's retry-later codes (2,
  4, 80007, 130429, 131000, 131016, 131056, 133004, 2494100).

**`EmailMessage.IdempotencyKey`** (optional) is the same value on every
attempt at one message, an outbox row's id say. The Resend sender sends it as
Resend's `Idempotency-Key` header, so Resend sends a retried message once
within 24 hours (a key over 256 characters goes as its SHA-256). The SMTP
sender makes the `Message-ID` its SHA-256 (the key itself never appears in
the message), so a retried message carries the same `Message-ID`; SMTP can
do no more than that. Never give two different messages one key.

The Resend sender classes Resend's refusals by its error reference: a `429`
(the rate limit and the daily and monthly quotas), a `5xx`, and a `409` for a
request with the same key still running are temporary; every other refusal
(an invalid field or key, an unverified sender domain, the same key with a
different body) is permanent.

`ErrInvalidAddress` covers a `From`, `To` or `Reply-To` that `net/mail` can't
parse or that carries a line break (which would add a header of its own); it
names the field, never the value. Display names are fine
(`"Acme <no-reply@acme.test>"`): the headers carry them, `MAIL FROM` and
`RCPT TO` the bare addresses. Logs show an address masked
(`a***@example.com`), including inside an SMTP reply that echoes it.

## Limitations / out of scope

- **SendStatusSent means "the vendor accepted the request," never
  "arrived" or "was read."** grpop has no vendor-side bounce/complaint/
  delivery-receipt feedback for either channel.
- **A caller's retry is at-least-once, not exactly-once.** A network error
  after the vendor already accepted a send is indistinguishable from one
  before, and the SMTP sender can't be cancelled mid-transaction (a timed-out
  send's goroutine is abandoned and may still deliver).
- **No message broker, queue or store anywhere in this package.** Every
  `Send` is direct and synchronous on the caller's goroutine.
- **SMS is not supported, and never was in scope for this design** — see
  `docs/plan/grpop-plan.md`'s revision note for the reasoning (dependency
  minimization; WhatsApp/email cover the four grounding use cases).
  Multi-recipient email (`EmailMessage.To []string`) is also cut: a real
  SMTP transaction can partially fail per-recipient in a way `SendResult`
  has no room to represent, so send once per recipient instead.
- **WhatsApp has no local test emulator.** The Meta Cloud API client is
  tested against a fake `WhatsAppCloudAPIClient` and an `httptest` server.
- **A WhatsApp message's checks are Meta's documented send-time rules (read
  2026-10-24), not a promise Meta will accept it.** Meta stays the
  authority: a message that passes can still be refused (an unknown
  template, a wrong count of values), and a rule Meta adds later is only
  Meta's refusal until grpop learns it.
- **`TemplateValidator` is advisory, with a 30-second window both ways.** A
  since-revoked template can look approved until the next listing (Meta
  then refuses the send). A template approved or edited in the last 30
  seconds can be refused by a permanent error, since a refusal gets at most
  one new listing per 30 seconds. A failed listing never blocks a send.
- **grpop never refreshes or rotates a Meta access token.** Token lifecycle
  is entirely the operator's responsibility.
- **Meta retires a Graph API version about two years after release.**
  `MetaCloudDispatcherDeps.GraphAPIVersion` defaults to `v26.0` (released
  2026-07-29); set it to move ahead of grpop's default.
- **`CircuitBreaker` state is per-process, deliberately not centralized** —
  avoids a synchronized thundering-herd retry the moment a relay recovers.

See [docs.go](docs.go)'s "Precise, non-aspirational claims" section for the
complete list this draws from.

## Testing

The SMTP sender's integration test runs against a real local Mailpit (it
skips when Mailpit is down, which drops coverage below the 95% gate). The
Resend sender is tested against a local fake of its API; one live test sends
to Resend's `delivered@resend.dev` only when `GRPOP_TEST_RESEND_API_KEY` is
set (`GRPOP_TEST_RESEND_FROM` picks the sender, default
`onboarding@resend.dev`). The WhatsApp sender, the listing and the validator
are tested against local fakes of Meta's API. One live test sends Meta's
`hello_world` sample template only when `GRPOP_TEST_META_PHONE_NUMBER_ID`,
`GRPOP_TEST_META_ACCESS_TOKEN` and `GRPOP_TEST_META_TO` (a number the account
may message) are set. With `GRPOP_TEST_META_BUSINESS_ACCOUNT_ID` set as well,
it first lists the account's templates and validates the send.

```sh
make docker-up   # Mailpit
make test        # or `make race` for the race detector
make docker-down # stop it when done
```

## Development

```sh
make docker-up   # start Mailpit
make precommit   # fmt + vet + lint + race + coverage-check
make docker-down # stop it when you're done
```

## Contributing

Issues and PRs are welcome at
[github.com/gourdian25/grpop](https://github.com/gourdian25/grpop). Please
run `make precommit` (fmt + vet + lint + race + coverage-check) before
submitting.

## License

MIT — see [LICENSE](LICENSE).
