// Command seed creates realistic local development data: the Khansama kitchen,
// its opening hours, and a menu.
//
// Why a Go program rather than a SQL file: it goes through the repositories, so
// it exercises the same validation and constraints the API does. A SQL seed can
// insert rows the application would reject, and then local development is
// testing against data that could never exist in production.
//
// Idempotent -- safe to run repeatedly. It skips anything already present rather
// than erroring or duplicating.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/modules/catalog"
	catalogdomain "github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Fixed IDs so the seeder is idempotent and so a developer can bookmark a menu
// URL that survives a re-seed.
var (
	khansamaKitchenID = id.MustParse("00000000-0000-0000-0000-0000000000a1")
	catBiryaniID      = id.MustParse("00000000-0000-0000-0000-0000000000b1")
	catKebabID        = id.MustParse("00000000-0000-0000-0000-0000000000b2")
	catBreadID        = id.MustParse("00000000-0000-0000-0000-0000000000b3")
	catDrinkID        = id.MustParse("00000000-0000-0000-0000-0000000000b4")
	groupPortionID    = id.MustParse("00000000-0000-0000-0000-0000000000c1")
	groupAddonID      = id.MustParse("00000000-0000-0000-0000-0000000000c2")
)

func main() {
	_ = godotenv.Load()

	force := flag.Bool("force", false, "re-create the kitchen even if it exists")
	flag.Parse()

	if err := run(*force); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

// existingNames runs a single-column name query and returns the results as a set.
//
// One query and a set membership test, rather than a lookup per row: a seeder
// with fifteen round trips to answer "does this already exist" is slower than the
// work it is guarding, and the set makes the skip decision obvious at the call
// site.
func existingNames(ctx context.Context, db *database.DB, q string, args ...any) (map[string]bool, error) {
	rows, err := db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("seed: list existing names: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("seed: scan name: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

func run(force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Refuse to run against production. A seeder that can create fake menu items
	// in a live kitchen is a foot-gun that only needs to fire once.
	if cfg.IsProduction() {
		return errors.New("refusing to seed a production database")
	}

	log := logger.New("info", "text")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()

	redis, err := cache.Connect(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer redis.Close()

	mod := catalog.New(catalog.Deps{
		Config: cfg,
		DB:     db,
		Cache:  redis,
		Events: eventbus.NewOutbox(db),
		Clock:  clock.System{},
	})

	kitchens := mod.KitchenRepo()
	menus := mod.MenuRepo()

	// --- Kitchen ---------------------------------------------------------
	existing, err := kitchens.FindByID(ctx, khansamaKitchenID)
	switch {
	case err == nil && !force:
		log.Info("kitchen already seeded, skipping", "kitchen", existing.Name)
	case err != nil && apperr.KindOf(err) != apperr.KindNotFound:
		return err
	default:
		now := time.Now().UTC()
		kitchen := &catalogdomain.Kitchen{
			ID:          khansamaKitchenID,
			MerchantID:  catalog.FirstPartyMerchantID,
			Name:        "Khansama",
			Slug:        "khansama",
			Description: "Slow-cooked Awadhi biryani and kebabs, made the way a khansama would.",
			// Served from the frontend's public/ directory, so a relative path is
			// correct -- same origin, no CDN, no remote-pattern config. These are
			// DEVELOPMENT placeholders: licensed, attributed, and not photographs
			// of this kitchen's food. See frontend/src/lib/dish-photos.generated.json.
			CoverImageURL: "/dishes/cover-mughlai.jpg",
			Cuisines:      []string{"Awadhi", "Mughlai", "Biryani", "North Indian"},
			Phone:         "+919876500000",
			Email:         "kitchen@khansama.in",

			AddressLine1: "Hazratganj",
			City:         "Lucknow",
			Pincode:      "226001",
			// Real Hazratganj coordinates, so distance and radius maths behave
			// like production rather than snapping to (0,0).
			Latitude:        26.8467,
			Longitude:       80.9462,
			DeliveryRadiusM: 7000,

			IsActive:          true,
			IsAcceptingOrders: true,

			CommissionBps:  0, // first-party: no commission to ourselves
			PackagingFee:   money.FromRupees(15),
			MinOrder:       money.FromRupees(149),
			AvgPrepMinutes: 25,

			FSSAILicense: "FIRST-PARTY",
			IsFirstParty: true,
			Version:      1,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := kitchen.Validate(); err != nil {
			return fmt.Errorf("kitchen invalid: %w", err)
		}
		if err := kitchens.Create(ctx, kitchen); err != nil {
			return err
		}
		log.Info("created kitchen", "name", kitchen.Name, "id", kitchen.ID)
	}

	// --- Opening hours ---------------------------------------------------
	// A split shift: lunch 11:00-15:00 and dinner 19:00-23:00, every day. Two
	// rows per weekday rather than one 11:00-23:00 shift, because the kitchen
	// genuinely closes in between and the customer should be told so.
	var hours []catalogdomain.OpeningHours
	for day := int16(0); day <= 6; day++ {
		hours = append(hours,
			catalogdomain.OpeningHours{
				KitchenID: khansamaKitchenID, DayOfWeek: day,
				OpensAt: 11 * time.Hour, ClosesAt: 15 * time.Hour,
			},
			catalogdomain.OpeningHours{
				KitchenID: khansamaKitchenID, DayOfWeek: day,
				OpensAt: 19 * time.Hour, ClosesAt: 23 * time.Hour,
			},
		)
	}
	if err := kitchens.ReplaceHours(ctx, khansamaKitchenID, hours); err != nil {
		return err
	}
	log.Info("set opening hours", "shifts", len(hours))

	// --- Categories ------------------------------------------------------
	categories := []catalogdomain.Category{
		{ID: catBiryaniID, Name: "Biryani", Description: "Sealed and slow-cooked on dum", DisplayOrder: 1},
		{ID: catKebabID, Name: "Kebabs", Description: "From the charcoal sigri", DisplayOrder: 2},
		{ID: catBreadID, Name: "Breads", DisplayOrder: 3},
		{ID: catDrinkID, Name: "Drinks", DisplayOrder: 4},
	}
	for i := range categories {
		c := &categories[i]
		c.KitchenID = khansamaKitchenID
		c.IsActive = true
		c.CreatedAt = time.Now().UTC()
		c.UpdatedAt = c.CreatedAt
		if err := menus.CreateCategory(ctx, c); err != nil {
			// Already seeded: an idempotent re-run should not fail.
			if apperr.KindOf(err) == apperr.KindInternal {
				log.Info("category exists, skipping", "name", c.Name)
				continue
			}
			return err
		}
	}
	log.Info("created categories", "count", len(categories))

	// --- Modifier groups -------------------------------------------------
	// Portion is a REQUIRED single choice (1,1): a biryani with no size has no
	// price. Add-ons are optional and multi-select (0,3).
	portionGroup := &catalogdomain.ModifierGroup{
		ID: groupPortionID, KitchenID: khansamaKitchenID, Name: "Choose portion",
		MinSelect: 1, MaxSelect: 1, DisplayOrder: 1,
	}
	addonGroup := &catalogdomain.ModifierGroup{
		ID: groupAddonID, KitchenID: khansamaKitchenID, Name: "Add-ons",
		MinSelect: 0, MaxSelect: 3, DisplayOrder: 2,
	}
	for _, g := range []*catalogdomain.ModifierGroup{portionGroup, addonGroup} {
		if err := menus.CreateModifierGroup(ctx, g); err != nil {
			log.Info("modifier group exists, skipping", "name", g.Name)
		}
	}

	modifiers := []catalogdomain.Modifier{
		{GroupID: groupPortionID, Name: "Half (serves 1)", Price: money.FromRupees(0), DisplayOrder: 1},
		{GroupID: groupPortionID, Name: "Full (serves 2)", Price: money.FromRupees(140), DisplayOrder: 2},
		{GroupID: groupAddonID, Name: "Extra raita", Price: money.FromRupees(40), DisplayOrder: 1},
		{GroupID: groupAddonID, Name: "Extra salan", Price: money.FromRupees(35), DisplayOrder: 2},
		{GroupID: groupAddonID, Name: "Boiled egg", Price: money.FromRupees(25), DisplayOrder: 3},
	}
	// Existing modifier names, so a re-run does not duplicate them.
	//
	// This loop assigned id.New() on every run, so there was never a primary-key
	// conflict to skip on and each `make seed` appended another full set. Six runs
	// during one afternoon left 30 modifiers where there should be 5 -- and the
	// menu items below had exactly the same bug, which is how a development
	// database ends up with six of every dish.
	//
	// Checked by NAME rather than by a fixed UUID per row because the name is
	// what makes them duplicates to a human reading the menu, and hardcoding
	// fifteen UUIDs to express "already there" is a lot of noise for a seeder.
	existingModifiers, err := existingNames(ctx, db,
		`SELECT m.name FROM modifiers m
		 JOIN modifier_groups g ON g.id = m.group_id
		 WHERE g.kitchen_id = $1`, khansamaKitchenID)
	if err != nil {
		return err
	}

	addedModifiers := 0
	for i := range modifiers {
		m := &modifiers[i]
		if existingModifiers[m.Name] {
			continue
		}
		m.ID = id.New()
		m.IsAvailable = true
		if err := menus.CreateModifier(ctx, m); err != nil {
			return err
		}
		addedModifiers++
	}
	log.Info("seeded modifiers", "added", addedModifiers, "already_present", len(existingModifiers))

	// --- Menu items ------------------------------------------------------
	type seedItem struct {
		name        string
		description string
		image       string // '' = the UI falls back to a generated tile
		category    id.ID
		rupees      float64
		compare     float64 // 0 = no offer
		taxBps      int32
		foodType    catalogdomain.FoodType
		spice       int16
		prepMinutes int32
		recommended bool
		withPortion bool
		withAddons  bool
	}

	items := []seedItem{
		{"Chicken Dum Biryani", "Long-grain rice sealed with chicken and whole spices, cooked on dum.",
			"/dishes/chicken-biryani.jpg", catBiryaniID, 289, 349, 500, catalogdomain.FoodNonVeg, 2, 30, true, true, true},
		{"Mutton Dum Biryani", "Slow-cooked mutton with saffron and kewra.",
			"/dishes/mutton-biryani.jpg", catBiryaniID, 389, 0, 500, catalogdomain.FoodNonVeg, 2, 40, true, true, true},
		{"Subz Dum Biryani", "Seasonal vegetables layered with rice and fried onion.",
			"/dishes/thali.jpg", catBiryaniID, 219, 0, 500, catalogdomain.FoodVeg, 1, 25, false, true, true},
		{"Galouti Kebab", "The famously soft Lucknawi kebab, six pieces.",
			"/dishes/seekh-kebab.jpg", catKebabID, 259, 0, 500, catalogdomain.FoodNonVeg, 1, 20, true, false, false},
		{"Kakori Kebab", "Minced mutton on a skewer, char-grilled.",
			"/dishes/tandoori-platter.jpg", catKebabID, 279, 0, 500, catalogdomain.FoodNonVeg, 2, 22, false, false, false},
		{"Paneer Tikka", "Char-grilled cottage cheese with pickled onion.",
			"/dishes/paneer-tikka.jpg", catKebabID, 229, 0, 500, catalogdomain.FoodVeg, 1, 18, false, false, false},
		{"Warqi Paratha", "Flaky layered paratha, two pieces.",
			"/dishes/naan.jpg", catBreadID, 79, 0, 500, catalogdomain.FoodVeg, 0, 10, false, false, false},
		{"Sheermal", "Saffron-sweetened flatbread.",
			"/dishes/naan.jpg", catBreadID, 69, 0, 500, catalogdomain.FoodVeg, 0, 10, false, false, false},
		// 18% GST, not 5%: packaged beverages are taxed differently from prepared
		// food. This is exactly why tax_bps is per item.
		{"Masala Chaas", "Spiced buttermilk, 300ml.",
			"", catDrinkID, 59, 0, 1800, catalogdomain.FoodVeg, 0, 2, false, false, false},
		{"Bottled Water", "1 litre.",
			"", catDrinkID, 20, 0, 1800, catalogdomain.FoodVeg, 0, 1, false, false, false},
	}

	existingItems, err := existingNames(ctx, db,
		`SELECT name FROM menu_items WHERE kitchen_id = $1`, khansamaKitchenID)
	if err != nil {
		return err
	}

	created := 0
	skipped := 0
	for _, si := range items {
		// Same idempotency problem as the modifiers above: without this, every
		// run appended a second copy of the whole menu.
		if existingItems[si.name] {
			skipped++
			continue
		}

		now := time.Now().UTC()
		cat := si.category

		item := &catalogdomain.MenuItem{
			ID:            id.New(),
			KitchenID:     khansamaKitchenID,
			CategoryID:    &cat,
			Name:          si.name,
			Description:   si.description,
			ImageURL:      si.image,
			Price:         money.FromRupees(si.rupees),
			TaxBps:        si.taxBps,
			PackagingFee:  money.FromRupees(5),
			FoodType:      si.foodType,
			SpiceLevel:    si.spice,
			Allergens:     []string{},
			Serves:        1,
			IsAvailable:   true,
			PrepMinutes:   si.prepMinutes,
			DisplayOrder:  int32(created + 1),
			IsRecommended: si.recommended,
			Version:       1,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if si.compare > 0 {
			cp := money.FromRupees(si.compare)
			item.ComparePrice = &cp
		}

		if err := item.Validate(); err != nil {
			return fmt.Errorf("item %q invalid: %w", si.name, err)
		}
		if err := menus.CreateItem(ctx, item); err != nil {
			return err
		}

		if si.withPortion {
			if err := menus.AttachModifierGroup(ctx, item.ID, groupPortionID); err != nil {
				return err
			}
		}
		if si.withAddons {
			if err := menus.AttachModifierGroup(ctx, item.ID, groupAddonID); err != nil {
				return err
			}
		}
		created++
	}
	log.Info("seeded menu items", "added", created, "already_present", skipped)

	// --- Offers -----------------------------------------------------------
	//
	// Seeded with SQL rather than through the promotion service, deliberately:
	// this command already owns the catalog module wiring, and pulling in a
	// second module's dependency graph to insert three rows would couple the
	// seeder to promotion's constructor signature for no gain.
	//
	// Fixed UUIDs plus ON CONFLICT DO NOTHING, so re-running is a no-op rather
	// than a growing pile of duplicate offers -- which is exactly what the
	// integration tests used to leave behind in this database.
	//
	// The conflict target is upper(code), NOT id. That is the column customers
	// actually collide on, and it is the real unique index: coupons_code_key is
	// UNIQUE on upper(code), so codes are case-insensitively unique while the
	// primary key is a surrogate. Targeting id looked right and failed on the
	// first run here, because WELCOME50 already existed under a different UUID --
	// a duplicate id is not the conflict that can happen, a duplicate code is.
	//
	// The empty kitchen_ids on the first two is meaningful: it means
	// platform-wide, and the listing UI reads it that way.
	const offers = `
		INSERT INTO coupons (
			id, code, description, discount_type, discount_value,
			max_discount_minor, min_order_minor, kitchen_ids, first_order_only,
			valid_from, max_redemptions_per_user, funded_by, is_active,
			created_at, updated_at
		) VALUES
			('00000000-0000-0000-0000-0000000000c1', 'WELCOME50',
			 '50% OFF up to Rs100', 'percentage', 5000, 10000, 19900, '{}', true,
			 now(), 1, 'platform', true, now(), now()),
			('00000000-0000-0000-0000-0000000000c2', 'CHOWK75',
			 'Rs75 OFF above Rs349', 'flat', 7500, NULL, 34900, '{}', false,
			 now(), 3, 'platform', true, now(), now()),
			('00000000-0000-0000-0000-0000000000c3', 'KHANSAMA60',
			 'Rs60 OFF on Awadhi', 'flat', 6000, NULL, 29900,
			 ARRAY['00000000-0000-0000-0000-0000000000a1']::uuid[], false,
			 now(), 2, 'merchant', true, now(), now())
		ON CONFLICT (upper(code)) DO NOTHING`

	// RowsAffected, not the literal 3: with DO NOTHING a re-run inserts nothing,
	// and a log line claiming it created three offers every time is a log that
	// lies about what happened.
	tag, err := db.Q(ctx).Exec(ctx, offers)
	if err != nil {
		return fmt.Errorf("seed offers: %w", err)
	}
	log.Info("seeded offers", "inserted", tag.RowsAffected())

	// The menu is cached; a stale entry would hide everything just seeded.
	if err := mod.Service().InvalidateMenu(ctx, khansamaKitchenID); err != nil {
		return err
	}

	fmt.Printf(`
Seeded Khansama.

  Kitchen ID : %s
  Menu       : GET /api/v1/kitchens/%s/menu
  Nearby     : GET /api/v1/kitchens/nearby?lat=26.8467&lng=80.9462

Open 11:00-15:00 and 19:00-23:00 IST daily, so /menu reports
is_open=false outside those hours -- that is correct behaviour, not a bug.
`, khansamaKitchenID, khansamaKitchenID)

	return nil
}
