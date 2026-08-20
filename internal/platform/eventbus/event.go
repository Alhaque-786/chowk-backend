// Package eventbus is the asynchronous seam between modules.
//
// Modules must not call each other's use cases for side effects. When an order
// is placed, `ordering` does not call `notification.SendPush` -- it publishes
// `order.placed`, and `notification` subscribes. This inverts the dependency:
// ordering does not know notification exists.
//
// That single rule is what makes the extraction to microservices mechanical
// rather than a rewrite. Today Dispatcher delivers in-process. When `delivery`
// moves to its own service, you point the outbox relay at Kafka/NATS and the
// publishing module's code does not change at all.
//
// Delivery is at-least-once, so every handler must be idempotent.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

// Name identifies an event type. Convention: "<aggregate>.<past-tense-verb>",
// e.g. "order.placed". Treat these as a public API -- renaming one breaks every
// subscriber, including future external services.
type Name string

// Event is the envelope carried between modules.
type Event struct {
	ID          id.ID           `json:"id"`
	Name        Name            `json:"name"`
	AggregateID id.ID           `json:"aggregate_id"`
	Payload     json.RawMessage `json:"payload"`
	OccurredAt  time.Time       `json:"occurred_at"`

	// TraceID ties an event back to the request that caused it, so a push
	// notification can be traced to the HTTP call that triggered it.
	TraceID string `json:"trace_id,omitempty"`

	// Attempt is set by the relay on redelivery; handlers can use it to decide
	// when to give up or escalate.
	Attempt int `json:"attempt,omitempty"`
}

// New builds an event, marshalling the payload. It returns an error rather than
// panicking so a bad payload fails the enclosing transaction cleanly.
func New(name Name, aggregateID id.ID, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("eventbus: marshal payload for %s: %w", name, err)
	}
	return Event{
		ID:          id.New(),
		Name:        name,
		AggregateID: aggregateID,
		Payload:     raw,
		OccurredAt:  time.Now().UTC(),
	}, nil
}

// Decode unmarshals an event payload into a typed struct.
func Decode[T any](e Event) (T, error) {
	var out T
	if err := json.Unmarshal(e.Payload, &out); err != nil {
		return out, fmt.Errorf("eventbus: decode %s: %w", e.Name, err)
	}
	return out, nil
}

// Publisher is what a module's use case depends on. It is deliberately the
// narrowest possible interface.
type Publisher interface {
	// Publish enqueues events. When called inside a database transaction, the
	// events are persisted in that same transaction and are only delivered if
	// it commits.
	Publish(ctx context.Context, events ...Event) error
}

// Handler processes one event. Returning an error triggers redelivery, so a
// handler must be idempotent: the same event may legitimately arrive twice.
type Handler func(ctx context.Context, e Event) error

// Subscriber registers handlers. Wiring happens once at startup in the
// composition root.
type Subscriber interface {
	Subscribe(name Name, handlerName string, h Handler)
}

// Bus is both halves, for components that need to do both.
type Bus interface {
	Publisher
	Subscriber
}
