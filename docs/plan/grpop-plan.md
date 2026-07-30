# grpop — Scope & Implementation Plan

**Status:** proposed — pre-Stage-0 design plan, not yet implemented. `grpop` is currently an empty repo (`~/Dev/gourdian25/grpop`, only `.bark.toml`/`bark.txt` present, no `go.mod`, no commits).

**Repo path:** `~/Dev/gourdian25/grpop`, module `github.com/gourdian25/grpop`.

**What grpop is:** a send-only, multi-channel **transactional-messaging delivery** library for **email and WhatsApp**. It is explicitly not an email-retrieval/POP3 library (despite the name) and not a push-notification library (that is the sibling `grnoti`'s job — `grpop` was named specifically to avoid that collision). It has no concept of "user" or "preferences" — delivery, not identity. The recipient address/number is supplied per-send by the caller.

**Revision note (this update, supersedes the original draft):** SMS has been dropped from scope entirely, and the whole design has been re-optimized for **minimum third-party dependencies**. Two direct consequences, worth reading before the rest of this doc:

1. **The WhatsApp vendor recommendation flips.** The original draft picked Twilio's Content API first, justified almost entirely by "Twilio is already the SMS vendor, WhatsApp comes free." With SMS gone, that reasoning evaporates. **Meta's WhatsApp Business Cloud API, direct, is now the only v1 implementation** — it has no official Go SDK regardless, so a hand-rolled client was always required for it; Twilio/Gupshup are just BSPs (Business Service Providers) sitting in front of that same Graph API with markup on top, and there is no longer any reason to prefer that indirection.
2. **Email drops every vendor-SDK candidate** (AWS SES, SendGrid) in favor of a **stdlib-only `net/smtp`-based sender** that talks to any SMTP-speaking provider. The vendor becomes an operator-side config value (host, port, credentials) at deploy time, not a `grpop`-side Go dependency at all — SES, SendGrid, Postmark, Mailgun, and a bare in-house relay all expose SMTP submission for exactly this kind of vendor-neutral integration.

**Net effect: `grpop`'s dispatch layer needs zero third-party Go dependencies.** The only third-party imports anywhere in the module are for its own storage/rate-limiting backends (`pgx/v5`, `go-redis/v9`, optionally `mongo-driver`) and `grcache`/`grevents`'s own lightweight root-interface packages — nothing vendor-SDK-shaped at all. This is stated as a real design win, not incidental: it also means email dispatch can, for the first time in this ecosystem, be tested against a **real local test double** (a Mailpit/MailHog SMTP server in Docker) rather than joining FCM/SES/Twilio's "no local emulator, test against a fake" club — only WhatsApp (Meta) remains in that club, since the Graph API has no local emulator.

**No message broker/queue sits between a caller and the vendor in v1 — every send is direct and synchronous.** See §4.4 and §9 item 4 for the full reasoning and what the extension point looks like if that changes later.

**Package shape: single flat package, no subpackages** — see §3.

---

## 0. Research method

This plan was produced by:

1. Reading `~/Dev/gourdian25/CONVENTIONS.md` in full (ecosystem-wide conventions confirmed 2026-07-10/2026-07-23) rather than trusting any single repo's own doc comments about the ecosystem.
2. Reading `grnoti` (the most recently built sibling, tagged `v0.1.0`-in-progress) directly on disk, file by file: `logger.go`, `interfaces.go`, `cache.idempotency.go`, `dlq.postgres.go`, `dispatcher.fcm.go`, `templateengine.go`, `ratelimiter.go`, `ratelimiter.redis.go`, `postgres.go`, `errors.go`, `docs.go`, and `docs/plan/grnoti-plan.md` (1064 lines) for its structure, rigor, and the exact section skeleton this document mirrors.
3. Reading `grcache/cache.go`, `graudit/postgres.go` (`pg_advisory_xact_lock`/`pg_advisory_lock` locking techniques), `grevents/bus.go`, and `grpolicy/docs.go` directly on disk.
4. Real web research (WebSearch/WebFetch) for the two mandatory research questions (§1.1 build-vs-adopt, §1.2 per-channel Go library survey) in the original draft of this plan — every claim is sourced, not guessed, and two of the citations (Novu's GitHub star count/license/push date, Twilio-go's latest release tag/date) were independently spot-checked live against the GitHub API and matched exactly.
5. **This revision pass**: re-derived §1.2, §3, §4, §6–§10 from scratch against a changed brief — SMS cut entirely, both remaining channels' vendor choice re-optimized for "as few dependencies as possible" rather than "fastest path to a working v1 per channel," and an explicit answer recorded on whether a message broker sits between `Service` and the vendor call (§4.4: it doesn't, in v1). The build-vs-adopt conclusion in §1.1 is unaffected by this change (it never depended on which channels were in scope) and was not re-litigated.

**Lesson carried over from `grnoti-plan.md`'s own §0, applied again here:** verify sibling state against the actual filesystem, not against a doc describing it — including this plan's *own* earlier draft. The Twilio-for-WhatsApp recommendation in the original draft was correct *given its own premises* (SMS in scope); re-deriving it after those premises changed, rather than patching the old conclusion in place, is what caught that it needed to flip rather than just lose a sentence.

---

## 1. Mandatory research questions — answered

### 1.1 Build vs. adopt: does an existing platform already solve this? ("Step 0")

**Unchanged from the original draft — this conclusion never depended on which channels are in scope.**

**Conclusion — build, don't adopt.** Novu is the one candidate that could technically satisfy self-hosting (~39.4k GitHub stars, actively pushed, MIT-cored/open-core license — both independently spot-checked live against the GitHub API), but its official Go SDK (`novuhq/novu-go`) is a Speakeasy-generated, auto-generated-from-OpenAPI client whose latest tag (`v0.3.0`) is over a year stale relative to the core repo's own activity — Novu is fundamentally a Node/TypeScript-first platform with a thin generated Go client bolted on, not a Go-native library. Adopting it means running an entire additional Node-based service (its own Postgres/Redis/MongoDB dependencies, a full workflow-builder concept, a UI) as infrastructure, for a Go backend whose actual requirement is a handful of call sites needing "send a token via email/WhatsApp, get back a delivery handle." Courier, Knock, and OneSignal are SaaS-only with no self-hosting story at all, immediately disqualified by skipp's self-hosted constraint. Building a thin, Go-native, self-hosted-by-construction library — now with an even smaller footprint than the original draft assumed, per the revision note above — is less overall system complexity than operating Novu, and stays consistent with every other `gourdian25` sibling's shape (a Go interface + pluggable backends, no separate service to run).

*(Full sourced findings — Courier/Knock/OneSignal/AWS End User Messaging/SendGrid-Twilio-multichannel comparisons, with citations — are unchanged from the original draft; omitted here only to avoid duplicating unaffected content.)*

### 1.2 Per-channel Go library survey ("Step 0.5") — re-derived for a 2-channel, minimum-dependency brief

SMS rows removed entirely (out of scope). Email and WhatsApp re-evaluated against "fewest third-party dependencies," not "fastest first vendor integration."

| Channel | Approach | Go dependency | Third-party? | Notes |
|---|---|---|---|---|
| Email | **stdlib `net/smtp` + `net/mail`/`mime/multipart`** | none — stdlib only | **No** | Talks to any SMTP-speaking provider: AWS SES's SMTP interface, SendGrid's SMTP relay, Postmark's SMTP endpoint, Mailgun, or a bare in-house relay. The vendor becomes a config value (host/port/credentials + optional STARTTLS/auth), not a Go import. Loses vendor-side deliverability tooling (bounce/complaint webhooks, open/click tracking, suppression-list management) — real capability cut, stated plainly, not silently absorbed. |
| Email (rejected for v1) | AWS SES via `aws-sdk-go-v2/service/sesv2` | official, actively maintained | Yes — and a heavy transitive tree (the AWS SDK v2 monorepo's shared runtime/config/credentials packages) | Rejected specifically on dependency-weight grounds now that "fewest dependencies" is the stated goal, not on capability grounds — SES itself is a fine vendor, reachable anyway via its own SMTP interface under the stdlib path above. |
| Email (rejected for v1) | SendGrid via `sendgrid-go` | official, actively maintained | Yes | Same reasoning — SendGrid is also reachable via SMTP relay under the stdlib path, so the dedicated SDK buys deliverability-analytics features at the cost of a dependency, not reachability. |
| WhatsApp | **Meta WhatsApp Business Cloud API, direct** | grpop's own hand-rolled HTTP client (`net/http` + `encoding/json`) | **No** | No official Go SDK exists from Meta at all (confirmed — Meta's own beta SDK is TypeScript-only), so a thin client was always required regardless of channel scope. This is now the only WhatsApp implementation, not "v1 of two." |
| WhatsApp (rejected for v1) | Twilio (Content API) | `github.com/twilio/twilio-go`, official, actively maintained | Yes | The original draft's "first implementation" pick — justified entirely by SMS-channel reuse, which no longer applies. Twilio remains a real, credible future option (removes the need to manage Meta's own Graph API request-signing/error taxonomy directly, at the cost of BSP markup and a real dependency) — kept as an explicit future extension point in §9, not built now. |
| WhatsApp (rejected for v1) | Gupshup | unofficial community package, low visibility | Yes (unofficial) | Same reasoning as Twilio — a real India-native BSP option, deferred, not designed away (`WhatsAppSender` stays vendor-agnostic). |

**Testing implication worth calling out explicitly:** because email now goes over plain SMTP, `dispatcher.email.smtp.go` can be tested against a **real local SMTP server** (e.g. Mailpit or MailHog, run the same way as the existing Postgres/Redis/Mongo/Kafka Docker containers) rather than only a hand-rolled fake — the first channel in this design that fully matches the ecosystem's "real local services, not mocks" testing philosophy rather than joining FCM/SES/Twilio's documented fake-client exception. Only the Meta WhatsApp client remains in that exception (§4.3, §7).

Sources for the underlying vendor/SDK facts (unchanged from the original per-channel survey, still applicable): [aws-sdk-go-v2/service/sesv2](https://github.com/aws/aws-sdk-go-v2/releases), [sendgrid-go](https://github.com/sendgrid/sendgrid-go), [twilio-go](https://github.com/twilio/twilio-go), [Meta WhatsApp Cloud API docs](https://developers.facebook.com/docs/whatsapp/cloud-api/).

### 1.3 Should `IdempotencyStore`/rate limiter build on `grcache.Cache`?

**Unchanged — `grcache.Cache` (`Get/Set/Delete/Exists/InvalidateTag/Stats/Close`) backs `IdempotencyStore` via one generic ~40-line adapter; the distributed `RateLimiter` still needs its own raw `*redis.Client` for an atomic multi-key refill-and-consume Lua script that `Get`/`Set` cannot provide without a read-modify-write race.** Neither conclusion depended on channel count.

### 1.4 grevents — lifecycle event publishing?

**Unchanged.** `grpop` reserves and publishes `"message.sent"`/`"message.failed"` topics (channel + send ID in `Event.Payload`) through an injected, nil-safe `grevents.Bus`; best-effort only, never blocks or fails the actual send. **This is the only "broker-shaped" thing in `grpop` at all, and it is one-directional and observability-only — see §4.4 for why it isn't a substitute for an actual message-broker-mediated send path.**

### 1.5 graudit precedent — Postgres locking technique

**Unchanged.** `ClaimRetryableEvents` uses `SELECT ... FOR UPDATE SKIP LOCKED` inside a single `UPDATE ... RETURNING *` statement (N workers claiming disjoint rows concurrently) — not `graudit`'s `pg_advisory_xact_lock` (a single global serialization point, wrong shape here). The unrelated, session-scoped `pg_advisory_lock` is still reused for schema-migration DDL serialization at connect time.

### 1.6 grpolicy — any fit?

**Unchanged. No.** `grpop` has no preferences/opt-out/quiet-hours/policy-evaluation concept in scope.

### 1.7 gourdiantoken precedent

**Unchanged.** Sentinel-error style, `sync.Once`+`atomic.Bool`-guarded idempotent `Close()`, flat single-package layout, and `New<Thing>With<Backend>(...)`-style constructor naming are all adopted directly.

---

## 2. Concrete real-world consumer requirements

These four call sites in `skipp.app.erp.golang.backend` remain the ground truth this design is checked against — stated plainly to ground the design, not to over-fit `grpop`'s public API to them. None of the four need `grpop` to own a "user" or "preferences" concept; each just needs: given a token/link and a recipient address/number, go from `Send(ctx, ...)` to a delivery-status handle in one call.

1. **`auth.ForgotPassword`** — issues a `gourdiantoken` verification token keyed to an email address already supplied by the request. Needs email delivery of a reset link/code today; **WhatsApp** fallback is a plausible future enhancement at the same call site, not required for v1. (SMS fallback, mentioned in the original draft, is no longer in scope.)
2. **`provider.IssueInvite`** — one of three call sites sharing one `platform.Invites` primitive (`Issue`/`Consume`/`Peek`), generating an opaque bearer token whose SHA-256 hash alone is persisted.
3. **`admin.IssueInvite`** — same `platform.Invites` primitive, different issuing role.
4. **A tenant's first-admin invite** — same `platform.Invites` primitive again. No recipient field exists anywhere in `platform.Invites` today — whichever feature issues the invite must supply the recipient address/number itself when calling `grpop`; `grpop` never looks it up.

Today, none of these four have any delivery mechanism at all — they return the raw secret in the API response body. That is the gap `grpop` closes. All four are synchronous request/response flows: an HTTP handler calls `grpop`, and the handler's own response depends on (or at least logs) the outcome — this is the concrete reason §4.4 keeps sends direct rather than broker-mediated in v1.

---

## 3. Package layout

**Decision: single flat package (`package grpop`), no subpackages.** With SMS gone and both remaining channels going through zero-third-party-dependency paths (stdlib SMTP, a hand-rolled Meta HTTP client), the traditional argument *against* a flat layout — "importing this pulls in every vendor SDK regardless of which one a consumer uses" — barely applies to the dispatch layer at all anymore. The only remaining multi-backend surface is storage (`Postgres`/`Mongo` for the DLQ, `Redis` for the distributed rate limiter), a smaller and lower-stakes version of the same tradeoff `grnoti`/`gourdiantoken` already accepted. Flat is, if anything, *more* justified now than in the original draft, not less — kept for ecosystem consistency at a cost that is now close to negligible.

**File-naming convention** — `<concern>.<backend>.go` for storage/rate-limiting (matching `grnoti` exactly), `dispatcher.<channel>.<vendor>.go` for the two dispatch files (kept from the original draft's three-segment convention, now with only one vendor per channel instead of up to three).

```
grpop/
├── interfaces.go              # EmailSender, WhatsAppSender, Service, EmailTemplateEngine,
│                               # IdempotencyStore, DLQHandler, RateLimiter, CircuitBreaker,
│                               # TemplateValidator, Metrics
├── types.go                    # EmailMessage, WhatsAppMessage, SendResult, SendStatus,
│                               # SendOptions, Channel, DLQMessage, DLQEvent, DLQStatus,
│                               # DLQRetryAttempt, RateLimiterStats, CircuitBreakerStats
├── errors.go                    # sentinels, "grpop: " prefix; construction-time validation uses
│                               # "grpop/<component>: " sub-prefix
├── logger.go                     # Logger interface + NopLogger/OrNop — verbatim grnoti shape
├── docs.go                        # godoc only: Package shape + Precise non-aspirational claims (§7)
├── service.go                      # Service orchestrator: idempotency check → rate-limit gate →
│                                   # channel dispatch → DLQ publish on exhausted failure →
│                                   # best-effort grevents publish. No broker in the middle — see §4.4.
├── retrystrategy.go                 # Full-Jitter backoff, stdlib-only, shared inline-retry policy
├── circuitbreaker.go                 # stdlib-only, verbatim grnoti shape, one instance per dispatcher
├── payloadvalidator.go                # per-channel size/shape checks before a vendor call is attempted
├── templateengine.email.go             # html/template-based EmailTemplateEngine (§4.8)
├── templatevalidator.whatsapp.go        # WhatsApp template-approval pre-check (§4.10), calls Meta's
│                                       # template-listing endpoint, small internal TTL cache
├── dryrun.go                             # DryRunEmailSender / DryRunWhatsAppSender (§4.11)
├── cache.idempotency.go                  # grcache.Cache-backed IdempotencyStore adapter (§1.3)
├── ratelimiter.go                         # local per-process, per-(channel,recipient) bounded token
│                                         # bucket (default/dev)
├── ratelimiter.redis.go                    # distributed, Lua-scripted, two-tier (per-channel +
│                                          # per-recipient) token bucket
├── postgres.go                              # connectPostgres shared helper, schema-ensure +
│                                            # pg_advisory_lock (mirrors grnoti/postgres.go, §1.5)
├── dlq.postgres.go                            # DLQHandler, primary (FOR UPDATE SKIP LOCKED)
├── dlq.mongo.go                                # DLQHandler, alt (findOneAndUpdate + $inc)
├── dispatcher.email.smtp.go                     # EmailSender via stdlib net/smtp — zero third-party
│                                               # deps, works against any SMTP-speaking vendor (§1.2)
├── dispatcher.whatsapp.metacloud.go              # WhatsAppSender via Meta Graph API direct — grpop's
│                                                # own hand-rolled HTTP client, only implementation (§1.2)
├── memory.go                                       # in-memory DLQHandler + in-memory fake
│                                                  # Email/WhatsAppSender for tests/dev, real
│                                                  # sync.RWMutex where mutable state exists
├── internal/postgresdb/                              # sqlc-generated Postgres query code (internal/,
│                                                     # not a public subpackage)
└── example/                                            # runnable demo, package main
```

**The dependency footprint, stated plainly:** third-party imports across the entire module are now limited to `pgx/v5`, `go.mongodb.org/mongo-driver` (v1.x, alt DLQ backend only), `redis/go-redis/v9` (distributed rate limiter only), and `grcache`/`grevents`'s own root interface packages. **Zero vendor-messaging SDKs, and zero message-broker client library** (no Kafka/NATS/RabbitMQ dependency either — see §4.4). A consumer using only the in-memory/local backends (e.g. for local dev or a single-instance deployment content with a local rate limit) pulls in none of even those.

**Testing:** the exact in-package `contract_*_test.go` pattern already used by `grnoti`/`grcache`/`graudit`/`grpolicy` (§7) for `IdempotencyStore`, `DLQHandler`, `RateLimiter`. `dispatcher.email.smtp.go` is tested against a **real local Mailpit/MailHog container** (new — the first vendor-facing dispatcher in this ecosystem's history that isn't a documented fake-only exception). `dispatcher.whatsapp.metacloud.go` remains the one deliberate exception (§4.3): no local emulator exists for Meta's Graph API, so its own batching/retry/error-classification logic is unit-tested against a fake `WhatsAppCloudAPIClient`.

---

## 4. Interface & type surface

### 4.1 Core domain types

```go
// Channel identifies which delivery channel a message/send/rate-limit
// bucket applies to.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelWhatsApp Channel = "whatsapp"
)

// EmailMessage is a freeform-body email. Either set TemplateName (and
// TemplateData) to render Subject/HTMLBody/TextBody via a registered
// EmailTemplateEngine template, or set Subject/HTMLBody/TextBody directly —
// not both; a message with TemplateName set ignores any literal
// Subject/HTMLBody/TextBody fields (see ErrTemplateAndLiteralBodyBothSet).
type EmailMessage struct {
	To           []string // one or more recipient addresses; required, len >= 1
	From         string   // optional; empty uses the dispatcher's configured default sender
	ReplyTo      string   // optional
	Subject      string
	HTMLBody     string
	TextBody     string // optional plain-text alternative part
	TemplateName string
	TemplateData map[string]any
}

// WhatsAppMessage is structurally different from EmailMessage on purpose:
// WhatsApp requires a pre-approved message template for anything sent
// outside a user-initiated 24-hour session window, which every one of
// grpop's ERP use cases falls into (all system-initiated, none a reply to
// a live user session). There is deliberately no freeform Body field.
type WhatsAppMessage struct {
	To                string            // E.164 phone number, required
	TemplateName      string            // required — the Meta-approved template's name
	TemplateVariables map[string]string // stringified positional keys ("1","2","3", in order),
	                                    // per Meta Cloud API's template-parameter convention
	LanguageCode      string            // e.g. "en_US", required, must match the approved
	                                    // template's registered language
}

// SendStatus is the result of a vendor call, not a delivery-receipt status.
// See docs.go's "precise, non-aspirational claims" section.
type SendStatus string

const (
	SendStatusSent   SendStatus = "sent"   // the vendor accepted the request
	SendStatusFailed SendStatus = "failed" // the vendor rejected the request, or the call itself errored
)

// SendResult is the delivery-status handle every Service.SendX call returns.
type SendResult struct {
	Channel           Channel
	ProviderMessageID string            // vendor's own message/tracking ID (Meta's wamid, or the
	                                    // generated SMTP Message-ID); empty if Status is SendStatusFailed
	Status            SendStatus
	SentAt            time.Time
	Raw               map[string]string // optional vendor-specific diagnostic fields — logging only
}

// SendOptions carries per-send cross-cutting behavior.
type SendOptions struct {
	IdempotencyKey string        // caller-supplied; empty disables the idempotency check — see §9
	IdempotencyTTL time.Duration // 0 uses ServiceConfig's configured default
	SkipRateLimit  bool          // escape hatch, e.g. an admin-triggered manual resend
}
```

### 4.2 Sender interfaces (per-channel, not unified)

Kept as **two separate interfaces**, not one unified `Sender` — `WhatsAppMessage`'s shape is not compatible with a single `Send(ctx, msg Message)` without losing the compile-time template-vs-freeform-body distinction that is the whole point of §4.1.

```go
type EmailSender interface {
	Send(ctx context.Context, msg EmailMessage) (SendResult, error)
	Close() error
}

type WhatsAppSender interface {
	Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error)
	Close() error
}
```

### 4.3 Vendor-narrow interfaces

```go
// SMTPDialer/SMTPClient are the subset of net/smtp's dial/auth/send flow the
// email dispatcher actually uses — narrow enough to fake in unit tests, and
// concrete enough that a real implementation is just a thin wrapper over
// net/smtp.Dial + smtp.Client. Unlike a vendor-SDK-shaped narrow interface,
// this one's real backend can ALSO be exercised against a real local
// Mailpit/MailHog SMTP server in integration tests (§3, §7) — it is a
// fakeability seam for unit tests, not a documented "no real backend
// available" exception.
type SMTPDialer interface {
	Dial(addr string) (SMTPClient, error)
}
type SMTPClient interface {
	Auth(a smtp.Auth) error
	Mail(from string) error
	Rcpt(to string) error
	Data() (io.WriteCloser, error)
	Close() error
}

// WhatsAppCloudAPIClient is grpop's own thin interface over Meta's Graph
// API messages endpoint — no official Go SDK exists for this (§1.2), so
// this is grpop's own hand-rolled HTTP client, kept narrow for the same
// fakeability reason. This is the one deliberate exception to grpop's
// real-services testing policy — Meta's Graph API has no local emulator.
type WhatsAppCloudAPIClient interface {
	SendTemplateMessage(ctx context.Context, req WhatsAppCloudAPIRequest) (WhatsAppCloudAPIResponse, error)
	GetApprovedTemplates(ctx context.Context) ([]WhatsAppTemplateInfo, error) // backs TemplateValidator, §4.10
}
```

Vendor-specific error classification stays internal to each dispatcher's own file, never leaking into `interfaces.go`. Cross-cutting concerns are injected via a `Deps` struct per dispatcher:

```go
type SMTPDispatcherDeps struct {
	Addr           string   // e.g. "email-smtp.us-east-1.amazonaws.com:587" — SES/SendGrid/Postmark/
	                        // Mailgun/a bare relay are all just different values here, never a
	                        // different Go dependency
	Auth           smtp.Auth
	DefaultFrom    string
	Dialer         SMTPDialer     // optional; nil uses a real net/smtp-backed dialer
	RateLimiter    RateLimiter    // optional
	CircuitBreaker CircuitBreaker // optional
	Metrics        Metrics        // optional
	Logger         Logger         // optional, OrNop'd at construction
}
```

### 4.4 `Service` — the orchestrator, and why there is no broker in the middle

```go
// Service is the top-level orchestrator: checks idempotency, gates through
// the rate limiter, dispatches via the channel-appropriate Sender directly
// (no queue/broker in between — see below), publishes to DLQHandler on
// exhausted inline-retry failure, and best-effort-publishes a lifecycle
// event via grevents. Every SendX method is synchronous on the calling
// goroutine.
type Service interface {
	SendEmail(ctx context.Context, msg EmailMessage, opts SendOptions) (SendResult, error)
	SendWhatsApp(ctx context.Context, msg WhatsAppMessage, opts SendOptions) (SendResult, error)
	Close() error
}
```

**Is there a message broker in between, or does `grpop` send directly? Direct, in v1 — no broker.** `SendEmail`/`SendWhatsApp` call the vendor (SMTP relay / Meta Graph API) synchronously on the caller's own goroutine, with an inline Full-Jitter retry (`retrystrategy.go`) for transient per-call failures, and only fall through to `DLQHandler.PublishToDLQ` once those inline retries are exhausted. There is no producer/consumer split, no queue a message sits in between "caller asked for a send" and "vendor call happened." This matches all four ERP use cases (§2) exactly: each is a synchronous HTTP request/response flow where the handler wants (or at least logs) the outcome of the send in the same request, not "enqueue and move on."

**What a broker *could* be used for, if this changes:** the natural extension point is exactly the one `grnoti` already proved — an `EventConsumer`-shaped adapter (`Start(ctx, handler func(context.Context, Event) error) error`) that calls `Service.SendEmail`/`SendWhatsApp` as its handler, composing purely through matching function signatures with zero import coupling into `service.go` itself. Candidate brokers for that adapter, if/when it's built (none chosen or implemented in v1 — see §9 item 4):
- **Kafka** — the most directly relevant option since `skipp.app.erp.golang.backend` already has a live Kafka connection today that nothing currently publishes to; `grnoti`'s own `consumer.kafka.go` (built on `github.com/IBM/sarama`) is the exact, already-proven pattern to copy if this is wired up later.
- **Redis Streams** — lower operational overhead than Kafka if the only reason for a queue is "smooth out a burst of invite-sends," and `grpop` already depends on Redis for the distributed rate limiter, so no new infrastructure would be needed, only a new client usage.
- **grpop's own in-process `WorkerPool`** (no external broker at all) — the lightest option: a bounded in-memory queue inside the process itself, for backpressure/batching without introducing any external system. This is what `grnoti`'s `Config.EnableBackpressure` + internal `*WorkerPool` gives it, and is the natural first step before reaching for an actual external broker.
None of these are built in v1. This is stated as a deliberate, reviewable scope cut (§9 item 4), not an oversight — revisit if/when a real batching, backpressure, or fire-and-forget-from-a-non-HTTP-context need shows up.

### 4.5 `IdempotencyStore` — grcache-backed, unchanged from the original draft

```go
type IdempotencyStore interface {
	IsProcessed(ctx context.Context, idempotencyKey string) (bool, error)
	MarkProcessed(ctx context.Context, idempotencyKey string, ttl time.Duration) error
	Close() error
}
```

Implementation is the single `NewCacheIdempotencyStore(cache grcache.Cache) IdempotencyStore` adapter (§1.3).

### 4.6 `DLQHandler` — atomic-claim, channel-tagged (SMS field removed)

```go
// DLQMessage is a channel-tagged envelope — exactly one of Email/WhatsApp
// is non-nil, matching Channel.
type DLQMessage struct {
	Channel  Channel
	Email    *EmailMessage
	WhatsApp *WhatsAppMessage
}

type DLQHandler interface {
	PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error
	ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error)
	MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error
	GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error)
	PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error)
	Close() error
}
```

No background reclaim loop inside `grpop` itself — `ClaimRetryableEvents` is a primitive a consuming application's own periodic worker/cron calls. **This is the closest thing to a "queue" that exists in v1** — a durable, pull-based retry table in Postgres/Mongo, not a broker: nothing pushes a claimed event anywhere, a caller's own process polls for work.

### 4.7 `RateLimiter` — per-recipient AND per-channel (unchanged design, now over 2 channels)

```go
type RateLimiter interface {
	Allow(ctx context.Context, channel Channel, recipient string) (bool, error)
	Wait(ctx context.Context, channel Channel, recipient string) error
	GetStats(ctx context.Context, channel Channel, recipient string) (RateLimiterStats, error)
}
```

Two backends, unchanged reasoning: local (per-process, bounded-LRU per-recipient map) and Redis (single Lua script, two `KEYS[]` entries per call — one per-channel, one per-(channel,recipient)).

### 4.8 `EmailTemplateEngine` — unchanged; SMS template engine removed entirely

```go
// EmailTemplateEngine renders EmailMessage.Subject/HTMLBody/TextBody. Uses
// html/template (NOT text/template) for HTMLBody specifically, because
// HTMLBody renders into a real HTML document in the recipient's mail
// client. Subject and TextBody use text/template. Compiled once at
// RegisterTemplate time, not re-parsed per render.
type EmailTemplateEngine interface {
	RegisterTemplate(name string, tmpl EmailTemplate) error
	Render(name string, data map[string]any) (subject, htmlBody, textBody string, err error)
}
```

**WhatsApp still has no `TemplateEngine` involvement at all** — `WhatsAppMessage.TemplateVariables` are substituted by Meta against its own pre-approved template content, not rendered by `grpop`.

### 4.9 Logger, Close, errors — unchanged, verbatim ecosystem shape

```go
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}
func NopLogger() Logger { return noopLogger{} }
func OrNop(l Logger) Logger { if l == nil { return NopLogger() }; return l }
```

`Close()`: `sync.Once` + `atomic.Bool` on every backend-holding type. Sentinels: `"grpop: message"` / `"grpop/<component>: ..."`. A backend-native error never leaks through a `grpop` interface unwrapped.

### 4.10 `TemplateValidator` — new: WhatsApp template-approval pre-check

Meta requires every WhatsApp template to go through an approval process before it can be used in a live send; a send against a not-yet-approved (or rejected, or since-deleted) template fails at the vendor. This interface lets a caller (or the dispatcher itself, if wired in) check first, turning a live-send failure into a cheap, cached pre-check:

```go
// TemplateValidator confirms a WhatsApp template name+language is currently
// approved on Meta's side, before a live send is attempted against it.
// Backed by Meta's own template-listing endpoint, with a short internal TTL
// cache (templates change rarely, so this is not re-fetched on every call).
type TemplateValidator interface {
	IsApproved(ctx context.Context, templateName, languageCode string) (bool, error)
	// Refresh forces an immediate re-fetch of the approved-template list,
	// bypassing the internal cache — e.g. call this right after registering
	// a new template with Meta, instead of waiting out the TTL.
	Refresh(ctx context.Context) error
}
```

`NewMetaTemplateValidator(deps MetaTemplateValidatorDeps) TemplateValidator` implements this over `WhatsAppCloudAPIClient.GetApprovedTemplates` (§4.3). **Design decision, flagged explicitly in §9**: this is wired into `dispatcher.whatsapp.metacloud.go` as an *optional* `Deps.TemplateValidator` field — if set, `Send` consults it (cheap, cache-backed) and returns `ErrWhatsAppTemplateNotApproved` early instead of making a doomed vendor call; if unset, `Send` behaves exactly as before, relying on Meta's own rejection. It is advisory, not mandatory, and not a scheduled background job inside `grpop` — the cache is only ever refreshed by a `Send`-time check or an explicit `Refresh` call.

### 4.11 `DryRunSender` — new: logging-only senders for staging

```go
// NewDryRunEmailSender returns an EmailSender that never contacts a real
// vendor: it logs the fully-resolved message (post-template-render) at
// Info level and returns a synthetic SendResult{Status: SendStatusSent,
// ProviderMessageID: "dryrun-<generated-id>"}. Distinct from memory.go's
// in-memory test fake — that one exists for contract/unit tests and
// records sends for assertions; this one exists for a staging/pre-prod
// deployment that should never actually deliver mail/WhatsApp messages
// but should otherwise exercise the exact same Service pipeline
// (idempotency, rate limiting; DLQ-on-failure never triggers since dry-run
// never fails).
func NewDryRunEmailSender(logger Logger) EmailSender
func NewDryRunWhatsAppSender(logger Logger) WhatsAppSender
```

A construction-time swap (pick `NewDryRunEmailSender` instead of `NewSMTPDispatcher` when building `ServiceDeps` in a staging environment), not a runtime toggle inside the real dispatchers — kept deliberately simple, per §9.

---

## 5. Polyglot persistence

| Store | Backend | Notes |
|---|---|---|
| `IdempotencyStore` | **Redis or Mongo, via `grcache`** | one generic adapter, backend choice is "which `grcache.Cache` the caller constructs" |
| `DLQHandler` | **PostgreSQL** primary (`FOR UPDATE SKIP LOCKED`) | **Mongo** alt (`findOneAndUpdate` + `$inc`) — this is a durable retry table, not a message broker (§4.4/§4.6) |
| `RateLimiter` | **Redis**-backed distributed two-tier token bucket, raw client | local in-memory, bounded-LRU variant stays default/dev |
| `CircuitBreaker` | in-memory, per-instance, per-dispatcher | not centralized, avoids synchronized thundering-herd retry across replicas |
| Lifecycle events (`message.sent`/`message.failed`) | **`grevents.Bus`**, optional/nil-safe | best-effort only, observability only — not a send path (§4.4) |
| Email delivery | **SMTP** to any vendor-configured relay | no vendor SDK; testable against a real local Mailpit/MailHog container (§3) |
| WhatsApp delivery | **Meta Graph API**, direct HTTP | the one remaining deliberate exception to real-services testing (§4.3) — no local emulator |

**No message broker/queue anywhere in v1** (§4.4) — every store above is either a durable-state table (DLQ, idempotency) or a rate-limit counter, never a transport mechanism a send passes through. **No separate delivery-status/send-log table either** — `DLQHandler`'s table only holds failed sends awaiting retry, not a full audit trail (see §9 for the "is this graudit's job instead" framing, unchanged).

---

## 6. Postgres schema

Unchanged mechanism (additive-only `CREATE ... IF NOT EXISTS`, `pg_advisory_lock`-serialized, `SkipSchemaEnsure` opt-out); `channel` column's value set narrows from three to two.

```sql
CREATE TABLE IF NOT EXISTS grpop_dlq (
    send_id VARCHAR(255) PRIMARY KEY,      -- caller's IdempotencyKey if supplied, else a
                                            -- generated UUID (see §9)
    channel VARCHAR(16) NOT NULL,          -- 'email' | 'whatsapp'
    message_data JSONB NOT NULL,           -- serialized DLQMessage envelope
    failure_reason TEXT NOT NULL DEFAULT '',
    retry_count INT NOT NULL DEFAULT 0,
    max_retries INT NOT NULL,
    first_failure_at TIMESTAMPTZ NOT NULL,
    last_attempt_at TIMESTAMPTZ NOT NULL,
    next_retry_at TIMESTAMPTZ NOT NULL,
    status VARCHAR(32) NOT NULL,           -- string enum, not a Postgres ENUM type
    attempt_history JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_next_retry
    ON grpop_dlq (status, next_retry_at);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_channel_status
    ON grpop_dlq (channel, status);
```

Claim statement (unchanged):

```sql
-- name: ClaimRetryableEvents :many
UPDATE grpop_dlq
SET status = 'retrying', updated_at = $2
WHERE send_id IN (
    SELECT send_id FROM grpop_dlq
    WHERE status = 'pending' AND next_retry_at <= $1
    ORDER BY next_retry_at
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

---

## 7. Ecosystem conventions to match

- `// File: <relative-path>` header on every `.go` file, maintained by `bark`.
- `Logger` interface + `NopLogger()`/`OrNop()`, verbatim `grnoti` shape.
- Sentinel errors: `"grpop: message"` / `"grpop/<component>: ..."` sub-prefix; `errors.Is`-matched, no `IsX(err) bool` helpers.
- `Close()` idempotent via `sync.Once` + `atomic.Bool`.
- `docs.go`: godoc only, "Package shape" + "Precise, non-aspirational claims" sections (mirroring `grnoti/docs.go`) — now including a note that `SendStatusSent` means "SMTP relay/Meta API accepted it," never "arrived in an inbox/on a phone," that `TemplateValidator`'s cache means "approved as of the last check," not a live guarantee, and that **there is no message broker anywhere in this package** — `Service.SendX` is a direct, synchronous vendor call (§4.4).
- Testing: real in-package `contract_*_test.go` files, real local Docker Postgres/Redis/Mongo/**Mailpit**, `t.Skip` when unreachable, `-race` mandatory. **Only** the Meta WhatsApp client is the documented fake-only exception now (§3, §4.3) — a first for a vendor-facing dispatcher in this ecosystem.
- Shared dependency versions: `jackc/pgx/v5`, `go.mongodb.org/mongo-driver` (v1, not v2), `redis/go-redis/v9`, aligned to whatever `grcache`'s go.sum currently pins. **No vendor-messaging-SDK version to track at all**, and no message-broker client library version either — a direct consequence of the dependency-minimization goal.

---

## 8. Implementation stages

**Stage 0 — Repo scaffolding.** `go.mod` (module `github.com/gourdian25/grpop`, Go 1.26.4), `docs.go`, `errors.go`, `logger.go`, `version.go`.

**Stage 1 — Core contracts.** `interfaces.go`, `types.go` — every interface/type in §4 (email + WhatsApp only).

**Stage 2 — Pure in-process logic.** `retrystrategy.go`, `circuitbreaker.go`, `payloadvalidator.go`.

**Stage 3 — `memory.go`: in-memory variants.** In-memory `DLQHandler` and fake `EmailSender`/`WhatsAppSender` for tests/local dev, no live service or vendor account needed.

**Stage 4 — `cache.idempotency.go` + local `RateLimiter`.** The `grcache`-backed adapter (§4.5); `ratelimiter.go`'s local, bounded-LRU, per-(channel,recipient) token bucket (§4.7).

**Stage 5 — `templateengine.email.go`.** `html/template`-based `EmailTemplateEngine` (§4.8).

**Stage 6 — PostgreSQL: `postgres.go` + `dlq.postgres.go`.** Shared connect helper, schema-ensure + advisory lock, sqlc-generated `internal/postgresdb`, the atomic-claim `DLQHandler`.

**Stage 7 — MongoDB: `dlq.mongo.go`.** Alt `DLQHandler` backend.

**Stage 8 — Cross-backend contract tests.** `contract_idempotencystore_test.go`, `contract_dlqhandler_test.go`, `contract_ratelimiter_test.go` — real local Docker Postgres/Redis/Mongo.

**Stage 9 — Redis: distributed `RateLimiter`.** `ratelimiter.redis.go`'s two-tier Lua-scripted bucket.

**Stage 10 — Email dispatcher.** `dispatcher.email.smtp.go`, stdlib `net/smtp`-based. Tested against a real local **Mailpit/MailHog** Docker container — this ecosystem's first vendor-facing dispatcher tested against a real local double rather than a fake.

**Stage 11 — WhatsApp dispatcher.** `dispatcher.whatsapp.metacloud.go`, `grpop`'s own hand-rolled Graph API HTTP client, tested against a fake `WhatsAppCloudAPIClient` (the one remaining deliberate real-services exception).

**Stage 12 — `templatevalidator.whatsapp.go`.** The Meta-backed `TemplateValidator` (§4.10), optionally wired into Stage 11's dispatcher.

**Stage 13 — `dryrun.go`.** `NewDryRunEmailSender`/`NewDryRunWhatsAppSender` (§4.11).

**Stage 14 — `events.go`: `grevents` integration.** Best-effort `message.sent`/`message.failed` publishing, nil-safe.

**Stage 15 — `service.go`: orchestration.** `Service` wired to every prior stage. Direct dispatch, no broker (§4.4).

**Stage 16 — Polish.** `example/` runnable demo, README quickstart (§10), coverage gate, `docs/architecture.md`.

---

## 9. Open decisions for review before Stage 0 starts

Judgment calls, flagged rather than buried:

1. **§1.2/§3**: **WhatsApp goes through Meta's Graph API direct, with no vendor SDK at all.** This is a real cost/capability tradeoff against Twilio/Gupshup: `grpop` itself now owns request-signing, the template-message JSON shape, media handling (if ever added), and Meta's own rate-limit/error taxonomy, rather than delegating that to a maintained SDK. Reversible later (`WhatsAppSender` stays vendor-agnostic) — a Twilio-backed alt implementation remains a legitimate v2 addition if Meta-direct proves too much maintenance burden.
2. **§1.2/§3**: **Email goes through stdlib SMTP only, no vendor HTTP API.** The explicit capability cost: no bounce/complaint webhooks, no open/click tracking, no suppression-list management — `grpop` only ever knows "the SMTP relay accepted it." If deliverability analytics become a real operational need later, a vendor-API-backed `EmailSender` (SES/SendGrid) is an additive alt implementation, not a redesign — `EmailSender`'s interface shape doesn't assume SMTP.
3. **§4.7**: Per-recipient-and-per-channel rate-limiter dimensioning stays in v1 scope (unchanged from the original draft) — the forgot-password-abuse scenario (§2 item 1) is a real, named risk, not hypothetical.
4. **§4.4, new framing**: **No message broker anywhere in v1 — every send is direct and synchronous, no queue in between.** All four ERP use cases (§2) are synchronous request/response flows with no batching/backpressure need yet. If that changes (`skipp.app.erp.golang.backend` has a live Kafka connection today that nothing publishes to — a natural future trigger), the intended extension point is an `EventConsumer`-shaped adapter calling `Service.SendEmail`/`SendWhatsApp` as its handler, exactly the composition `grnoti`'s `consumer.Start(ctx, service.Submit)` already proved — matching function signatures, zero import coupling, no redesign of `Service` itself required. Kafka, Redis Streams, and an in-process `WorkerPool` are named as candidate mechanisms in §4.4 without picking one; that choice is explicitly deferred until there's a concrete need driving it.
5. **§4.1**: Email attachments remain cut from v1 — **reconsidered and re-confirmed as a cut, not silently carried over.** The original draft's reasoning (vendor-API attachment-shape differences between SES/SendGrid) no longer applies now that email is SMTP-only, and a MIME `multipart/mixed` attachment part is genuinely low-effort to add on top of the multipart builder Stage 10 already needs to write. Still cut for v1 because none of the four ERP use cases need one — flagged here specifically so it isn't forgotten as "easy to add whenever it's actually needed," rather than assumed out of reach.
6. **§5**: No durable send-log/delivery-status table beyond the DLQ (unchanged) — a full audit trail of every send is arguably `graudit`'s job, not `grpop`'s.
7. **§4.2**: Two separate `EmailSender`/`WhatsAppSender` interfaces, not one unified `Sender` (unchanged reasoning, now over two channels instead of three).
8. **§4.5/§4.6**: `SendOptions.IdempotencyKey` stays optional, not required; an empty key silently disables the idempotency check and, if the send later needs the DLQ, a random UUID becomes its `sendID` (unchanged from the original draft).
9. **§4.10, new**: `TemplateValidator` is advisory and cache-backed, not a scheduled background refresh job inside `grpop` — a stale cache (Meta approves/rejects a template between `grpop`'s last check and a live send) means the pre-check can be wrong in either direction for up to the cache's TTL. This is accepted because the vendor call itself is still the ultimate source of truth (`Send` doesn't skip actually calling Meta just because the pre-check passed) — the validator only ever short-circuits an *already-known-bad* combination, it can't create a false negative that blocks a send that would have succeeded.
10. **§4.11, new**: `DryRunSender` is a construction-time swap (choose it instead of the real dispatcher when building `ServiceDeps`), not a runtime flag on the real dispatchers — kept this way deliberately to avoid a `if dryRun { ... }` branch living inside otherwise-production dispatch code.
11. **§1.2**: Twilio/Gupshup for WhatsApp, and any vendor-API email path (SES/SendGrid), are **explicitly deferred, not designed away** — both remain additive alt implementations behind the existing vendor-agnostic interfaces whenever a concrete need (BSP relationship, deliverability analytics) makes the added dependency worth it.

---

## 10. How a consuming application wires this in

Described generically — no ERP-repo-specific integration code. Note there is no broker/queue client to configure anywhere in this example (§4.4) — every dependency here is either a storage backend or a vendor connection detail.

```go
cache := grcacheredis.NewCache(grcacheredis.Config{Addr: "localhost:6379"})
idem := grpop.NewCacheIdempotencyStore(cache)

dlq, err := grpop.NewPostgresDLQHandler(grpop.PostgresDLQHandlerConfig{
	PostgresConfig: grpop.PostgresConfig{DSN: dsn},
	MaxRetries:     5,
})

rateLimiter, err := grpop.NewRedisRateLimiter(grpop.RedisRateLimiterConfig{
	Addr: "localhost:6379",
	// per-channel and per-recipient limits configured here, §4.7
})

// Vendor is a config value, not a Go dependency — swap Addr/Auth for
// SES/SendGrid/Postmark/Mailgun/a bare relay without changing any import.
emailSender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
	Addr:        "email-smtp.us-east-1.amazonaws.com:587",
	Auth:        smtp.PlainAuth("", sesSMTPUser, sesSMTPPass, "email-smtp.us-east-1.amazonaws.com"),
	DefaultFrom: "no-reply@skipp.app",
	RateLimiter: rateLimiter,
	Logger:      logger,
})

templateValidator := grpop.NewMetaTemplateValidator(grpop.MetaTemplateValidatorDeps{ /* ... */ })
whatsappSender, err := grpop.NewMetaCloudWhatsAppDispatcher(grpop.MetaCloudDispatcherDeps{
	TemplateValidator: templateValidator, // optional, §4.10
	RateLimiter:       rateLimiter,
	Logger:            logger,
	/* ... */
})

// In a staging environment, swap either sender for its dry-run equivalent
// instead of a real vendor connection:
// emailSender := grpop.NewDryRunEmailSender(logger)

svc, err := grpop.NewService(grpop.ServiceDeps{
	IdempotencyStore: idem,
	DLQHandler:       dlq,
	RateLimiter:      rateLimiter,
	EmailSender:      emailSender,
	WhatsAppSender:   whatsappSender,
	Logger:           logger,
})
```

A call site (e.g. wherever `auth.ForgotPassword` currently returns the raw token) then does, in outline — directly, in the same request, no broker/queue in between:

```go
result, err := svc.SendEmail(ctx, grpop.EmailMessage{
	To:           []string{recipientEmail},
	TemplateName: "password-reset",
	TemplateData: map[string]any{"ResetLink": link},
}, grpop.SendOptions{IdempotencyKey: hashOf(token)})
```

`result.Status`/`result.ProviderMessageID` is the delivery-status handle the call site logs or surfaces; a failed send after inline retries is durably recorded in `grpop_dlq` for a separately-run reclaim worker to retry later — the call site itself never blocks on that retry, but it also never handed the send off to any broker to begin with (§4.4).

---

## 11. Next steps

Stage 0, on approval of this plan.

### Critical files for implementation

- `/Users/varun/Dev/gourdian25/grpop/interfaces.go` (to be created) — the entire public contract (§4); every later stage depends on getting this right first.
- `/Users/varun/Dev/gourdian25/grpop/types.go` (to be created) — `EmailMessage`/`WhatsAppMessage`/`SendResult`/`DLQMessage`, encoding the WhatsApp-template-vs-freeform-body distinction this whole design turns on.
- `/Users/varun/Dev/gourdian25/grpop/dispatcher.email.smtp.go` (to be created) — the zero-dependency stdlib SMTP sender; the one file most different from every prior `gourdian25` vendor-facing dispatcher (testable against a real local double, not a fake).
- `/Users/varun/Dev/gourdian25/grpop/dispatcher.whatsapp.metacloud.go` (to be created) — the hand-rolled Meta Graph API client; the one remaining deliberate fake-only testing exception.
- `/Users/varun/Dev/gourdian25/grpop/dlq.postgres.go` (to be created) — the atomic-claim DLQ, modeled directly on `/Users/varun/Dev/gourdian25/grnoti/dlq.postgres.go`'s `FOR UPDATE SKIP LOCKED` pattern.
- `/Users/varun/Dev/gourdian25/grnoti/docs/plan/grnoti-plan.md` (existing, reference only) — the structural/rigor template this document mirrors.
