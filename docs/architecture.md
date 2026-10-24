# grpop architecture

This document records how grpop is put together and *why*, with particular
attention to decisions made or revised during implementation that the
original plan didn't fully anticipate (see
[docs/plan/grpop-plan.md](plan/grpop-plan.md) for the stage-by-stage
account). It is a design reference, not a tutorial — see the
[README](../README.md) for a quickstart and
[example/main.go](../example/main.go) for a runnable, narrated walkthrough.

## 0. Delivery only (0.2.0, 2026-10-23)

grpop sends one message once and reports what the vendor said; it keeps
nothing and retries nothing. Up to 0.1.0 it also had an orchestrating
`Service` (an idempotency check, an inline retry, a dead-letter retry table
in memory, Postgres or Mongo, an optional payload encryptor, lifecycle
events on grevents) and its own two-tier rate limiters (in-process, and a
Lua-scripted Redis one). Those were removed for grsentry's email work
([plan/2026-10-23-grpop-delivery-only.md](plan/2026-10-23-grpop-delivery-only.md)):

- **Retry, durability and duplicates belong to the caller.** Only the
  caller knows the transaction a message belongs to and how long its link
  stays valid, and only a store written in that transaction (an outbox) can
  promise a committed message is never lost. A dead-letter table that only
  catches a send after its in-memory retries failed can't: a crash during
  those retries lost the message. Code review on 2026-10-23 also found the
  removed layer could lose a message outright (a failed dead-letter write
  still marked the key processed), left a crashed worker's claimed row
  `retrying` forever (no lease), raced its duplicate check, and refused an
  email-only setup.
- **No rate limiter of grpop's own.** `RateLimiter` is
  `Allow(ctx, key) (bool, error)`, grsentry's `pkg/grlimiter.Limiter`'s
  method set, so that limiter plugs in without an import either way (grpop
  importing grsentry would make the two modules require each other).

`FullJitterBackoff` stays, exported, for the caller's retry loop. The
sections below describe what is left; the removed parts' design is in git
history and in [plan/grpop-plan.md](plan/grpop-plan.md).

## 1. Package shape

grpop's public API is a single flat package with no subpackages and no
third-party dependencies:

```
interfaces.go / types.go / errors.go / logger.go / docs.go
retrystrategy.go            circuitbreaker.go           payloadvalidator.go
ratelimiter.go (the shared two-limiter check)
templateengine.email.go
dispatcher.email.smtp.go    dispatcher.whatsapp.metacloud.go
templatevalidator.whatsapp.go
dryrun.go                   memory.go (in-memory senders)
example/
```

File naming: `dispatcher.<channel>.<vendor>.go` for the senders — a third
segment, since grpop's dispatch layer has a genuinely two-dimensional axis
(channel × vendor) that `grnoti`'s single-channel, single-vendor dispatch
layer didn't need. Email goes over stdlib `net/smtp`; WhatsApp goes over a
hand-rolled `net/http` client against Meta's Graph API (no official Go SDK
exists). See `docs.go`'s package-level doc comment.

## 2. Interfaces and implementations

| Interface | Implementations |
|---|---|
| `EmailSender` | `smtpDispatcher` (stdlib SMTP, any vendor); `dryRunEmailSender` (staging); `MemoryEmailSender` (tests) |
| `WhatsAppSender` | `metaCloudDispatcher` (Meta Graph API, direct); `dryRunWhatsAppSender` (staging); `MemoryWhatsAppSender` (tests) |
| `RateLimiter` | none: the caller's (grsentry's `pkg/grlimiter` fits as it is) |
| `CircuitBreaker` | `standardCircuitBreaker` — the one implementation, returned as the interface type |
| `EmailTemplateEngine` | `defaultEmailTemplateEngine` (`html/template` + `text/template`) |
| `TemplateValidator` | `metaTemplateValidator` — TTL-cached over `WhatsAppCloudAPIClient.GetApprovedTemplates` |
| `WhatsAppCloudAPIClient` | `metaCloudAPIClient` (real, `net/http`, `NewMetaCloudAPIClient`; one serves a sender and a validator); fake in tests — Meta's Graph API has no local emulator (§5) |

## 3. Key design decisions

### 3.1 Two rate limiters, both the caller's: per channel and per recipient

A sender takes two optional `RateLimiter`s: `RateLimiter`, keyed by the
channel alone, and `RecipientRateLimiter`, keyed by the channel and a
SHA-256 of the recipient (so the limiter's backend never stores an address
or number). A per-recipient limit alone can't stop one actor from
triggering many distinct recipients' sends in a burst (a scripted
forgot-password sweep across a whole user list); a per-channel limit alone
can't stop one recipient from being spammed by many distinct callers.
`checkRateLimits` asks the recipient's first, so a send one recipient's
bucket refuses spends nothing from the channel's. A refusal is
`ErrRateLimited` at once (a sender never waits for a token: the caller's
retry spaces it out), and a limiter that errors stops the send (fail
closed).

### 3.2 Rate limiting and template rendering live on the senders

Both gate or shape every individual `Send`: the limiters and the circuit
breaker on `SMTPDispatcherDeps`/`MetaCloudDispatcherDeps`, and template
rendering on `SMTPDispatcherDeps.TemplateEngine`, since content-mode
resolution (`TemplateName`/`InlineTemplate`/literal) has to happen
immediately before the MIME message is built.

Every sender calls its vendor through `executeThroughBreaker`, with the
send function returning the already-classified error. That helper lets the
breaker count only a temporary failure. A permanent one is about the
message (a mailbox that doesn't exist, a template Meta refuses), and the
vendor did answer, so the breaker records it as a success and the error
comes back unchanged (decided 2026-10-24). Counted as failures, a run of
bad addresses would open the breaker and hold back the good messages
behind them. This is why the SMTP sender classes a `5xx` inside the send
function, not after the breaker.

### 3.3 A failure is the returned error, and says whether to retry

`Send` returns `(SendResult{Status: SendStatusFailed}, err)` for a failed
attempt and `(result, nil)` for an accepted one; there is no second channel
for "failed but handled". `errors.Is(err, ErrPermanent)` tells a caller's
retry to stop (0.2.0, P2): every validation sentinel is built with
`permanent`, so it matches `ErrPermanent` and still itself, and a sender
wraps a vendor's final refusal the same way, keeping its message and chain
(`errors.As` still finds the `*textproto.Error`). SMTP: a `5xx` reply is
permanent, a `4xx` and every transport failure temporary. Meta: its
retry-later codes (read from its error reference, listed in
`metaTemporaryErrorCodes`), a `429` and a `5xx` are temporary, every other
refusal permanent. The circuit breaker still counts every failure, a
permanent one included.

### 3.4 One preparation step for every email sender

`prepareEmail` (`dispatcher.email.go`, 0.2.0 P3) is the only path from an
`EmailMessage` to what a vendor is sent: check it (`validateEmailMessage`),
render its one content mode (`renderEmailContent`), apply the default
sender, parse `From`, `To` and `Reply-To`. Every email sender builds its
request from the `preparedEmail` it returns, so two vendors send the same
message for the same input and refuse the same ones, with the same
permanent errors. It also carries `EmailMessage.IdempotencyKey`: a vendor
with idempotency gets it as is; the SMTP sender makes the `Message-ID` the
key's SHA-256 at the sender's domain (`smtpMessageID`), the same on every
attempt and never the key itself.

### 3.5 Resend: a hand-rolled client, idempotent, classed by its error reference

`NewResendDispatcher` (`dispatcher.email.resend.go`, 0.2.0 P4) posts one
`POST /emails` per send, `net/http` and `encoding/json` only, like the Meta
client: grpop takes no vendor SDK, and the SDK would add nothing to one POST.
It builds the body from `prepareEmail`, so it sends what the SMTP sender
would. `EmailMessage.IdempotencyKey` goes as Resend's `Idempotency-Key`
(1 to 256 characters, kept 24 hours; a longer key as its SHA-256 hex), which
makes a caller's retry safe within that window. Resend's reply is
`{"id"}` on success and `{"message","name","statusCode"}` on a refusal (seen
on 2026-10-24 with a request that carried no key). `resendErrorIsTemporary`
classes a refusal from Resend's error reference: `429` (the per-second limit
and the daily and monthly quotas), `5xx`, and a `409` other than
`invalid_idempotent_request` are temporary; everything else is permanent.
Addresses go as Resend's docs write them (`Name <address>`, quoted only when
the name needs it, never RFC 2047-encoded, since the body is JSON). The API
key is in the `Authorization` header and nowhere else.

### 3.6 Addresses are parsed, and logged masked

`From`, `To` and `Reply-To` go through `net/mail.ParseAddress`
(`parseEmailAddress`), and a CR or LF anywhere is refused first: written
into a header unchecked, a line break let `Reply-To` add a `Bcc` line
(found 2026-10-23; `MAIL FROM` and `RCPT TO` were safe only because
`net/smtp` refuses line breaks there). The headers carry the parsed form (a
display name quoted or Q-encoded), the envelope and the recipient's
rate-limit key the bare address, and a bad `DefaultFrom` fails at
construction. Log lines carry `maskEmail`'s `a***@example.com`, and an SMTP
reply that echoes an address is masked before it is logged.

### 3.7 The Meta Cloud API client: hand-rolled, narrow, fake-tested

`WhatsAppCloudAPIClient` is grpop's own thin interface over two Graph API
endpoints (`SendTemplateMessage`, `GetApprovedTemplates`) — no official Go
SDK exists for WhatsApp regardless of vendor, so a hand-rolled `net/http` +
`encoding/json` client (`metaCloudAPIClient`) was always required. Sending
uses `PhoneNumberID`; listing approved templates (which backs
`TemplateValidator`, §3.8) needs the WhatsApp Business Account ID instead —
a separate ID Meta assigns, added to `MetaCloudDispatcherDeps` as
`BusinessAccountID` during implementation once this distinction became
concrete (the original plan draft didn't separate the two IDs). This is
the **one deliberate exception** to grpop's real-local-backend testing
policy: Meta's Graph API has no local emulator, so `metaCloudDispatcher`'s
own rate-limiter/circuit-breaker wiring is tested against a fake
`WhatsAppCloudAPIClient`, and the real HTTP client's own request-building/
auth-header/error-envelope-parsing logic is separately tested against a
real local `httptest.Server` (not Meta itself, but a real HTTP round trip). Every call names `GraphAPIVersion` (default
`v26.0`; Meta retires a version about two years after release, and the
earlier hard-coded `v20.0` stopped on 2026-09-24).

A `WhatsAppMessage` fills its template with typed values (since 0.2.0,
replacing `TemplateVariables map[string]string`, which carried only
positional body text and silently cut the list at a gap in its keys):

- `Header`: text, image, video, document or location;
- `Body`: text, currency or date and time, positional or named, never mixed;
- `Buttons`: a URL suffix, a quick-reply payload, a coupon code or a
  one-time code, each by its index.

The real client turns them into Meta's `components`. A button carries its
`sub_type` and a string `index`, and a one-time code goes as the `url`
sub_type, sent twice (body and button 0) as Meta's copy-code and one-tap
authentication templates require. `WhatsAppOTP` builds that pair.

`validateWhatsAppMessage` refuses, before any call, what Meta's send-time
rules refuse (read 2026-10-24). The tests pin the exact JSON of every kind
against an `httptest` server.

### 3.8 `TemplateValidator`: advisory, TTL-cached, a short false-negative window

Meta requires every WhatsApp template to go through approval before use.
`TemplateValidator.Validate` checks that the template is approved in the
message's language, and that the message fills what it takes
(`WhatsAppTemplateInfo`, read from Meta's listing):
- a text header's placeholder, by name in a named template; media of the
  header's kind; a location;
- the body's placeholders, by count or by name (counted with a regexp, so
  `{{25}}` and `{{first_name}}` count; until 0.2.0 counting stopped at
  `{{20}}` and missed names);
- every button that requires a value (a URL with a placeholder, a coupon, a
  one-time code). Meta lists a created one-time-code button as a `URL`, so
  an OTP button fits one.

That catches the commonest refusals before a live send.

**The listing** pages by Meta's cursor to the end (until 0.2.0 it read one
page, so a template past it read as unapproved). It asks for
`status=APPROVED` and filters again itself. It never fetches the
`paging.next` URL: the access token goes only to the configured host. A
repeated cursor, or more than 100 pages of 100, is an error rather than a
cut-off list.

**The cache** (default 5 minute TTL, refreshed lazily on a stale `Validate`
call or eagerly via `Refresh`) means "approved as of the last check," not a
live guarantee. The validator stays advisory, and Meta is the authority:
- a failed listing never blocks a send (until 0.2.0 it did): it checks
  against the last list it had, or lets the send through when it has none;
- after a failure it lists again only after 30 seconds, so a Meta outage
  costs one listing per 30 seconds, not one per send;
- a refusal is checked once more against a new listing before it is
  returned, at most one listing per 30 seconds.

So a since-revoked template can briefly look approved (a false positive,
and Meta's own refusal follows). A template approved or edited in the last
30 seconds can be refused (a false negative). That window is the cost of
not listing on every refusal, and a passed check never skips the call to
Meta.

### 3.9 SMTP: zero dependencies, a narrow fakeable client, timeout-by-goroutine-race

`SMTPClient` (`Auth`/`Mail`/`Rcpt`/`Data`/`Close`) is satisfied directly by
`*net/smtp.Client` — no adapter type needed. `SMTPDialer.Dial` performs the
full connect + (for `SMTPTLSStartTLS`) `StartTLS` handshake internally,
since the narrow `SMTPClient` interface has no room for `Hello`/`StartTLS`
themselves; `Mail`/`Auth` trigger an implicit `EHLO` internally via
`net/smtp`'s own behavior, so no explicit `Hello` call is needed either.

`net/smtp`'s API is fully blocking with no way to cancel an in-flight call,
so `ConnectTimeout`/`SendTimeout` enforcement races the whole
dial-through-close transaction against a timer on a background goroutine
(`transact`) rather than threading a context through `net/smtp` itself. A
timed-out call's goroutine is abandoned, not interrupted — it keeps running
to its own eventual completion or I/O error in the background. This is a
known, documented limitation (see `transact`'s own doc comment), not an
oversight: the alternative (no timeout at all) risks a hung vendor
connection stalling a caller's request indefinitely, which is strictly
worse.

## 4. Notable bugs caught before they shipped

Up to 0.1.0, two classes of bug in the since-removed storage layer were
caught by empirical verification against real backends: ciphertext written
straight to a Postgres JSONB column (fixed by base64-wrapping), and five
points of an external review of the Redis rate limiter (a client-supplied
clock, unhashed recipient keys, an unjittered poll, an undocumented
fail-open/fail-closed policy, an unchecked type assertion). The hashed
recipient key and the fail-closed policy carry over to `checkRateLimits`
(§3.1).

## 5. Testing philosophy and coverage

The SMTP sender is tested against a **real local Mailpit** (`t.Skip`-ing
when it isn't reachable) as well as a fake `SMTPClient`; `-race` is
mandatory. The Resend sender is tested against a local `httptest` fake of
its API, plus one live test that runs only with `GRPOP_TEST_RESEND_API_KEY`
set. The Meta Cloud API client is the one documented exception
(§3.7): a fake client for the sender's wiring, an `httptest.Server` for the
real HTTP client. `make coverage-check` gates the package at 95%.
