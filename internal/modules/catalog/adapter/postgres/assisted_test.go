package postgres_test

import (
	"context"
	"testing"

	catalogpg "github.com/zevoxa/chowk-backend/internal/modules/catalog/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
The assisted-vendor lookup, tested where the SQL lives.

Ordering decides what to do with the answer — defer capture, route to the ops queue
— but it cannot import catalog (make check-boundaries forbids it, in test files
too), so its tests hand a fake a known answer. That leaves the join itself covered
only here.

BOTH FORMS, because they are separate queries that must agree: a plural list for the
ops queue and a single EXISTS for the checkout path. Disagreement means an order
that defers capture while never appearing in the queue that would capture it —
food delivered and never charged for.
*/
func TestAssistedLookupAgreesBetweenBothForms(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	repo := catalogpg.NewKitchenRepository(db)

	assisted := testutil.AssistedKitchen(t, db)
	selfServe := testutil.Kitchen(t, db)

	yes, err := repo.IsAssisted(ctx, assisted)
	if err != nil {
		t.Fatalf("IsAssisted: %v", err)
	}
	if !yes {
		t.Error("an assisted merchant's kitchen reported as self-serve; its orders " +
			"would capture immediately and never reach the ops queue")
	}

	no, err := repo.IsAssisted(ctx, selfServe)
	if err != nil {
		t.Fatalf("IsAssisted: %v", err)
	}
	if no {
		t.Error("a self-serve kitchen reported as assisted; its payments would be " +
			"authorised and left uncaptured until somebody noticed")
	}

	ids, err := repo.AssistedIDs(ctx)
	if err != nil {
		t.Fatalf("AssistedIDs: %v", err)
	}
	var sawAssisted, sawSelfServe bool
	for _, k := range ids {
		switch k {
		case assisted:
			sawAssisted = true
		case selfServe:
			sawSelfServe = true
		}
	}
	if !sawAssisted {
		t.Error("AssistedIDs omitted a kitchen that IsAssisted confirmed — the two " +
			"queries disagree, which is the failure that loses money silently")
	}
	if sawSelfServe {
		t.Error("AssistedIDs included a self-serve kitchen")
	}
}

func TestDeactivatingAKitchenRemovesItFromTheOpsQueue(t *testing.T) {
	// A deactivated outlet's orders are not something an agent should be asked to
	// phone about. IsAssisted still answers truthfully — the merchant's mode has not
	// changed — but the queue must not surface it.
	db := testutil.DB(t)
	ctx := context.Background()
	repo := catalogpg.NewKitchenRepository(db)

	assisted := testutil.AssistedKitchen(t, db)
	if _, err := db.Q(ctx).Exec(ctx,
		`UPDATE kitchens SET is_active = false WHERE id = $1`, assisted); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	ids, err := repo.AssistedIDs(ctx)
	if err != nil {
		t.Fatalf("AssistedIDs: %v", err)
	}
	for _, k := range ids {
		if k == assisted {
			t.Error("a deactivated kitchen is still in the ops queue")
		}
	}
}
