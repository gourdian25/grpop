# grpop

[![Go Reference](https://pkg.go.dev/badge/github.com/gourdian25/grpop.svg)](https://pkg.go.dev/github.com/gourdian25/grpop)
[![Go Version](https://img.shields.io/badge/go-1.26.4+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Send-only, multi-channel transactional-messaging delivery library for the
gourdian ecosystem (`github.com/gourdian25/grpop`): email (stdlib SMTP) and
WhatsApp (Meta Cloud API, direct) senders that each **send one message once
and report what the vendor said**, with templates, optional circuit breaking
and optional per-channel and per-recipient rate limiting through a limiter
you supply. No third-party Go dependencies.

grpop is **not** an email-retrieval/POP3 library despite the name, and
**not** a push-notification library — that's the sibling module
[grnoti](https://github.com/gourdian25/grnoti)'s job. grpop has no concept
of "user" or "preferences"; it's about delivery, not identity — the
recipient address/number is supplied per-send by the caller.

Status: being reshaped to delivery only, then given a Resend sender
([docs/plan/2026-10-23-grpop-delivery-only.md](docs/plan/2026-10-23-grpop-delivery-only.md)).
`v0.1.0` had an orchestrating `Service` with an idempotency store and a
dead-letter retry table; they are gone (see
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
`smtp.resend.com`, a bare relay) is a config value, not a Go dependency.
WhatsApp goes directly against Meta's Graph API over a hand-rolled
`net/http` client (Meta ships no official Go SDK). There is no rate-limiter
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
  `ErrWhatsAppTemplateArityMismatch`), a template that won't render, an SMTP
  `5xx` reply (a mailbox that doesn't exist, a refused sender, bad
  credentials), and a Meta refusal of the request, the account or the
  template.
- **Temporary:** `ErrRateLimited`, `ErrCircuitOpen`, `ErrTooManyRequests`,
  an SMTP `4xx` reply, a dial, TLS or network failure, a timeout, a
  cancelled context, a Meta `429` or `5xx`, and Meta's retry-later codes (2,
  4, 80007, 130429, 131000, 131016, 131056, 133004, 2494100).

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
skips when Mailpit is down, which drops coverage below the 95% gate):

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
