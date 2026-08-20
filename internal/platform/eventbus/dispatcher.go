package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// subscription pairs a handler with a name, so logs and metrics can attribute a
// failure to a specific consumer rather than to "a handler for order.placed".
type subscription struct {
	handlerName string
	handle      Handler
}

// Dispatcher fans an event out to every subscriber registered for its name.
//
// It is the in-process implementation of the message broker. The outbox relay
// feeds it; nothing else should. Handlers run concurrently and independently:
// one failing does not prevent the others from seeing the event, because they
// represent genuinely separate concerns (notify the customer / decrement stock /
// bump analytics) and coupling their fates would be wrong.
type Dispatcher struct {
	mu            sync.RWMutex
	subscriptions map[Name][]subscription
	log           *slog.Logger
	handlerTimout time.Duration
}

func NewDispatcher(log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		subscriptions: make(map[Name][]subscription),
		log:           log,
		handlerTimout: 30 * time.Second,
	}
}

// Subscribe registers a handler. Call only during startup wiring -- it takes a
// write lock and is not meant for the hot path.
func (d *Dispatcher) Subscribe(name Name, handlerName string, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.subscriptions[name] = append(d.subscriptions[name], subscription{handlerName: handlerName, handle: h})
	d.log.Info("event handler registered", "event", string(name), "handler", handlerName)
}

// Dispatch delivers one event to all its subscribers and reports whether every
// handler succeeded. The relay uses that to decide between marking the event
// processed and scheduling a retry.
func (d *Dispatcher) Dispatch(ctx context.Context, e Event) (allOK bool) {
	d.mu.RLock()
	subs := d.subscriptions[e.Name]
	d.mu.RUnlock()

	if len(subs) == 0 {
		// Not an error. Many events exist purely for future consumers, and an
		// unconsumed event should not be retried forever.
		d.log.Debug("event has no subscribers", "event", string(e.Name), "event_id", e.ID)
		return true
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed int
	)

	for _, sub := range subs {
		wg.Add(1)
		go func(sub subscription) {
			defer wg.Done()
			defer func() {
				// A panicking handler must not take down the relay goroutine
				// and stall every other event in the outbox.
				if r := recover(); r != nil {
					d.log.Error("event handler panicked",
						"event", string(e.Name), "handler", sub.handlerName,
						"event_id", e.ID, "panic", r)
					mu.Lock()
					failed++
					mu.Unlock()
				}
			}()

			hCtx, cancel := context.WithTimeout(ctx, d.handlerTimout)
			defer cancel()

			start := time.Now()
			if err := sub.handle(hCtx, e); err != nil {
				d.log.Error("event handler failed",
					"event", string(e.Name), "handler", sub.handlerName,
					"event_id", e.ID, "attempt", e.Attempt, "error", err)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			d.log.Debug("event handled",
				"event", string(e.Name), "handler", sub.handlerName,
				"event_id", e.ID, "duration_ms", time.Since(start).Milliseconds())
		}(sub)
	}

	wg.Wait()
	return failed == 0
}

// HandlerNames lists registered handlers per event, for a startup sanity log and
// for the /internal/events introspection endpoint.
func (d *Dispatcher) HandlerNames() map[string][]string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	out := make(map[string][]string, len(d.subscriptions))
	for name, subs := range d.subscriptions {
		names := make([]string, 0, len(subs))
		for _, s := range subs {
			names = append(names, s.handlerName)
		}
		out[string(name)] = names
	}
	return out
}
