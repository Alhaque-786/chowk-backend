package domain_test

import (
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/catalog/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

/*
The FSSAI gate, which until now was a claim rather than a control.

Migration 0006 indexed merchants.fssai_expiry with the comment "An expired FSSAI
means the outlet must stop selling immediately". Nothing read the column. This is
the enforcement, and these are the cases that decide whether it is safe to point a
juice cart at it — the assisted-vendor model puts our least documented supply
behind exactly this check.
*/

// openKitchen is a marketplace kitchen that passes every other gate, so the only
// thing any test below can be measuring is the licence.
func openKitchen(expiry *time.Time) *domain.Kitchen {
	return &domain.Kitchen{
		Name:              "Test Juice Cart",
		IsActive:          true,
		IsAcceptingOrders: true,
		IsFirstParty:      false,
		FSSAIExpiry:       expiry,
		Hours: []domain.OpeningHours{
			// Every day, all day, so opening hours can never be the reason.
			{DayOfWeek: 0, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 1, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 2, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 3, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 4, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 5, OpensAt: 0, ClosesAt: 24 * time.Hour},
			{DayOfWeek: 6, OpensAt: 0, ClosesAt: 24 * time.Hour},
		},
	}
}

func day(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func TestExpiredFSSAIStopsTheKitchenSelling(t *testing.T) {
	now := time.Date(2026, time.June, 15, 12, 0, 0, 0, clock.IST)
	k := openKitchen(day(2026, time.June, 1)) // lapsed a fortnight ago

	ok, reason := k.CanAcceptOrder(now)
	if ok {
		t.Fatal("a kitchen with a lapsed FSSAI accepted an order; 0006 says it must " +
			"stop selling immediately")
	}
	if reason != domain.ReasonFSSAILapsed {
		t.Errorf("reason = %q, want %q", reason, domain.ReasonFSSAILapsed)
	}
}

func TestLicenceIsValidThroughItsExpiryDate(t *testing.T) {
	/*
		An FSSAI expires ON a date, not at an instant. A licence dated 15 June is
		valid for the whole of 15 June — treating it as "expired at 00:00" would stop
		a vendor selling on a day he is legally permitted to, and treating the
		timestamp as UTC would do it at 05:30 IST, in the middle of morning prep.
	*/
	expiry := day(2026, time.June, 15)
	k := openKitchen(expiry)

	for _, tc := range []struct {
		label string
		now   time.Time
		want  bool
	}{
		{"morning of the expiry day", time.Date(2026, time.June, 15, 9, 0, 0, 0, clock.IST), true},
		{"late evening of the expiry day", time.Date(2026, time.June, 15, 23, 30, 0, 0, clock.IST), true},
		{"the day after", time.Date(2026, time.June, 16, 9, 0, 0, 0, clock.IST), false},
		{"the day before", time.Date(2026, time.June, 14, 9, 0, 0, 0, clock.IST), true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			ok, reason := k.CanAcceptOrder(tc.now)
			if ok != tc.want {
				t.Errorf("accepting = %v, want %v (reason %q)", ok, tc.want, reason)
			}
		})
	}
}

func TestMissingExpiryBlocksAMarketplaceKitchen(t *testing.T) {
	/*
		"We have no record" and "it expired" are the same position when a regulator
		asks. Defaulting to allow is how a half-onboarded outlet starts selling —
		which is precisely the risk with a cart vendor whose paperwork we are helping
		him obtain.
	*/
	now := time.Date(2026, time.June, 15, 12, 0, 0, 0, clock.IST)
	k := openKitchen(nil)

	ok, reason := k.CanAcceptOrder(now)
	if ok {
		t.Fatal("a marketplace kitchen with NO recorded FSSAI accepted an order")
	}
	if reason != domain.ReasonFSSAILapsed {
		t.Errorf("reason = %q, want %q", reason, domain.ReasonFSSAILapsed)
	}
}

func TestFirstPartyIsExemptFromTheMissingExpiry(t *testing.T) {
	// Our own licence. Its absence from our own records is a data-entry gap, not an
	// unverified third party, and blocking the first-party kitchen on it would take
	// the whole platform offline over a null.
	now := time.Date(2026, time.June, 15, 12, 0, 0, 0, clock.IST)
	k := openKitchen(nil)
	k.IsFirstParty = true

	if ok, reason := k.CanAcceptOrder(now); !ok {
		t.Errorf("first-party kitchen blocked with reason %q", reason)
	}
}

func TestMoreUrgentReasonsWinOverTheLicence(t *testing.T) {
	/*
		Ordering of the switches is a product decision, not an accident. Somebody
		browsing at 3am should be told the kitchen is closed. Telling them about a
		licence is useless to them, and the earlier reasons are the ones they can act
		on by coming back later.
	*/
	now := time.Date(2026, time.June, 15, 12, 0, 0, 0, clock.IST)

	paused := openKitchen(day(2020, time.January, 1)) // also long expired
	paused.IsAcceptingOrders = false
	if _, reason := paused.CanAcceptOrder(now); reason != domain.ReasonPaused {
		t.Errorf("paused kitchen reported %q, want %q", reason, domain.ReasonPaused)
	}

	inactive := openKitchen(day(2020, time.January, 1))
	inactive.IsActive = false
	if _, reason := inactive.CanAcceptOrder(now); reason != domain.ReasonInactive {
		t.Errorf("inactive kitchen reported %q, want %q", reason, domain.ReasonInactive)
	}
}

func TestTheCustomerMessageSaysNothingAboutLicences(t *testing.T) {
	/*
		A customer cannot act on our compliance problem, and publishing "this
		merchant's FSSAI expired" on a public menu page is a defamatory-sounding
		claim about a named business. The message is deliberately bland.
	*/
	msg := domain.ReasonFSSAILapsed.CustomerMessage("Sharma Juice Corner")
	for _, leak := range []string{"FSSAI", "fssai", "licen", "expir", "complian"} {
		if contains(msg, leak) {
			t.Errorf("customer message %q leaks %q", msg, leak)
		}
	}
	if !contains(msg, "Sharma Juice Corner") {
		t.Errorf("message should still name the kitchen: %q", msg)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
