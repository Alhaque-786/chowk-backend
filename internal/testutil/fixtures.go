package testutil

import (
	"context"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
Database fixtures: the minimum rows a test needs, created by the test.

WHY THIS EXISTS. Twelve promotion tests used to hardcode the seeder's kitchen id
with the comment "seeded Khansama", so they only passed on a database somebody had
run `cmd/seed` against. CI applies migrations and does not seed, so they failed
there with a foreign key violation that reads as a schema problem rather than a
missing fixture — and nobody had found out, because that repository has no remote
and CI has never run.

The rule these helpers exist to make easy: A TEST THAT NEEDS A ROW CREATES THAT ROW.
Seed data is DEVELOPMENT fixture data. It exists so a human can click around the
app, and it is free to change shape — rename a kitchen, drop one, change its
hours — without anybody thinking about a test file.

Every helper registers its own cleanup, in reverse dependency order, so a test
leaves the database as it found it. That matters more than it sounds: these run
against a shared development database, and rows left behind are the reason the
"outbox has dead-lettered events" alarm fires for no reason a week later.

Ids are random rather than fixed, so tests can run in parallel and repeatedly
without colliding on a unique index.
*/

// Merchant inserts a merchant and returns its id.
//
// Needed because kitchens.merchant_id is NOT NULL since the marketplace
// migration, so "just make a kitchen" is not possible.
func Merchant(t *testing.T, db *database.DB) id.ID {
	t.Helper()
	ctx := context.Background()
	merchantID := id.New()
	suffix := id.HumanCode(8)

	if _, err := db.Q(ctx).Exec(ctx, `
		INSERT INTO merchants (id, legal_name, display_name, slug, contact_phone)
		VALUES ($1, $2, $3, $4, '+910000000000')`,
		merchantID, "Test Foods Pvt Ltd", "Test Foods", "test-merchant-"+suffix,
	); err != nil {
		t.Fatalf("testutil: create merchant: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.Q(c).Exec(c, `DELETE FROM merchants WHERE id = $1`, merchantID)
	})
	return merchantID
}

// AssistedKitchen inserts a kitchen whose merchant cannot take orders itself.
//
// The vendor archetype is a cart selling fresh juice: no dashboard, no app, no
// intention of acquiring either. Orders for this outlet only ever reach the ops
// queue, so a test asserting that routing needs one of these.
func AssistedKitchen(t *testing.T, db *database.DB) id.ID {
	t.Helper()
	ctx := context.Background()
	kitchenID := Kitchen(t, db)

	if _, err := db.Q(ctx).Exec(ctx, `
		UPDATE merchants SET operating_mode = 'assisted'
		WHERE id = (SELECT merchant_id FROM kitchens WHERE id = $1)`, kitchenID,
	); err != nil {
		t.Fatalf("testutil: mark merchant assisted: %v", err)
	}
	return kitchenID
}

// Kitchen inserts a kitchen owned by a fresh merchant.
//
// Coordinates are Lucknow, matching the launch city, so a distance or
// serves-location check behaves the way it would in production.
func Kitchen(t *testing.T, db *database.DB) id.ID {
	t.Helper()
	ctx := context.Background()
	merchantID := Merchant(t, db)
	kitchenID := id.New()

	if _, err := db.Q(ctx).Exec(ctx, `
		INSERT INTO kitchens (id, merchant_id, name, slug, latitude, longitude, city)
		VALUES ($1, $2, $3, $4, 26.85, 80.947, 'Lucknow')`,
		kitchenID, merchantID, "Test Kitchen", "test-kitchen-"+id.HumanCode(8),
	); err != nil {
		t.Fatalf("testutil: create kitchen: %v", err)
	}

	// Registered BEFORE the merchant's cleanup runs, and t.Cleanup is LIFO, so the
	// kitchen is deleted first and the merchant's FK is satisfied.
	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.Q(c).Exec(c, `DELETE FROM kitchens WHERE id = $1`, kitchenID)
	})
	return kitchenID
}

// User inserts a customer and returns its id.
func User(t *testing.T, db *database.DB) id.ID {
	t.Helper()
	ctx := context.Background()
	userID := id.New()
	suffix := id.HumanCode(8)

	if _, err := db.Q(ctx).Exec(ctx, `
		INSERT INTO users (id, phone, full_name, status, referral_code)
		VALUES ($1, $2, 'Test Customer', 'active', $3)`,
		// Digits only, and unique per fixture. Derived from the id rather than a
		// counter so parallel tests cannot collide.
		userID, "+9199"+digits(userID, 8), suffix,
	); err != nil {
		t.Fatalf("testutil: create user: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.Q(c).Exec(c, `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID
}

// digits renders n decimal digits that are unique per call.
//
// READS THE UUID FROM THE END, NOT THE START, and that is the whole point.
// id.New() is UUIDv7, which leads with a millisecond timestamp — so ids generated
// back to back share their leading characters. Taking the FIRST digits produced
// identical output for five consecutive ids, which in a phone-number column meant
// two fixtures silently became the same user.
//
// That is exactly how it was found: an auth test registered one phone, generated a
// "nonexistent" phone the same way, and the login for the nonexistent phone
// succeeded — because it was the same number. It read as an authentication bypass
// for a minute. The random half of a v7 lives at the end.
func digits(from id.ID, n int) string {
	raw := from.String()
	out := make([]byte, 0, n)
	for i := len(raw) - 1; i >= 0 && len(out) < n; i-- {
		if raw[i] >= '0' && raw[i] <= '9' {
			out = append(out, raw[i])
		}
	}
	for len(out) < n {
		out = append(out, '0')
	}
	return string(out)
}

// OrderOpts tunes the fixture order. The zero value is a valid pending-payment
// order, which is what most tests want.
type OrderOpts struct {
	Status        string
	PaymentMethod string
	TotalMinor    int64
}

// Order inserts an order for the given user and kitchen.
//
// Returns the id. Callers that need the row's other columns should read them back
// through the repository under test rather than trusting this helper — the point of
// a fixture is to satisfy foreign keys, not to duplicate production write logic.
func Order(t *testing.T, db *database.DB, userID, kitchenID id.ID, opts OrderOpts) id.ID {
	t.Helper()
	ctx := context.Background()

	if opts.Status == "" {
		opts.Status = "pending_payment"
	}
	if opts.PaymentMethod == "" {
		opts.PaymentMethod = "upi"
	}

	orderID := id.New()
	if _, err := db.Q(ctx).Exec(ctx, `
		INSERT INTO orders (
			id, number, user_id, kitchen_id, status, payment_method,
			subtotal_minor, total_minor, currency, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 'INR', 1, now(), now())`,
		orderID, "TST-"+id.HumanCode(8), userID, kitchenID,
		opts.Status, opts.PaymentMethod, opts.TotalMinor,
	); err != nil {
		t.Fatalf("testutil: create order: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		// order_status_events and payments both reference the order, so they go
		// first. Cleanup is best-effort: a failure here must not mask the test's
		// own result.
		_, _ = db.Q(c).Exec(c, `DELETE FROM order_status_events WHERE order_id = $1`, orderID)
		_, _ = db.Q(c).Exec(c, `DELETE FROM payments WHERE order_id = $1`, orderID)
		_, _ = db.Q(c).Exec(c, `DELETE FROM order_lines WHERE order_id = $1`, orderID)
		_, _ = db.Q(c).Exec(c, `DELETE FROM orders WHERE id = $1`, orderID)
	})
	return orderID
}
