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
	"log/slog"
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

// devDish is one row of a neighbouring kitchen's menu.
type devDish struct {
	category string
	name     string
	desc     string
	image    string
	rupees   float64
	foodType catalogdomain.FoodType
	prepMins int32
	spice    int16
	popular  bool
}

// seedDevMenus gives every DEV-FIXTURE kitchen a small, believable menu.
//
// Idempotent by NAME, like the Khansama seeding above: the existing dish names for
// a kitchen are read once and anything already present is skipped. Without that,
// each run appends another copy — which is exactly the bug this seeder had before.
func seedDevMenus(
	ctx context.Context,
	db *database.DB,
	menus interface {
		CreateCategory(context.Context, *catalogdomain.Category) error
		CreateItem(context.Context, *catalogdomain.MenuItem) error
	},
	// InvalidateMenu is required, not optional. See the call at the end of the
	// loop for why leaving it out silently produced empty menus.
	invalidate func(context.Context, id.ID) error,
	log *slog.Logger,
) error {
	dishes := map[string][]devDish{
		"tunday-kababi": {
			{"Kebabs", "Tunday Galouti", "The one the city is named for. Melts before you chew.", "/dishes/seekh-kebab.jpg", 189, catalogdomain.FoodNonVeg, 15, 2, true},
			{"Kebabs", "Mutton Seekh", "Minced mutton, charred on the skewer.", "/dishes/tandoori-platter.jpg", 229, catalogdomain.FoodNonVeg, 18, 2, false},
			{"Kebabs", "Veg Shami", "Chana and potato, bound and shallow fried.", "/dishes/samosa.jpg", 149, catalogdomain.FoodVeg, 14, 1, false},
			{"Breads", "Ulte Tawe Ka Paratha", "Cooked on an upturned griddle, as it should be.", "/dishes/naan.jpg", 59, catalogdomain.FoodVeg, 8, 0, false},
			{"Kebabs", "Boti Kebab", "Cubed mutton, marinated overnight in raw papaya.", "/dishes/tandoori-platter.jpg", 249, catalogdomain.FoodNonVeg, 20, 2, false},
			{"Kebabs", "Kakori Kebab", "So fine it is strained twice. Falls off the skewer.", "/dishes/seekh-kebab.jpg", 269, catalogdomain.FoodNonVeg, 22, 1, true},
			{"Curries", "Mutton Korma", "Yoghurt and fried onion, no tomato.", "/dishes/rogan-josh.jpg", 349, catalogdomain.FoodNonVeg, 30, 2, false},
			{"Breads", "Rumali Roti", "Thin enough to see through, folded like a handkerchief.", "/dishes/naan.jpg", 39, catalogdomain.FoodVeg, 5, 0, false},
			{"Drinks", "Rooh Afza Sharbat", "Rose and khus, over ice.", "", 69, catalogdomain.FoodVeg, 3, 0, false},
		},
		"dakshin-tiffin": {
			{"Dosa", "Masala Dosa", "Fermented overnight, filled with potato masala.", "/dishes/dosa.jpg", 129, catalogdomain.FoodVeg, 12, 1, true},
			{"Dosa", "Ghee Roast", "Thin, crisp, and more ghee than is sensible.", "/dishes/dosa.jpg", 149, catalogdomain.FoodVeg, 12, 1, false},
			{"Thali", "South Indian Thali", "Rice, sambar, three vegetables, curd, payasam.", "/dishes/thali.jpg", 199, catalogdomain.FoodVeg, 15, 1, true},
			{"Drinks", "Filter Coffee", "Decoction and milk, poured properly.", "", 49, catalogdomain.FoodVeg, 4, 0, false},
			{"Dosa", "Onion Rava Dosa", "Lacy, crisp, semolina rather than rice.", "/dishes/dosa.jpg", 139, catalogdomain.FoodVeg, 14, 1, false},
			{"Idli & Vada", "Idli Sambar", "Three idli, sambar, two chutneys.", "/dishes/thali.jpg", 89, catalogdomain.FoodVeg, 8, 1, false},
			{"Idli & Vada", "Medu Vada", "Fermented urad, fried to order.", "/dishes/samosa.jpg", 79, catalogdomain.FoodVeg, 10, 1, true},
			{"Rice", "Curd Rice", "With pomegranate and a tempering of curry leaf.", "/dishes/kheer.jpg", 99, catalogdomain.FoodVeg, 6, 0, false},
			{"Drinks", "Rose Milk", "Cold, and pinker than it needs to be.", "", 59, catalogdomain.FoodVeg, 3, 0, false},
		},
		"chowk-chaat-house": {
			{"Chaat", "Tokri Chaat", "A basket of fried potato, filled and collapsed.", "/dishes/cover-streetfood.jpg", 119, catalogdomain.FoodVeg, 8, 2, true},
			{"Chaat", "Aloo Tikki", "Griddled, split, and drowned in chutney.", "/dishes/samosa.jpg", 89, catalogdomain.FoodVeg, 7, 2, false},
			{"Chaat", "Samosa Chaat", "Two samosas, broken, with chole over them.", "/dishes/samosa.jpg", 99, catalogdomain.FoodVeg, 8, 2, false},
			{"Snacks", "Bhalla Papdi", "Soft dahi bhalla under crisp papdi.", "/dishes/chole-bhature.jpg", 109, catalogdomain.FoodVeg, 6, 1, false},
			{"Chaat", "Pani Batasha", "Six puris, five waters, one of them fierce.", "/dishes/cover-streetfood.jpg", 79, catalogdomain.FoodVeg, 5, 3, true},
			{"Chaat", "Dahi Bhalla", "Soaked, pressed, and buried under sweet curd.", "/dishes/chole-bhature.jpg", 99, catalogdomain.FoodVeg, 6, 0, false},
			{"Snacks", "Kachori Sabzi", "Two urad kachori with hing-heavy aloo.", "/dishes/samosa.jpg", 89, catalogdomain.FoodVeg, 8, 2, false},
			{"Snacks", "Matar Tikki", "Winter peas, griddled, with green chutney.", "/dishes/samosa.jpg", 95, catalogdomain.FoodVeg, 9, 2, false},
		},
		"ram-asrey-sweets": {
			{"Sweets", "Malai Gilori", "Rolled khoya, silver leaf, sold by the piece.", "/dishes/gulab-jamun.jpg", 60, catalogdomain.FoodVeg, 2, 0, true},
			{"Sweets", "Makhan Malai", "Winter only, and gone by nine in the morning.", "/dishes/kheer.jpg", 80, catalogdomain.FoodVeg, 2, 0, true},
			{"Sweets", "Gulab Jamun", "Two pieces, warm, in syrup.", "/dishes/gulab-jamun.jpg", 70, catalogdomain.FoodVeg, 3, 0, false},
			{"Sweets", "Motichoor Laddu", "By the piece or the kilo.", "/dishes/gulab-jamun.jpg", 55, catalogdomain.FoodVeg, 2, 0, false},
			{"Sweets", "Kalakand", "Grainy, set that morning.", "/dishes/kheer.jpg", 90, catalogdomain.FoodVeg, 2, 0, true},
			{"Namkeen", "Dalmoth", "The salty half of the counter.", "/dishes/samosa.jpg", 120, catalogdomain.FoodVeg, 2, 1, false},
		},
		"nawabi-dum-pukht": {
			{"Biryani", "Dum Pukht Gosht Biryani", "Sealed with dough and cooked six hours.", "/dishes/chicken-biryani.jpg", 549, catalogdomain.FoodNonVeg, 48, 2, true},
			{"Biryani", "Subz Dum Biryani", "The same handi, vegetables instead.", "/dishes/mutton-biryani.jpg", 379, catalogdomain.FoodVeg, 42, 1, false},
			{"Curries", "Nihari", "Overnight shank. Order it with sheermal.", "/dishes/rogan-josh.jpg", 449, catalogdomain.FoodNonVeg, 45, 3, true},
			{"Curries", "Shahi Paneer", "Cashew and cardamom, no tomato.", "/dishes/butter-chicken.jpg", 329, catalogdomain.FoodVeg, 25, 1, false},
			{"Breads", "Sheermal", "Saffron, slightly sweet, from the tandoor.", "/dishes/naan.jpg", 69, catalogdomain.FoodVeg, 10, 0, false},
			{"Curries", "Dal Sultani", "Yellow dal finished with cream and smoke.", "/dishes/dal-makhani.jpg", 249, catalogdomain.FoodVeg, 20, 1, false},
			{"Kebabs", "Kakori Kebab", "Minced on a stone, not in a machine.", "/dishes/seekh-kebab.jpg", 299, catalogdomain.FoodNonVeg, 25, 1, true},
			{"Breads", "Warqi Paratha", "Layered, and shatters when you tear it.", "/dishes/naan.jpg", 89, catalogdomain.FoodVeg, 12, 0, false},
			{"Desserts", "Shahi Tukda", "Fried bread, rabri, pistachio.", "/dishes/kheer.jpg", 149, catalogdomain.FoodVeg, 8, 0, true},
		},
	}

	type row struct {
		kitchenID id.ID
		slug      string
	}
	var kitchens []row

	rows, err := db.Q(ctx).Query(ctx,
		`SELECT id, slug FROM kitchens WHERE fssai_license = 'DEV-FIXTURE' ORDER BY slug`)
	if err != nil {
		return fmt.Errorf("seed: list dev kitchens: %w", err)
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kitchenID, &r.slug); err != nil {
			rows.Close()
			return fmt.Errorf("seed: scan dev kitchen: %w", err)
		}
		kitchens = append(kitchens, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	added := 0
	for _, k := range kitchens {
		menu, ok := dishes[k.slug]
		if !ok {
			continue
		}

		existing, err := existingNames(ctx, db,
			`SELECT name FROM menu_items WHERE kitchen_id = $1`, k.kitchenID)
		if err != nil {
			return err
		}

		// Categories are created on first use and reused after, so the order of
		// `menu` decides display order and no separate category list is needed.
		catIDs := make(map[string]id.ID)
		order := int32(0)

		for _, d := range menu {
			catID, seen := catIDs[d.category]
			if !seen {
				order++
				catID = id.New()
				now := time.Now().UTC()
				c := &catalogdomain.Category{
					ID: catID, KitchenID: k.kitchenID, Name: d.category,
					DisplayOrder: order, IsActive: true, CreatedAt: now, UpdatedAt: now,
				}
				if err := menus.CreateCategory(ctx, c); err != nil {
					// Already there from a previous run; look it up instead.
					var found id.ID
					if e := db.Q(ctx).QueryRow(ctx,
						`SELECT id FROM menu_categories WHERE kitchen_id = $1 AND name = $2`,
						k.kitchenID, d.category).Scan(&found); e != nil {
						return fmt.Errorf("seed: category %q for %s: %w", d.category, k.slug, err)
					}
					catID = found
				}
				catIDs[d.category] = catID
			}

			if existing[d.name] {
				continue
			}

			now := time.Now().UTC()
			cat := catID
			item := &catalogdomain.MenuItem{
				ID: id.New(), KitchenID: k.kitchenID, CategoryID: &cat,
				Name: d.name, Description: d.desc, ImageURL: d.image,
				Price: money.FromRupees(d.rupees),
				// 5% GST on prepared food, matching the Khansama items. Drinks
				// would be 18%, but none of these fixtures is a packaged beverage.
				TaxBps:       500,
				PackagingFee: money.FromRupees(5),
				FoodType:     d.foodType,
				SpiceLevel:   d.spice,
				Allergens:    []string{},
				Serves:       1,
				IsAvailable:  true,
				PrepMinutes:  d.prepMins,
				// A seed menu is tens of items; len() cannot approach int32.
				DisplayOrder:  order*100 + int32(len(existing)), //nolint:gosec // bounded by the seed fixture
				IsRecommended: d.popular,
				Version:       1,
				CreatedAt:     now,
				UpdatedAt:     now,
			}
			if err := item.Validate(); err != nil {
				return fmt.Errorf("dev dish %q invalid: %w", d.name, err)
			}
			if err := menus.CreateItem(ctx, item); err != nil {
				return err
			}
			existing[d.name] = true
			added++
		}

		/*
		  INVALIDATE, ALWAYS — not only when something was added.

		  The menu is cached in Redis. Two of these kitchens had been browsed while
		  they still had no dishes, so an EMPTY menu was cached for them, and after
		  seeding the API kept serving that empty menu: four dishes in Postgres,
		  zero over HTTP. The rows were fine; the cache was authoritative and wrong.

		  Unconditional rather than `if added > 0`, because the stale entry is not
		  caused by this run — it was cached earlier, by a read. A seeder that only
		  invalidates what it just wrote cannot fix a cache poisoned before it ran,
		  which is precisely the case that bit here.
		*/
		if err := invalidate(ctx, k.kitchenID); err != nil {
			return fmt.Errorf("seed: invalidate menu for %s: %w", k.slug, err)
		}
	}

	log.Info("seeded dev menus", "kitchens", len(kitchens), "dishes_added", added)
	return nil
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

			CommissionBps: 0, // first-party: no commission to ourselves
			PackagingFee:  money.FromRupees(15),
			MinOrder:      money.FromRupees(149),
			// What two people actually spend here: a biryani, a kebab and a bread.
			// Merchant-declared, not derived -- see the column comment in 0013.
			CostForTwo:     money.FromRupees(650),
			AvgPrepMinutes: 25,
			// The flagship had no reputation at all, so the one kitchen with a full
			// menu, categories and modifiers sorted last on anything rating-based
			// and showed no chip. A demo kitchen that looks worse than the fixtures
			// around it is a poor advertisement for the fixture set.
			RatingAverage: 4.8,
			RatingCount:   2417,

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

	// --- Neighbouring kitchens, for development only ----------------------
	//
	// Khansama alone is one card in a three-column grid, which tells you nothing
	// about how a listing page actually looks or behaves: you cannot judge the
	// grid, the filters have nothing to filter, and "open first" sorting is
	// untestable with a single row.
	//
	// These are DEVELOPMENT FIXTURES and they are shaped to exercise real
	// branches rather than to look plausible: one is closed (is_accepting_orders
	// false, so the card must dim and label itself), one is slow (48 min prep, so
	// the "under 30 min" filter must exclude it), one is unrated (rating_count 0,
	// so the rating chip must be absent rather than showing 0.0), and they sit at
	// different distances so the distance line and radius filtering have
	// something to do.
	//
	// Third-party rather than first-party, with a real commission_bps, because
	// every kitchen being IsFirstParty means the marketplace path is never
	// exercised locally.
	type devKitchen struct {
		id               string
		name, slug, desc string
		cuisines         []string
		lat, lng         float64
		cover            string
		prepMinutes      int32
		rating           float64
		ratingCount      int32
		accepting        bool
		minOrderRupees   float64
		// What two people typically spend. Merchant-declared, so the fixtures set
		// it explicitly rather than deriving it -- see migration 0013.
		costForTwoRupees float64
	}

	// Shared by every dev fixture, so one edit moves them all.
	fssaiValidUntil := time.Now().UTC().AddDate(2, 0, 0)

	devKitchens := []devKitchen{
		{"00000000-0000-0000-0000-0000000000b1", "Tunday Kababi", "tunday-kababi",
			"Galouti and shami kebabs from a recipe older than the city's clock tower.",
			[]string{"Awadhi", "Mughlai", "Kebabs"}, 26.8520, 80.9410,
			"/dishes/cover-tandoor.jpg", 20, 4.6, 1284, true, 129, 450},
		{"00000000-0000-0000-0000-0000000000b2", "Dakshin Tiffin", "dakshin-tiffin",
			"Dosa, idli and filter coffee, ground and fermented on the day.",
			[]string{"South Indian", "Breakfast", "Vegetarian"}, 26.8601, 80.9505,
			"/dishes/cover-southindian.jpg", 18, 4.4, 612, true, 99, 300},
		{"00000000-0000-0000-0000-0000000000b3", "Chowk Chaat House", "chowk-chaat-house",
			"Tokri chaat, batasha and aloo tikki, fried to order.",
			[]string{"Street Food", "Chaat", "Snacks"}, 26.8390, 80.9120,
			// 0/0, not 4.2/0. An average with no reviews behind it is data that
			// cannot exist, and a fixture that ships impossible data teaches the UI
			// to handle a case production will never send. This is the unrated
			// kitchen: the rating chip must be absent, not showing 0.0.
			"/dishes/cover-streetfood.jpg", 12, 0, 0, true, 79, 250},
		// Closed: the card must dim and say so rather than accepting an order.
		{"00000000-0000-0000-0000-0000000000b4", "Ram Asrey Sweets", "ram-asrey-sweets",
			"Malai gilori, makhan malai and mithai by the kilo.",
			[]string{"Sweets", "Desserts"}, 26.8455, 80.9398,
			"/dishes/cover-sweets.jpg", 15, 4.7, 903, false, 149, 400},
		// Slow AND unrated: exercises the prep-time filter and the missing-rating
		// branch at the same time.
		{"00000000-0000-0000-0000-0000000000b5", "Nawabi Dum Pukht", "nawabi-dum-pukht",
			"Sealed-pot biryani and nihari, cooked for six hours. Worth the wait.",
			[]string{"Awadhi", "Biryani", "Mughlai"}, 26.8700, 80.9600,
			// Its own photograph. Sharing cover-mughlai.jpg with Khansama put the
			// same picture twice in one grid, which reads as a bug rather than as
			// two restaurants.
			"/dishes/chicken-biryani.jpg", 48, 0, 0, true, 299, 900},
	}

	addedKitchens := 0
	for _, dk := range devKitchens {
		now := time.Now().UTC()
		k := &catalogdomain.Kitchen{
			ID:            id.MustParse(dk.id),
			MerchantID:    catalog.FirstPartyMerchantID,
			Name:          dk.name,
			Slug:          dk.slug,
			Description:   dk.desc,
			Cuisines:      dk.cuisines,
			CoverImageURL: dk.cover,
			Phone:         "+919876500000",
			Email:         dk.slug + "@example.invalid",

			AddressLine1: "Lucknow",
			City:         "Lucknow",
			Pincode:      "226001",
			Latitude:     dk.lat,
			Longitude:    dk.lng,
			// Wide enough that all of them reach the home page's search centre,
			// so the grid is populated while distances still differ.
			DeliveryRadiusM: 12000,

			IsActive:          true,
			IsAcceptingOrders: dk.accepting,

			// A real commission, unlike Khansama's zero: these stand in for
			// onboarded third parties, which is the case the pricing code needs
			// exercised.
			CommissionBps:  600,
			PackagingFee:   money.FromRupees(15),
			MinOrder:       money.FromRupees(dk.minOrderRupees),
			CostForTwo:     money.FromRupees(dk.costForTwoRupees),
			AvgPrepMinutes: dk.prepMinutes,
			RatingAverage:  dk.rating,
			RatingCount:    dk.ratingCount,

			FSSAILicense: "DEV-FIXTURE",
			IsFirstParty: false,
			/*
				A licence that is valid, and dated rather than left nil.

				Migration 0017 made a third-party kitchen with no fssai_expiry count
				as LAPSED, which is the right default -- an unknown licence is not a
				valid one, and the alternative is serving food on an expired
				registration. It also silently closed every dev kitchen here: they
				were created before that column existed, so they all had nil, and the
				listing showed "not available right now" for the three that were
				otherwise open. The seeder was never updated alongside the migration.

				Two years out, so this fixture does not quietly rot and reproduce the
				same confusing state next year.
			*/
			FSSAIExpiry: &fssaiValidUntil,
			Version:     1,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := k.Validate(); err != nil {
			return fmt.Errorf("dev kitchen %s invalid: %w", dk.slug, err)
		}
		// Create returns a slug conflict on a re-run, which is the idempotency
		// check -- the slug is fixed, so a second run cannot duplicate these.
		if err := kitchens.Create(ctx, k); err != nil {
			continue
		}
		addedKitchens++
	}
	log.Info("seeded dev kitchens", "added", addedKitchens, "total", len(devKitchens))

	/*
		Opening hours for the dev kitchens.

		They had none, and a kitchen with no hours is CLOSED -- CanAcceptOrder
		returns ReasonNoHoursSet rather than defaulting to open, so a half-onboarded
		kitchen cannot take orders it has no one to cook. Correct in production, and
		it meant five of the six kitchens on the development site were permanently
		shut. With Khansama on a split shift too, there were stretches of the working
		day when nothing at all was open and the cart, checkout and order flow could
		not be reached without changing the system clock.

		MOST OF THESE ARE DELIBERATELY WIDE, and that is a fixture decision rather
		than a realistic one. A development database exists to be worked against; a
		realistic 11-to-3 kitchen is realistic and useless at four in the afternoon.

		Nawabi Dum Pukht keeps a narrow split shift on purpose, so the outside-hours
		branch and the "closed between lunch and dinner" message stay exercised. One
		kitchen is enough to test that. Six made the site unusable.
	*/
	type devHours struct {
		kitchenID string
		shifts    [][2]time.Duration
	}
	allDay := [][2]time.Duration{{7 * time.Hour, 23*time.Hour + 30*time.Minute}}
	splitShift := [][2]time.Duration{
		{11 * time.Hour, 15 * time.Hour},
		{19 * time.Hour, 23 * time.Hour},
	}
	devSchedules := []devHours{
		{"00000000-0000-0000-0000-0000000000b1", allDay}, // Tunday Kababi
		{"00000000-0000-0000-0000-0000000000b2", allDay}, // Dakshin Tiffin
		{"00000000-0000-0000-0000-0000000000b3", allDay}, // Chowk Chaat House
		// Ram Asrey is paused (is_accepting_orders false), so it reads as closed
		// for a different reason. It still gets hours -- otherwise two separate
		// causes would be collapsed into one and the paused branch would never be
		// the reason shown.
		{"00000000-0000-0000-0000-0000000000b4", allDay},
		// The one narrow schedule, kept to exercise ReasonOutsideHours.
		{"00000000-0000-0000-0000-0000000000b5", splitShift},
	}
	/*
		Ratings, applied to rows that already exist.

		The seeder is idempotent by design -- Create returns a slug conflict on a
		re-run and the loop moves on -- which means every field only ever gets
		written on the very first run. Ratings were being set on the domain object
		and dropped by an INSERT that did not list the columns, and once that was
		fixed the fixtures STILL showed 0.00, because by then the rows existed and
		Create was never reached. Idempotent and self-repairing are different
		properties, and a fixture wants both.

		Scoped to DEV-FIXTURE rows so this can never touch a real kitchen's
		reputation. Ratings are derived from customer reviews in production and
		nothing else should ever write them.
	*/
	ratings := []struct {
		id    string
		avg   float64
		count int32
		guard string
		label string
	}{
		// Khansama is guarded on FIRST-PARTY, the dev kitchens on DEV-FIXTURE. Both
		// guards exist so this can never reach a real onboarded merchant, whose
		// rating belongs to its customers.
		{khansamaKitchenID.String(), 4.8, 2417, "FIRST-PARTY", "Khansama"},
	}
	for _, dk := range devKitchens {
		ratings = append(ratings, struct {
			id    string
			avg   float64
			count int32
			guard string
			label string
		}{dk.id, dk.rating, dk.ratingCount, "DEV-FIXTURE", dk.slug})
	}
	for _, r := range ratings {
		if _, err := db.Q(ctx).Exec(ctx, `
			UPDATE kitchens SET rating_average = $2, rating_count = $3, updated_at = now()
			WHERE id = $1 AND fssai_license = $4`,
			r.id, r.avg, r.count, r.guard,
		); err != nil {
			return fmt.Errorf("seed: ratings for %s: %w", r.label, err)
		}
		// Same self-repair reasoning as the ratings: rows created before migration
		// 0017 have a nil expiry and read as lapsed, and Create is never reached on
		// a re-run to fix them.
		if _, err := db.Q(ctx).Exec(ctx, `
			UPDATE kitchens SET fssai_expiry = $2, updated_at = now()
			WHERE id = $1 AND fssai_license = 'DEV-FIXTURE' AND fssai_expiry IS NULL`,
			r.id, fssaiValidUntil,
		); err != nil {
			return fmt.Errorf("seed: fssai expiry for %s: %w", r.label, err)
		}
	}

	shiftRows := 0
	for _, sched := range devSchedules {
		kid, err := id.Parse(sched.kitchenID)
		if err != nil {
			return fmt.Errorf("seed: dev kitchen id %q: %w", sched.kitchenID, err)
		}
		var kh []catalogdomain.OpeningHours
		for day := int16(0); day <= 6; day++ {
			for _, sh := range sched.shifts {
				kh = append(kh, catalogdomain.OpeningHours{
					KitchenID: kid, DayOfWeek: day, OpensAt: sh[0], ClosesAt: sh[1],
				})
			}
		}
		if err := kitchens.ReplaceHours(ctx, kid, kh); err != nil {
			return fmt.Errorf("seed: hours for %s: %w", sched.kitchenID, err)
		}
		shiftRows += len(kh)
	}
	log.Info("set dev kitchen hours", "kitchens", len(devSchedules), "shifts", shiftRows)

	// --- Opening hours ---------------------------------------------------
	//
	// Open 07:00-23:30 daily, and this used to be a split shift -- lunch
	// 11:00-15:00, dinner 19:00-23:00 -- which was more realistic and made the
	// development site unusable for most of the working day. Khansama is the
	// flagship fixture: it is the kitchen with the full menu, the categories and
	// the modifiers, so it is the one somebody testing a cart or a checkout
	// actually needs open.
	//
	// The split-shift case did not go away with it. Nawabi Dum Pukht carries that
	// schedule now, which keeps ReasonOutsideHours exercised without every route
	// through the app depending on the time of day.
	var hours []catalogdomain.OpeningHours
	for day := int16(0); day <= 6; day++ {
		hours = append(hours,
			catalogdomain.OpeningHours{
				KitchenID: khansamaKitchenID, DayOfWeek: day,
				OpensAt: 7 * time.Hour, ClosesAt: 23*time.Hour + 30*time.Minute,
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

	// --- Menus for the neighbouring dev kitchens ---------------------------
	//
	// Without these, five of the six kitchens on the home page led to "Nothing
	// available right now". The grid looked finished and four fifths of it was a
	// dead end — which is worse than showing fewer kitchens, because the customer
	// only finds out after a tap.
	//
	// Deliberately SMALL menus, three to five dishes each. The point is a
	// believable second kitchen to navigate to, not a fixture that takes longer to
	// read than the code around it. Each carries a real image so the cards and the
	// dish thumbnails are populated, and a mix of veg and non-veg so the veg filter
	// has something to do on more than one kitchen.
	if err := seedDevMenus(ctx, db, menus, mod.Service().InvalidateMenu, log); err != nil {
		return err
	}

	// The menu is cached; a stale entry would hide everything just seeded.
	if err := mod.Service().InvalidateMenu(ctx, khansamaKitchenID); err != nil {
		return err
	}

	/*
		The seeder checks its own work.

		This exists because of a silent failure that lasted weeks. Migration 0017
		made a third-party kitchen with no fssai_expiry count as lapsed -- correct,
		and the seeder was not updated with it, so every dev kitchen quietly became
		"not available right now". Nothing errored. The seed log said it had seeded
		five kitchens, and it had; they were simply all shut.

		A fixture set that cannot be ordered from is not a fixture set, and the only
		way to notice was to open the site and wonder. So the seeder now reports what
		it actually produced, and fails when the answer is nothing usable.
	*/
	var open, closed int
	rows, err := db.Q(ctx).Query(ctx, `
		SELECT k.name, k.is_active, k.is_accepting_orders, k.is_first_party,
		       k.fssai_expiry, count(h.id) AS shifts
		FROM kitchens k
		LEFT JOIN kitchen_hours h ON h.kitchen_id = k.id
		WHERE k.fssai_license IN ('DEV-FIXTURE', 'FIRST-PARTY')
		GROUP BY k.id ORDER BY k.name`)
	if err != nil {
		return fmt.Errorf("seed: verify fixtures: %w", err)
	}
	type fixtureState struct {
		name   string
		usable bool
		why    string
	}
	var states []fixtureState
	for rows.Next() {
		var (
			name                          string
			active, accepting, firstParty bool
			expiry                        *time.Time
			shifts                        int
		)
		if err := rows.Scan(&name, &active, &accepting, &firstParty, &expiry, &shifts); err != nil {
			rows.Close()
			return fmt.Errorf("seed: verify fixtures: %w", err)
		}
		st := fixtureState{name: name, usable: true}
		switch {
		case !active:
			st.usable, st.why = false, "inactive"
		case !accepting:
			// Deliberate: one paused kitchen keeps that branch exercised.
			st.usable, st.why = false, "paused (intentional fixture)"
		case shifts == 0:
			st.usable, st.why = false, "NO OPENING HOURS -- will always read as closed"
		case !firstParty && expiry == nil:
			st.usable, st.why = false, "NO FSSAI EXPIRY -- reads as a lapsed licence"
		case !firstParty && expiry.Before(time.Now()):
			st.usable, st.why = false, "FSSAI EXPIRY IN THE PAST"
		}
		states = append(states, st)
		if st.usable {
			open++
		} else {
			closed++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed: verify fixtures: %w", err)
	}

	fmt.Printf("\nSeeded %d kitchens: %d orderable, %d not.\n\n", len(states), open, closed)
	for _, st := range states {
		mark := "ok  "
		if !st.usable {
			mark = "shut"
		}
		fmt.Printf("  %s  %-22s %s\n", mark, st.name, st.why)
	}

	if open == 0 {
		return fmt.Errorf("seed: every fixture kitchen is closed -- the development " +
			"site cannot be ordered from, which is a broken seed rather than a state " +
			"worth shipping")
	}

	fmt.Printf(`
  Kitchen ID : %s
  Menu       : GET /api/v1/kitchens/%s/menu
  Nearby     : GET /api/v1/kitchens/nearby?lat=26.8467&lng=80.9462

Nawabi Dum Pukht keeps a split shift (11:00-15:00, 19:00-23:00 IST) so the
outside-hours branch stays testable; the rest are open 07:00-23:30.
`, khansamaKitchenID, khansamaKitchenID)

	return nil
}
