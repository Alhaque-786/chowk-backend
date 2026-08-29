package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/app"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
	Who may report a position, and who may see one.

	The read rules here are STRICTER than the delivery record's, and that is the
	part worth pinning: a customer may watch their rider while he is carrying their
	food and not afterwards. After delivery the position stops being information
	about their order and becomes a live feed of a named person's movements, which
	a completed food order does not buy.
*/

type fakeLive struct{ pos *domain.Position }

func (f *fakeLive) Put(_ context.Context, p domain.Position, _ time.Duration) error {
	f.pos = &p
	return nil
}
func (f *fakeLive) Get(context.Context, id.ID) *domain.Position { return f.pos }
func (f *fakeLive) Clear(context.Context, id.ID) error          { f.pos = nil; return nil }

type fakeTrack struct {
	points []domain.Position
}

func (f *fakeTrack) Append(_ context.Context, p domain.Position) error {
	f.points = append(f.points, p)
	return nil
}
func (f *fakeTrack) Latest(context.Context, id.ID) (*domain.Position, error) {
	if len(f.points) == 0 {
		return nil, nil
	}
	return &f.points[len(f.points)-1], nil
}
func (f *fakeTrack) List(context.Context, id.ID) ([]domain.Position, error) { return f.points, nil }

var at = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func locationFixture(t *testing.T, status domain.Status, riderID *id.ID, owner id.ID) (
	*app.LocationService, *fakeLive, *fakeTrack, *domain.Delivery,
) {
	t.Helper()
	delivery := &domain.Delivery{ID: id.New(), Status: status, RiderID: riderID}
	live := &fakeLive{}
	track := &fakeTrack{}
	svc := app.NewLocationService(
		&fakeDeliveries{delivery: delivery},
		live, track,
		&fakeOwnership{owner: owner},
		&clock.Fixed{T: at},
	)
	return svc, live, track, delivery
}

func TestARiderCanReportOnHisOwnDelivery(t *testing.T) {
	rider := id.New()
	svc, live, track, d := locationFixture(t, domain.StatusPickedUp, &rider, id.New())

	err := svc.Report(context.Background(), app.ReportInput{
		DeliveryID: d.ID, RiderID: rider, Latitude: 26.85, Longitude: 80.94, RecordedAt: at,
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if live.pos == nil {
		t.Error("nothing was written to the live store, so no dot would appear")
	}
	// The first point of a track is always kept — it cannot be interpolated.
	if len(track.points) != 1 {
		t.Errorf("track has %d points, want the first one kept", len(track.points))
	}
}

func TestARiderCannotReportOnSomebodyElsesDelivery(t *testing.T) {
	assigned := id.New()
	svc, live, _, d := locationFixture(t, domain.StatusPickedUp, &assigned, id.New())

	err := svc.Report(context.Background(), app.ReportInput{
		DeliveryID: d.ID, RiderID: id.New(), Latitude: 26.85, Longitude: 80.94, RecordedAt: at,
	})
	if err == nil {
		t.Fatal("a rider wrote a position onto another rider's delivery")
	}
	// Not-yours and does-not-exist are the same answer, so probing ids teaches
	// nothing about which deliveries are real.
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("kind = %v, want NotFound", apperr.KindOf(err))
	}
	if live.pos != nil {
		t.Error("the position was stored anyway")
	}
}

func TestNobodyCanReportOnADeliveryWithNoRider(t *testing.T) {
	// A third-party delivery has no rider_id. A nil pointer must not compare equal
	// to whoever happens to be calling.
	svc, _, _, d := locationFixture(t, domain.StatusPickedUp, nil, id.New())

	if err := svc.Report(context.Background(), app.ReportInput{
		DeliveryID: d.ID, RiderID: id.New(), Latitude: 26.85, Longitude: 80.94, RecordedAt: at,
	}); err == nil {
		t.Fatal("a position was accepted for a delivery with no assigned rider")
	}
}

func TestReportingStopsWhenTheDeliveryIsOver(t *testing.T) {
	rider := id.New()
	svc, live, _, d := locationFixture(t, domain.StatusDelivered, &rider, id.New())

	if err := svc.Report(context.Background(), app.ReportInput{
		DeliveryID: d.ID, RiderID: rider, Latitude: 26.85, Longitude: 80.94, RecordedAt: at,
	}); err == nil {
		t.Fatal("a delivered delivery kept accepting positions")
	}
	if live.pos != nil {
		t.Error("a position was stored for a finished delivery")
	}
}

func TestTheCustomerSeesTheirRider(t *testing.T) {
	rider, owner := id.New(), id.New()
	svc, _, _, d := locationFixture(t, domain.StatusPickedUp, &rider, owner)
	_ = svc.Report(context.Background(), app.ReportInput{
		DeliveryID: d.ID, RiderID: rider, Latitude: 26.85, Longitude: 80.94, RecordedAt: at,
	})

	got, err := svc.ForOrder(context.Background(), id.New(), owner, false)
	if err != nil {
		t.Fatalf("ForOrder: %v", err)
	}
	if got == nil {
		t.Fatal("the customer got no position for their own in-flight order")
	}
}

func TestTrackingStopsAtDelivery(t *testing.T) {
	/*
		The rule worth arguing about, so it is pinned. Once the food has arrived the
		rider's position is no longer about this customer's order — it is a live
		feed of a named individual for the rest of his shift.
	*/
	rider, owner := id.New(), id.New()
	svc, live, _, _ := locationFixture(t, domain.StatusDelivered, &rider, owner)
	// A position is present in the store; the rule, not the absence of data, is
	// what must stop it being shown.
	live.pos = &domain.Position{Latitude: 26.85, Longitude: 80.94, RecordedAt: at}

	got, err := svc.ForOrder(context.Background(), id.New(), owner, false)
	if err != nil {
		t.Fatalf("ForOrder: %v", err)
	}
	if got != nil {
		t.Error("the rider was still trackable after the food had arrived")
	}
}

func TestAStalePositionIsNotShown(t *testing.T) {
	// A stationary dot reads as "parked round the corner". The truth is usually a
	// dead handset, and no dot says that honestly.
	rider, owner := id.New(), id.New()
	svc, live, _, _ := locationFixture(t, domain.StatusPickedUp, &rider, owner)
	live.pos = &domain.Position{
		Latitude: 26.85, Longitude: 80.94, RecordedAt: at.Add(-10 * time.Minute),
	}

	got, err := svc.ForOrder(context.Background(), id.New(), owner, false)
	if err != nil {
		t.Fatalf("ForOrder: %v", err)
	}
	if got != nil {
		t.Error("a ten-minute-old position was presented as the rider's current location")
	}
}

func TestAStrangerCannotWatchSomebodyElsesRider(t *testing.T) {
	rider, owner := id.New(), id.New()
	svc, live, _, _ := locationFixture(t, domain.StatusPickedUp, &rider, owner)
	live.pos = &domain.Position{Latitude: 26.85, Longitude: 80.94, RecordedAt: at}

	if _, err := svc.ForOrder(context.Background(), id.New(), id.New(), false); err == nil {
		t.Fatal("a stranger watched another customer's rider move")
	}
}
