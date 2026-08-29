package domain_test

import (
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

var (
	now      = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	someTrip = id.New()
	someRide = id.New()
)

func pos(t *testing.T, lat, lng float64, recorded time.Time) domain.Position {
	t.Helper()
	p, err := domain.NewPosition(someTrip, someRide, lat, lng, 10, recorded, now)
	if err != nil {
		t.Fatalf("NewPosition: %v", err)
	}
	return p
}

func TestNewPositionRejectsImpossibleCoordinates(t *testing.T) {
	// Untrusted input: it comes from a phone, over a network, from somebody with
	// an incentive to be somewhere other than where they are.
	for _, tc := range []struct {
		name     string
		lat, lng float64
	}{
		{"latitude past the pole", 91, 80},
		{"negative latitude past the pole", -91, 80},
		{"longitude past the meridian", 26, 181},
		{"nonsense", 999, 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.NewPosition(someTrip, someRide, tc.lat, tc.lng, 10, now, now)
			if err == nil {
				t.Fatal("accepted an impossible coordinate")
			}
			if apperr.CodeOf(err) != "INVALID_POSITION" {
				t.Errorf("code = %q, want INVALID_POSITION", apperr.CodeOf(err))
			}
		})
	}
}

func TestNewPositionClampsAFutureDeviceClock(t *testing.T) {
	/*
		A reading from the future is a broken handset clock. Trusting it would pin a
		stale position at the top of the track for as long as the skew lasts, so it
		is clamped to now — not rejected, because the rider cannot fix their phone's
		clock and should not become undeliverable over it.
	*/
	p, err := domain.NewPosition(someTrip, someRide, 26.85, 80.94, 10, now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("NewPosition: %v", err)
	}
	if p.RecordedAt.After(now) {
		t.Errorf("RecordedAt = %v, want clamped to %v", p.RecordedAt, now)
	}
}

func TestNewPositionKeepsASmallClockSkew(t *testing.T) {
	// Two minutes of skew is ordinary and must not be flattened, or every rider's
	// track collapses onto the server's clock and ordering by device time stops
	// meaning anything.
	slightlyAhead := now.Add(30 * time.Second)
	p, err := domain.NewPosition(someTrip, someRide, 26.85, 80.94, 10, slightlyAhead, now)
	if err != nil {
		t.Fatalf("NewPosition: %v", err)
	}
	if !p.RecordedAt.Equal(slightlyAhead) {
		t.Errorf("RecordedAt = %v, want the reported %v", p.RecordedAt, slightlyAhead)
	}
}

func TestStalePositionIsNotPresentedAsCurrent(t *testing.T) {
	/*
		A stale dot is worse than no dot. The customer watches a rider who is not
		moving and concludes he is parked round the corner, when his phone died
		twenty minutes ago.
	*/
	fresh := pos(t, 26.85, 80.94, now.Add(-30*time.Second))
	if fresh.IsStale(now) {
		t.Error("a 30-second-old position was called stale")
	}
	old := pos(t, 26.85, 80.94, now.Add(-10*time.Minute))
	if !old.IsStale(now) {
		t.Error("a ten-minute-old position was presented as current")
	}
}

func TestDecimationKeepsTheFirstPoint(t *testing.T) {
	// The start of a track is the one point that cannot be interpolated from its
	// neighbours.
	if !pos(t, 26.85, 80.94, now).ShouldPersist(nil) {
		t.Error("the first point of a track was discarded")
	}
}

func TestDecimationDiscardsGPSJitter(t *testing.T) {
	/*
		A stationary phone wanders by tens of metres. Writing those is how a track
		table fills with a rider standing still outside a kitchen for ten minutes.
	*/
	previous := pos(t, 26.8500, 80.9400, now)
	// Roughly 11 metres north — well inside the noise floor.
	jitter := pos(t, 26.8501, 80.9400, now.Add(5*time.Second))
	if jitter.ShouldPersist(&previous) {
		t.Errorf("persisted a %.0fm move, which is jitter",
			domain.DistanceM(26.8500, 80.9400, 26.8501, 80.9400))
	}
}

func TestDecimationKeepsRealMovement(t *testing.T) {
	previous := pos(t, 26.8500, 80.9400, now)
	// Roughly 110 metres north — unambiguous movement.
	moved := pos(t, 26.8510, 80.9400, now.Add(5*time.Second))
	if !moved.ShouldPersist(&previous) {
		t.Errorf("discarded a %.0fm move",
			domain.DistanceM(26.8500, 80.9400, 26.8510, 80.9400))
	}
}

func TestDecimationKeepsAPointWhenStuckInTraffic(t *testing.T) {
	/*
		Time is the second threshold, and it exists so a gap in a track means "no
		signal" rather than "not moving". Without it a rider stationary at a level
		crossing looks identical to a rider whose phone is off.
	*/
	previous := pos(t, 26.8500, 80.9400, now)
	stillThere := pos(t, 26.8500, 80.9400, now.Add(time.Minute))
	if !stillThere.ShouldPersist(&previous) {
		t.Error("a minute passed with no point written; a signal gap is now indistinguishable from standing still")
	}
}

func TestDistanceIsAccurateOverShortHops(t *testing.T) {
	// The decimation threshold is 60m, so the error over ~100m has to be small
	// relative to that or the threshold means nothing.
	//
	// 0.001 degrees of latitude is 111.2m anywhere on earth.
	got := domain.DistanceM(26.8500, 80.9400, 26.8510, 80.9400)
	if got < 110 || got > 113 {
		t.Errorf("DistanceM over 0.001 degrees latitude = %.1fm, want about 111m", got)
	}
	// Same point must be zero, not a rounding artefact that trips the threshold.
	if d := domain.DistanceM(26.85, 80.94, 26.85, 80.94); d != 0 {
		t.Errorf("distance from a point to itself = %v, want 0", d)
	}
}
