package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// testDB connects using the same config loader the app uses, so a test cannot
// pass against pool settings production would never run.
//
// Skips rather than fails when there is no database: `go test ./...` on a machine
// without Docker running should not be a wall of red.
func testDB(t *testing.T) *database.DB {
	t.Helper()

	_ = godotenv.Load("../../../../../.env")
	if os.Getenv("POSTGRES_DSN") == "" {
		t.Skip("POSTGRES_DSN not set; skipping integration test")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// TestLoadModifierGroupsAttachesEveryGroup is a regression test for a real bug.
//
// An item with TWO modifier groups came back with modifiers attached to only one
// of them. The customer therefore saw a required "Choose portion" group with no
// options, making the dish impossible to order.
//
// The test creates the exact shape that broke: one item, two groups, each with
// its own modifiers.
func TestLoadModifierGroupsAttachesEveryGroup(t *testing.T) {
	db := testDB(t)
	repo := NewMenuRepository(db)
	kitchens := NewKitchenRepository(db)

	ctx := context.Background()
	now := time.Now().UTC()

	// Everything is created inside a transaction that is never committed, so the
	// test leaves no rows behind and can run repeatedly.
	err := db.InTx(ctx, func(ctx context.Context) error {
		merchantID := id.MustParse("00000000-0000-0000-0000-000000000001")

		kitchen := &domain.Kitchen{
			ID: id.New(), MerchantID: merchantID,
			Name: "Test Kitchen", Slug: "test-" + id.HumanCode(8),
			Latitude: 26.8467, Longitude: 80.9462, DeliveryRadiusM: 5000,
			IsActive: true, IsAcceptingOrders: true,
			PackagingFee: money.Zero(money.INR), MinOrder: money.Zero(money.INR),
			Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := kitchens.Create(ctx, kitchen); err != nil {
			return err
		}

		item := &domain.MenuItem{
			ID: id.New(), KitchenID: kitchen.ID, Name: "Test Biryani",
			Price: money.FromRupees(289), TaxBps: 500,
			PackagingFee: money.Zero(money.INR),
			FoodType:     domain.FoodNonVeg, Allergens: []string{}, Serves: 1,
			IsAvailable: true, PrepMinutes: 30, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.CreateItem(ctx, item); err != nil {
			return err
		}

		// Two groups, mirroring the shape that failed: a required single-select
		// and an optional multi-select.
		portion := &domain.ModifierGroup{
			ID: id.New(), KitchenID: kitchen.ID, Name: "Choose portion",
			MinSelect: 1, MaxSelect: 1, DisplayOrder: 1,
		}
		addons := &domain.ModifierGroup{
			ID: id.New(), KitchenID: kitchen.ID, Name: "Add-ons",
			MinSelect: 0, MaxSelect: 3, DisplayOrder: 2,
		}
		for _, g := range []*domain.ModifierGroup{portion, addons} {
			if err := repo.CreateModifierGroup(ctx, g); err != nil {
				return err
			}
			if err := repo.AttachModifierGroup(ctx, item.ID, g.ID); err != nil {
				return err
			}
		}

		mods := []*domain.Modifier{
			{ID: id.New(), GroupID: portion.ID, Name: "Half", Price: money.Zero(money.INR), IsAvailable: true, DisplayOrder: 1},
			{ID: id.New(), GroupID: portion.ID, Name: "Full", Price: money.FromRupees(140), IsAvailable: true, DisplayOrder: 2},
			{ID: id.New(), GroupID: addons.ID, Name: "Extra raita", Price: money.FromRupees(40), IsAvailable: true, DisplayOrder: 1},
		}
		for _, m := range mods {
			if err := repo.CreateModifier(ctx, m); err != nil {
				return err
			}
		}

		// --- the assertion ---
		byItem, err := repo.LoadModifierGroups(ctx, []id.ID{item.ID})
		if err != nil {
			return err
		}

		groups := byItem[item.ID]
		if len(groups) != 2 {
			t.Fatalf("got %d modifier groups, want 2", len(groups))
		}

		for _, g := range groups {
			if len(g.Modifiers) == 0 {
				t.Errorf("group %q came back with NO modifiers; a required group with no options makes the dish unorderable", g.Name)
			}
		}

		byName := map[string][]domain.Modifier{}
		for _, g := range groups {
			byName[g.Name] = g.Modifiers
		}
		if got := len(byName["Choose portion"]); got != 2 {
			t.Errorf("Choose portion has %d modifiers, want 2", got)
		}
		if got := len(byName["Add-ons"]); got != 1 {
			t.Errorf("Add-ons has %d modifiers, want 1", got)
		}

		// A zero-price modifier is legitimate ("Half" costs nothing extra) and
		// must not be dropped as if it were missing.
		for _, m := range byName["Choose portion"] {
			if m.Name == "Half" && !m.Price.IsZero() {
				t.Errorf("Half price = %s, want 0", m.Price)
			}
		}

		// Roll back: this is test data, not seed data.
		return errRollback
	})

	if err != nil && err != errRollback {
		t.Fatalf("transaction failed: %v", err)
	}
}

// errRollback aborts the test transaction so nothing is persisted.
var errRollback = errRollbackType{}

type errRollbackType struct{}

func (errRollbackType) Error() string { return "intentional test rollback" }

// TestItemPricesSurviveTheReadPath guards the money columns specifically.
//
// Written after prices came back as zero through the cache: the Amount type has
// unexported fields, so anything that loses them reads as free food.
func TestItemPricesSurviveTheReadPath(t *testing.T) {
	db := testDB(t)
	repo := NewMenuRepository(db)

	ctx := context.Background()

	// Read whatever the seeder created; this asserts the real read path end to end.
	const q = `SELECT id FROM menu_items WHERE price_minor > 0 LIMIT 1`
	var itemID id.ID
	if err := db.Q(ctx).QueryRow(ctx, q).Scan(&itemID); err != nil {
		t.Skip("no seeded menu items; run `make seed` first")
	}

	item, err := repo.FindItemByID(ctx, itemID)
	if err != nil {
		t.Fatalf("FindItemByID: %v", err)
	}
	if item.Price.Minor() <= 0 {
		t.Errorf("Price.Minor() = %d; a zero price means customers are charged nothing", item.Price.Minor())
	}
}
