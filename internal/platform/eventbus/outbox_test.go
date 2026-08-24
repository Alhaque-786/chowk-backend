package eventbus_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
The outbox insert, tested because it is one hand-written statement carrying every
domain event in the system and it had no coverage at all.

It was rewritten from a loop of single-row Execs into one `unnest` insert to save a
network round trip per event — 29.7ms each against a database in ap-south-1. That
is a change to SQL, and SQL that is only exercised in production is a guess.
*/

func TestPublishWritesEveryEventInOneStatement(t *testing.T) {
	db := testutil.DB(t)
	outbox := eventbus.NewOutbox(db)
	ctx := context.Background()

	aggregate := id.New()
	events := []eventbus.Event{
		{Name: "test.first", AggregateID: aggregate, Payload: json.RawMessage(`{"n":1}`), TraceID: "trace-1"},
		{Name: "test.second", AggregateID: aggregate, Payload: json.RawMessage(`{"n":2}`), TraceID: "trace-2"},
		{Name: "test.third", AggregateID: aggregate, Payload: json.RawMessage(`{"n":3}`)},
	}

	if err := outbox.Publish(ctx, events...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rows, err := db.Q(ctx).Query(ctx, `
		SELECT event_name, payload::text, coalesce(trace_id,''), status, occurred_at
		FROM outbox_events WHERE aggregate_id = $1 ORDER BY event_name`, aggregate)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()

	type row struct {
		name, payload, trace, status string
		occurred                     time.Time
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.payload, &r.trace, &r.status, &r.occurred); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}

	if len(got) != 3 {
		t.Fatalf("wrote %d events, want 3 — a batched insert that drops rows is the "+
			"failure mode this test exists for", len(got))
	}

	wantNames := []string{"test.first", "test.second", "test.third"}
	for i, w := range wantNames {
		if got[i].name != w {
			t.Errorf("event %d name = %q, want %q", i, got[i].name, w)
		}
		if got[i].status != "pending" {
			t.Errorf("event %d status = %q, want pending — the relay only picks up pending", i, got[i].status)
		}
		if got[i].occurred.IsZero() {
			t.Errorf("event %d has a zero occurred_at; the relay orders by it and the "+
				"lag gauge measures from it", i)
		}
	}

	// Payloads must not be shuffled between rows. unnest zips arrays positionally,
	// so an off-by-one in how the slices are built would pair the wrong payload with
	// the wrong event name — and every field would still be individually valid.
	for i, w := range []string{`{"n": 1}`, `{"n": 2}`, `{"n": 3}`} {
		if got[i].payload != w {
			t.Errorf("event %q has payload %s, want %s — arrays are misaligned",
				got[i].name, got[i].payload, w)
		}
	}

	// An absent trace must be empty, not the previous row's value.
	if got[2].trace != "" {
		t.Errorf("third event trace = %q, want empty", got[2].trace)
	}
}

func TestPublishAssignsMissingIDsAndTimestamps(t *testing.T) {
	db := testutil.DB(t)
	outbox := eventbus.NewOutbox(db)
	ctx := context.Background()

	aggregate := id.New()
	// Neither ID nor OccurredAt set — Publish fills both. In the loop version this
	// happened per iteration; in the batched version it happens while building the
	// arrays, which is easy to drop in a rewrite.
	if err := outbox.Publish(ctx, eventbus.Event{
		Name: "test.defaults", AggregateID: aggregate, Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var eventID id.ID
	var occurred time.Time
	if err := db.Q(ctx).QueryRow(ctx,
		`SELECT id, occurred_at FROM outbox_events WHERE aggregate_id = $1`, aggregate,
	).Scan(&eventID, &occurred); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if eventID == id.Nil {
		t.Error("id was not assigned; a nil primary key would violate the PK constraint")
	}
	if occurred.IsZero() {
		t.Error("occurred_at was not assigned")
	}
	if time.Since(occurred) > time.Minute {
		t.Errorf("occurred_at is %v old; looks like a zero value that survived", time.Since(occurred))
	}
}

func TestPublishWithNoEventsIsANoop(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	// Must not send a statement with empty arrays, and must not error. Called on
	// every write path that happens to produce no events.
	if err := eventbus.NewOutbox(db).Publish(ctx); err != nil {
		t.Fatalf("publishing nothing should be a no-op: %v", err)
	}
}

func TestPublishHandlesALargeBatch(t *testing.T) {
	/*
		The parameter-limit case. A multi-row VALUES list with seven columns hits the
		65535-placeholder protocol ceiling at ~9300 events; unnest passes six arrays
		regardless of size, so this is the test that proves the approach scales rather
		than just being tidier.
	*/
	db := testutil.DB(t)
	ctx := context.Background()
	aggregate := id.New()

	const n = 2000
	events := make([]eventbus.Event, n)
	for i := range events {
		events[i] = eventbus.Event{
			Name: "test.bulk", AggregateID: aggregate, Payload: json.RawMessage(`{}`),
		}
	}

	if err := eventbus.NewOutbox(db).Publish(ctx, events...); err != nil {
		t.Fatalf("publishing %d events: %v", n, err)
	}

	var count int
	if err := db.Q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE aggregate_id = $1`, aggregate).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Errorf("wrote %d of %d events", count, n)
	}
}
