package app_test

import (
	"context"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/app"
	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

/*
	Who may see a delivery.

	This is a regression test for a live information leak, not a hypothetical. The
	tracking endpoint checked only that the caller held a valid token, so ANY
	signed-in user could read ANY order's delivery by id and receive the rider's
	name and personal phone number. Order ids are UUIDv7 and therefore
	time-ordered, so consecutive orders share a long prefix and the ids are
	partially guessable rather than opaque.

	Fakes rather than a database: the question is entirely about which branch runs,
	and a real Postgres would add setup without adding coverage.
*/

type fakeOwnership struct {
	owner id.ID
	calls int
}

func (f *fakeOwnership) IsOrderOwnedBy(_ context.Context, _, userID id.ID) (bool, error) {
	f.calls++
	return userID == f.owner, nil
}

type fakeDeliveries struct{ delivery *domain.Delivery }

func (f *fakeDeliveries) FindByOrderID(context.Context, id.ID) (*domain.Delivery, error) {
	return f.delivery, nil
}
func (f *fakeDeliveries) Create(context.Context, *domain.Delivery) error { return nil }
func (f *fakeDeliveries) Update(context.Context, *domain.Delivery) error { return nil }
func (f *fakeDeliveries) FindByID(context.Context, id.ID) (*domain.Delivery, error) {
	return f.delivery, nil
}
func (f *fakeDeliveries) FindByProviderRef(context.Context, string, string) (*domain.Delivery, error) {
	return f.delivery, nil
}
func (f *fakeDeliveries) FindByOrderIDForUpdate(context.Context, id.ID) (*domain.Delivery, error) {
	return f.delivery, nil
}
func (f *fakeDeliveries) ListActiveForKitchen(context.Context, id.ID) ([]*domain.Delivery, error) {
	return nil, nil
}
func (f *fakeDeliveries) ListActiveForRider(context.Context, id.ID) ([]*domain.Delivery, error) {
	return nil, nil
}

func newTracking(owner id.ID) (*app.DispatchService, *fakeOwnership) {
	own := &fakeOwnership{owner: owner}
	deliveries := &fakeDeliveries{delivery: &domain.Delivery{
		RiderName: "Imran", RiderPhone: "+919812345670", Status: domain.StatusPickedUp,
	}}
	// Only these two collaborators are on this path; the rest are untouched by
	// GetForOrder and passing them would be pretending otherwise.
	return app.NewDispatchService(nil, deliveries, nil, nil, own, nil, nil), own
}

func TestACustomerCanTrackTheirOwnDelivery(t *testing.T) {
	owner := id.New()
	svc, _ := newTracking(owner)

	got, err := svc.GetForOrder(context.Background(), id.New(), owner, false)
	if err != nil {
		t.Fatalf("the owner was refused their own delivery: %v", err)
	}
	if got.RiderPhone == "" {
		t.Error("the owner got a delivery with no rider phone; they need it to call him")
	}
}

func TestAStrangerCannotTrackSomebodyElsesDelivery(t *testing.T) {
	svc, _ := newTracking(id.New())

	_, err := svc.GetForOrder(context.Background(), id.New(), id.New(), false)
	if err == nil {
		t.Fatal("a stranger read another customer's delivery, exposing the rider's phone number")
	}
	// NOT FOUND, not forbidden. Distinguishing "not yours" from "does not exist"
	// turns the endpoint into an oracle for which order ids are real.
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("kind = %v, want NotFound so the endpoint reveals nothing", apperr.KindOf(err))
	}
}

func TestStaffCanTrackAnyDelivery(t *testing.T) {
	// Support answering "where is my food" cannot do it through the customer's own
	// account, so this path has to exist.
	svc, own := newTracking(id.New())

	if _, err := svc.GetForOrder(context.Background(), id.New(), id.New(), true); err != nil {
		t.Fatalf("staff were refused: %v", err)
	}
	// And it must not have consulted ownership at all — a staff read is authorised
	// by role, and asking anyway would mean a support agent's access depended on
	// whose order it was.
	if own.calls != 0 {
		t.Errorf("ownership was checked %d times on the staff path; it should not be consulted", own.calls)
	}
}

func TestTheViewerIsNotOptional(t *testing.T) {
	/*
		The zero value must DENY, not permit.

		platformActor is a boolean rather than a nil-able viewer id precisely so
		that forgetting to populate it fails closed. A caller that passes an empty
		viewer and false is a caller that forgot something, and the safe answer to
		that is no.
	*/
	svc, _ := newTracking(id.New())

	if _, err := svc.GetForOrder(context.Background(), id.New(), id.ID{}, false); err == nil {
		t.Fatal("an empty viewer id was allowed to read a delivery")
	}
}
