// File: docs.go

// Package grpop provides a send-only, multi-channel transactional-messaging
// delivery library for the gourdian ecosystem: email and WhatsApp senders
// that each send one message once and report what the vendor said, with
// templates, optional circuit breaking and optional per-channel and
// per-recipient rate limiting through a limiter the caller supplies.
//
// grpop is not an email-retrieval/POP3 library, despite the name, and not
// a push-notification library — that is the sibling gourdian25 module
// grnoti's job. grpop has no concept of "user" or "preferences"; it is
// about delivery, not identity, and the recipient address/number is
// supplied per-send by the caller.
//
// # Delivery only
//
// grpop keeps nothing and retries nothing (since 0.2.0). Retrying a failed
// send, keeping a message durably until it goes, and suppressing a
// duplicate are the caller's: only the caller knows the transaction a
// message belongs to and how long its link stays valid, and only a store in
// that transaction (an outbox) can promise a committed message is never
// lost. FullJitterBackoff is exported for the caller's retry loop. Earlier
// versions had an orchestrating Service with an idempotency store and a
// dead-letter retry table (memory, Postgres, Mongo); they were removed, and
// docs/plan/2026-10-23-grpop-delivery-only.md records why.
//
// # Package shape
//
// grpop's public API is a single flat package with no subpackages, every
// vendor sender (a stdlib net/smtp-based email sender, a hand-rolled Meta
// WhatsApp Cloud API client) distinguished by file-naming convention:
// "dispatcher.<channel>.<vendor>.go" (dispatcher.email.smtp.go,
// dispatcher.whatsapp.metacloud.go), since the dispatch layer has a
// genuinely two-dimensional axis (channel x vendor).
//
// grpop has no third-party Go dependencies at all: email goes over plain
// SMTP via the standard library, and WhatsApp goes directly against Meta's
// Graph API over a hand-rolled net/http client, since Meta ships no
// official Go SDK. Importing grpop pulls in no vendor-messaging SDK (no AWS
// SDK, no Twilio, no SendGrid), no message-broker client and no storage
// driver. A rate limiter is the caller's: RateLimiter is
// grsentry's pkg/grlimiter.Limiter's method set, so that limiter (or any
// other with the same single method) plugs in without an import.
//
// # Precise, non-aspirational claims
//
// SendStatusSent means "the vendor (SMTP relay, or Meta's Graph API)
// accepted the request" — never "arrived in a recipient's inbox" or "was
// read." grpop has no vendor-side bounce/complaint/delivery-receipt
// feedback for either channel in this version; a SendStatusSent result is
// not proof of actual delivery.
//
// A caller's retry is at-least-once, not exactly-once: a network error
// after a vendor has already accepted a send is indistinguishable, from
// grpop's side, from an error before acceptance, so a retry can re-send a
// message the vendor already took. The SMTP sender can't be cancelled
// mid-transaction either: on a timeout its goroutine is abandoned, not
// stopped, and may still deliver. A caller that must not double-send needs
// a vendor-side idempotency key; EmailMessage.IdempotencyKey is where it
// goes. The SMTP sender can only derive a stable Message-ID from it, which
// receiving systems may or may not use to drop a duplicate.
//
// ErrPermanent separates a failure that will recur on every attempt (a
// malformed or unrenderable message, an unparseable address, an SMTP 5xx
// reply, a Meta refusal of the request, account or template) from one worth
// another attempt (a rate limit, an open breaker, an SMTP 4xx, a network
// failure, a timeout, a Meta 429, 5xx or retry-later code). Meta's list was
// read from its error reference on 2026-10-23; a code Meta adds later is
// classed by its HTTP status.
//
// There is no message broker or queue anywhere in this package. Every Send
// dispatches directly and synchronously to the vendor on the caller's own
// goroutine, and a rate-limited send fails at once (ErrRateLimited) rather
// than waiting.
//
// A WhatsApp sender names one Graph API version on every call
// (MetaCloudDispatcherDeps.GraphAPIVersion, default v26.0). Meta retires
// each version about two years after release, so the default needs a bump
// before then (v20.0, the earlier hard-coded one, stopped on 2026-09-24).
//
// grpop never refreshes or rotates a WhatsApp dispatcher's Meta access
// token. Token lifecycle — rotating a long-lived/System User token before
// it expires — is entirely the operator's responsibility; grpop only ever
// uses whatever token it was constructed with.
//
// TemplateValidator's approval cache means "approved as of the last
// check," not a live guarantee — Meta can approve, reject, or delete a
// template between grpop's last cache refresh and a live send. The
// underlying vendor call remains the ultimate source of truth: a passed
// TemplateValidator check never causes Send to skip actually calling Meta.
package grpop
