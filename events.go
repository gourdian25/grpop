// File: events.go

package grpop

import (
	"context"
	"time"

	"github.com/gourdian25/grevents"
)

// Topic* are the grevents topics Service publishes to — the only
// "broker-shaped" thing in grpop, and one-directional/observability-only,
// never a substitute for an actual message-broker-mediated send path (see
// docs.go). Deliberately carry Channel + SendID only, never the recipient
// address/number or any message content: an event bus is often subscribed
// to broadly within a consuming application, and grpop's payloads
// themselves are frequently security-sensitive (reset links, invite
// tokens) — see MessageEncryptor's own doc comment for the same class of
// concern applied to durable storage instead of this side channel.
const (
	// TopicMessageSent fires after Service's own inline retry succeeds.
	TopicMessageSent = "message.sent"
	// TopicMessageFailed fires after Service's inline retry is exhausted
	// and the event has (successfully or not) been handed to DLQHandler.
	TopicMessageFailed = "message.failed"
)

// MessageSentPayload is published on TopicMessageSent.
type MessageSentPayload struct {
	// SendID is the caller's own SendOptions.IdempotencyKey — the same
	// identifier DLQEvent.SendID uses, so a subscriber can correlate this
	// event with a DLQ entry if one exists.
	SendID            string
	Channel           Channel
	ProviderMessageID string
	Timestamp         time.Time
}

// MessageFailedPayload is published on TopicMessageFailed.
type MessageFailedPayload struct {
	SendID    string
	Channel   Channel
	Reason    string
	Timestamp time.Time
}

// PublishMessageSent publishes a TopicMessageSent event for payload via
// bus. Following grnoti's own PublishSent/graudit's PublishRecorded
// precedent: bus may be nil (a silent no-op), and any error bus.Publish
// returns is only logged, never propagated to the caller — grevents
// delivery is a best-effort side channel on top of whatever durable/
// authoritative work already happened (the actual send), never allowed to
// fail or block it.
func PublishMessageSent(ctx context.Context, bus grevents.Bus, logger Logger, payload MessageSentPayload) {
	if bus == nil {
		return
	}
	if payload.Timestamp.IsZero() {
		payload.Timestamp = time.Now()
	}
	event := grevents.Event{
		Topic:   TopicMessageSent,
		Payload: payload,
		Metadata: map[string]string{
			"send_id": payload.SendID,
			"channel": string(payload.Channel),
		},
	}
	if err := bus.Publish(ctx, event); err != nil {
		logger.Warn("grpop: publish failed", "topic", TopicMessageSent, "send_id", payload.SendID, "error", err)
	}
}

// PublishMessageFailed publishes a TopicMessageFailed event. See
// PublishMessageSent for the nil-bus/best-effort contract.
func PublishMessageFailed(ctx context.Context, bus grevents.Bus, logger Logger, payload MessageFailedPayload) {
	if bus == nil {
		return
	}
	if payload.Timestamp.IsZero() {
		payload.Timestamp = time.Now()
	}
	event := grevents.Event{
		Topic:   TopicMessageFailed,
		Payload: payload,
		Metadata: map[string]string{
			"send_id": payload.SendID,
			"channel": string(payload.Channel),
		},
	}
	if err := bus.Publish(ctx, event); err != nil {
		logger.Warn("grpop: publish failed", "topic", TopicMessageFailed, "send_id", payload.SendID, "error", err)
	}
}
