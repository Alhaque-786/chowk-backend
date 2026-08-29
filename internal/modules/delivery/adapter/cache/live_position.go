// Package cache holds the delivery module's live-position store.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
The live position store.

A CACHE RATHER THAN A TABLE, and the expiry is a feature rather than a
limitation. When a rider stops reporting the key expires, a read returns
nothing, and "we do not know where he is" is the honest answer to show. A row
would sit there indefinitely looking current, and the customer would watch a
stationary dot and conclude the rider was parked nearby rather than that his
phone had died.

Losing every live position in a Redis restart is acceptable and worth stating:
riders report every few seconds, so the map repopulates within one interval.
Nothing durable depends on this — the track in Postgres is the record.
*/
type LivePositionStore struct{ cache cache.Cache }

func NewLivePositionStore(c cache.Cache) *LivePositionStore {
	return &LivePositionStore{cache: c}
}

func key(deliveryID id.ID) string { return "delivery:pos:" + deliveryID.String() }

// stored is the wire form. A struct rather than the domain type so a field
// rename in the domain cannot silently change what is already in Redis.
type stored struct {
	RiderID    string  `json:"rider_id"`
	Latitude   float64 `json:"lat"`
	Longitude  float64 `json:"lng"`
	AccuracyM  float64 `json:"acc"`
	RecordedAt int64   `json:"at"`
}

func (s *LivePositionStore) Put(ctx context.Context, p domain.Position, ttl time.Duration) error {
	body, err := json.Marshal(stored{
		RiderID:    p.RiderID.String(),
		Latitude:   p.Latitude,
		Longitude:  p.Longitude,
		AccuracyM:  p.AccuracyM,
		RecordedAt: p.RecordedAt.Unix(),
	})
	if err != nil {
		return apperr.Internal(fmt.Errorf("delivery: encode position: %w", err))
	}
	if err := s.cache.Set(ctx, key(p.DeliveryID), body, ttl); err != nil {
		return apperr.Internal(fmt.Errorf("delivery: store position: %w", err))
	}
	return nil
}

// Get returns nil, nil when nothing is known, which is an ordinary state.
func (s *LivePositionStore) Get(ctx context.Context, deliveryID id.ID) (*domain.Position, error) {
	body, err := s.cache.Get(ctx, key(deliveryID))
	if err != nil || len(body) == 0 {
		// A cache miss and a cache failure are treated alike ON PURPOSE. Both mean
		// "no current position", and a rider's dot is not worth failing a page for:
		// the order status, the ETA and the phone number are all still useful with
		// Redis down.
		return nil, nil
	}

	var raw stored
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil // unreadable is the same as absent
	}
	riderID, err := id.Parse(raw.RiderID)
	if err != nil {
		return nil, nil
	}

	return &domain.Position{
		DeliveryID: deliveryID,
		RiderID:    riderID,
		Latitude:   raw.Latitude,
		Longitude:  raw.Longitude,
		AccuracyM:  raw.AccuracyM,
		RecordedAt: time.Unix(raw.RecordedAt, 0).UTC(),
	}, nil
}

// Clear drops a position, called when a delivery finishes. The TTL would expire
// it anyway; doing it explicitly means a delivered order stops showing a rider
// immediately rather than for another two minutes.
func (s *LivePositionStore) Clear(ctx context.Context, deliveryID id.ID) error {
	if err := s.cache.Delete(ctx, key(deliveryID)); err != nil {
		return apperr.Internal(fmt.Errorf("delivery: clear position: %w", err))
	}
	return nil
}
