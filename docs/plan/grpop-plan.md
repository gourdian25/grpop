# grpop — Scope & Implementation Plan

**Status:** proposed — pre-Stage-0 design plan, not yet implemented. `grpop` is currently an empty repo (`~/Dev/gourdian25/grpop`, only `.bark.toml`/`bark.txt` present, no `go.mod`, no commits).

**Repo path:** `~/Dev/gourdian25/grpop`, module `github.com/gourdian25/grpop`.

**What grpop is:** a send-only, multi-channel **transactional-messaging delivery** library for email, SMS, and WhatsApp. It is explicitly not an email-retrieval/POP3 library (despite the name) and not a push-notification library (that is the sibling `grnoti`'s job — `grpop` was named specifically to avoid that collision). It has no concept of "user" or "preferences" — delivery, not identity. The recipient address/number is supplied per-send by the caller.

**Package shape: single flat package, no subpackages**, following `grnoti`'s and `gourdiantoken`'s precedent rather than `grcache`/`graudit`'s subpackage-per-backend layout — see §3 for the tradeoff, stated explicitly rather than silently copied.

---

## 0. Research method

This plan was produced by:

1. Reading `~/Dev/gourdian25/CONVENTIONS.md` in full (ecosystem-wide conventions confirmed 2026-07-10/2026-07-23) rather than trusting any single repo's own doc comments about the ecosystem.
2. Reading `grnoti` (the most recently built sibling, tagged `v0.1.0`-in-progress) directly on disk, file by file, not from its own plan doc's summary alone: `logger.go`, `interfaces.go`, `cache.idempotency.go`, `dlq.postgres.go`, `dispatcher.fcm.go`, `templateengine.go`, `ratelimiter.go`, `ratelimiter.redis.go`, `postgres.go`, `errors.go`, `version.go`, `docs.go`, and `docs/plan/grnoti-plan.md` (1064 lines) for its structure, rigor, and the exact section skeleton this document mirrors.
3. Reading `grcache/cache.go` (the `Cache` interface `IdempotencyStore`/rate-limiting decisions build on), `graudit/postgres.go` (the `pg_advisory_xact_lock`/`pg_advisory_lock` locking techniques), `grevents/bus.go` (the `Bus`/`Event` shape), and `grpolicy/docs.go` (to confirm its own stated scope boundary) directly on disk.
4. Checking `grcache/go.mod` and `grnoti/go.mod` directly for the actual currently-pinned dependency versions (`pgx/v5 v5.10.0`, `go.mongodb.org/mongo-driver v1.17.9`, `github.com/redis/go-redis/v9 v9.21.0`, Go `1.26.4`) rather than assuming CONVENTIONS.md's prose was current.
5. Real web research (WebSearch/WebFetch, current as of 2026-07-30) for the two mandatory research questions below (§1.1 build-vs-adopt, §1.2 per-channel Go library survey) — every claim in those two sections is sourced, not guessed. Sources are cited inline and collected at the end of each subsection.

**Lesson carried over from `grnoti-plan.md`'s own §0:** verify sibling state against the actual filesystem, not against a doc describing it. Applied here too — e.g. `grnoti`'s own `IdempotencyStore`/`DLQHandler`/`RateLimiter` code was read directly rather than trusted from this task's own prompt summary, and one real discrepancy was caught doing so: `grnoti`'s `DLQHandler.PublishToDLQ` signature in the live code is `(ctx, event Event, failureReason string) error`, singular string reason, not the richer shape a paraphrase might imply — `grpop`'s own `DLQHandler` in §4 is modeled on the real signature, not a guess.

---

## 1. Mandatory research questions — answered

### 1.1 Build vs. adopt: does an existing platform already solve this? ("Step 0")

**Question:** does an existing platform already solve "deliver an invite/password-reset token via email/SMS/WhatsApp from a Go backend," well enough that building `grpop` is the wrong call for `skipp.app.erp.golang.backend`'s stated need (self-hosted within skipp's own infra, no vendor lock-in decision made yet)?

**Findings, per candidate:**

- **Novu** (`github.com/novuhq/novu`) — the most credible self-hostable candidate. ~39.4k GitHub stars, 22,000+ commits, actively pushed as recently as May 2026 — a real, large, maintained project, not a toy. Dual-licensed: **MIT for the core**, with enterprise-only features gated behind a separate commercial license (an "open-core" model, not pure MIT for the whole product). It genuinely supports self-hosting (their own docs describe running Novu on your own infrastructure, moving between cloud/self-host/hybrid on the same codebase), and it covers email, SMS, push, chat, and in-app channels, including a WhatsApp Business chat provider. **The blocker for `skipp.app.erp.golang.backend` specifically: no mature, first-class Go integration.** Novu's official Go SDK, `github.com/novuhq/novu-go`, is a Speakeasy-generated (auto-generated from an OpenAPI spec, explicitly "any manual changes ... will be overwritten") client whose latest tagged release (`v0.3.0`) is from May 2025 — over a year stale relative to the core repo's own May 2026 activity, and it superseded an even older, now fully deprecated `go-novu` SDK (EOL March 2025). Novu is fundamentally a Node/TypeScript-first platform (its own web/worker/API services are Node) with a thin generated Go client bolted on, not a Go-native library — adopting it would mean running an entire separate Node-based service (with its own Postgres/Redis/MongoDB dependencies, a full "notification workflow" concept, and a UI) as infrastructure just to get "send email/SMS/WhatsApp from Go," when the actual requirement is four call sites returning a delivery handle from a function call.
- **Courier** (courier.com) — SaaS-only, no self-hosted deployment option at all. Free tier 10k notifications/month, Business plan from $99/month, custom enterprise pricing above that. Immediately disqualified by skipp's stated "self-hosted within skipp's own infra" constraint — there is no infrastructure to host, it is a hosted API skipp's data would flow through.
- **Knock** (knock.app) — also cloud-only; "your notification data lives on their servers," no self-hosting story found. Same disqualification as Courier.
- **OneSignal** — primarily a push/in-app-messaging platform with transactional email and SMS bolted on (delivery/click/open tracking documented for email; SMS SDK support exists across OneSignal's client SDKs, and a Go API client, `github.com/onesignal/onesignal-go-api`, does exist and is officially maintained). Still SaaS-hosted (no self-host option found), and its center of gravity is push notifications with email/SMS as secondary channels layered on top — the opposite shape of what's needed (grpop needs email/SMS/WhatsApp as the primary surface, push is explicitly out of scope, owned by `grnoti`).
- **AWS End User Messaging** (formerly bundled under Amazon Pinpoint's SMS/Voice functionality; Pinpoint's own campaign/journey features are being sunset — AWS announced Pinpoint's end-of-support for October 2026, while the underlying End User Messaging SMS/Voice/WhatsApp APIs continue independently) — this is not a competing "platform" in the Novu/Courier/Knock sense at all; it's AWS's own vendor API (SMS/MMS/voice/WhatsApp), i.e. exactly one more per-channel vendor to plug into a `grpop`-shaped abstraction, not an alternative to building one. Relevant to §1.2, not to this build-vs-adopt question.
- **SendGrid/Twilio's own "multi-channel" story** — Twilio's Content API/Content Template Builder lets one approved template fan out across WhatsApp, RCS, Messenger, SMS/MMS from a single vendor, and SendGrid (owned by Twilio) covers email — but this is "one vendor covering multiple channels," still just a vendor, with no self-hosting concept, no delivery-status/DLQ/idempotency/rate-limiting layer of its own, and no unified Go interface across it and a second/third vendor for redundancy.

**Conclusion — build, don't adopt.** None of the SaaS-only options (Courier, Knock, OneSignal, "AWS as a platform") satisfy the explicit self-hosted constraint at all. Novu is the one candidate that could technically satisfy self-hosting, but the real cost of adopting it is standing up and operating an entire additional Node-based service with its own datastore requirements and workflow-builder concept, integrated through a client library that is a year-stale, auto-generated wrapper around a REST API — for a Go backend whose actual requirement is four call sites that need "send a token via email/SMS/WhatsApp, get back a delivery handle." Building a thin, Go-native, self-hosted-by-construction library that talks directly to per-channel vendor APIs (§1.2) is less overall system complexity than running Novu as infrastructure, and it stays consistent with every other `gourdian25` sibling (`grcache`, `graudit`, `grnoti`, ...), which are all exactly this shape: a Go interface + pluggable backends, no separate service to operate. **This does not mean "never use a vendor API" — it means don't adopt a whole notification-orchestration *platform*; `grpop` will still be built as a thin layer over the same underlying vendor SES/Twilio/Meta/etc. APIs Novu itself would ultimately call.**

Sources: [novuhq/novu](https://github.com/novuhq/novu), [novuhq/novu-go](https://github.com/novuhq/novu-go), [novuhq/go-novu (deprecated)](https://github.com/novuhq/go-novu), [Novu Go SDK docs](https://docs.novu.co/platform/sdks/server/go), [Courier push notification pricing](https://www.courier.com/integrations/pricing/push-notifications), [Courier vs Novu comparison](https://www.sequenzy.com/versus/courier-vs-novu), [Knock](https://knock.app/), [Novu vs Knock comparison](https://novu.co/comparison/knock/), [OneSignal transactional emails docs](https://documentation.onesignal.com/docs/en/setup-transactional-emails), [OneSignal SMS SDK support](https://onesignal.com/blog/sdk-support-for-sms/), [onesignal-go-api](https://github.com/onesignal/onesignal-go-api), [AWS End User Messaging SMS/Voice v2 migration guide](https://aws.amazon.com/blogs/messaging-and-targeting/aws-end-user-messaging-sms-and-voice-v2-api-a-migration-guide-from-v1/), [Twilio Content API overview](https://www.twilio.com/docs/content/overview), [Twilio WhatsApp template approval statuses](https://www.twilio.com/docs/whatsapp/tutorial/message-template-approvals-statuses).

### 1.2 Per-channel Go library survey ("Step 0.5")

| Channel | Vendor | Go library (import path) | Maintained? | Last-release recency (checked 2026-07-30) | Notes |
|---|---|---|---|---|---|
| Email | AWS SES | `github.com/aws/aws-sdk-go-v2/service/sesv2` (v2/`sesv2`, or the older `ses` package, both present) | **Yes** | Actively released; `sesv2` at v1.64.0+ recently added SES's new Essentials/Pro/Enterprise pricing-plan support — the SDK tracks SES product changes closely | Official, part of the actively-developed `aws-sdk-go-v2` monorepo. `aws-sdk-go` (v1) is heading to end-of-support; only build against v2. |
| Email | SendGrid | `github.com/sendgrid/sendgrid-go` | **Yes** | Official, maintained/funded directly by Twilio SendGrid; recent releases (v3.16.x line) into 2025/2026 | "The Official Twilio SendGrid Golang API Library." Auto-generated + hand-maintained hybrid, actively accepting PRs. |
| Email | Postmark | *(no official Go client)* — best community option: `github.com/mrz1836/postmark` | Community only | `mrz1836/postmark` published as recently as May 2026, MIT-licensed — the most actively-maintained of several small community clients (`hjr265/postmark.go`, `keighl/postmark`, `mattevans/postmark-go`, `diffeo/postmark`, all older/quieter) | Postmark itself has no first-party Go SDK at all — every option here is a third party depending on Postmark's stable REST API surface, not on Postmark's own release cadence. |
| Email | plain `net/smtp` | stdlib | Yes (stdlib) | N/A | Only relevant as a last-resort fallback (e.g. an on-prem SMTP relay); no vendor-specific bounce/delivery-status API, no built-in retry/rate-limit signal from the vendor side. Not recommended as a primary path — see recommendation below. |
| SMS | Twilio | `github.com/twilio/twilio-go` | **Yes** | v1.30.9 released May 7 2026 — actively released, roughly monthly cadence | Official, auto-generated from Twilio's OpenAPI spec since v1.20.0. Covers Programmable Messaging (SMS) *and* WhatsApp (via the Content API) from the same client — see WhatsApp row below. |
| SMS | MSG91 | *(no official Go SDK found)* | REST-only | N/A | MSG91 ships official SDKs for Node.js, Python, Android, Swift — no official Go package. A handful of tiny unofficial Go wrappers exist (e.g. buried inside unrelated projects) but nothing resembling a maintained standalone client. First integration effort for Go is "hand-write an HTTP client against MSG91's REST API," same order of effort as WhatsApp Cloud API below. |
| SMS | Gupshup | *(no official Go SDK found)* | REST-only | N/A | Same shape as MSG91: official SDKs skew Node/other languages; Gupshup's own developer docs are REST-first. One small unofficial community Go package exists for Gupshup's WhatsApp product specifically (see WhatsApp row) but nothing for Gupshup SMS. |
| WhatsApp | Meta WhatsApp Business Cloud API (direct) | *(no official Go SDK)* — Meta's own beta SDK is TypeScript-only | Meta doesn't ship one | N/A | Confirmed no first-party Go client from Meta. `grpop` needs its own thin hand-rolled HTTP client against the Graph API's `POST /{phone-number-id}/messages` endpoint for this vendor — the same "one deliberate exception to the real-services testing policy" pattern `grnoti` already established for FCM (§4), since there is no local emulator for the Graph API either. |
| WhatsApp | Twilio (WhatsApp via Content API) | `github.com/twilio/twilio-go` (same package as SMS) | **Yes** | Same v1.30.9, May 2026 | Rides on the *same* dependency already needed for SMS — a genuinely different tradeoff from Meta-direct: one already-maintained official Go client covers two of `grpop`'s three channels, at the cost of Twilio's own WhatsApp markup/BSP fees on top of Meta's. |
| WhatsApp | Gupshup | `github.com/jhidalgoesp/gupshup-whatsapp-go` (unofficial) | Community only, low visibility | Small, single-maintainer project | Gupshup is an official Meta-selected BSP (Business Service Provider) and is India's largest conversational-messaging platform by volume — relevant for an India-based ("skipp.co.in") consumer, but the Go tooling for it is thin. |

**A real, non-obvious wrinkle worth flagging for an India-based consumer specifically:** Indian carriers require **TRAI DLT (Distributed Ledger Technology) template pre-registration** for *all* SMS content sent to Indian numbers — transactional included, not just promotional — with a registered Entity ID and per-template Template ID that must be attached to every send. Twilio does support this (their Help Center documents the DLT registration submission process for Indian Sender IDs), but it is manual, operator-adjacent paperwork layered on top of Twilio's API, not something the SDK handles for you. MSG91/Gupshup, being India-native platforms, tend to have smoother self-serve DLT registration flows built into their own dashboards (still real compliance work either way — DLT is a regulatory requirement independent of which vendor is chosen) — but neither ships a maintained Go client, so choosing them trades smoother-DLT-onboarding for hand-rolled REST integration effort in Go. This is a real tradeoff, not a "which vendor is more famous" choice, and it directly affects the recommendation below.

**Recommendation, per channel, for which vendor gets implemented first — and why:**

- **Email: AWS SES first.** Official, actively-maintained `aws-sdk-go-v2/service/sesv2`, and if `skipp.app.erp.golang.backend` already runs in/near AWS infrastructure (plausible, unconfirmed — flagged as an assumption to verify with the consuming team, not asserted as fact here) it's the lowest-friction, most cost-effective choice, with no separate vendor relationship to set up beyond an AWS account already likely in use. SendGrid is the clearly-justified **second** implementation (also official, also actively maintained) for teams that want deliverability tooling (templates, suppression lists, analytics) SES doesn't provide natively. Postmark and `net/smtp` are both explicitly deferred (no official Go client for Postmark; `net/smtp` has no vendor-side bounce/delivery signal at all) — not cut from the design (the `EmailSender` interface must not assume SES-specific behavior), just not first to be *implemented*.
- **SMS: Twilio first**, specifically *because* it's the one vendor whose already-official, actively-maintained Go client (`twilio-go`) also covers WhatsApp — implementing Twilio once buys two of three channels' vendor integrations, which is a real engineering-effort argument, not "Twilio because it's famous." MSG91/Gupshup are real, credible, India-native alternatives worth having on the roadmap specifically for DLT-onboarding friction and India-specific deliverability, but are explicitly deferred to v2+ (§9) because neither has any official Go SDK — first integration effort is materially higher (hand-rolled REST client, same category of work as the Meta WhatsApp client below) for a channel where Twilio already provides a working, low-effort path.
- **WhatsApp: Twilio's Content API first, Meta-direct as v2/alt.** Given Twilio is already being integrated for SMS, extending it to WhatsApp costs re-using an already-present dependency plus building the domain-type mapping (§4) — versus Meta-direct, which requires `grpop` to write and maintain its own Graph API HTTP client from scratch (real, non-trivial work: webhook verification isn't in scope but request signing, template-message JSON shape, media handling, and Meta's own rate-limit/error taxonomy all are). Meta-direct is still worth building as a second implementation specifically because it removes Twilio's BSP markup and gives skipp a direct relationship with Meta if volume grows — but it is not the fastest path to a working v1.

Sources: [aws-sdk-go-v2/service/sesv2 releases](https://github.com/aws/aws-sdk-go-v2/releases), [sesv2 package docs](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/sesv2), [ses package docs](https://pkg.go.dev/github.com/aws/aws-sdk-go-v2/service/ses), [sendgrid-go](https://github.com/sendgrid/sendgrid-go), [sendgrid-go package docs](https://pkg.go.dev/github.com/sendgrid/sendgrid-go), [mrz1836/postmark](https://pkg.go.dev/github.com/mrz1836/postmark), [hjr265/postmark.go](https://github.com/hjr265/postmark.go), [twilio-go](https://github.com/twilio/twilio-go), [twilio-go package docs](https://pkg.go.dev/github.com/twilio/twilio-go), [Twilio Content Types overview](https://www.twilio.com/docs/content/content-types-overview), [Twilio WhatsApp API overview](https://www.twilio.com/docs/whatsapp/api), [Meta WhatsApp Cloud API docs](https://developers.facebook.com/docs/whatsapp/cloud-api/), [jhidalgoesp/gupshup-whatsapp-go](https://pkg.go.dev/github.com/jhidalgoesp/gupshup-whatsapp-go), [Twilio Sender ID / DLT registration docs (India)](https://help.twilio.com/articles/21162166457755-Documents-Required-and-Instructions-to-Register-Your-Alphanumeric-Sender-ID-in-India), [Message Central: India SMS regulations/DLT guide](https://www.messagecentral.com/sms-guideline/india).

### 1.3 Should `IdempotencyStore`/rate limiter build on `grcache.Cache`?

**Yes for `IdempotencyStore`, no for the distributed `RateLimiter` — identical reasoning to `grnoti`'s §1.1, re-verified against `grcache/cache.go` directly rather than assumed.** `grcache.Cache` is `Get/Set/Delete/Exists/InvalidateTag/Stats/Close`, `[]byte` + TTL + tags, with memory/redis/memcached/postgres/mongo backends already built and contract-tested. `IsProcessed`/`MarkProcessed` map onto `Exists`/`Set(..., ttl)` exactly — one ~40-line generic adapter (`NewCacheIdempotencyStore(cache grcache.Cache) IdempotencyStore`), reusing `grnoti`'s exact interface shape with `eventID` renamed to a send-scoped `idempotencyKey`, replaces what would otherwise be per-backend hand-rolled clients. A distributed, **per-recipient-and-per-channel-dimensioned** rate limiter (§4, a deliberate addition beyond `grnoti`'s single global bucket) needs an atomic multi-key refill-and-consume check that `Get`/`Set` cannot provide without a read-modify-write race — same conclusion as `grnoti`: this gets its own raw `*redis.Client` with a Lua script, not a `grcache.Cache` adapter.

### 1.4 grevents — lifecycle event publishing?

**Yes, optional and best-effort, following `graudit`'s and `grnoti`'s identical precedent, verified against the real `grevents/bus.go` (`Bus.Publish(ctx, Event) error`, `Event{Topic, Payload, Timestamp, Metadata}`) rather than a stale summary.** `grpop` reserves and publishes `"message.sent"` / `"message.failed"` topics (channel and send ID in `Event.Payload`) through an injected `grevents.Bus`; nil bus is a silent no-op, a publish failure is logged and never blocks or fails the actual send. This is cosmetic/observability-only — nothing about idempotency, rate limiting, or DLQ durability depends on it, matching the "precise, non-aspirational claims" discipline `grnoti`'s own `docs.go` established (§3, §4).

### 1.5 graudit precedent — Postgres locking technique

**The technique needs to differ, not be copied verbatim — same conclusion `grnoti` reached, re-derived independently for `grpop`'s own DLQ shape.** `graudit`'s `pg_advisory_xact_lock` (confirmed at `graudit/postgres.go:334`) is a single global serialization point, correct because there is exactly one hash chain and exactly one writer may append at a time. `grpop`'s DLQ retry-claiming is the opposite shape: N worker replicas should each claim a *different* pending row concurrently, with no reason to serialize them — so `ClaimRetryableEvents` uses `SELECT ... FOR UPDATE SKIP LOCKED` inside a single `UPDATE ... RETURNING *` statement (§6), exactly `grnoti`'s `dlq.postgres.go` pattern. What *is* reused from `graudit` verbatim: the separate, unrelated use of `pg_advisory_lock` (non-transactional, session-scoped) to serialize concurrent replicas' schema-migration DDL at connect time (`graudit/postgres.go:262`, `grnoti/postgres.go:204`) — `grpop`'s own `postgres.go` reuses this for its `SkipSchemaEnsure`-gated automatic schema application (§6).

### 1.6 grpolicy — any fit?

**No, out of scope — confirmed by reading `grpolicy/docs.go` directly, not assumed.** `grpolicy` is an attribute-based expression/policy engine (`Compile`/`Evaluate` over `map[string]any`) explicitly staged as the future `grauth` repo's primary dependency, with its own doc comment stating it "knows nothing about users, roles, or permissions" and any higher-level vocabulary "belongs to the calling application." `grpop` has no preferences/opt-out/quiet-hours concept at all (explicitly excluded from scope per the user's brief) — there is no boolean-expression-evaluation problem in `grpop`'s design for `grpolicy` to solve. `grnoti`'s own plan reached the same "viable in theory, not adopted" conclusion for its (excluded-from-`grpop`) `PreferencesFilter`; `grpop` doesn't even have the shallower version of that problem.

### 1.7 gourdiantoken precedent

Sentinel-error style (`errors.New("grpop: message")` prefix, `errors.Is`-matched, no `IsX(err) bool` helpers) and `sync.Once`+`atomic.Bool`-guarded idempotent `Close()` are adopted directly, per `CONVENTIONS.md`. `gourdiantoken`'s flat single-package layout (not `grcache`/`graudit`'s subpackage-per-backend split) is the layout `grnoti` already adopted and `grpop` adopts too (§3) — its `New<Thing>With<Backend>(...)`-style constructor naming convention (`NewPostgresDLQHandler`, `NewRedisRateLimiter`, ...) is followed as well.

---

## 2. Concrete real-world consumer requirements

These four call sites in `skipp.app.erp.golang.backend` are the ground truth this design is checked against — **stated plainly to ground the design, not to over-fit `grpop`'s public API to them.** None of the four need `grpop` to own a "user" or "preferences" concept; each just needs: given a token/link and a recipient address/number, go from `Send(ctx, ...)` to a delivery-status handle in one call.

1. **`auth.ForgotPassword`** — issues a `gourdiantoken` verification token keyed to an email address already supplied by the request. Needs email delivery of a reset link/code today; SMS/WhatsApp fallback is a plausible future enhancement at the same call site, not required for v1.
2. **`provider.IssueInvite`** — one of three call sites sharing one `platform.Invites` primitive (`Issue`/`Consume`/`Peek`), generating an opaque bearer token whose SHA-256 hash alone is persisted.
3. **`admin.IssueInvite`** — same `platform.Invites` primitive, different issuing role.
4. **A tenant's first-admin invite** — same `platform.Invites` primitive again. **No recipient field exists anywhere in `platform.Invites` today** (its target is a tenant slug or a "platform" sentinel, never a person) — whichever feature issues the invite must supply the recipient address/number itself when calling `grpop`; `grpop` never looks it up.

Today, none of these four have any delivery mechanism at all — they return the raw secret in the API response body. That is the gap `grpop` closes.

---

## 3. Package layout

**Decision: single flat package (`package grpop`), no subpackages — the same tradeoff `grnoti` explicitly took, re-evaluated on its own terms for `grpop` rather than silently copied.**

`grpop` needs roughly three vendor SDKs (`aws-sdk-go-v2/service/sesv2`, `sendgrid-go`, `twilio-go`) plus Postgres (`pgx/v5`) and Redis (`go-redis/v9`) for its own storage/rate-limiting backends, and an unofficial hand-rolled HTTP client for Meta's Graph API (no SDK dependency at all for that one). This is a *smaller* backend count than `grnoti`'s five (Mongo+Postgres+Redis+Kafka+Firebase) — the dependency-graph cost of "importing `grpop` pulls in every vendor SDK regardless of which channel/vendor a consumer actually uses" is real but proportionally smaller here. The decision is made the same way anyway, for the same reason: consistency with `grnoti`'s and `gourdiantoken`'s precedent in this ecosystem outweighs the smaller avoided-dependency benefit a `grcache`/`graudit`-style subpackage-per-vendor split would buy, and a flat package keeps `grpop`'s public API a single `go get` + single import, matching every other sibling a consumer of this ecosystem will already be used to.

**File-naming convention — a deliberate small divergence from `grnoti`'s `<concern>.<backend>.go` scheme, stated explicitly:** `grnoti` had one channel (push) and one primary vendor (FCM) per storage concern, so `<concern>.<backend>.go` (e.g. `dlq.postgres.go`) was unambiguous. `grpop` has a genuinely two-dimensional axis for its dispatch layer — channel × vendor — so vendor-dispatcher files use `dispatcher.<channel>.<vendor>.go` (three segments, not two). Every other concern (storage backends, cross-cutting logic) keeps `grnoti`'s exact two-segment convention.

```
grpop/
├── interfaces.go              # EmailSender, SMSSender, WhatsAppSender, Service, TemplateEngine,
│                               # IdempotencyStore, DLQHandler, RateLimiter, CircuitBreaker, Metrics
├── types.go                    # EmailMessage, SMSMessage, WhatsAppMessage, SendResult, SendStatus,
│                               # SendOptions, Channel, DLQMessage, DLQEvent, DLQStatus,
│                               # DLQRetryAttempt, RateLimiterStats, CircuitBreakerStats
├── errors.go                    # sentinels, "grpop: " prefix; construction-time validation uses
│                               # "grpop/<component>: " sub-prefix
├── logger.go                     # Logger interface + NopLogger/OrNop — verbatim grnoti shape
├── docs.go                        # godoc only: Package shape + Precise non-aspirational claims (§7)
├── service.go                      # Service orchestrator: idempotency check → rate-limit gate →
│                                   # channel dispatch → DLQ publish on exhausted failure →
│                                   # best-effort grevents publish
├── retrystrategy.go                 # Full-Jitter backoff, stdlib-only, shared inline-retry policy
├── circuitbreaker.go                 # stdlib-only, verbatim grnoti shape, one instance per vendor dispatcher
├── payloadvalidator.go                # per-channel size/shape checks (SMS length, WhatsApp variable
│                                     # count, email size) before a vendor call is attempted
├── templateengine.email.go             # html/template-based EmailTemplateEngine (§4 — diverges from
│                                       # grnoti's text/template stance, justified there)
├── templateengine.sms.go                # text/template-based SMSTemplateEngine (matches grnoti's stance)
├── cache.idempotency.go                  # grcache.Cache-backed IdempotencyStore adapter (§1.3)
├── ratelimiter.go                         # local per-process, per-(channel,recipient) bounded token
│                                         # bucket (default/dev)
├── ratelimiter.redis.go                    # distributed, Lua-scripted, two-tier (per-channel +
│                                          # per-recipient) token bucket (§4)
├── postgres.go                              # connectPostgres shared helper, schema-ensure +
│                                            # pg_advisory_lock (mirrors grnoti/postgres.go, §1.5)
├── dlq.postgres.go                            # DLQHandler, primary (FOR UPDATE SKIP LOCKED)
├── dlq.mongo.go                                # DLQHandler, alt (findOneAndUpdate + $inc)
├── dispatcher.email.ses.go                      # EmailSender via AWS SES (sesv2), first-implemented (§1.2)
├── dispatcher.email.sendgrid.go                  # EmailSender alt via SendGrid
├── dispatcher.sms.twilio.go                       # SMSSender via Twilio, first-implemented (§1.2)
├── dispatcher.whatsapp.twilio.go                   # WhatsAppSender via Twilio Content API, first-implemented
├── dispatcher.whatsapp.metacloud.go                 # WhatsAppSender via Meta Graph API direct (hand-rolled
│                                                    # HTTP client — no official SDK exists, §1.2), v2/alt
├── memory.go                                          # in-memory DLQHandler + in-memory fake
│                                                      # Email/SMS/WhatsAppSender for tests/dev, real
│                                                      # sync.RWMutex where mutable state exists
├── internal/postgresdb/                                # sqlc-generated Postgres query code (internal/,
│                                                      # not a public subpackage — doesn't reopen the
│                                                      # "no subpackages" decision, same as grnoti/graudit)
└── example/                                             # runnable demo, package main
```

**The tradeoff this creates, stated plainly:** importing `grpop` pulls in `aws-sdk-go-v2/service/sesv2`, `sendgrid-go`, `twilio-go`, `pgx/v5`, and `go-redis/v9` into every consumer's build, regardless of which channel/vendor combination they actually use. This is a real cost, smaller in absolute driver count than `grnoti`'s equivalent cost but not zero. Accepted for the same reason `grnoti` accepted its larger version: ecosystem consistency (one flat-package precedent, not two competing layout conventions among siblings) outweighs the avoided-dependency benefit here.

**Testing:** the exact in-package `contract_*_test.go` pattern already used by `grnoti`/`grcache`/`graudit`/`grpolicy` — a private `test<Thing>Contract(t *testing.T, factory func(t *testing.T) <Interface>)` helper running shared `t.Run(subtestName, ...)` behavioral assertions, called once per backend from a public `Test<Thing>_Contract(t *testing.T)`, each backend's subtest `t.Skipf(...)` if that backend's real local service isn't reachable — **not** an invented "conformance" package. Applies to `IdempotencyStore`, `DLQHandler`, `RateLimiter`. Vendor dispatchers (SES/SendGrid/Twilio/Meta) are the one deliberate exception (§4) — none has a local emulator, so their own batching/retry/error-classification logic is unit-tested against a fake narrow vendor-SDK-subset interface instead, exactly `grnoti`'s `FCMClient` precedent.

---

## 4. Interface & type surface

### 4.1 Core domain types

```go
// Channel identifies which delivery channel a message/send/rate-limit
// bucket applies to.
type Channel string

const (
	ChannelEmail    Channel = "email"
	ChannelSMS      Channel = "sms"
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

// SMSMessage is a freeform-body SMS.
type SMSMessage struct {
	To           string // E.164 phone number, required
	From         string // optional sender ID/number override; required in practice for
	                    // India-bound sends once a DLT-registered Sender ID applies (§1.2) —
	                    // grpop does not itself validate DLT registration, that is the
	                    // caller's/vendor account's responsibility
	Body         string
	TemplateName string
	TemplateData map[string]string
}

// WhatsAppMessage is structurally different from EmailMessage/SMSMessage on
// purpose: WhatsApp requires a pre-approved message template for anything
// sent outside a user-initiated 24-hour session window, which every one of
// grpop's four ERP use cases falls into (all system-initiated, none a reply
// to a live user session). There is deliberately no freeform Body field —
// pretending WhatsApp is "email with a phone number" would let a caller
// write code that works in dev (a live test-number session) and then fails
// in production the first time it's used against a real, cold recipient.
type WhatsAppMessage struct {
	To                string            // E.164 phone number, required
	TemplateName      string            // required — the vendor-approved template's name/SID
	TemplateVariables map[string]string // meaning of keys is vendor-specific — see the "precise,
	                                    // non-aspirational claims" note in docs.go: Meta Cloud API
	                                    // expects stringified positional keys ("1","2","3", in
	                                    // order); Twilio Content API expects the named variables
	                                    // defined on that specific approved Content Template. grpop
	                                    // does not normalize between the two — passing the same
	                                    // TemplateVariables map to both a Meta-direct and a
	                                    // Twilio-backed WhatsAppSender for what looks like "the same
	                                    // template" is not portable unless the caller already knows
	                                    // this
	LanguageCode      string            // e.g. "en_US", required, must match the approved
	                                    // template's registered language
}

// SendStatus is the result of a vendor API call, not a delivery-receipt
// status. See docs.go's "precise, non-aspirational claims" section.
type SendStatus string

const (
	SendStatusSent   SendStatus = "sent"   // the vendor API accepted the request
	SendStatusFailed SendStatus = "failed" // the vendor API rejected the request, or the call itself errored
)

// SendResult is the delivery-status handle every Service.SendX call returns.
type SendResult struct {
	Channel           Channel
	ProviderMessageID string            // vendor's own message/tracking ID; empty if Status is SendStatusFailed
	Status            SendStatus
	SentAt            time.Time
	Raw               map[string]string // optional vendor-specific diagnostic fields — for logging
	                                    // only, never parsed/branched on by grpop itself
}

// SendOptions carries per-send cross-cutting behavior, kept separate from
// the channel-specific message types so EmailMessage/SMSMessage/
// WhatsAppMessage stay pure content/recipient shapes.
type SendOptions struct {
	IdempotencyKey string        // caller-supplied; empty disables the idempotency check for
	                             // this one send — see docs.go and §9 item 9
	IdempotencyTTL time.Duration // 0 uses ServiceConfig's configured default
	SkipRateLimit  bool          // escape hatch, e.g. an admin-triggered manual resend
}
```

### 4.2 Sender interfaces (per-channel, not unified)

Following `grnoti`'s vendor-backend pattern (`dispatcher.fcm.go`): one thin interface per channel, taking only domain types, never a vendor SDK type — kept as **three separate interfaces, not one unified `Sender`**, precisely because `WhatsAppMessage`'s shape is not compatible with a single `Send(ctx, msg Message) (SendResult, error)` taking an `any`/interface-typed `msg` without losing compile-time safety on the template-vs-freeform-body distinction that is the whole point of §4.1's design.

```go
type EmailSender interface {
	Send(ctx context.Context, msg EmailMessage) (SendResult, error)
	Close() error
}

type SMSSender interface {
	Send(ctx context.Context, msg SMSMessage) (SendResult, error)
	Close() error
}

type WhatsAppSender interface {
	Send(ctx context.Context, msg WhatsAppMessage) (SendResult, error)
	Close() error
}
```

### 4.3 Vendor-narrow interfaces (the `grnoti` `FCMClient` pattern, per vendor)

```go
// SESClient is the subset of aws-sdk-go-v2/service/sesv2's client used by
// the SES-backed EmailSender — exists so the dispatcher's own retry/
// error-classification logic can be unit-tested against a fake, since SES
// (like every vendor here) has no local emulator. The one deliberate
// exception to grpop's real-services testing policy, per §3.
type SESClient interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// TwilioMessagesClient is the subset of twilio-go's Messages resource used
// by both the SMS and WhatsApp Twilio-backed dispatchers (they share one
// vendor client, differing only in how To/Body/ContentSid are populated).
type TwilioMessagesClient interface {
	CreateMessage(params *openapi.CreateMessageParams) (*openapi.ApiV2010Message, error)
}

// WhatsAppCloudAPIClient is grpop's own thin interface over Meta's Graph
// API messages endpoint — no official Go SDK exists for this (§1.2), so
// this is grpop's own hand-rolled HTTP client, kept narrow for the same
// fakeability reason as SESClient/TwilioMessagesClient above.
type WhatsAppCloudAPIClient interface {
	SendTemplateMessage(ctx context.Context, req WhatsAppCloudAPIRequest) (WhatsAppCloudAPIResponse, error)
}
```

Vendor-specific error classification (e.g. Twilio's REST error-code taxonomy vs. SES's throttling/bounce-suppression error shapes vs. Meta's Graph API error object) stays internal to each vendor's own `dispatcher.<channel>.<vendor>.go` file — never leaks into `interfaces.go`, matching `grnoti`'s `classifyFCMError` precedent.

Cross-cutting concerns are injected via a `Deps` struct per dispatcher, not baked in — same shape as `grnoti`'s `FCMDispatcherDeps`:

```go
type SESDispatcherDeps struct {
	Client         SESClient // required
	DefaultFrom    string
	RateLimiter    RateLimiter    // optional
	CircuitBreaker CircuitBreaker // optional
	Metrics        Metrics        // optional
	Logger         Logger         // optional, OrNop'd at construction
}
```

### 4.4 `Service` — the orchestrator

```go
// Service is the top-level orchestrator: checks idempotency, gates through
// the rate limiter, dispatches via the channel-appropriate Sender,
// publishes to DLQHandler on exhausted inline-retry failure, and
// best-effort-publishes a lifecycle event via grevents. Every SendX method
// is synchronous on the calling goroutine — matching grnoti's
// ProcessEvent/Submit split (§9 item 3: grpop has no Submit/async-ingestion
// entrypoint in v1 at all, a deliberate cut, not an oversight).
type Service interface {
	SendEmail(ctx context.Context, msg EmailMessage, opts SendOptions) (SendResult, error)
	SendSMS(ctx context.Context, msg SMSMessage, opts SendOptions) (SendResult, error)
	SendWhatsApp(ctx context.Context, msg WhatsAppMessage, opts SendOptions) (SendResult, error)

	// Close stops nothing background (there is no worker pool in v1 — see
	// §9 item 3) but releases any owned connections (DLQHandler,
	// RateLimiter). Idempotent.
	Close() error
}
```

### 4.5 `IdempotencyStore` — grcache-backed, `grnoti`'s exact shape

```go
// IdempotencyStore records which idempotency keys have already produced a
// successful send, so a caller retrying a request (e.g. a browser
// resubmitting a forgot-password POST) doesn't trigger a second SMS/email.
type IdempotencyStore interface {
	IsProcessed(ctx context.Context, idempotencyKey string) (bool, error)
	MarkProcessed(ctx context.Context, idempotencyKey string, ttl time.Duration) error
	Close() error // idempotent, no-op — the underlying grcache.Cache is caller-owned
}
```

Implementation is the single `NewCacheIdempotencyStore(cache grcache.Cache) IdempotencyStore` adapter (§1.3) — no bespoke Redis/Mongo client, ~40 lines, identical in shape to `grnoti/cache.idempotency.go`.

### 4.6 `DLQHandler` — atomic-claim, channel-tagged

```go
// DLQMessage is a channel-tagged envelope so DLQHandler can persist any of
// the three message types without an `any` in the public interface —
// exactly one of Email/SMS/WhatsApp is non-nil, matching Channel.
type DLQMessage struct {
	Channel  Channel
	Email    *EmailMessage
	SMS      *SMSMessage
	WhatsApp *WhatsAppMessage
}

// DLQHandler durably tracks send failures across retries and process
// restarts. See §1.5/§6 for the Postgres claim technique.
type DLQHandler interface {
	// PublishToDLQ records a new failure for sendID (the caller's
	// IdempotencyKey if one was supplied, otherwise a generated UUID —
	// see §9 item 9), or appends to its existing attempt history if
	// sendID already has a pending/retrying record.
	PublishToDLQ(ctx context.Context, sendID string, msg DLQMessage, failureReason string) error

	// ClaimRetryableEvents atomically selects up to limit events whose
	// NextRetryAt has passed and Status is DLQStatusPending, transitioning
	// each to DLQStatusRetrying as part of the same operation — so N
	// concurrent worker replicas each claim disjoint events. Postgres:
	// one UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)
	// RETURNING * statement. Mongo: loops findOneAndUpdate per document
	// (atomic per-document, no transaction needed).
	ClaimRetryableEvents(ctx context.Context, limit int) ([]*DLQEvent, error)

	// MarkRetried records a retry attempt's outcome and transitions sendID
	// out of DLQStatusRetrying (Resolved on success, Exhausted if retries
	// are exhausted, back to Pending with a recomputed NextRetryAt
	// otherwise). Returns ErrDLQEventNotClaimed if sendID is not currently
	// DLQStatusRetrying.
	MarkRetried(ctx context.Context, sendID string, success bool, attemptErr error) error

	GetEventByID(ctx context.Context, sendID string) (*DLQEvent, error)

	// PurgeExpiredEvents deletes Resolved/Exhausted events, and any event
	// older than maxAge regardless of status. Returns the count deleted.
	PurgeExpiredEvents(ctx context.Context, maxAge time.Duration) (int64, error)

	Close() error
}
```

There is **no background reclaim loop inside `grpop` itself** — `ClaimRetryableEvents` is a primitive a consuming application's own periodic worker/cron calls, exactly as `grnoti`'s DLQ is designed to be driven from outside. This is stated explicitly in §9 as a scope boundary, not left implicit.

### 4.7 `RateLimiter` — per-recipient AND per-channel, a deliberate addition beyond `grnoti`'s shape

`grnoti`'s `RateLimiter` is a single global bucket with no per-user/per-channel dimensioning — correct for its own use case (one FCM quota to protect), wrong for `grpop`'s: without per-recipient dimensioning, a bug or abuse pattern hitting `auth.ForgotPassword` repeatedly for one address would only be caught by a *global* limit shared across every other legitimate recipient, i.e. it protects the vendor's API quota but not an individual inbox/phone from being spammed.

```go
// RateLimiter bounds outbound sends along two dimensions simultaneously: a
// coarse per-channel bucket (protects the vendor API quota, grnoti's own
// shape) and a fine per-(channel,recipient) bucket (protects one recipient
// from being spammed by a misbehaving/abused endpoint — new relative to
// grnoti, per the ERP forgot-password-abuse scenario this is designed
// against).
type RateLimiter interface {
	// Allow reports whether a send to recipient on channel may proceed
	// right now, without blocking. Consumes a token from both the
	// per-channel and the per-recipient bucket if true; false if either
	// bucket is exhausted.
	Allow(ctx context.Context, channel Channel, recipient string) (bool, error)

	// Wait blocks until both buckets have a token available or ctx is done.
	Wait(ctx context.Context, channel Channel, recipient string) error

	GetStats(ctx context.Context, channel Channel, recipient string) (RateLimiterStats, error)
}
```

Two backends, same split as `grnoti`:

- **Local (`ratelimiter.go`)**: per-process, `golang.org/x/time/rate` for the per-channel bucket (one limiter per `Channel`, small fixed set); the per-recipient dimension uses a **capacity-bounded, LRU-evicted map** of per-recipient limiters (not an unbounded map — recipient strings are attacker-influenced input, e.g. via `auth.ForgotPassword`'s email field, so an unbounded per-recipient map is itself a memory-exhaustion vector). A background sweep goroutine (matching `grcache/memory`'s own sweep-goroutine pattern) evicts idle-longer-than-`X` recipient buckets in addition to LRU eviction on insert.
- **Redis (`ratelimiter.redis.go`)**: one Lua script evaluating *both* the per-channel key and the per-(channel,recipient) key atomically in a single round-trip (extending `grnoti`'s single-bucket `tokenBucketScript` to two `KEYS[]` entries) — avoiding a TOCTOU gap between checking the two dimensions separately. Each recipient's bucket key carries its own `EXPIRE` (same idle-TTL mechanism `grnoti`'s Redis limiter already uses), which naturally bounds Redis-side memory growth from recipient cardinality over time without needing an LRU eviction policy the way the local variant does. `Wait` polls `Allow` (no server-side blocking primitive for a scripted bucket, same as `grnoti`).

### 4.8 `TemplateEngine` — a genuine divergence from `grnoti`'s stance, decided explicitly

`grnoti` uses `text/template` (not `html/template`) for both title and body, with no injection sanitization, an explicit documented scope exclusion — reasonable for grnoti because a push notification's title/body render into a native OS notification tray, not an HTML document; there is no HTML-injection surface. **Email is a different risk category**: `EmailMessage.HTMLBody` renders into a real HTML document in the recipient's mail client, so template data interpolated without context-aware escaping is a genuine HTML/script-injection vector if any `TemplateData` value ever originates from less-trusted input (e.g. an invite's target tenant name). `grpop` therefore splits its `TemplateEngine`:

```go
// EmailTemplateEngine renders EmailMessage.Subject/HTMLBody/TextBody from a
// registered EmailTemplate + TemplateData. Uses html/template (NOT
// text/template) for HTMLBody specifically — a deliberate divergence from
// grnoti's text/template-only stance, because HTMLBody renders into a real
// HTML document in the recipient's mail client, unlike a push
// notification's title/body. Subject and TextBody use text/template (no
// HTML document context to escape into). Compiled once at
// RegisterTemplate time, not re-parsed per render — matching grnoti's
// compileTemplate discipline.
type EmailTemplateEngine interface {
	RegisterTemplate(name string, tmpl EmailTemplate) error
	Render(name string, data map[string]any) (subject, htmlBody, textBody string, err error)
}

// SMSTemplateEngine renders SMSMessage.Body. Uses text/template — SMS has
// no markup/HTML-document rendering context, so grnoti's "no injection
// sanitization, caller's responsibility" stance applies unchanged here.
type SMSTemplateEngine interface {
	RegisterTemplate(name string, tmpl SMSTemplate) error
	Render(name string, data map[string]string) (string, error)
}
```

**WhatsApp deliberately has no `TemplateEngine` involvement at all** — `WhatsAppMessage.TemplateVariables` are substituted by the vendor (Meta/Twilio) against the vendor's own pre-approved template content, not rendered by `grpop`. Registering a `grpop`-side "WhatsApp template" would create a second, redundant, easy-to-drift copy of content that is already vendor-side source of truth.

### 4.9 Logger, Close, errors — verbatim ecosystem shape

`Logger` is copied byte-for-byte from `grnoti/logger.go` with `s/grnoti/grpop/`:

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

`Close()` on every backend-holding type: `sync.Once` + `atomic.Bool`, every method checks `closed.Load()` first and returns `ErrClosed`; in-memory variants need neither (nothing to close, trivially `return nil`). Sentinel errors: `errors.New("grpop: message")`; construction-time validation uses a `"grpop/<component>: ..."` sub-prefix (e.g. `"grpop/ses: DefaultFrom is required"`). A backend-native error (`pgx.ErrNoRows`, `redis.Nil`, a vendor SDK's own error type) never leaks through a `grpop` interface unwrapped — always translated to a `grpop` sentinel first, then `fmt.Errorf("...: %w", errors.Join(sentinel, ErrBackendUnavailable))`, matching `grnoti`'s exact discipline.

---

## 5. Polyglot persistence

| Store | Backend | Notes |
|---|---|---|
| `IdempotencyStore` | **Redis or Mongo, via `grcache`** | one generic adapter (§4.5), not a bespoke store per backend — backend choice is "which `grcache.Cache` the caller constructs," identical to `grnoti` |
| `DLQHandler` | **PostgreSQL** primary (`FOR UPDATE SKIP LOCKED`) | **Mongo** alt (`findOneAndUpdate` + `$inc`, no transaction needed) |
| `RateLimiter` | **Redis**-backed distributed two-tier token bucket, raw client | local in-memory, bounded-LRU variant stays default/dev |
| `CircuitBreaker` | in-memory, per-instance, per-vendor-dispatcher, deliberately not centralized | centralizing risks a synchronized thundering-herd retry against a recovering vendor across every replica at once |
| Lifecycle events (`message.sent`/`message.failed`) | **`grevents.Bus`**, optional/nil-safe | best-effort only, per §1.4 |
| Vendor calls themselves (SES/SendGrid/Twilio/Meta) | HTTP over each vendor's own API | no local emulator for any of the four (§4.3) — the one deliberate exception to real-services testing |

**No separate "delivery-status"/send-log table in v1.** `SendResult` is the in-process, non-persisted delivery handle every `Service.SendX` call returns; `DLQHandler`'s Postgres/Mongo table is the only durable storage `grpop` itself owns, and it only holds *failed* sends awaiting retry, not a full audit trail of every send attempted. A durable log of every send (successful or not) is explicitly out of scope for v1 — see §9 item 5 for the "is this graudit's job instead" framing.

---

## 6. Postgres schema

Schema application: additive-only (`CREATE ... IF NOT EXISTS`), applied automatically by `NewPostgresDLQHandler`, serialized behind a `pg_advisory_lock` so concurrent replicas don't race on DDL at startup, with a `SkipSchemaEnsure` config opt-out for teams running their own migration pipeline — identical mechanism to `grnoti/postgres.go` and `graudit/postgres.go`'s schema-lock (distinct from `graudit`'s *other*, unrelated `pg_advisory_xact_lock` per-chain-write serialization, §1.5).

```sql
CREATE TABLE IF NOT EXISTS grpop_dlq (
    send_id VARCHAR(255) PRIMARY KEY,      -- caller's IdempotencyKey if supplied, else a
                                            -- generated UUID (see §9 item 9)
    channel VARCHAR(16) NOT NULL,          -- 'email' | 'sms' | 'whatsapp'
    message_data JSONB NOT NULL,           -- serialized DLQMessage envelope
    failure_reason TEXT NOT NULL DEFAULT '',
    retry_count INT NOT NULL DEFAULT 0,
    max_retries INT NOT NULL,
    first_failure_at TIMESTAMPTZ NOT NULL,
    last_attempt_at TIMESTAMPTZ NOT NULL,
    next_retry_at TIMESTAMPTZ NOT NULL,
    status VARCHAR(32) NOT NULL,           -- string enum, not a Postgres ENUM type, for
                                            -- schema-evolution flexibility (matches grnoti_dlq)
    attempt_history JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_grpop_dlq_status_next_retry
    ON grpop_dlq (status, next_retry_at);

-- Supports "purge everything for a channel" / per-channel DLQ dashboards —
-- new relative to grnoti_dlq (grnoti has only one channel, push, so this
-- dimension didn't exist there).
CREATE INDEX IF NOT EXISTS idx_grpop_dlq_channel_status
    ON grpop_dlq (channel, status);
```

`ClaimRetryableEvents`'s claim statement (mirroring `grnoti/internal/postgresdb`'s `dlq.sql` shape exactly, adapted table name):

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

`internal/postgresdb/` holds the sqlc-generated `Queries` type for this table (same `internal/`-but-real-subpackage exception `grnoti`/`graudit` already established — doesn't reopen §3's "no subpackages" decision, since it's not importable outside this module).

---

## 7. Ecosystem conventions to match

- `// File: <relative-path>` header on every `.go` file, maintained by `bark` (`.bark.toml` already present in the repo).
- `Logger` interface + `NopLogger()`/`OrNop()`, verbatim `grnoti` shape (§4.9); every constructor calls `OrNop` once at construction, no nil checks scattered through method bodies.
- Sentinel errors: `"grpop: message"` prefix; `"grpop/<component>: ..."` sub-prefix for construction-time validation; `errors.Is`-matched, no `IsX(err) bool` helpers; a backend-native error never leaks through a `grpop` interface unwrapped.
- `Close()` idempotent via `sync.Once` + `atomic.Bool` on every component holding a connection; in-memory variants need neither.
- `docs.go`: godoc only, two sections — "Package shape" (the flat-layout tradeoff, §3) and "Precise, non-aspirational claims" (SendStatus meaning, IdempotencyStore's actual guarantee, local-vs-Redis RateLimiter's actual guarantee under recipient-cardinality pressure, DLQHandler durability being independent of any consuming app's own Kafka/event-bus, ServiceConfig flags that are cosmetic bookkeeping vs. behavior-affecting) — mirroring `grnoti/docs.go`'s exact two-section structure.
- Testing: real in-package `contract_*_test.go` files (§3), real local Docker Postgres/Redis/Mongo in tests, `t.Skip` (never fail) when a backend isn't reachable locally, `-race` mandatory. Vendor dispatchers (SES/SendGrid/Twilio/Meta) are the one deliberate, documented exception — unit-tested against a fake narrow vendor-SDK-subset interface (§4.3), since none has a local emulator.
- Shared dependency versions: `jackc/pgx/v5`, `go.mongodb.org/mongo-driver` (**v1**, not v2 — deliberate ecosystem-wide choice per `CONVENTIONS.md`), `redis/go-redis/v9`, `golang.org/x/crypto`, aligned to whatever `grcache`'s go.sum currently pins (confirmed directly: `pgx/v5 v5.10.0`, `mongo-driver v1.17.9`, `go-redis/v9 v9.21.0`, Go `1.26.4`, as of this writing).
- Subpackage-naming collision-avoidance rule (from `CONVENTIONS.md`) doesn't currently apply — `grpop` has no public subpackages (§3) — but is worth remembering if a future version ever splits one out.

---

## 8. Implementation stages

**Stage 0 — Repo scaffolding.** `go.mod` (module `github.com/gourdian25/grpop`, Go 1.26.4, dependency versions per §7), `docs.go`, `errors.go` (sentinels), `logger.go` (verbatim `grnoti` shape), `version.go`. No logic yet.

**Stage 1 — Core contracts.** `interfaces.go`, `types.go` — every interface and type in §4, no implementations. This is the file pair reviewed hardest before any code lands on top of it, since every later stage depends on it being right.

**Stage 2 — Pure in-process logic (zero external dependencies).** `retrystrategy.go` (Full-Jitter backoff, stdlib-only), `circuitbreaker.go` (stdlib-only, copied from `grnoti`'s implementation), `payloadvalidator.go` (per-channel size/shape checks).

**Stage 3 — `memory.go`: in-memory variants.** In-memory `DLQHandler` and fake `EmailSender`/`SMSSender`/`WhatsAppSender` for tests and local dev with no live service/vendor account needed. Real `sync.RWMutex` where mutable state exists.

**Stage 4 — `cache.idempotency.go` + local `RateLimiter`.** The `grcache`-backed `IdempotencyStore` adapter (§4.5); `ratelimiter.go`'s local, bounded-LRU, per-(channel,recipient) token bucket (§4.7).

**Stage 5 — Template engines.** `templateengine.email.go` (`html/template`-based), `templateengine.sms.go` (`text/template`-based) — §4.8.

**Stage 6 — PostgreSQL: `postgres.go` + `dlq.postgres.go`.** Shared `connectPostgres` helper, schema-ensure + advisory lock (§6), sqlc-generated `internal/postgresdb`, the atomic-claim `DLQHandler` (§4.6).

**Stage 7 — MongoDB: `dlq.mongo.go`.** Alt `DLQHandler` backend, `findOneAndUpdate` + `$inc` claim semantics.

**Stage 8 — Cross-backend contract tests.** `contract_idempotencystore_test.go`, `contract_dlqhandler_test.go`, `contract_ratelimiter_test.go` — real local Docker Postgres/Redis/Mongo, `t.Skip` when unreachable (§3/§7).

**Stage 9 — Redis: distributed `RateLimiter`.** `ratelimiter.redis.go`'s two-tier Lua-scripted bucket (§4.7).

**Stage 10 — Email dispatchers.** `dispatcher.email.ses.go` first (§1.2), then `dispatcher.email.sendgrid.go`, each tested against a fake `SESClient`/SendGrid-client-subset interface (§4.3).

**Stage 11 — SMS dispatcher.** `dispatcher.sms.twilio.go`, tested against a fake `TwilioMessagesClient`.

**Stage 12 — WhatsApp dispatchers.** `dispatcher.whatsapp.twilio.go` first (reuses Stage 11's `TwilioMessagesClient`, §1.2), then `dispatcher.whatsapp.metacloud.go` (grpop's own hand-rolled Graph API HTTP client + `WhatsAppCloudAPIClient` fake for tests).

**Stage 13 — `events.go`: `grevents` integration.** Best-effort `message.sent`/`message.failed` publishing (§1.4), nil-safe.

**Stage 14 — `service.go`: orchestration.** `Service` wired to `IdempotencyStore` + `RateLimiter` + every channel's `Sender` + `DLQHandler` + optional `grevents.Bus`/`Metrics`/`Logger` — the integration point every earlier stage was built to compose into.

**Stage 15 — Polish.** `example/` runnable demo, README quickstart (§10), coverage gate, `docs/architecture.md` recording the divergences called out in §3/§4.8/§4.7 as permanent decision records once code exists (matching every sibling repo's stated convention).

---

## 9. Open decisions for review before Stage 0 starts

Judgment calls made in this plan, flagged rather than buried — explicit scope cuts, stated plainly:

1. **§1.2/§3**: **WhatsApp goes through Twilio's Content API first, Meta-direct second.** Chosen because Twilio is already the SMS vendor being integrated (one dependency, two channels) and has an official, actively-maintained Go SDK, versus Meta having none at all. This is a real cost/speed tradeoff against Meta-direct's lower per-message cost (no BSP markup) and India-native alternatives (MSG91/Gupshup)'s smoother DLT-onboarding story (§1.2) — reversible later, not a permanent lock-in, since `WhatsAppSender` is vendor-agnostic by design.
2. **§4.7**: **Per-recipient-and-per-channel rate-limiter dimensioning is in v1 scope from day one, not deferred to v2.** This is the one interface where `grpop` deliberately does *not* mirror `grnoti`'s simpler single-global-bucket shape, specifically because the forgot-password-endpoint-abuse scenario this guards against is a real, named risk in the ERP use case (§2 item 1), not a hypothetical. Flagged as a judgment call because it does add real implementation complexity (a two-tier Lua script, a bounded-LRU local variant) relative to just shipping `grnoti`'s simpler shape and calling per-recipient limiting a v2 addition.
3. **§4.4**: **No `Submit`/async-ingestion entrypoint, no Kafka `EventConsumer`, in v1.** `skipp.app.erp.golang.backend` has a live Kafka connection today that nothing currently publishes to — tempting to wire one up preemptively, matching `grnoti`'s `Submit`+`WorkerPool`+`EventConsumer` composition pattern. Deliberately cut for v1: all four ERP use cases (§2) are synchronous request/response flows (issue an invite, get a delivery handle back in the same HTTP request) with no batching/backpressure need yet. If async ingestion is wanted later, the exact `consumer.Start(ctx, service.SendEmail)`-shaped composition `grnoti` proved (matching function signatures, zero import coupling) is the intended extension point — not designed away, just not built now.
4. **§4.1**: **Email attachments are cut from v1's `EmailMessage` entirely**, not merely deprioritized. None of the four ERP use cases need one (a reset link/invite token is text, not a file); adding an `Attachments []Attachment` field now would mean designing size limits, MIME handling, and per-vendor attachment-API differences (SES vs. SendGrid) against zero real demand. Easy additive change later; not scaffolded now to avoid a field nothing exercises.
5. **§5**: **No durable send-log/delivery-status table beyond the DLQ.** `DLQHandler`'s table only persists *failures* awaiting retry, not a full record of every send attempted. If skipp wants a durable audit trail of every password-reset/invite email ever sent (for compliance/support purposes), the better home for that is arguably `graudit` (an append-only audit log with pluggable backends already built for exactly this) rather than `grpop` reinventing a second, narrower audit log of its own — flagged as a real design question for the consuming team, not resolved here.
6. **§4.2**: **Three separate `EmailSender`/`SMSSender`/`WhatsAppSender` interfaces, not one unified `Sender`.** Justified in §4.2 by `WhatsAppMessage`'s structurally different shape, but stated as a judgment call: a unified interface taking an `any`-typed message (with a runtime type switch) was considered and rejected specifically to keep the template-vs-freeform-body distinction a compile-time property, not a runtime one.
7. **§3**: **Flat single package, no subpackages** — smaller dependency-graph cost than `grnoti`'s equivalent decision (three vendor SDKs + Postgres + Redis, not five backends), but decided the same way for ecosystem consistency. Worth re-litigating if `grpop` ever grows a second vendor per channel for every channel (six+ vendor SDKs) — the point at which `grcache`/`graudit`'s subpackage-per-backend tradeoff starts looking more attractive again.
8. **§4.5**: **`SendOptions.IdempotencyKey` is optional, not required.** An empty key silently disables the idempotency check for that send. Every one of the four ERP use cases has a natural key available (a hash of the reset/invite token), so in practice callers should always supply one — but `Service` does not enforce this, to avoid forcing a key onto hypothetical future callers with no natural one. Flagged as a real footgun risk worth strongly calling out in the README's quickstart (§10), not just the godoc.
9. **§4.6**: **DLQ `sendID` generation when `SendOptions.IdempotencyKey` is empty.** When a caller doesn't supply an idempotency key (item 8 above) and a send still needs to go to the DLQ, `Service` generates a random UUID as the `sendID` — meaning that specific failed send has no caller-correlatable identity beyond what's inside `message_data` itself. Acceptable given item 8's premise (callers *should* supply a key), but worth being explicit that DLQ lookups by `GetEventByID` are only convenient when a real key was supplied.
10. **§1.2**: **MSG91 and Gupshup (both channels) are v2+, not v1**, purely because neither has an official Go SDK — first-integration effort is materially higher (hand-rolled REST client, same category of work as the Meta WhatsApp client) for vendors that are otherwise strong, India-native candidates given skipp's own market. Revisit once/if India-specific SMS deliverability or DLT-onboarding friction with Twilio becomes a real operational pain point, not preemptively.

---

## 10. How a consuming application wires this in

Described generically — no ERP-repo-specific integration code, matching `grnoti`'s own README quickstart pattern:

A consuming service constructs the backends it wants (a `grcache.Cache` for idempotency, a Postgres `pgxpool.Pool` or DSN for the DLQ, optionally a `*redis.Client`-backed `RateLimiter` for production, a vendor client for each channel it needs), then wires them into `Service` via a `ServiceDeps`-shaped struct at startup:

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

emailSender, err := grpop.NewSESDispatcher(grpop.SESDispatcherDeps{
	Client:      sesv2.NewFromConfig(awsCfg),
	DefaultFrom: "no-reply@skipp.app",
	RateLimiter: rateLimiter,
	Logger:      logger,
})

smsSender, err := grpop.NewTwilioSMSDispatcher(grpop.TwilioDispatcherDeps{ /* ... */ })
whatsappSender, err := grpop.NewTwilioWhatsAppDispatcher(grpop.TwilioDispatcherDeps{ /* ... */ })

svc, err := grpop.NewService(grpop.ServiceDeps{
	IdempotencyStore: idem,
	DLQHandler:       dlq,
	RateLimiter:      rateLimiter,
	EmailSender:      emailSender,
	SMSSender:        smsSender,
	WhatsAppSender:   whatsappSender,
	Logger:           logger,
})
```

A call site (e.g. wherever `auth.ForgotPassword` currently returns the raw token) then does, in outline:

```go
result, err := svc.SendEmail(ctx, grpop.EmailMessage{
	To:           []string{recipientEmail},
	TemplateName: "password-reset",
	TemplateData: map[string]any{"ResetLink": link},
}, grpop.SendOptions{IdempotencyKey: hashOf(token)})
```

`result.Status`/`result.ProviderMessageID` is the delivery-status handle the call site logs or surfaces; a failed send after inline retries is durably recorded in `grpop_dlq` for a separately-run reclaim worker to retry later (§4.6, §9 item 3) — the call site itself never blocks on that retry.

---

## 11. Next steps

Stage 0, on approval of this plan.

### Critical Files for Implementation

- `/Users/varun/Dev/gourdian25/grpop/interfaces.go` (to be created) — the entire public contract (§4); every later stage depends on getting this right first, per Stage 1.
- `/Users/varun/Dev/gourdian25/grpop/types.go` (to be created) — `EmailMessage`/`SMSMessage`/`WhatsAppMessage`/`SendResult`/`DLQMessage`, the types that encode the WhatsApp-template-vs-freeform-body distinction this whole design turns on.
- `/Users/varun/Dev/gourdian25/grpop/dlq.postgres.go` (to be created) — the atomic-claim DLQ, modeled directly on `/Users/varun/Dev/gourdian25/grnoti/dlq.postgres.go`'s `FOR UPDATE SKIP LOCKED` pattern.
- `/Users/varun/Dev/gourdian25/grpop/ratelimiter.redis.go` (to be created) — the two-tier per-channel-and-per-recipient Lua script, the one interface that meaningfully diverges from `/Users/varun/Dev/gourdian25/grnoti/ratelimiter.redis.go`'s single-bucket shape.
- `/Users/varun/Dev/gourdian25/grnoti/docs/plan/grnoti-plan.md` (existing, reference only) — the structural/rigor template this document mirrors; worth re-reading alongside `grpop`'s own eventual `docs/architecture.md` once Stage 0+ is underway.
