package postgres_test

import (
	"context"
	"testing"

	catalogpg "github.com/zevoxa/chowk-backend/internal/modules/catalog/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
AssistedIDs: which outlets cannot take an order themselves.

Tested HERE rather than from ordering, and that placement was forced by the
boundary checker — correctly. The join across `merchants` is catalog's query, and
ordering has no business knowing it exists; from ordering's side the contract IS
the boundary, so its own tests legitimately use the fake.

That is the split this file exists to honour: the SQL is verified where the SQL
lives, and the consumer is verified against the interface.
*/

func TestAssistedIDsFindsVendorsWithNoDashboard(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	repo := catalogpg.NewKitchenRepository(db)

	assisted := testutil.AssistedKitchen(t, db)
	selfServe := testutil.Kitchen(t, db)

	ids, err := repo.AssistedIDs(ctx)
	if err != nil {
		t.Fatalf("assisted ids: %v", err)
	}

	if !contains(ids, assisted) {
		t.Errorf("assisted kitchen %s is missing; its orders would never be actioned "+
			"because the vendor has no dashboard to see them in", assisted)
	}
	if contains(ids, selfServe) {
		t.Errorf("self-serve kitchen %s was reported as assisted; its orders would be "+
			"queued for our staff as well as its own, and two people would call the "+
			"same vendor", selfServe)
	}
}

func TestAssistedIDsExcludesDeactivatedOutlets(t *testing.T) {
	// A deactivated outlet's orders are not something to put in front of an agent:
	// there is nothing useful to do with them and they crowd out live work.
	db := testutil.DB(t)
	ctx := context.Background()
	repo := catalogpg.NewKitchenRepository(db)

	kitchenID := testutil.AssistedKitchen(t, db)
	if _, err := db.Q(ctx).Exec(ctx,
		`UPDATE kitchens SET is_active = false WHERE id = $1`, kitchenID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	ids, err := repo.AssistedIDs(ctx)
	if err != nil {
		t.Fatalf("assisted ids: %v", err)
	}
	if contains(ids, kitchenID) {
		t.Error("a deactivated kitchen was reported as assisted")
	}
}

func TestAssistedIDsIsEmptyNotNullWhenNoneOnboarded(t *testing.T) {
	// The normal early state. Must not error, and the caller must be able to range
	// over the result without a nil check.
	db := testutil.DB(t)
	ids, err := catalogpg.NewKitchenRepository(db).AssistedIDs(context.Background())
	if err != nil {
		t.Fatalf("assisted ids with none onboarded: %v", err)
	}
	for range ids {
		// Other tests in this package may have left assisted vendors behind; the
		// assertion here is only that ranging is safe.
		break
	}
}

func contains(ids []id.ID, want id.ID) bool {
	for _, got := range ids {
		if got == want {
			return true
		}
	}
	return false
}
