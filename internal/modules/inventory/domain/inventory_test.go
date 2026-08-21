package domain

import (
	"testing"

	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

var (
	rice    = id.MustParse("00000000-0000-0000-0000-0000000000e1")
	chicken = id.MustParse("00000000-0000-0000-0000-0000000000e2")
	mint    = id.MustParse("00000000-0000-0000-0000-0000000000e3")

	biryani = id.MustParse("00000000-0000-0000-0000-0000000000f1")
	pulao   = id.MustParse("00000000-0000-0000-0000-0000000000f2")
)

// TestExplodeBOMAggregatesAcrossDishes is the test this whole file exists for.
//
// THE BUG IT PREVENTS: two dishes, or two lines of the same dish, both need rice.
// Checking each line independently passes twice against the same portion and
// oversells. Summing per ingredient BEFORE comparing against stock is the fix,
// and it is the single most common inventory bug there is.
func TestExplodeBOMAggregatesAcrossDishes(t *testing.T) {
	recipes := map[id.ID][]RecipeLine{
		biryani: {
			{IngredientID: rice, Quantity: 0.300, IngredientName: "Rice", Unit: UnitKilogram},
			{IngredientID: chicken, Quantity: 0.250, IngredientName: "Chicken", Unit: UnitKilogram},
		},
		pulao: {
			// Pulao ALSO uses rice. This is the overlap that must sum.
			{IngredientID: rice, Quantity: 0.200, IngredientName: "Rice", Unit: UnitKilogram},
		},
	}

	// 2 biryani + 3 pulao.
	requirements := ExplodeBOM(map[id.ID]int32{biryani: 2, pulao: 3}, recipes)

	got := make(map[id.ID]float64, len(requirements))
	for _, req := range requirements {
		got[req.IngredientID] = req.Quantity
	}

	// Rice: (0.300 × 2) + (0.200 × 3) = 0.600 + 0.600 = 1.200
	if want := 1.200; !approx(got[rice], want) {
		t.Errorf("rice = %.3f, want %.3f — requirements are not being aggregated across dishes", got[rice], want)
	}
	// Chicken: only biryani uses it. 0.250 × 2 = 0.500
	if want := 0.500; !approx(got[chicken], want) {
		t.Errorf("chicken = %.3f, want %.3f", got[chicken], want)
	}
	// One entry per ingredient, not per dish.
	if len(requirements) != 2 {
		t.Errorf("got %d requirements, want 2 (one per distinct ingredient)", len(requirements))
	}
}

// TestExplodeBOMAppliesWastage checks that expected loss is included.
//
// Ignoring wastage makes every food-cost figure optimistic and every reorder
// point too low, so the kitchen runs out earlier than the system predicts.
func TestExplodeBOMAppliesWastage(t *testing.T) {
	recipes := map[id.ID][]RecipeLine{
		biryani: {{IngredientID: rice, Quantity: 1.000, WastageBps: 1000}}, // 10% loss
	}

	requirements := ExplodeBOM(map[id.ID]int32{biryani: 1}, recipes)
	if len(requirements) != 1 {
		t.Fatalf("got %d requirements, want 1", len(requirements))
	}
	if want := 1.100; !approx(requirements[0].Quantity, want) {
		t.Errorf("quantity = %.3f, want %.3f (1.0 + 10%% wastage)", requirements[0].Quantity, want)
	}
}

// TestExplodeBOMOptionalityIsPessimistic covers a subtle aggregation rule.
//
// If mint is a garnish (optional) for one dish but essential for another, the
// AGGREGATE requirement must be non-optional. Otherwise a shortage excused by the
// garnish would silently block the dish that genuinely needs it.
func TestExplodeBOMOptionalityIsPessimistic(t *testing.T) {
	recipes := map[id.ID][]RecipeLine{
		biryani: {{IngredientID: mint, Quantity: 0.010, IsOptional: true}},  // garnish
		pulao:   {{IngredientID: mint, Quantity: 0.050, IsOptional: false}}, // essential
	}

	requirements := ExplodeBOM(map[id.ID]int32{biryani: 1, pulao: 1}, recipes)
	if len(requirements) != 1 {
		t.Fatalf("got %d requirements, want 1", len(requirements))
	}
	if requirements[0].IsOptional {
		t.Error("aggregate requirement is optional, but one dish needs the ingredient essentially")
	}
}

func TestExplodeBOMIgnoresItemsWithoutRecipes(t *testing.T) {
	// A bottled drink is stocked as a finished good and has no BOM. It must not
	// error, and must not invent a requirement.
	requirements := ExplodeBOM(
		map[id.ID]int32{biryani: 1},
		map[id.ID][]RecipeLine{}, // no recipes at all
	)
	if len(requirements) != 0 {
		t.Errorf("got %d requirements for an item with no recipe, want 0", len(requirements))
	}
}

func TestExplodeBOMIgnoresZeroQuantities(t *testing.T) {
	recipes := map[id.ID][]RecipeLine{
		biryani: {{IngredientID: rice, Quantity: 0.300}},
	}
	if got := ExplodeBOM(map[id.ID]int32{biryani: 0}, recipes); len(got) != 0 {
		t.Errorf("zero servings produced %d requirements, want 0", len(got))
	}
}

// TestServingsAvailableTakesTheMinimum is what drives automatic sold-out.
//
// A dish needs ALL its ingredients, so the scarcest one is the limit. Taking an
// average or a maximum would let the menu advertise dishes that cannot be cooked.
func TestServingsAvailableTakesTheMinimum(t *testing.T) {
	recipes := []RecipeLine{
		{IngredientID: rice, Quantity: 0.300},
		{IngredientID: chicken, Quantity: 0.250},
	}
	stock := map[id.ID]*Ingredient{
		// Rice allows 10 servings...
		rice: {ID: rice, OnHand: 3.000, Reserved: 0},
		// ...but chicken only allows 2. The answer is 2.
		chicken: {ID: chicken, OnHand: 0.500, Reserved: 0},
	}

	if got := ServingsAvailable(recipes, stock); got != 2 {
		t.Errorf("ServingsAvailable = %d, want 2 (limited by the scarcest ingredient)", got)
	}
}

// TestServingsAvailableUsesAvailableNotOnHand: stock already promised to open
// orders cannot be promised again.
func TestServingsAvailableUsesAvailableNotOnHand(t *testing.T) {
	recipes := []RecipeLine{{IngredientID: rice, Quantity: 0.300}}
	stock := map[id.ID]*Ingredient{
		// 0.900 on hand, but 0.600 is already reserved -> only 0.300 free -> 1 serving.
		rice: {ID: rice, OnHand: 0.900, Reserved: 0.600},
	}

	if got := ServingsAvailable(recipes, stock); got != 1 {
		t.Errorf("ServingsAvailable = %d, want 1; reserved stock must not be promised twice", got)
	}
}

func TestServingsAvailableSkipsOptionalIngredients(t *testing.T) {
	recipes := []RecipeLine{
		{IngredientID: rice, Quantity: 0.300},
		{IngredientID: mint, Quantity: 0.010, IsOptional: true},
	}
	stock := map[id.ID]*Ingredient{
		rice: {ID: rice, OnHand: 0.600},
		mint: {ID: mint, OnHand: 0}, // garnish exhausted
	}

	// Running out of a garnish must NOT take the dish off the menu.
	if got := ServingsAvailable(recipes, stock); got != 2 {
		t.Errorf("ServingsAvailable = %d, want 2; an exhausted optional ingredient must not block the dish", got)
	}
}

// TestServingsAvailableFailsClosedOnMissingIngredient: a recipe referencing an
// unknown ingredient is a data error. Reporting zero surfaces it as sold-out and
// gets it investigated, rather than guessing and overselling.
func TestServingsAvailableFailsClosedOnMissingIngredient(t *testing.T) {
	recipes := []RecipeLine{{IngredientID: rice, Quantity: 0.300}}

	if got := ServingsAvailable(recipes, map[id.ID]*Ingredient{}); got != 0 {
		t.Errorf("ServingsAvailable = %d, want 0 when an ingredient row is missing", got)
	}
}

// TestServingsAvailableUnlimitedWithoutRecipe: -1 means "not governed by
// ingredients", which is different from 0 (sold out).
func TestServingsAvailableUnlimitedWithoutRecipe(t *testing.T) {
	if got := ServingsAvailable(nil, map[id.ID]*Ingredient{}); got != -1 {
		t.Errorf("ServingsAvailable = %d, want -1 (no BOM means inventory does not limit it)", got)
	}
}

func TestIngredientAvailableAndReorder(t *testing.T) {
	i := &Ingredient{OnHand: 5.000, Reserved: 1.500, ReorderLevel: 2.000}

	if want := 3.500; !approx(i.Available(), want) {
		t.Errorf("Available() = %.3f, want %.3f", i.Available(), want)
	}
	if i.NeedsReorder() {
		t.Error("NeedsReorder() is true at 5.0 on hand with a 2.0 reorder level")
	}

	// Reorder triggers on ON HAND, not on available: what to buy depends on what
	// physically exists, not on what is promised.
	i.OnHand = 1.500
	if !i.NeedsReorder() {
		t.Error("NeedsReorder() is false at 1.5 on hand with a 2.0 reorder level")
	}
}

func TestRecipeLineRequiredFor(t *testing.T) {
	line := RecipeLine{Quantity: 0.250, WastageBps: 500} // 5% wastage

	// 0.250 × 4 × 1.05 = 1.050
	if got, want := line.RequiredFor(4), 1.050; !approx(got, want) {
		t.Errorf("RequiredFor(4) = %.4f, want %.4f", got, want)
	}
	if got := line.RequiredFor(0); got != 0 {
		t.Errorf("RequiredFor(0) = %.4f, want 0", got)
	}
}

// approx compares floats with gram-level tolerance, matching the NUMERIC(14,3)
// precision the database stores.
func approx(a, b float64) bool {
	const epsilon = 0.0005
	d := a - b
	return d < epsilon && d > -epsilon
}
