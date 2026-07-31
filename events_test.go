// File: events_test.go

package grpop

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gourdian25/grevents"
)

// fakeEventBus is a minimal fake grevents.Bus recording every Publish call,
// for asserting Service's/events.go's publish behavior without a real bus.
type fakeEventBus struct {
	mu         sync.Mutex
	published  []grevents.Event
	publishErr error
}

func (b *fakeEventBus) Publish(_ context.Context, event grevents.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishErr != nil {
		return b.publishErr
	}
	b.published = append(b.published, event)
	return nil
}
func (b *fakeEventBus) Subscribe(_ string, _ grevents.HandlerFunc, _ ...grevents.SubscribeOption) (grevents.Unsubscribe, error) {
	return func() {}, nil
}
func (b *fakeEventBus) Use(_ grevents.Middleware) {}
func (b *fakeEventBus) Stats(_ context.Context) (grevents.Stats, error) {
	return grevents.Stats{}, nil
}
func (b *fakeEventBus) Close() error { return nil }

func (b *fakeEventBus) events() []grevents.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]grevents.Event, len(b.published))
	copy(out, b.published)
	return out
}

func TestPublishMessageSent_NilBusIsNoop(t *testing.T) {
	PublishMessageSent(context.Background(), nil, NopLogger(), MessageSentPayload{SendID: "id-1", Channel: ChannelEmail})
}

func TestPublishMessageSent_PublishesExpectedTopicAndMetadata(t *testing.T) {
	bus := &fakeEventBus{}
	PublishMessageSent(context.Background(), bus, NopLogger(), MessageSentPayload{
		SendID: "id-1", Channel: ChannelEmail, ProviderMessageID: "smtp-123",
	})

	events := bus.events()
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Topic != TopicMessageSent {
		t.Fatalf("Topic = %q, want %q", events[0].Topic, TopicMessageSent)
	}
	if events[0].Metadata["send_id"] != "id-1" || events[0].Metadata["channel"] != string(ChannelEmail) {
		t.Fatalf("Metadata = %v, want send_id=id-1 channel=email", events[0].Metadata)
	}
	payload, ok := events[0].Payload.(MessageSentPayload)
	if !ok {
		t.Fatalf("Payload type = %T, want MessageSentPayload", events[0].Payload)
	}
	if payload.Timestamp.IsZero() {
		t.Fatal("Payload.Timestamp is zero, want it filled in by PublishMessageSent")
	}
}

func TestPublishMessageSent_PublishErrorIsLoggedNotPropagated(t *testing.T) {
	bus := &fakeEventBus{publishErr: errors.New("bus unavailable")}
	// Must not panic; the whole point is that a failing bus.Publish never
	// surfaces to the caller.
	PublishMessageSent(context.Background(), bus, NopLogger(), MessageSentPayload{SendID: "id-1", Channel: ChannelEmail})
}

func TestPublishMessageFailed_NilBusIsNoop(t *testing.T) {
	PublishMessageFailed(context.Background(), nil, NopLogger(), MessageFailedPayload{SendID: "id-1", Channel: ChannelWhatsApp})
}

func TestPublishMessageFailed_PublishErrorIsLoggedNotPropagated(t *testing.T) {
	bus := &fakeEventBus{publishErr: errors.New("bus unavailable")}
	PublishMessageFailed(context.Background(), bus, NopLogger(), MessageFailedPayload{SendID: "id-1", Channel: ChannelWhatsApp})
}

func TestPublishMessageFailed_PublishesExpectedTopicAndReason(t *testing.T) {
	bus := &fakeEventBus{}
	PublishMessageFailed(context.Background(), bus, NopLogger(), MessageFailedPayload{
		SendID: "id-2", Channel: ChannelWhatsApp, Reason: "vendor rejected",
	})

	events := bus.events()
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].Topic != TopicMessageFailed {
		t.Fatalf("Topic = %q, want %q", events[0].Topic, TopicMessageFailed)
	}
	payload, ok := events[0].Payload.(MessageFailedPayload)
	if !ok {
		t.Fatalf("Payload type = %T, want MessageFailedPayload", events[0].Payload)
	}
	if payload.Reason != "vendor rejected" {
		t.Fatalf("Payload.Reason = %q, want %q", payload.Reason, "vendor rejected")
	}
}
