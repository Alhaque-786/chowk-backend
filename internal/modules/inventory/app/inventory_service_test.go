package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	inventorypg "github.com/zevoxa/chowk-backend/internal/modules/inventory/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/app"
	"github.com/zevoxa/chowk-backend/internal/modules/inventory/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

// fixture is a kitchen with one dish and a known amount of stock.
type fixture struct {
	db        *database.DB
	service   *app.InventoryService
	kitchenID id.ID
	menuItem  id.ID
	rice      id.ID
	orderIDs  []id.ID
}

// setup creates real rows and returns a cleanup function.
//
// It COMMITS rather than working inside a rolled-back transaction, because the
// point of these tests is genuine concurrency: two transactions must be able to
// see and block on each other, which is impossible inside a single one.
func setup(t *testing.T, riceKg float64, orderCount int) *fixture {
	t.Helper()

	db := testutil.DB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	f := &fixture{
		db:        db,
		kitchenID: id.MustParse("00000000-0000-0000-0000-0000000000a1"), // seeded Khansama
		menuItem:  id.New(),
		rice:      id.New(),
	}

	ingredients := inventorypg.NewIngredientRepository(db)
	recipes := inventorypg.NewRecipeRepository(db)
	reservations := inventorypg.NewReservationRepository(db)

	f.service = app.NewInventoryService(db, ingredients, recipes, reservations,
		testutil.NewRecordingPublisher(), clock.System{})

	// A menu item is needed because recipes reference it by foreign key.
	if _, err := db.Q(ctx).Exec(ctx, `
		INSERT INTO menu_items (id, kitchen_id, name, price_minor, tax_bps, food_type,
		                        allergens, serves, is_available, prep_minutes, version,
		                        created_at, updated_at)
		VALUES ($1,$2,'Test Dish',10000,500,'veg','{}',1,true,10,1,now(),now())`,
		f.menuItem, f.kitchenID); err != nil {
		t.Skipf("cannot create menu item (is the DB seeded?): %v", err)
	}

	if err := ingredients.Create(ctx, &domain.Ingredient{
		ID: f.rice, KitchenID: f.kitchenID, Name: "Test Rice",
		SKU: "TEST-" + id.HumanCode(6), Unit: domain.UnitKilogram,
		OnHand: riceKg, ReorderLevel: 0, UnitCost: money.FromRupees(120),
		IsActive: true, Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create ingredient: %v", err)
	}

	// 0.300 kg per serving, so riceKg/0.3 servings are possible.
	if err := recipes.Upsert(ctx, &domain.RecipeLine{
		MenuItemID: f.menuItem, IngredientID: f.rice, Quantity: 0.300,
	}); err != nil {
		t.Fatalf("create recipe: %v", err)
	}

	// Real orders, because stock_reservations.order_id has a foreign key to them.
	// That FK is exactly what caught a production bug in checkout, so the test
	// must honour it rather than work around it.
	for i := 0; i < orderCount; i++ {
		orderID := id.New()
		if _, err := db.Q(ctx).Exec(ctx, `
			INSERT INTO orders (id, number, user_id, kitchen_id, status, payment_method,
			                    subtotal_minor, total_minor, currency, version, created_at, updated_at)
			VALUES ($1,$2,(SELECT id FROM users LIMIT 1),$3,'pending_payment','cod',0,0,'INR',1,now(),now())`,
			orderID, "TEST-"+id.HumanCode(8), f.kitchenID); err != nil {
			t.Skipf("cannot create test order (is there a user?): %v", err)
		}
		f.orderIDs = append(f.orderIDs, orderID)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// Order matters: children before parents.
		_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM stock_reservations WHERE ingredient_id = $1`, f.rice)
		_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM stock_movements WHERE ingredient_id = $1`, f.rice)
		_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM recipes WHERE menu_item_id = $1`, f.menuItem)
		for _, orderID := range f.orderIDs {
			_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM orders WHERE id = $1`, orderID)
		}
		_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM ingredients WHERE id = $1`, f.rice)
		_, _ = db.Q(cleanupCtx).Exec(cleanupCtx, `DELETE FROM menu_items WHERE id = $1`, f.menuItem)
	})

	return f
}

func (f *fixture) reserve(ctx context.Context, orderID id.ID, servings int32) error {
	return f.db.InTx(ctx, func(ctx context.Context) error {
		return f.service.Reserve(ctx, orderID, f.kitchenID,
			[]contracts.ReservationLine{{MenuItemID: f.menuItem, Quantity: servings}})
	})
}

func (f *fixture) available(t *testing.T) float64 {
	t.Helper()
	var onHand, reserved float64
	if err := f.db.Q(context.Background()).QueryRow(context.Background(),
		`SELECT on_hand, reserved FROM ingredients WHERE id = $1`, f.rice).Scan(&onHand, &reserved); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	return onHand - reserved
}

// TestConcurrentReservationsDoNotOversell is THE test for this module.
//
// Three goroutines reserve one serving each, simultaneously, against stock that
// only covers two. Exactly two must succeed. This is the guarantee the whole
// module exists to provide, and asserting it in a comment is not the same as
// proving it.
//
// It must run with -race and against real Postgres: the serialisation comes from
// SELECT ... FOR UPDATE, which no fake can reproduce.
func TestConcurrentReservationsDoNotOversell(t *testing.T) {
	const servings = 2
	f := setup(t, 0.600, 3) // 0.600 kg / 0.300 per serving = exactly 2

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		conflicts int
		otherErrs []error
	)

	// A start barrier makes the goroutines contend as tightly as possible, rather
	// than drifting apart as they are spawned.
	start := make(chan struct{})

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(orderID id.ID) {
			defer wg.Done()
			<-start

			err := f.reserve(context.Background(), orderID, 1)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case apperr.KindOf(err) == apperr.KindConflict:
				conflicts++
			default:
				otherErrs = append(otherErrs, err)
			}
		}(f.orderIDs[i])
	}

	close(start)
	wg.Wait()

	for _, err := range otherErrs {
		t.Errorf("unexpected error (not a stock conflict): %v", err)
	}

	if succeeded != servings {
		t.Errorf("%d reservations succeeded, want exactly %d — stock was oversold or under-allocated",
			succeeded, servings)
	}
	if conflicts != 3-servings {
		t.Errorf("%d conflicts, want %d", conflicts, 3-servings)
	}

	// The balance must be exactly consumed, never negative. The DB CHECK
	// constraint would reject a negative, so this also proves no write was lost.
	if got := f.available(t); got > 0.0005 || got < -0.0005 {
		t.Errorf("available stock = %.3f after full allocation, want 0", got)
	}
}

// TestConcurrentReservationsWithLargerQuantities checks the aggregation path
// under contention, not just single servings.
func TestConcurrentReservationsWithLargerQuantities(t *testing.T) {
	// 1.500 kg = 5 servings. Two orders want 3 each, so exactly one can win.
	f := setup(t, 1.500, 2)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	start := make(chan struct{})

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(orderID id.ID) {
			defer wg.Done()
			<-start
			if err := f.reserve(context.Background(), orderID, 3); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(f.orderIDs[i])
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d reservations of 3 servings succeeded against 5 servings of stock, want 1", succeeded)
	}
	// 5 - 3 = 2 servings left = 0.600 kg.
	if got, want := f.available(t), 0.600; got < want-0.0005 || got > want+0.0005 {
		t.Errorf("available = %.3f, want %.3f", got, want)
	}
}

// TestReserveIsAtomicWithinTheCallersTransaction proves a rollback undoes the
// hold.
//
// This is what lets checkout call Reserve inside its own transaction: if anything
// later in checkout fails, the reservation disappears with it and no stock is
// orphaned.
func TestReserveIsAtomicWithinTheCallersTransaction(t *testing.T) {
	f := setup(t, 0.600, 1)
	before := f.available(t)

	sentinel := apperr.Invalid("TEST_ABORT", "deliberate failure after reserving")

	err := f.db.InTx(context.Background(), func(ctx context.Context) error {
		if err := f.service.Reserve(ctx, f.orderIDs[0], f.kitchenID,
			[]contracts.ReservationLine{{MenuItemID: f.menuItem, Quantity: 1}}); err != nil {
			return err
		}
		// Simulate a later checkout step failing.
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("expected the sentinel error, got %v", err)
	}

	if got := f.available(t); got != before {
		t.Errorf("available = %.3f after rollback, want %.3f — the reservation was not rolled back", got, before)
	}

	var held int
	if err := f.db.Q(context.Background()).QueryRow(context.Background(),
		`SELECT count(*) FROM stock_reservations WHERE order_id = $1`, f.orderIDs[0]).Scan(&held); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if held != 0 {
		t.Errorf("%d reservation rows survived a rolled-back transaction, want 0", held)
	}
}

// TestReleaseReturnsStock covers the cancellation path.
func TestReleaseReturnsStock(t *testing.T) {
	f := setup(t, 0.600, 1)
	ctx := context.Background()

	if err := f.reserve(ctx, f.orderIDs[0], 1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got, want := f.available(t), 0.300; got < want-0.0005 || got > want+0.0005 {
		t.Fatalf("available after reserve = %.3f, want %.3f", got, want)
	}

	if err := f.db.InTx(ctx, func(ctx context.Context) error {
		return f.service.Release(ctx, f.orderIDs[0])
	}); err != nil {
		t.Fatalf("release: %v", err)
	}

	if got, want := f.available(t), 0.600; got < want-0.0005 || got > want+0.0005 {
		t.Errorf("available after release = %.3f, want %.3f", got, want)
	}

	// Release must be idempotent: it runs from cancellation AND from the reaper,
	// and both can legitimately fire for the same order.
	if err := f.db.InTx(ctx, func(ctx context.Context) error {
		return f.service.Release(ctx, f.orderIDs[0])
	}); err != nil {
		t.Errorf("second Release returned an error; it must be idempotent: %v", err)
	}
	if got, want := f.available(t), 0.600; got < want-0.0005 || got > want+0.0005 {
		t.Errorf("available after double release = %.3f, want %.3f — stock was returned twice", got, want)
	}
}

// TestConsumeDropsOnHandAndWritesLedger covers the second phase of the hold.
func TestConsumeDropsOnHandAndWritesLedger(t *testing.T) {
	f := setup(t, 0.600, 1)
	ctx := context.Background()

	if err := f.reserve(ctx, f.orderIDs[0], 1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.db.InTx(ctx, func(ctx context.Context) error {
		return f.service.Consume(ctx, f.orderIDs[0])
	}); err != nil {
		t.Fatalf("consume: %v", err)
	}

	var onHand, reserved float64
	if err := f.db.Q(ctx).QueryRow(ctx,
		`SELECT on_hand, reserved FROM ingredients WHERE id = $1`, f.rice).Scan(&onHand, &reserved); err != nil {
		t.Fatalf("read stock: %v", err)
	}

	// on_hand drops (the food is cooked) and the hold is discharged.
	if want := 0.300; onHand < want-0.0005 || onHand > want+0.0005 {
		t.Errorf("on_hand = %.3f, want %.3f", onHand, want)
	}
	if reserved > 0.0005 {
		t.Errorf("reserved = %.3f, want 0 — the hold was not discharged", reserved)
	}

	// The ledger entry is what makes "we should have X, the shelf has Y"
	// answerable later.
	var movements int
	var quantity float64
	if err := f.db.Q(ctx).QueryRow(ctx,
		`SELECT count(*), COALESCE(sum(quantity),0) FROM stock_movements
		 WHERE ingredient_id = $1 AND movement_type = 'consumption'`, f.rice).Scan(&movements, &quantity); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if movements != 1 {
		t.Errorf("%d consumption movements, want 1", movements)
	}
	// Signed: consumption is negative, so the balance is a plain SUM.
	if want := -0.300; quantity < want-0.0005 || quantity > want+0.0005 {
		t.Errorf("ledger quantity = %.3f, want %.3f (consumption must be negative)", quantity, want)
	}

	// Idempotent: the handler runs from an at-least-once event.
	if err := f.db.InTx(ctx, func(ctx context.Context) error {
		return f.service.Consume(ctx, f.orderIDs[0])
	}); err != nil {
		t.Errorf("second Consume returned an error; it must be idempotent: %v", err)
	}
	if err := f.db.Q(ctx).QueryRow(ctx,
		`SELECT count(*) FROM stock_movements WHERE ingredient_id = $1`, f.rice).Scan(&movements); err != nil {
		t.Fatalf("recount ledger: %v", err)
	}
	if movements != 1 {
		t.Errorf("%d movements after a duplicate Consume, want 1 — stock was consumed twice", movements)
	}
}

// TestReserveRejectsWhenShortAndNamesTheIngredient checks the error is actionable.
//
// "Out of stock" forces the kitchen to guess what to restock; naming the
// ingredient does not.
func TestReserveRejectsWhenShortAndNamesTheIngredient(t *testing.T) {
	f := setup(t, 0.200, 1) // less than one serving

	err := f.reserve(context.Background(), f.orderIDs[0], 1)
	if err == nil {
		t.Fatal("reserving more than the available stock succeeded")
	}
	if apperr.KindOf(err) != apperr.KindConflict {
		t.Errorf("error kind = %v, want conflict", apperr.KindOf(err))
	}

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an apperr: %v", err)
	}
	if appErr.Code != "INSUFFICIENT_STOCK" {
		t.Errorf("code = %q, want INSUFFICIENT_STOCK", appErr.Code)
	}
	if !contains(appErr.Message, "Test Rice") {
		t.Errorf("message %q does not name the ingredient that ran out", appErr.Message)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
