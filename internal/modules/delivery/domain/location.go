package domain

import (
	"context"
	"math"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
	Where a rider is.

	Two stores, deliberately, and the split is the whole design.

	LIVE POSITION IS A CACHE ENTRY with a short TTL. A rider posting every five
	seconds is 720 writes an hour each; a row per update would be a table that is
	almost entirely write traffic for data nobody reads. The customer wants one
	answer -- where is he now -- and that is a single key.

	THE TRACK IS DECIMATED and kept in Postgres. A point is written when the rider
	has moved far enough or enough time has passed, which is small enough to keep
	and detailed enough to answer the only question ever asked of a finished
	delivery: the customer says nobody came, and a line on a map settles it.

	The TTL matters as much as the write. A position that stops refreshing must
	disappear rather than sit there looking current -- a stale dot is worse than no
	dot, because the customer watches a rider who is not moving and concludes he is
	parked round the corner when in fact his phone died twenty minutes ago.
*/

// LivePositionTTL is how long a reported position stays current.
//
// Generous relative to the reporting interval, because a rider through a tunnel
// or a basement stops reporting for a minute at a time and their dot should not
// blink out. Short enough that a dead handset stops being represented as a
// moving rider within a couple of minutes.
const LivePositionTTL = 2 * time.Minute

// Decimation thresholds. A point is persisted when EITHER is crossed.
const (
	// TrackMinDistanceM is far enough to be movement rather than GPS jitter. A
	// stationary phone wanders by tens of metres; 60 is comfortably outside that.
	TrackMinDistanceM = 60.0
	// TrackMinInterval keeps a point appearing even when a rider is stuck in
	// traffic, so a gap in the track means "no signal" rather than "not moving".
	TrackMinInterval = 45 * time.Second
)

// Position is a rider's location at a moment.
type Position struct {
	DeliveryID id.ID
	RiderID    id.ID
	Latitude   float64
	Longitude  float64
	// AccuracyM is what the handset claimed, in metres. Kept because it is the
	// only way to tell a rider who genuinely moved from a phone that lost its fix
	// and guessed: a two-kilometre jump with three-kilometre accuracy is noise.
	AccuracyM float64
	// RecordedAt is when the DEVICE took the reading, not when we received it. A
	// rider who was out of signal uploads a burst on reconnect, and ordering those
	// by arrival draws a track that teleports.
	RecordedAt time.Time
}

// NewPosition validates a reported position.
//
// Everything here is untrusted: it arrives from a phone, over a network, from a
// person with an incentive to be somewhere other than where they are.
func NewPosition(deliveryID, riderID id.ID, lat, lng, accuracy float64, recordedAt, now time.Time) (Position, error) {
	if math.IsNaN(lat) || math.IsNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return Position{}, apperr.Invalid("INVALID_POSITION", "That is not a valid location.").
			WithField("latitude", "out_of_range")
	}

	// A reading from the future is a broken device clock, and trusting it would
	// pin a stale position at the top of the track for as long as the skew lasts.
	// Small skew is normal and tolerated by clamping rather than rejecting: the
	// rider cannot fix their phone's clock and should not be undeliverable for it.
	if recordedAt.IsZero() || recordedAt.After(now.Add(2*time.Minute)) {
		recordedAt = now
	}

	// Older than the TTL is not worth storing as "live". It still gets written to
	// the track if it decimates in, because a late burst from a dead spot is real
	// history — it is only useless as an answer to "where is he now".
	return Position{
		DeliveryID: deliveryID,
		RiderID:    riderID,
		Latitude:   lat,
		Longitude:  lng,
		AccuracyM:  accuracy,
		RecordedAt: recordedAt,
	}, nil
}

// IsStale reports whether this position is too old to present as current.
func (p Position) IsStale(now time.Time) bool {
	return now.Sub(p.RecordedAt) > LivePositionTTL
}

/*
ShouldPersist decides whether this point earns a row.

Called with the last point written for this delivery. A nil previous means the
first point, which is always kept -- the start of a track is the one point that
cannot be interpolated from its neighbours.
*/
func (p Position) ShouldPersist(previous *Position) bool {
	if previous == nil {
		return true
	}
	if p.RecordedAt.Sub(previous.RecordedAt) >= TrackMinInterval {
		return true
	}
	return DistanceM(previous.Latitude, previous.Longitude, p.Latitude, p.Longitude) >= TrackMinDistanceM
}

/*
DistanceM is the great-circle distance in metres.

Haversine rather than the equirectangular approximation used for kitchen
distances. Over the tens of metres this function is asked about, the cheap
approximation's error is a meaningful fraction of the answer, and the answer is
what decides whether a point is movement or jitter.
*/
func DistanceM(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusM = 6371000.0

	φ1 := lat1 * math.Pi / 180
	φ2 := lat2 * math.Pi / 180
	Δφ := (lat2 - lat1) * math.Pi / 180
	Δλ := (lng2 - lng1) * math.Pi / 180

	a := math.Sin(Δφ/2)*math.Sin(Δφ/2) +
		math.Cos(φ1)*math.Cos(φ2)*math.Sin(Δλ/2)*math.Sin(Δλ/2)
	return earthRadiusM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// LivePositionStore holds the current position of an in-flight delivery.
//
// A cache, not a database: entries expire, and an expired entry means "we do not
// know where this rider is", which is a true and useful answer.
type LivePositionStore interface {
	Put(ctx context.Context, p Position, ttl time.Duration) error
	// Get returns nil, nil when nothing is known — an ordinary state, not an error.
	Get(ctx context.Context, deliveryID id.ID) (*Position, error)
	Clear(ctx context.Context, deliveryID id.ID) error
}

// TrackRepository stores the decimated history.
type TrackRepository interface {
	Append(ctx context.Context, p Position) error
	// Latest returns the last persisted point, which is what decimation compares
	// against. Nil when the track is empty.
	Latest(ctx context.Context, deliveryID id.ID) (*Position, error)
	List(ctx context.Context, deliveryID id.ID) ([]Position, error)
}
