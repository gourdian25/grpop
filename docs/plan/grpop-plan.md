# grpop — Scope & Implementation Plan

**Status:** proposed — pre-Stage-0 design plan, not yet implemented. `grpop` is currently an empty repo (`~/Dev/gourdian25/grpop`, only `.bark.toml`/`bark.txt` present, no `go.mod`, no commits).

**Repo path:** `~/Dev/gourdian25/grpop`, module `github.com/gourdian25/grpop`.

**What grpop is:** a send-only, multi-channel **transactional-messaging delivery** library for **email and WhatsApp**. It is explicitly not an email-retrieval/POP3 library (despite the name) and not a push-notification library (that is the sibling `grnoti`'s job — `grpop` was named specifically to avoid that collision). It has no concept of "user" or "preferences" — delivery, not identity. The recipient address/number is supplied per-send by the caller.

**Revision note (this update, supersedes the original draft):** SMS has been dropped from scope entirely, and the whole design has been re-optimized for **minimum third-party dependencies**. Two direct consequences, worth reading before the rest of this doc:

1. **The WhatsApp vendor recommendation flips.** The original draft picked Twilio's Content API first, justified almost entirely by "Twilio is already the SMS vendor, WhatsApp comes free." With SMS gone, that reasoning evaporates. **Meta's WhatsApp Business Cloud API, direct, is now the only v1 implementation** — it has no official Go SDK regardless, so a hand-rolled client was always required for it; Twilio/Gupshup are just BSPs (Business Service Providers) sitting in front of that same Graph API with markup on top, and there is no longer any reason to prefer that indirection.
2. **Email drops every vendor-SDK candidate** (AWS SES, SendGrid) in favor of a **stdlib-only `net/smtp`-based sender** that talks to any SMTP-speaking provider. The vendor becomes an operator-side config value (host, port, credentials) at deploy time, not a `grpop`-side Go dependency at all — SES, SendGrid, Postmark, Mailgun, and a bare in-house relay all expose SMTP submission for exactly this kind of vendor-neutral integration.

**Net effect: `grpop`'s dispatch layer needs zero third-party Go dependencies.** The only third-party imports anywhere in the module are for its own storage/rate-limiting backends (`pgx/v5`, `go-redis/v9`, optionally `mongo-driver`) and `grcache`/`grevents`'s own lightweight root-interface packages — nothing vendor-SDK-shaped at all. This is stated as a real design win, not incidental: it also means email dispatch can, for the first time in this ecosystem, be tested against a **real local test double** (a Mailpit/MailHog SMTP server in Docker) rather than joining FCM/SES/Twilio's "no local emulator, test against a fake" club — only WhatsApp (Meta) remains in that club, since the Graph API has no local emulator.

**No message broker/queue sits between a caller and the vendor in v1 — every send is direct and synchronous.** See §4.4 and §9 item 4 for the full reasoning and what the extension point looks like if that changes later.

**Retry expiry (this update):** `DLQHandler` retries are now bounded by a hard wall-clock deadline (`ExpiresAt`), not just a retry-count ceiling — once that deadline passes, an event stops being retried at all, regardless of how many `MaxRetries` attempts remain unused. See §4.6 for the full design and §9 item 12 for the reasoning: a password-reset link or invite token is itself time-boxed, and retrying a send past that point just delivers a dead link.

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

// EmailTemplate is the raw template content, usable two ways: registered
// once by name (EmailTemplateEngine.RegisterTemplate, compiled once, cheap
// to reuse) or passed inline per-send (EmailMessage.InlineTemplate,
// compiled fresh each call) — same fields, same rendering rules either
// way (§4.8).
//
// SECURITY NOTE, InlineTemplate specifically (§9): the plausible real
// source for an inline template is per-tenant custom branding — i.e.
// content that may trace back to a tenant admin's own input, not a
// trusted grpop operator. html/template auto-escapes DATA values safely,
// but the template STRUCTURE itself is not sandboxed against referencing
// TemplateData fields the caller didn't intend to expose (e.g. an
// internal field accidentally present in the map) — InlineTemplate
// content must come from a trusted admin-configured path, never raw
// end-user input, and RenderInline enforces a size cap
// (EmailTemplateEngineConfig.MaxInlineTemplateBytes, default 64KiB,
// returns ErrInlineTemplateTooLarge) so an oversized/pathological
// template can't cost unbounded parse/render CPU per call — a real cost
// specifically because RenderInline, unlike Render, is never cached.
type EmailTemplate struct {
	SubjectTemplate  string // text/template
	HTMLBodyTemplate string // html/template — see §4.8 for why HTML specifically differs from grnoti
	TextBodyTemplate string // text/template, optional
}

// EmailMessage is an email with exactly one of three mutually-exclusive
// content modes set (see ErrMultipleContentModesSet, a new sentinel this
// revision adds for exactly this validation):
//  1. TemplateName (+ TemplateData) — renders via a template already
//     registered with EmailTemplateEngine.RegisterTemplate.
//  2. InlineTemplate (+ TemplateData) — a caller-supplied custom template,
//     rendered on the fly via EmailTemplateEngine.RenderInline without
//     requiring prior registration. This is the "pass a custom template
//     at send time" path — for content not known ahead of RegisterTemplate
//     time, e.g. per-tenant custom branding on an invite email.
//  3. Literal Subject/HTMLBody/TextBody — no templating at all, used
//     verbatim.
type EmailMessage struct {
	To             string // single recipient address, required — see §9 for why this isn't []string
	From           string // optional; empty uses the dispatcher's configured default sender
	ReplyTo        string // optional
	Subject        string
	HTMLBody       string
	TextBody       string // optional plain-text alternative part
	TemplateName   string
	InlineTemplate *EmailTemplate // new — see the type's doc comment and mode 2 above
	TemplateData   map[string]any
}

// WhatsAppMessage is structurally different from EmailMessage on purpose:
// WhatsApp requires a pre-approved message template for anything sent
// outside a user-initiated 24-hour session window, which every one of
// grpop's ERP use cases falls into (all system-initiated, none a reply to
// a live user session). There is deliberately no freeform Body field.
//
// "Custom template" for WhatsApp means something different from email's
// InlineTemplate (§4.1) — TemplateName is a free string, not restricted to
// a fixed/hardcoded set, so ANY template a caller has had approved by Meta
// (however new, however tenant/feature-specific) can be sent by name with
// no grpop-side registration step at all. What's NOT possible, as a hard
// vendor constraint rather than a grpop design gap: there is no ad-hoc/
// unregistered-with-Meta WhatsApp template — grpop cannot render or invent
// WhatsApp template content itself the way EmailTemplateEngine.RenderInline
// can for email, because Meta's own approval process is the source of
// truth for what content is allowed to go out. Getting a new template
// approved is a Meta-side action a caller takes outside grpop entirely;
// TemplateValidator (§4.10) only checks against whatever is already
// approved, it doesn't submit new ones.
type WhatsAppMessage struct {
	To                string            // E.164 phone number, required
	TemplateName      string            // required — the Meta-approved template's name; any
	                                    // approved name works, nothing hardcoded/enumerated grpop-side
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

	// Duplicate is true if this call short-circuited on
	// IdempotencyStore.IsProcessed (the same IdempotencyKey was already
	// marked processed) rather than performing a new vendor call — new
	// field this revision (§9): a dedup hit was previously silent (no
	// error, no distinguishing signal), which hides a real class of
	// caller bug (the same key reused for two genuinely different
	// message bodies) as "why didn't the second invite email go out."
	// Callers that care can now check this explicitly; Service also logs
	// a Warn and increments Metrics.IncIdempotencyDedupHit on every hit
	// regardless of whether the caller inspects this field.
	Duplicate bool
}

// SendOptions carries per-send cross-cutting behavior.
type SendOptions struct {
	// IdempotencyKey is REQUIRED — Service.SendEmail/SendWhatsApp return
	// ErrIdempotencyKeyRequired if it's empty. Changed from optional in
	// this revision (see §9): grpop's inline retry-on-transient-failure is
	// at-least-once, not exactly-once (docs.go) — a network error after
	// the vendor already accepted the message is indistinguishable from
	// one before, so a retry can double-send. Every one of the four ERP
	// use cases already has a natural key (a hash of the reset/invite
	// token); there is no real caller today with no natural key to supply.
	IdempotencyKey string
	IdempotencyTTL time.Duration // 0 uses ServiceConfig's configured default
	SkipRateLimit  bool          // escape hatch, e.g. an admin-triggered manual resend

	// RetryExpiresAt is the hard wall-clock deadline after which grpop
	// stops retrying a failed send entirely — see §4.6. Zero value uses
	// ServiceConfig.DefaultMaxRetryAge, measured from the first failure,
	// not from this call. Callers who know their own message's real
	// expiry (a password-reset link's TTL, an invite token's expiry)
	// should set this explicitly rather than relying on the library
	// default — see §9 item 12.
	RetryExpiresAt time.Time
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
// SMTPTLSMode controls how dispatcher.email.smtp.go secures its connection
// to Addr. Mandatory-by-default, not implicit — see §9: this replaces
// vendor SDKs (SES/SendGrid) that handled transport security internally,
// so grpop now owns getting this right.
type SMTPTLSMode string

const (
	SMTPTLSStartTLS       SMTPTLSMode = "starttls"        // default; upgrades a plaintext
	                                                      // connection via STARTTLS before AUTH/MAIL
	SMTPTLSImplicit       SMTPTLSMode = "implicit"        // TLS from the first byte (e.g. port 465)
	SMTPTLSInsecureNoTLS  SMTPTLSMode = "insecure_no_tls" // deliberately loud name — plaintext,
	                                                      // intended only for a local Mailpit/MailHog
	                                                      // test container (§3, §7), never production
)

type SMTPDispatcherDeps struct {
	Addr        string // e.g. "email-smtp.us-east-1.amazonaws.com:587" — SES/SendGrid/Postmark/
	                   // Mailgun/a bare relay are all just different values here, never a
	                   // different Go dependency
	Auth        smtp.Auth
	DefaultFrom string

	// TLSMode defaults to SMTPTLSStartTLS if unset (zero value maps to the
	// secure default, not to SMTPTLSInsecureNoTLS). Constructing with
	// Auth set and TLSMode == SMTPTLSInsecureNoTLS is a construction-time
	// error (credentials over plaintext) unless AllowInsecureAuth is also
	// explicitly set — see §9.
	TLSMode           SMTPTLSMode
	AllowInsecureAuth bool // explicit escape hatch for #TLSMode's construction-time guard; not
	                       // expected to be set outside of a deliberately-isolated internal relay

	// ConnectTimeout/SendTimeout bound one Send call end-to-end so a
	// hanging vendor connection can't stall the caller's own request
	// indefinitely (§4.4: every Send is synchronous on the caller's
	// goroutine). Both apply as a ceiling in addition to, never instead
	// of, ctx's own deadline if the caller supplied one — Send uses
	// whichever deadline is sooner. Defaults: ConnectTimeout 5s,
	// SendTimeout 15s (covers the full MAIL/RCPT/DATA sequence, not just
	// the initial dial).
	ConnectTimeout time.Duration
	SendTimeout    time.Duration

	Dialer         SMTPDialer     // optional; nil uses a real net/smtp-backed dialer
	RateLimiter    RateLimiter    // optional
	CircuitBreaker CircuitBreaker // optional
	Metrics        Metrics        // optional, §4.12
	Logger         Logger         // optional, OrNop'd at construction
}

// MetaCloudDispatcherDeps configures dispatcher.whatsapp.metacloud.go —
// this struct was previously only referenced (§10) without being spelled
// out; defined here now.
type MetaCloudDispatcherDeps struct {
	PhoneNumberID string // Meta's phone-number-id path segment for the messages endpoint

	// AccessToken is a long-lived/System User access token. grpop does
	// NOT refresh or rotate this token — token lifecycle (rotation before
	// expiry) is entirely the operator's responsibility. See §9.
	AccessToken string

	// RequestTimeout bounds one Graph API HTTP call, same reasoning as
	// SMTPDispatcherDeps.SendTimeout above — applies in addition to ctx's
	// own deadline, whichever is sooner. Default: 10s.
	RequestTimeout time.Duration

	Client            WhatsAppCloudAPIClient // optional; nil constructs a real net/http-backed one
	TemplateValidator TemplateValidator      // optional, §4.10
	RateLimiter       RateLimiter            // optional
	CircuitBreaker    CircuitBreaker         // optional
	Metrics           Metrics                // optional, §4.12
	Logger            Logger                 // optional, OrNop'd at construction
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

**Delivery guarantee, precisely (new, matching grnoti's own "precise, non-aspirational claims" discipline):** grpop's inline retry is **at-least-once, not exactly-once**. A network error after the vendor has already accepted the message but before its response reaches `grpop` is indistinguishable, from `grpop`'s side, from an error before acceptance — the inline retry (and any later DLQ-driven retry) cannot tell these apart and will attempt the send again either way. This is exactly why `SendOptions.IdempotencyKey` is required, not optional (§4.1, §9): the guarantee against a caller-visible double-send comes entirely from the caller supplying a stable key and `IdempotencyStore` catching the redelivery, not from any property of the send path itself.

`SendEmail`/`SendWhatsApp` return `ErrIdempotencyKeyRequired` immediately, before any rate-limit check or vendor call, if `opts.IdempotencyKey == ""`.

When a send does fall through to the DLQ, `Service` computes the `DLQMessage.ExpiresAt` it publishes with (§4.6) as `opts.RetryExpiresAt` if the caller set one, otherwise `time.Now().Add(cfg.DefaultMaxRetryAge)` — a new `ServiceConfig.DefaultMaxRetryAge` field (proposed default: `24 * time.Hour`, flagged as a judgment call in §9 item 12) that only matters when a caller doesn't supply a more precise deadline of their own. If a caller-supplied `RetryExpiresAt` is already at or before `time.Now()` at the moment of publish (clock skew, or a bug in the caller's own token-TTL computation), `Service` still calls `PublishToDLQ` — the event is durably recorded, then immediately picked up as `DLQStatusExpired` on the next `ClaimRetryableEvents` sweep — but logs a `Warn` first, so this surfaces as "your token TTL is misconfigured," not an unexplained zero-retry DLQ entry someone has to reverse-engineer later (§9).

**Idempotency dedup hits are logged, not silent (new, §9):** when `IdempotencyStore.IsProcessed` returns true for an incoming `IdempotencyKey`, `Service` returns immediately with `SendResult{Duplicate: true, ...}` (no vendor call, no rate-limit consumption) — but first logs a `Warn` (key + channel only, never message content, same discipline as `DryRunSender`'s redaction) and increments `Metrics.IncIdempotencyDedupHit(channel)`. A dedup hit is the expected, correct behavior for a genuine retried request (e.g. a browser resubmitting a forgot-password POST) — but it's indistinguishable, without this logging, from a real caller bug (the same key accidentally reused for two different message bodies, silently dropping the second one). Visibility here costs one log line and one counter increment per hit; the alternative is a support ticket asking "why didn't the second invite go out" with nothing in any log to explain it.

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

### 4.6 `DLQHandler` — atomic-claim, channel-tagged, with a hard retry-expiry deadline

```go
// DLQStatus is a terminal-or-in-flight state for one DLQEvent.
type DLQStatus string

const (
	DLQStatusPending   DLQStatus = "pending"
	DLQStatusRetrying  DLQStatus = "retrying"
	DLQStatusResolved  DLQStatus = "resolved"
	DLQStatusExhausted DLQStatus = "exhausted" // RetryCount reached MaxRetries
	DLQStatusExpired   DLQStatus = "expired"   // ExpiresAt passed before a successful retry —
	                                           // see the note below distinguishing this from
	                                           // PurgeExpiredEvents' unrelated use of "expired"
)

// DLQMessage is a channel-tagged envelope — exactly one of Email/WhatsApp
// is non-nil, matching Channel.
type DLQMessage struct {
	Channel  Channel
	Email    *EmailMessage
	WhatsApp *WhatsAppMessage

	// ExpiresAt is the hard wall-clock deadline after which this event
	// stops being retried at all, regardless of remaining RetryCount
	// budget — set by Service from SendOptions.RetryExpiresAt or
	// ServiceConfig.DefaultMaxRetryAge (§4.4) before PublishToDLQ is
	// called. New in this revision: previously retries were bounded only
	// by MaxRetries, with no time-based cutoff at all.
	ExpiresAt time.Time
}

// DLQEvent is the durable record of one failed send, awaiting retry or
// already Resolved/Exhausted/Expired.
type DLQEvent struct {
	SendID         string
	MessageData    DLQMessage // carries Channel and ExpiresAt
	FailureReason  string
	RetryCount     int
	MaxRetries     int
	FirstFailureAt time.Time
	LastAttemptAt  time.Time
	NextRetryAt    time.Time
	Status         DLQStatus
	AttemptHistory []DLQRetryAttempt
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type DLQHandler interface {
	PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error

	// ClaimRetryableEvents does two things, in order, each call:
	//  1. Proactively transitions any DLQStatusPending event whose
	//     msg.ExpiresAt has already passed to DLQStatusExpired — a plain
	//     bulk UPDATE, not part of the atomic-claim step below, since it
	//     needs no cross-replica coordination. Without this step, an
	//     event whose deadline passes while nothing happens to call
	//     ClaimRetryableEvents for it would sit invisibly in Pending
	//     forever instead of surfacing as a terminal, dashboard-visible
	//     state.
	//  2. Atomically selects up to limit of the remaining events whose
	//     NextRetryAt has passed, Status is still DLQStatusPending, and
	//     ExpiresAt is still in the future, transitioning each to
	//     DLQStatusRetrying as part of the same operation — so N
	//     concurrent worker replicas each claim disjoint events. Postgres:
	//     one UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)
	//     RETURNING * statement, with an added "AND expires_at > now()"
	//     predicate (§6). Mongo: loops findOneAndUpdate per document.
	ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error)

	// MarkRetried records a retry attempt's outcome and transitions
	// sendID out of DLQStatusRetrying: to DLQStatusResolved on success;
	// to DLQStatusExpired if the recomputed NextRetryAt (on a failure
	// with retries remaining) would land at or past msg.ExpiresAt — this
	// check runs before the MaxRetries check, so a message that expires
	// with retry budget still unused is reported as Expired, not
	// Exhausted; to DLQStatusExhausted if RetryCount reaches MaxRetries
	// (and ExpiresAt hasn't passed); otherwise back to DLQStatusPending
	// with the recomputed NextRetryAt. Returns ErrDLQEventNotClaimed if
	// sendID is not currently DLQStatusRetrying.
	MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error

	GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error)

	// PurgeExpiredEvents deletes DLQStatusResolved/DLQStatusExhausted/
	// DLQStatusExpired events, and any event older than maxAge regardless
	// of status. Its name predates this revision's new DLQStatusExpired
	// status and refers to a different sense of "expired" (old enough to
	// clean up), not the retry-deadline concept above — despite the
	// naming overlap, the two are unrelated: an event can be
	// DLQStatusExpired (gave up retrying) for a long time before
	// PurgeExpiredEvents(ctx, maxAge) actually deletes its row.
	PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error)

	Close() error
}
```

No background reclaim loop inside `grpop` itself — `ClaimRetryableEvents` is a primitive a consuming application's own periodic worker/cron calls. **This is the closest thing to a "queue" that exists in v1** — a durable, pull-based retry table in Postgres/Mongo, not a broker: nothing pushes a claimed event anywhere, a caller's own process polls for work.

**`grpop_dlq` is a secrets store, not just a retry log — new, flagged explicitly (§9):** `DLQMessage` carries the full `EmailMessage`/`WhatsAppMessage`, including `TemplateData` — for `auth.ForgotPassword`, that's the raw reset link, sitting in `message_data` JSONB in the clear for up to `ExpiresAt` plus however long it takes a `PurgeExpiredEvents` sweep to actually delete it. This is the same class of exposure the `DryRunSender` logging fix (§4.11) addresses for logs, just for durable storage instead. Two mitigations, not mutually exclusive:

```go
// MessageEncryptor optionally encrypts DLQMessage's serialized bytes
// before they're written to message_data, and decrypts on read back —
// grpop provides only this seam, not a concrete implementation or any
// key management (consistent with this ecosystem's stated scope: no
// repo here does secret storage — see ECOSYSTEM_SCOPE.md). A consumer
// wanting encryption at rest supplies their own (e.g. AES-GCM keyed from
// whatever secrets manager already backs their gourdiantoken signing
// keys).
type MessageEncryptor interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}
```

`PostgresDLQHandlerConfig`/`MongoDLQHandlerConfig` gain an optional `Encryptor MessageEncryptor` field (nil is the default — plaintext, unchanged behavior). **If left nil** (the likely v1 default given no consuming team has asked for this yet), `grpop_dlq` must be operated with the same access-control rigor as a credentials table — network-isolated, RBAC'd, not queryable by anyone who wouldn't already be trusted with the reset links/invite tokens it can contain. This is stated in `docs.go` (§7) directly, not left to be discovered.

**`AttemptHistory` is capped, not unbounded — new (§9):** each backend's `PublishToDLQ`/retry-recording path keeps at most `MaxAttemptHistoryEntries` (config field, default 20, on both Postgres/Mongo configs) entries per event, dropping the oldest (FIFO) once exceeded. Only matters for an event that keeps failing right up against `MaxRetries`/`ExpiresAt` — at expected volumes this is a non-issue, but leaving it uncapped would mean `attempt_history` JSONB grows without bound for a pathological repeatedly-failing event, so a cap is specified now rather than left to be noticed later.

**Why a deadline independent of `MaxRetries` at all:** a retry-count ceiling alone assumes every failure is equally worth retrying no matter how much wall-clock time has passed — true for a generic delivery failure, but not for `grpop`'s actual payloads. A password-reset link or invite token is itself time-boxed (the token expires, independent of `grpop`); retrying a send for five more hours past that point doesn't help the recipient, it just spends vendor-call budget and rate-limit headroom delivering a message that's already useless. `ExpiresAt` lets the caller (or the library default) say "don't bother past this point," and `DLQStatusExpired` makes that outcome visible and distinguishable from `DLQStatusExhausted` (a real, possibly-alertable vendor-side problem) in dashboards/queries.

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
// client. Subject and TextBody use text/template.
type EmailTemplateEngine interface {
	// RegisterTemplate compiles tmpl once, under name, for repeated cheap
	// reuse via Render — the path for templates known ahead of time.
	RegisterTemplate(name string, tmpl EmailTemplate) error
	Render(name string, data map[string]any) (subject, htmlBody, textBody string, err error)

	// RenderInline compiles and renders tmpl on the fly, with no prior
	// RegisterTemplate call — the "pass a custom template at send time"
	// path (EmailMessage.InlineTemplate, §4.1), for content not known
	// ahead of RegisterTemplate time (e.g. per-tenant custom branding).
	// Same html/template-for-HTMLBody, text/template-for-Subject/TextBody
	// rendering rules as Render. Not cached against any name — a caller
	// sending the identical inline template repeatedly at high volume
	// should register it via RegisterTemplate instead for the
	// compile-once benefit; RenderInline compiles fresh every call.
	RenderInline(tmpl EmailTemplate, data map[string]any) (subject, htmlBody, textBody string, err error)
}
```

**WhatsApp still has no `TemplateEngine` involvement at all** — `WhatsAppMessage.TemplateVariables` are substituted by Meta against its own pre-approved template content, not rendered by `grpop`. See `WhatsAppMessage`'s own doc comment (§4.1) for why "pass a custom WhatsApp template" means "reference any Meta-approved template by name," not an email-style ad-hoc rendering path — that path structurally doesn't exist for WhatsApp.

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
// WhatsAppTemplateInfo describes one Meta-approved template, as returned
// by WhatsAppCloudAPIClient.GetApprovedTemplates (§4.3).
type WhatsAppTemplateInfo struct {
	Name           string
	LanguageCode   string
	ParameterCount int // number of positional variables ("1","2","3", ...) the approved
	                   // template body actually expects — added this revision (§9) so
	                   // Validate can catch an arity mismatch, not just approval status
}

// TemplateValidator confirms a WhatsApp template name+language is currently
// approved on Meta's side AND that the supplied variables' count matches
// the approved template's expected parameter count — catching the two
// most common vendor-rejection causes before a live send is attempted,
// not just approval status alone (Validate replaces an earlier
// IsApproved-only design per review — approval-without-arity-checking
// still fails at the vendor on a mismatched variable count). Backed by
// Meta's own template-listing endpoint, with a short internal TTL cache
// (templates change rarely, so this is not re-fetched on every call).
type TemplateValidator interface {
	// Validate returns nil if templateName+languageCode is approved and
	// len(variables) matches its ParameterCount; ErrWhatsAppTemplateNotApproved
	// if not approved; ErrWhatsAppTemplateArityMismatch (new sentinel) if
	// approved but the variable count doesn't match.
	Validate(ctx context.Context, templateName, languageCode string, variables map[string]string) error
	// Refresh forces an immediate re-fetch of the approved-template list,
	// bypassing the internal cache — e.g. call this right after registering
	// a new template with Meta, instead of waiting out the TTL.
	Refresh(ctx context.Context) error
}
```

`NewMetaTemplateValidator(deps MetaTemplateValidatorDeps) TemplateValidator` implements this over `WhatsAppCloudAPIClient.GetApprovedTemplates` (§4.3). **Design decision, flagged explicitly in §9**: this is wired into `dispatcher.whatsapp.metacloud.go` as an *optional* `Deps.TemplateValidator` field (§4.3) — if set, `Send` consults it (cheap, cache-backed) and returns the specific error early instead of making a doomed vendor call; if unset, `Send` behaves exactly as before, relying on Meta's own rejection. It is advisory, not mandatory, and not a scheduled background job inside `grpop` — the cache is only ever refreshed by a `Send`-time check or an explicit `Refresh` call.

### 4.11 `DryRunSender` — new: logging-only senders for staging

```go
// NewDryRunEmailSender returns an EmailSender that never contacts a real
// vendor: it logs recipient + channel + template name (or "literal-body"/
// "inline-template" if no TemplateName was set) at Info level, and returns
// a synthetic SendResult{Status: SendStatusSent, ProviderMessageID:
// "dryrun-<generated-id>"}. It deliberately does NOT log the rendered
// Subject/HTMLBody/TextBody or TemplateData — staging environments still
// write to shared log aggregation, and grpop's actual payloads are
// security-sensitive (password-reset links, invite tokens); logging a
// fully-rendered dry-run message would leak exactly the secret grpop
// exists to deliver, into every staging log for however long retention
// lasts. See §9 — this was flagged as a real vulnerability in review, not
// a style preference. Distinct from memory.go's in-memory test fake —
// that one exists for contract/unit tests and DOES record full sends for
// assertions (test-only, not shipped to a shared log sink); this one
// exists for a staging/pre-prod deployment that should never actually
// deliver mail/WhatsApp messages but should otherwise exercise the exact
// same Service pipeline (idempotency, rate limiting; DLQ-on-failure never
// triggers since dry-run never fails).
func NewDryRunEmailSender(logger Logger) EmailSender
func NewDryRunWhatsAppSender(logger Logger) WhatsAppSender
```

A construction-time swap (pick `NewDryRunEmailSender` instead of `NewSMTPDispatcher` when building `ServiceDeps` in a staging environment), not a runtime toggle inside the real dispatchers — kept deliberately simple, per §9.

### 4.12 `Metrics` — new: concretely specified, not left implicit

Every earlier section referenced an optional `Metrics Metrics` `Deps` field without ever defining the interface — nailed down now rather than left to be retrofitted later:

```go
// Metrics is the optional observability surface every dispatcher/Service
// component accepts. All methods are fire-and-forget from the caller's
// perspective — a nil Metrics is a silent no-op (OrNop-equivalent, §4.9's
// pattern), and a real implementation should never block or error out the
// operation it's instrumenting.
type Metrics interface {
	ObserveSendLatency(channel Channel, duration time.Duration)
	IncSendResult(channel Channel, status SendStatus)
	IncRateLimitRejected(channel Channel)
	IncDLQPublished(channel Channel)
	IncCircuitBreakerStateChange(channel Channel, newState string)

	// IncIdempotencyDedupHit records every SendResult.Duplicate == true
	// outcome (§4.1, §9) — a real-time counter, alongside Service's own
	// Warn-level log line, for a caller-bug class (an IdempotencyKey
	// reused across two genuinely different message bodies) that would
	// otherwise manifest only as "the second message silently never went
	// out," with nothing in any log or metric to explain why.
	IncIdempotencyDedupHit(channel Channel)

	// ObserveSMTPResponseCode records the raw SMTP reply code (2xx/4xx/5xx)
	// from every send attempt, even though grpop itself takes no action on
	// the code beyond retry/DLQ classification. Added specifically as a
	// deliverability-reputation canary (§9): a rising rate of 5xx (or a
	// creeping 4xx rate) on an otherwise-succeeding send path is an early
	// signal of sender-reputation damage on the configured relay — visible
	// here well before it would show up as a drop in actual delivered
	// mail, which grpop has no way to observe at all (§1.2's stated
	// capability cut: no vendor-side bounce/complaint feedback).
	ObserveSMTPResponseCode(code int)
}
```

No default/no-op implementation ships as part of the public interface contract beyond "nil is safe" (matching `Logger`'s `OrNop` pattern) — a concrete Prometheus/OpenTelemetry-backed `Metrics` is left to the consuming application, same as every other optional collaborator in this design.

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
    expires_at TIMESTAMPTZ,                 -- NEW (§4.6): hard retry cutoff, independent of
                                            -- retry_count/max_retries — set from
                                            -- SendOptions.RetryExpiresAt or
                                            -- ServiceConfig.DefaultMaxRetryAge at PublishToDLQ time.
                                            -- Nullable, not NOT NULL (revised during implementation):
                                            -- NULL means "no deadline" — only reachable by
                                            -- constructing a DLQMessage directly with a zero
                                            -- ExpiresAt, bypassing Service (which always sets one) —
                                            -- matching memoryDLQHandler's identical treatment.
    status VARCHAR(32) NOT NULL,           -- string enum, not a Postgres ENUM type; now includes
                                            -- 'expired' alongside pending/retrying/resolved/exhausted
    attempt_history JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_next_retry
    ON grpop_dlq (status, next_retry_at);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_channel_status
    ON grpop_dlq (channel, status);

-- NEW: supports ClaimRetryableEvents' bulk expire-sweep step (below) —
-- finding all still-Pending rows whose deadline has passed without
-- scanning the whole table.
CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_expires_at
    ON grpop_dlq (status, expires_at);
```

`ClaimRetryableEvents` now runs two statements every call, in order (§4.6):

```sql
-- name: ExpirePastDeadlineEvents :exec
-- Step 1: proactively transition any Pending row whose deadline has
-- already passed to 'expired', regardless of whether this call's own
-- `limit` will get to it below. Plain UPDATE, no FOR UPDATE SKIP LOCKED —
-- multiple replicas running this concurrently is redundant but harmless
-- (each row transitions once; a second attempt on an already-'expired'
-- row matches zero rows), so no cross-replica coordination is needed here
-- the way it is for the claim step.
UPDATE grpop_dlq
SET status = 'expired', updated_at = $2
WHERE status = 'pending' AND expires_at <= $1;

-- name: ClaimRetryableEvents :many
-- Step 2: the original claim, with an added
-- "AND (expires_at IS NULL OR expires_at > $1)" so a row can never be
-- claimed for retry past its own deadline — this predicate is what
-- actually enforces the cutoff; step 1 above only exists so an event
-- that nobody claims in time still surfaces as 'expired' instead of
-- sitting silently in 'pending' forever. NULL must be explicitly OR'd
-- in since "NULL > $1" evaluates to NULL, not true, in SQL.
UPDATE grpop_dlq
SET status = 'retrying', updated_at = $2
WHERE send_id IN (
    SELECT send_id FROM grpop_dlq AS candidate
    WHERE status = 'pending' AND next_retry_at <= $1
        AND (expires_at IS NULL OR expires_at > $1)
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
- `docs.go`: godoc only, "Package shape" + "Precise, non-aspirational claims" sections (mirroring `grnoti/docs.go`) — now including a note that `SendStatusSent` means "SMTP relay/Meta API accepted it," never "arrived in an inbox/on a phone," that `TemplateValidator`'s cache means "approved as of the last check," not a live guarantee, that **there is no message broker anywhere in this package** — `Service.SendX` is a direct, synchronous vendor call (§4.4) — that `DLQStatusExpired` (§4.6) is a deadline-based cutoff wholly independent of `PurgeExpiredEvents`' unrelated "expired" (old-enough-to-delete) usage, despite the shared word, that **retries are at-least-once, not exactly-once** (§4.4 — the reason `IdempotencyKey` is required, not optional), that **`grpop` never refreshes or rotates a Meta access token** — token lifecycle is entirely the operator's responsibility (§4.3, §9), and that **`grpop_dlq` stores full send payloads (including secrets like reset links) in the clear unless a `MessageEncryptor` is configured** — operate it with credentials-table-grade access control by default (§4.6, §9).
- Testing: real in-package `contract_*_test.go` files, real local Docker Postgres/Redis/Mongo/**Mailpit**, `t.Skip` when unreachable, `-race` mandatory. **Only** the Meta WhatsApp client is the documented fake-only exception now (§3, §4.3) — a first for a vendor-facing dispatcher in this ecosystem.
- Shared dependency versions: `jackc/pgx/v5`, `go.mongodb.org/mongo-driver` (v1, not v2), `redis/go-redis/v9`, aligned to whatever `grcache`'s go.sum currently pins. **No vendor-messaging-SDK version to track at all**, and no message-broker client library version either — a direct consequence of the dependency-minimization goal.

---

## 8. Implementation stages

**Stage 0 — Repo scaffolding.** `go.mod` (module `github.com/gourdian25/grpop`, Go 1.26.4), `docs.go`, `errors.go`, `logger.go`, `version.go`.

**Stage 1 — Core contracts.** `interfaces.go`, `types.go` — every interface/type in §4 (email + WhatsApp only).

**Stage 2 — Pure in-process logic.** `retrystrategy.go`, `circuitbreaker.go`, `payloadvalidator.go`.

**Stage 3 — `memory.go`: in-memory variants.** In-memory `DLQHandler` (including the `ExpiresAt`/`DLQStatusExpired` deadline logic from §4.6 — the in-memory variant is the first place this state machine gets built and unit-tested, before Stage 6 repeats it in SQL) and fake `EmailSender`/`WhatsAppSender` for tests/local dev, no live service or vendor account needed.

**Stage 4 — `cache.idempotency.go` + local `RateLimiter`.** The `grcache`-backed adapter (§4.5); `ratelimiter.go`'s local, bounded-LRU, per-(channel,recipient) token bucket (§4.7).

**Stage 5 — `templateengine.email.go`.** `html/template`-based `EmailTemplateEngine` (§4.8).

**Stage 6 — PostgreSQL: `postgres.go` + `dlq.postgres.go`.** Shared connect helper, schema-ensure + advisory lock, sqlc-generated `internal/postgresdb`, the atomic-claim `DLQHandler` — including the `expires_at` column, its index, the `ExpirePastDeadlineEvents`/`ClaimRetryableEvents` two-step sequence, and `MarkRetried`'s expiry-before-exhaustion check (§4.6, §6).

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
8. **§4.1/§4.4, changed this revision**: `SendOptions.IdempotencyKey` is now **required**, reversing the original draft's "optional, empty silently disables the check" design. Raised in review: grpop's retries are at-least-once, not exactly-once (§4.4) — a network error after the vendor already accepted a send is indistinguishable from one before, so a retry can double-send a password-reset/invite email. The original "optional" framing was explicitly flagged as "the wrong footgun for a production library" given every one of the four ERP use cases already has a natural key. `Service.SendX` now returns `ErrIdempotencyKeyRequired` immediately if `opts.IdempotencyKey == ""`, before any rate-limit check or vendor call. `DLQEvent.SendID` is consequently always the caller's real key — an earlier draft's "generate a random UUID when no key was supplied" fallback no longer applies and has been removed as dead design now that a key is guaranteed to exist.
9. **§4.10, new**: `TemplateValidator` is advisory and cache-backed, not a scheduled background refresh job inside `grpop` — a stale cache (Meta approves/rejects a template between `grpop`'s last check and a live send) means the pre-check can be wrong in either direction for up to the cache's TTL. This is accepted because the vendor call itself is still the ultimate source of truth (`Send` doesn't skip actually calling Meta just because the pre-check passed) — the validator only ever short-circuits an *already-known-bad* combination, it can't create a false negative that blocks a send that would have succeeded.
10. **§4.11, new**: `DryRunSender` is a construction-time swap (choose it instead of the real dispatcher when building `ServiceDeps`), not a runtime flag on the real dispatchers — kept this way deliberately to avoid a `if dryRun { ... }` branch living inside otherwise-production dispatch code.
11. **§1.2**: Twilio/Gupshup for WhatsApp, and any vendor-API email path (SES/SendGrid), are **explicitly deferred, not designed away** — both remain additive alt implementations behind the existing vendor-agnostic interfaces whenever a concrete need (BSP relationship, deliverability analytics) makes the added dependency worth it.
12. **§4.6, new**: **`ServiceConfig.DefaultMaxRetryAge` defaults to `24 * time.Hour`** as the fallback retry deadline when a caller leaves `SendOptions.RetryExpiresAt` zero. This is a genuine guess, not derived from any of the four ERP use cases' actual token TTLs (which this plan doesn't know precisely — a password-reset token's real lifetime and an invite token's real lifetime are plausibly quite different from each other and from 24h). Flagged strongly: **callers should set `RetryExpiresAt` explicitly to match their own message's real expiry** rather than relying on the default — this is the same class of footgun as item 8's `IdempotencyKey`, and worth equal prominence in the README's quickstart (§10), not just the godoc. Revisit the default itself once the consuming team confirms real TTL values for both token types.
13. **§4.6, new**: **`DLQStatusExpired` is checked before `DLQStatusExhausted` in `MarkRetried`.** A message whose deadline passes with retry budget still unused is reported as `Expired`, not `Exhausted` — these are different failure modes worth distinguishing in monitoring (`Exhausted` suggests a vendor-side problem worth alerting on; `Expired` suggests the message simply outlived its own usefulness, which is expected/benign behavior, not an incident). Stated as a judgment call because the two could have been collapsed into one terminal "gave up" status instead — kept separate specifically so a dashboard/alert can treat them differently without parsing `attempt_history`.
14. **§4.3, new — must-fix per review, adopted**: **`SMTPDispatcherDeps.TLSMode` defaults to `SMTPTLSStartTLS` (mandatory), not plaintext.** `EmailSender` previously delegated transport security to whichever vendor SDK was in use (SES/SendGrid both handle TLS internally); now that `grpop` owns the raw connection, it must get this right itself rather than leaving it implicit. `SMTPTLSInsecureNoTLS` exists only for the local Mailpit/MailHog test container (§3), given a deliberately loud name to discourage accidental production use, and construction fails outright if `Auth` is set alongside it unless `AllowInsecureAuth` is also explicitly set (credentials over plaintext should require two deliberate opt-ins, not one).
15. **§4.3, new — must-fix per review, adopted**: **`SMTPDispatcherDeps`/`MetaCloudDispatcherDeps` both get explicit `ConnectTimeout`/`SendTimeout`/`RequestTimeout` fields**, defaulting to 5s/15s/10s respectively (proposed, not derived from any real measurement — worth revisiting once real vendor latency is observed in practice). Every `Send` is synchronous on the caller's own HTTP goroutine (§4.4) with no queue to absorb a hang, so a vendor connection with no bound could stall a caller's request indefinitely and, at volume, exhaust a handler goroutine/connection pool. These act as a ceiling in addition to — never instead of — the caller's own `ctx` deadline.
16. **§4.1, new — adopted per review**: **`EmailMessage.To` is a single `string`, not `[]string`.** A real SMTP transaction can accept some `RCPT TO` recipients and reject others in the same transaction, which `SendResult`'s singular `Status`/`ProviderMessageID` shape has no way to represent — rather than redesigning `SendResult` to carry per-recipient outcomes for a capability none of the four ERP use cases need (every one is single-recipient already), multi-recipient sends are cut entirely. A caller needing to notify several people (e.g. both a tenant admin and a platform admin) calls `SendEmail` once per recipient, each with its own `IdempotencyKey` — which is also the semantically correct behavior anyway (one recipient's failure and retry shouldn't be entangled with another's).
17. **§4.12, new**: **`Metrics` is now concretely specified** (latency, send-result counts, rate-limit rejections, DLQ publishes, circuit-breaker transitions, and raw SMTP response codes) rather than left as a forward reference with no defined shape — cheap to nail down now, per review, versus retrofitting call sites later. `ObserveSMTPResponseCode` specifically exists as a deliverability-reputation canary: `grpop` has no vendor-side bounce/complaint feedback at all (§1.2), so a rising 4xx/5xx rate on the SMTP response code itself is the earliest signal available that something (e.g. a bad-address pattern from `IssueInvite`) is degrading sender reputation on the configured relay.
18. **§4.10, new — adopted per review**: **`TemplateValidator.Validate` checks variable-count arity, not just approval status.** Confirming a template is *approved* but not that `TemplateVariables`' length matches its expected parameter count still lets an arity mismatch fail at the vendor — `WhatsAppTemplateInfo.ParameterCount` (a new field, populated from the same `GetApprovedTemplates` call already being cached) closes this gap at negligible extra cost.
19. **§4.3, new**: **`grpop` never refreshes or rotates `MetaCloudDispatcherDeps.AccessToken`.** A long-lived/System User Meta token still has a real (if long) expiry; rotating it before that happens is entirely the operator's job — `grpop` just uses whatever token it's constructed with and returns whatever auth-failure error Meta gives back once it's stale. Stated explicitly so this isn't assumed to be handled somewhere it isn't.
20. **§4.1/§4.8, new**: **Email supports a "custom template passed at send time" path (`EmailMessage.InlineTemplate` + `EmailTemplateEngine.RenderInline`) alongside the existing registered-by-name path**, for content not known ahead of `RegisterTemplate` time (e.g. per-tenant custom branding on an invite email). This is compiled fresh on every call, with no caching — a real cost if the identical inline template is sent at high volume, in which case registering it by name is the better fit. **WhatsApp has no equivalent inline path, and this is a hard vendor constraint, not an oversight**: `TemplateName` is already a free string (any Meta-approved template, however new, can be sent by name with zero grpop-side registration), but there is no way for `grpop` to render or invent WhatsApp template content itself — Meta's own approval process is the sole source of truth for what content may go out on that channel.
21. **§4.6, new — flagged, not fully resolved**: **`grpop_dlq.message_data` is stored in the clear by default**, encryption-at-rest available only via an optional caller-supplied `MessageEncryptor` (a seam, not a shipped implementation — matching this ecosystem's stated "no repo here does secret storage" boundary). This is a real open question for the consuming team, not settled here: is network isolation + RBAC on the Postgres/Mongo instance itself sufficient, or does the actual sensitivity of what transits this table (live password-reset links, invite tokens) warrant application-level encryption from day one? Leaning toward "start with access control, add `MessageEncryptor` if/when a security review calls for it" — but this is exactly the kind of call worth the consuming team making explicitly rather than inheriting silently.
22. **§4.1/§4.11, new**: **`EmailTemplate.InlineTemplate` content must originate from a trusted admin-configured path, never raw end-user input** — html/template protects against HTML injection *from data values*, not against the *template itself* being written to reference `TemplateData` fields the caller didn't intend to expose. `RenderInline` additionally enforces `MaxInlineTemplateBytes` (default 64KiB) purely as a CPU/resource bound (no caching applies to this path, unlike `Render`), not as a substitute for the trust-boundary requirement.
23. **§4.1/§4.12, new — adopted per review**: **`SendResult` gains a `Duplicate bool` field, and a dedup hit is now logged (`Warn`) and counted (`Metrics.IncIdempotencyDedupHit`), not silent.** Making `IdempotencyKey` required (item 8) closes the double-send risk but opens a quieter one: a caller bug that accidentally reuses one key for two different message bodies now silently drops the second send with zero signal anywhere. This trades a small amount of log/metric noise on the (expected, common) genuine-retry case for visibility into the (rarer, worse) caller-bug case.
24. **§4.4/§4.6, new**: **A `RetryExpiresAt` already in the past at `PublishToDLQ` time is accepted, not rejected** — the event is still durably recorded and immediately surfaces as `DLQStatusExpired` on the next sweep, but `Service` logs a `Warn` first. Rejecting outright (returning an error to the original `SendX` caller) was considered and not chosen: the send has already failed by this point, and the caller's HTTP request has already gotten (or is about to get) a `SendResult{Status: SendStatusFailed}` — erroring a second time on top of that for what's fundamentally a caller-side TTL-computation bug adds complexity without changing the outcome. The `Warn` log is judged sufficient for someone to notice the misconfiguration.
25. **§4.6, new**: **`AttemptHistory` is capped at `MaxAttemptHistoryEntries` (default 20), oldest-dropped-first**, rather than unbounded. Only relevant for an event repeatedly failing right up to `MaxRetries`/`ExpiresAt` — at the volumes any of the four ERP use cases would plausibly produce this is not a real concern today, but specifying the cap now avoids `attempt_history` JSONB growing without bound for a pathological case later.

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
// TLSMode defaults to SMTPTLSStartTLS (mandatory) if left zero — §9 item 14.
emailSender, err := grpop.NewSMTPDispatcher(grpop.SMTPDispatcherDeps{
	Addr:           "email-smtp.us-east-1.amazonaws.com:587",
	Auth:           smtp.PlainAuth("", sesSMTPUser, sesSMTPPass, "email-smtp.us-east-1.amazonaws.com"),
	DefaultFrom:    "no-reply@skipp.app",
	ConnectTimeout: 5 * time.Second,  // defaults shown explicitly; §9 item 15
	SendTimeout:    15 * time.Second,
	RateLimiter:    rateLimiter,
	Metrics:        metrics, // §4.12
	Logger:         logger,
})

templateValidator := grpop.NewMetaTemplateValidator(grpop.MetaTemplateValidatorDeps{ /* ... */ })
whatsappSender, err := grpop.NewMetaCloudWhatsAppDispatcher(grpop.MetaCloudDispatcherDeps{
	PhoneNumberID:     phoneNumberID,
	AccessToken:       metaAccessToken, // grpop does not rotate this — §9 item 19
	RequestTimeout:    10 * time.Second,
	TemplateValidator: templateValidator, // optional, §4.10
	RateLimiter:       rateLimiter,
	Metrics:           metrics,
	Logger:            logger,
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
	To:           recipientEmail, // single address — §9 item 16
	TemplateName: "password-reset",
	TemplateData: map[string]any{"ResetLink": link},
}, grpop.SendOptions{
	IdempotencyKey: hashOf(token), // required — §9 item 8; omitting this is a compile-time-visible
	                              // struct field miss, but a runtime ErrIdempotencyKeyRequired if
	                              // left empty
	RetryExpiresAt: tokenExpiresAt, // the same expiry already computed for the reset token itself
	                                // (§9 item 12) — don't rely on ServiceConfig's 24h default here,
	                                // it won't generally match the real token TTL
})
```

A per-tenant custom-branded invite, using the inline-template path (§4.1, §9 item 20) instead of a pre-registered one:

```go
result, err := svc.SendEmail(ctx, grpop.EmailMessage{
	To: recipientEmail,
	InlineTemplate: &grpop.EmailTemplate{
		SubjectTemplate:  tenant.CustomSubjectTemplate,  // not known at RegisterTemplate time
		HTMLBodyTemplate: tenant.CustomHTMLBodyTemplate,
	},
	TemplateData: map[string]any{"InviteLink": link, "TenantName": tenant.Name},
}, grpop.SendOptions{IdempotencyKey: hashOf(inviteToken), RetryExpiresAt: inviteExpiresAt})
```

`result.Status`/`result.ProviderMessageID` is the delivery-status handle the call site logs or surfaces; a failed send after inline retries is durably recorded in `grpop_dlq` (with that `RetryExpiresAt` as its `ExpiresAt`, §4.6) for a separately-run reclaim worker to retry later — the call site itself never blocks on that retry, but it also never handed the send off to any broker to begin with (§4.4), and the reclaim worker itself will stop retrying once `tokenExpiresAt` passes rather than continuing to deliver a link that's already dead.

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
