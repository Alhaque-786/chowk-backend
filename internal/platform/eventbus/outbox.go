package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// Outbox is the transactional-outbox Publisher.
//
// The problem it solves: "insert the order, then publish order.placed" is two
// systems and no atomicity. If the process dies between them you either have an
// order nobody was told about, or a notification for an order that was rolled
// back. Both are real bugs that show up as "customer charged but no order" at
// scale.
//
// The fix: write the event as a row in the same transaction as the state change.
// Either both land or neither does. A separate relay then reads committed rows
// and delivers them. Delivery becomes at-least-once instead of maybe-once, which
// is why every handler must be idempotent.
type Outbox struct {
	db *database.DB
}

func NewOutbox(db *database.DB) *Outbox { return &Outbox{db: db} }

// Publish appends events to the outbox using the ambient transaction if there is
// one. Callers should be inside db.InTx -- otherwise each event commits on its
// own and the atomicity guarantee is lost.
func (o *Outbox) Publish(ctx context.Context, events ...Event) error {
	if len(events) == 0 {
		return nil
	}

	const q = `
		INSERT INTO outbox_events (id, event_name, aggregate_id, payload, trace_id, occurred_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending')`

	q2 := o.db.Q(ctx)
	for _, e := range events {
		if e.ID == id.Nil {
			e.ID = id.New()
		}
		if e.OccurredAt.IsZero() {
			e.OccurredAt = time.Now().UTC()
		}
		if _, err := q2.Exec(ctx, q, e.ID, string(e.Name), e.AggregateID, []byte(e.Payload), e.TraceID, e.OccurredAt); err != nil {
			return fmt.Errorf("eventbus: enqueue %s: %w", e.Name, err)
		}
	}
	return nil
}

// Relay drains the outbox and hands events to the dispatcher.
//
// This is the swap point for microservices: replace `dispatch` with a Kafka or
// NATS producer and every publishing module keeps compiling untouched.
type Relay struct {
	db          *database.DB
	dispatcher  *Dispatcher
	log         *slog.Logger
	batchSize   int
	pollEvery   time.Duration
	maxAttempts int
}

func NewRelay(db *database.DB, d *Dispatcher, log *slog.Logger) *Relay {
	return &Relay{
		db:          db,
		dispatcher:  d,
		log:         log,
		batchSize:   100,
		pollEvery:   time.Second,
		maxAttempts: 8,
	}
}

// Run polls until the context is cancelled.
//
// Polling rather than LISTEN/NOTIFY on purpose: NOTIFY is fire-and-forget and
// drops messages if no session is listening, which would silently lose events
// during a deploy. A 1s poll on an indexed partial index is cheap and cannot
// lose anything.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.pollEvery)
	defer ticker.Stop()

	r.log.Info("outbox relay started", "batch_size", r.batchSize, "poll_interval", r.pollEvery)

	for {
		select {
		case <-ctx.Done():
			r.log.Info("outbox relay stopped")
			return nil
		case <-ticker.C:
			// Keep draining while batches come back full, so a burst is not
			// rate-limited to batchSize per tick.
			for {
				n, err := r.drainBatch(ctx)
				if err != nil {
					r.log.Error("outbox drain failed", "error", err)
					break
				}
				if n < r.batchSize {
					break
				}
			}
		}
	}
}

// drainBatch claims and processes one batch, returning how many it handled.
func (r *Relay) drainBatch(ctx context.Context) (int, error) {
	// FOR UPDATE SKIP LOCKED is what makes this safe to run on every replica:
	// each relay claims a disjoint set of rows, no coordination needed, and no
	// event is delivered twice concurrently.
	const claim = `
		WITH claimed AS (
			SELECT id
			FROM outbox_events
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY occurred_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events o
		SET status = 'processing', attempts = o.attempts + 1, updated_at = now()
		FROM claimed c
		WHERE o.id = c.id
		RETURNING o.id, o.event_name, o.aggregate_id, o.payload, o.trace_id, o.occurred_at, o.attempts`

	var batch []Event
	err := r.db.InTx(ctx, func(ctx context.Context) error {
		rows, err := r.db.Q(ctx).Query(ctx, claim, r.batchSize)
		if err != nil {
			return fmt.Errorf("claim outbox rows: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var e Event
			var name string
			var payload []byte
			if err := rows.Scan(&e.ID, &name, &e.AggregateID, &payload, &e.TraceID, &e.OccurredAt, &e.Attempt); err != nil {
				return fmt.Errorf("scan outbox row: %w", err)
			}
			e.Name = Name(name)
			e.Payload = payload
			batch = append(batch, e)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}

	for _, e := range batch {
		if ok := r.dispatcher.Dispatch(ctx, e); ok {
			if err := r.markProcessed(ctx, e.ID); err != nil {
				r.log.Error("mark outbox processed failed", "event_id", e.ID, "error", err)
			}
			continue
		}
		if err := r.scheduleRetry(ctx, e); err != nil {
			r.log.Error("schedule outbox retry failed", "event_id", e.ID, "error", err)
		}
	}
	return len(batch), nil
}

func (r *Relay) markProcessed(ctx context.Context, eventID id.ID) error {
	const q = `UPDATE outbox_events SET status = 'processed', processed_at = now(), updated_at = now() WHERE id = $1`
	_, err := r.db.Q(ctx).Exec(ctx, q, eventID)
	return err
}

// scheduleRetry applies exponential backoff, or parks the event in the dead
// letter state once it has exhausted its attempts. Dead-lettering rather than
// dropping matters: a failed `payment.captured` event is money, and it needs a
// human to look at it, not to vanish.
func (r *Relay) scheduleRetry(ctx context.Context, e Event) error {
	if e.Attempt >= r.maxAttempts {
		const dead = `UPDATE outbox_events SET status = 'dead_letter', updated_at = now() WHERE id = $1`
		_, err := r.db.Q(ctx).Exec(ctx, dead, e.ID)
		r.log.Error("event dead-lettered after max attempts",
			"event", string(e.Name), "event_id", e.ID, "attempts", e.Attempt)
		return err
	}

	backoff := time.Duration(1<<uint(e.Attempt)) * time.Second
	if backoff > 10*time.Minute {
		backoff = 10 * time.Minute
	}

	const q = `
		UPDATE outbox_events
		SET status = 'pending', next_attempt_at = now() + $2::interval, updated_at = now()
		WHERE id = $1`
	_, err := r.db.Q(ctx).Exec(ctx, q, e.ID, fmt.Sprintf("%d seconds", int(backoff.Seconds())))
	return err
}

var _ Publisher = (*Outbox)(nil)
