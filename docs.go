// File: docs.go

// Package grpop provides a send-only, multi-channel transactional-messaging
// delivery library for the gourdian ecosystem: email and WhatsApp dispatch,
// idempotent send processing, a hard-deadline dead-letter retry queue,
// circuit breaking, and per-channel-and-per-recipient distributed rate
// limiting, behind a set of storage- and vendor-agnostic interfaces.
//
// grpop is not an email-retrieval/POP3 library, despite the name, and not
// a push-notification library — that is the sibling gourdian25 module
// grnoti's job. grpop has no concept of "user" or "preferences"; it is
// about delivery, not identity, and the recipient address/number is
// supplied per-send by the caller.
//
// # Package shape
//
// grpop's public API is a single flat package with no subpackages — every
// backend (PostgreSQL, MongoDB, Redis) and every vendor dispatcher (a
// stdlib net/smtp-based email sender, a hand-rolled Meta WhatsApp Cloud
// API client) lives in this one module, distinguished by file-naming
// convention: "<concern>.<backend>.go" for storage/rate-limiting (e.g.
// dlq.postgres.go, ratelimiter.redis.go), and
// "dispatcher.<channel>.<vendor>.go" for the two dispatch files
// (dispatcher.email.smtp.go, dispatcher.whatsapp.metacloud.go) — a third
// segment, since grpop's dispatch layer has a genuinely two-dimensional
// axis (channel x vendor) that grnoti's single-channel, single-vendor
// dispatch layer didn't need. The one exception is internal/postgresdb,
// sqlc's generated query code — a real Go subpackage, but an unexported
// internal/ one, not importable outside this module, so it doesn't
// undermine the "flat public API" claim.
//
// This follows grnoti's and gourdiantoken's precedent rather than
// grcache's/graudit's subpackage-per-backend layout, which exists
// specifically to keep unused backend drivers out of a consumer's
// dependency graph. grpop's version of this tradeoff is unusually cheap
// compared to grnoti's own: grpop's dispatch layer has ZERO third-party Go
// dependencies at all (email goes over plain SMTP via the standard
// library; WhatsApp goes directly against Meta's Graph API over a
// hand-rolled net/http client, since Meta ships no official Go SDK) — the
// only third-party imports anywhere in this module are for its own
// storage/rate-limiting backends (pgx/v5, go-redis/v9, optionally
// mongo-driver) and grcache's/grevents' own lightweight root-interface
// packages. Importing grpop does not pull in any vendor-messaging SDK
// (no AWS SDK, no Twilio, no SendGrid) and no message-broker client
// library (no Kafka/NATS/RabbitMQ) regardless of which backends a given
// deployment actually uses. See docs/plan/grpop-plan.md §3 for the full
// reasoning.
//
// # Precise, non-aspirational claims
//
// SendStatusSent means "the vendor (SMTP relay, or Meta's Graph API)
// accepted the request" — never "arrived in a recipient's inbox" or "was
// read." grpop has no vendor-side bounce/complaint/delivery-receipt
// feedback for either channel in this version; a SendStatusSent result is
// not proof of actual delivery.
//
// grpop's inline retry (and any later DLQ-driven retry) is at-least-once,
// not exactly-once: a network error after a vendor has already accepted a
// send is indistinguishable, from grpop's side, from an error before
// acceptance, so a retry can re-attempt a send the vendor already
// processed. This is why SendOptions.IdempotencyKey is required, not
// optional — the guarantee against a caller-visible double-send comes
// entirely from the caller supplying a stable key and IdempotencyStore
// catching the redelivery, not from any property of the send path itself.
//
// There is no message broker or queue anywhere in this package. Every
// Service.SendX call dispatches directly and synchronously to the vendor
// on the caller's own goroutine; DLQHandler's Postgres/Mongo table is a
// durable, pull-based retry store a consuming application's own worker
// polls (via ClaimRetryableEvents), not a transport mechanism a send
// passes through on its way out.
//
// DLQStatusExpired (a DLQEvent's terminal state once its ExpiresAt
// deadline passes without a successful retry) is unrelated to
// DLQHandler.PurgeExpiredEvents' use of "expired," despite the shared
// word — PurgeExpiredEvents' sense of "expired" means "old enough to
// delete" and applies to Resolved/Exhausted/Expired events alike after
// maxAge; DLQStatusExpired means "gave up retrying because the message's
// own deadline passed," independent of how long the row has existed.
//
// grpop never refreshes or rotates a WhatsApp dispatcher's Meta access
// token. Token lifecycle — rotating a long-lived/System User token before
// it expires — is entirely the operator's responsibility; grpop only ever
// uses whatever token it was constructed with.
//
// grpop_dlq (the Postgres DLQHandler's table) stores each failed send's
// full message payload — including any secrets it carries, such as a
// password-reset link or invite token — in grpop_dlq.message_data, in the
// clear, unless a MessageEncryptor is configured. Absent one, grpop_dlq
// must be operated with the same access-control rigor as a credentials
// table: network-isolated, RBAC'd, not queryable by anyone who would not
// already be trusted with the secrets it can contain.
//
// TemplateValidator's approval cache means "approved as of the last
// check," not a live guarantee — Meta can approve, reject, or delete a
// template between grpop's last cache refresh and a live send. The
// underlying vendor call remains the ultimate source of truth: a passed
// TemplateValidator check never causes Send to skip actually calling Meta.
package grpop
