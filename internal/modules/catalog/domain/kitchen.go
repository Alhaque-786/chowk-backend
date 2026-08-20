// Package domain holds the catalog module's entities and repository ports.
//
// Catalog owns kitchens (outlets), their opening hours, delivery reach, and the
// menu — including the authoritative price of everything. It is the module that
// answers "can this be ordered, and what does it cost", which makes it the one
// ordering depends on most.
package domain

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// Kitchen is an outlet: one physical location with its own menu, stock and
// hours. A merchant may own several.
type Kitchen struct {
	ID          id.ID
	MerchantID  id.ID
	Name        string
	Slug        string
	Description string
	Cuisines    []string
	Phone       string
	Email       string

	AddressLine1 string
	City         string
	Pincode      string
	Latitude     float64
	Longitude    float64
	// DeliveryRadiusM is straight-line metres. A crude proxy for road distance,
	// but it is one comparison instead of a paid routing call on every menu view.
	DeliveryRadiusM int32

	// Three independent switches, because each suppresses ordering for a
	// different reason and the customer needs a different message for each:
	//   IsActive          — business-level on/off (offboarded, suspended)
	//   IsAcceptingOrders — the staff kill switch when the kitchen is swamped
	//   opening hours     — scheduled closure
	IsActive          bool
	IsAcceptingOrders bool
	PauseReason       string

	CommissionBps  int32
	PackagingFee   money.Amount
	MinOrder       money.Amount
	AvgPrepMinutes int32

	RatingAverage float64
	RatingCount   int32

	FSSAILicense string
	GSTIN        string

	IsFirstParty bool
	Version      int32
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// Hours is loaded on demand; nil means "not loaded", which is different from
	// "no hours configured". Callers that need it must ask for it explicitly.
	Hours []OpeningHours
}

// OpeningHours is one shift on one weekday.
//
// Several rows per weekday are allowed so a kitchen can close between lunch and
// dinner — a single open/close pair per day could not express that.
type OpeningHours struct {
	ID        id.ID
	KitchenID id.ID
	// DayOfWeek matches time.Weekday: 0 = Sunday.
	DayOfWeek int16
	// Wall-clock times in Asia/Kolkata, not instants. "Opens at 11:00" is a rule,
	// not a moment.
	OpensAt  time.Duration // since midnight
	ClosesAt time.Duration
}

// ClosedReason explains why a kitchen cannot take an order right now. Empty
// means it can.
type ClosedReason string

const (
	ReasonOpen         ClosedReason = ""
	ReasonInactive     ClosedReason = "kitchen_inactive"
	ReasonPaused       ClosedReason = "kitchen_paused"
	ReasonOutsideHours ClosedReason = "outside_opening_hours"
	ReasonNoHoursSet   ClosedReason = "no_hours_configured"
)

// CustomerMessage renders a reason as something worth showing a customer.
// Kept next to the reason so the copy cannot drift from the logic.
func (r ClosedReason) CustomerMessage(kitchenName string) string {
	switch r {
	case ReasonOpen:
		return ""
	case ReasonPaused:
		return kitchenName + " is not taking orders at the moment. Please try again shortly."
	case ReasonOutsideHours, ReasonNoHoursSet:
		return kitchenName + " is closed right now."
	default:
		return kitchenName + " is unavailable."
	}
}

// CanAcceptOrder reports whether the kitchen can take an order at `now`.
//
// The three switches are checked in order of specificity, so the customer gets
// the most informative message rather than a generic "closed".
//
// Requires Hours to be loaded. A kitchen with no hours configured is treated as
// CLOSED, not as always-open: defaulting to open would let a half-onboarded
// kitchen start receiving orders it cannot cook.
func (k *Kitchen) CanAcceptOrder(now time.Time) (bool, ClosedReason) {
	if !k.IsActive {
		return false, ReasonInactive
	}
	if !k.IsAcceptingOrders {
		return false, ReasonPaused
	}
	if len(k.Hours) == 0 {
		return false, ReasonNoHoursSet
	}
	if !k.IsOpenAt(now) {
		return false, ReasonOutsideHours
	}
	return true, ReasonOpen
}

// IsOpenAt checks the schedule.
//
// Converts to IST first: the stored times are Indian wall-clock, and comparing
// them against a UTC instant would shift every kitchen's hours by 5.5 hours.
func (k *Kitchen) IsOpenAt(now time.Time) bool {
	local := now.In(clock.IST)
	day := int16(local.Weekday())
	sinceMidnight := time.Duration(local.Hour())*time.Hour +
		time.Duration(local.Minute())*time.Minute

	for _, h := range k.Hours {
		if h.DayOfWeek != day {
			continue
		}
		if sinceMidnight >= h.OpensAt && sinceMidnight < h.ClosesAt {
			return true
		}
	}
	return false
}

// ServesPoint reports whether a destination is within delivery reach.
func (k *Kitchen) ServesPoint(lat, lng float64) bool {
	return HaversineMetres(k.Latitude, k.Longitude, lat, lng) <= float64(k.DeliveryRadiusM)
}

// DistanceTo returns straight-line metres to a point.
func (k *Kitchen) DistanceTo(lat, lng float64) float64 {
	return HaversineMetres(k.Latitude, k.Longitude, lat, lng)
}

// HaversineMetres is great-circle distance between two lat/lng points.
//
// Used to filter exactly after a bounding-box index scan has narrowed the
// candidates. The box alone is wrong — its corners are up to 41% further away
// than the radius — so the precise check is not optional.
func HaversineMetres(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0

	φ1 := lat1 * math.Pi / 180
	φ2 := lat2 * math.Pi / 180
	Δφ := (lat2 - lat1) * math.Pi / 180
	Δλ := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	return earthRadiusM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// BoundingBox returns lat/lng bounds enclosing a radius around a point, for the
// indexed prefilter.
//
// The cos(latitude) term is the part that is easy to get wrong: a degree of
// longitude is ~111 km at the equator but only ~105 km at Mumbai's latitude.
// Omitting it makes the box too narrow east-west and silently drops kitchens at
// the edge of the radius.
func BoundingBox(lat, lng float64, radiusM int32) (minLat, maxLat, minLng, maxLng float64) {
	const metresPerDegreeLat = 111_320.0

	dLat := float64(radiusM) / metresPerDegreeLat

	// Guard against division by ~0 near the poles, which would produce ±Inf
	// bounds. Irrelevant for India, but a NaN in a SQL parameter is not a
	// failure mode worth leaving open.
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat < 0.01 {
		cosLat = 0.01
	}
	dLng := float64(radiusM) / (metresPerDegreeLat * cosLat)

	return lat - dLat, lat + dLat, lng - dLng, lng + dLng
}

// Validate checks a kitchen is coherent before persistence.
func (k *Kitchen) Validate() error {
	err := apperr.Invalid("INVALID_KITCHEN", "Please complete all required kitchen fields.")
	bad := false

	if strings.TrimSpace(k.Name) == "" {
		err.WithField("name", "required")
		bad = true
	}
	if strings.TrimSpace(k.Slug) == "" {
		err.WithField("slug", "required")
		bad = true
	}
	if k.Latitude < -90 || k.Latitude > 90 {
		err.WithField("latitude", "out_of_range")
		bad = true
	}
	if k.Longitude < -180 || k.Longitude > 180 {
		err.WithField("longitude", "out_of_range")
		bad = true
	}
	// (0,0) is in the Gulf of Guinea and would pass a naive radius check.
	if k.Latitude == 0 && k.Longitude == 0 {
		err.WithField("location", "required")
		bad = true
	}
	if k.DeliveryRadiusM < 100 || k.DeliveryRadiusM > 50000 {
		err.WithField("delivery_radius_m", "must_be_100_to_50000")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Repository ports
// ---------------------------------------------------------------------------

type KitchenRepository interface {
	Create(ctx context.Context, k *Kitchen) error
	// Update uses optimistic locking: it must fail with a conflict if the
	// caller's Version is stale, so two admins editing the same kitchen cannot
	// silently overwrite each other.
	Update(ctx context.Context, k *Kitchen) error
	FindByID(ctx context.Context, kitchenID id.ID) (*Kitchen, error)
	FindBySlug(ctx context.Context, slug string) (*Kitchen, error)
	// FindNearby does the bounding-box prefilter; exact distance filtering is the
	// caller's job via ServesPoint.
	FindNearby(ctx context.Context, lat, lng float64, radiusM int32, limit int) ([]*Kitchen, error)
	ListForMerchant(ctx context.Context, merchantID id.ID) ([]*Kitchen, error)
	SetAcceptingOrders(ctx context.Context, kitchenID id.ID, accepting bool, reason string) error

	ReplaceHours(ctx context.Context, kitchenID id.ID, hours []OpeningHours) error
	LoadHours(ctx context.Context, kitchenIDs []id.ID) (map[id.ID][]OpeningHours, error)
}
