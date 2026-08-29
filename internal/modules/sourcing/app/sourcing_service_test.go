package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/app"
	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/phone"
)

/*
A fake repository rather than a real database.

Deliberate, and different from how promotion tests are written. Those need a
real Postgres because what they are testing IS the concurrency: two
SERIALIZABLE transactions have to be able to conflict. Nothing here has a
transaction or an invariant across rows, so a real database would only make
these slower and would not test anything the fake does not.

It counts CountByPhone calls, because one of the tests below is about how many
times the service asks.
*/
type fakeRepo struct {
	leads       []*domain.Lead
	countCalls  int
	failOnCount bool
}

func (f *fakeRepo) Create(_ context.Context, l *domain.Lead) error {
	f.leads = append(f.leads, l)
	return nil
}

func (f *fakeRepo) FindByID(_ context.Context, leadID id.ID) (*domain.Lead, error) {
	for _, l := range f.leads {
		if l.ID == leadID && l.IsActive {
			return l, nil
		}
	}
	return nil, apperr.NotFound("LEAD_NOT_FOUND", "That lead could not be found.")
}

func (f *fakeRepo) Update(_ context.Context, _ *domain.Lead) error { return nil }

func (f *fakeRepo) List(_ context.Context, filter domain.LeadFilter) ([]*domain.Lead, error) {
	out := []*domain.Lead{}
	for _, l := range f.leads {
		if !l.IsActive {
			continue
		}
		if filter.Status != "" && l.Status != filter.Status {
			continue
		}
		out = append(out, l)
	}
	if filter.Offset < len(out) {
		out = out[filter.Offset:]
	} else {
		out = nil
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeRepo) CountByPhone(_ context.Context, p phone.Phone) (int, error) {
	f.countCalls++
	if f.failOnCount {
		return 0, apperr.Internal(context.Canceled)
	}
	n := 0
	for _, l := range f.leads {
		if l.ContactPhone == p && l.IsActive {
			n++
		}
	}
	return n, nil
}

func newService() (*app.SourcingService, *fakeRepo) {
	repo := &fakeRepo{}
	clk := &clock.Fixed{T: time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)}
	return app.NewSourcingService(repo, clk), repo
}

func submit(t *testing.T, s *app.SourcingService, business, phoneNum string) *domain.Lead {
	t.Helper()
	lead, err := s.Submit(context.Background(), app.SubmitInput{
		Kind:         domain.KindReferral,
		BusinessName: business,
		ContactPhone: phoneNum,
		Area:         "Hazratganj",
		VendorKind:   domain.VendorJuiceCart,
	})
	if err != nil {
		t.Fatalf("Submit(%q): %v", business, err)
	}
	return lead
}

// TestSubmitDoesNotRejectARepeatMention is the central test in this package.
//
// The instinct when building a lead form is to treat a second submission for the
// same number as a duplicate and reject it. That instinct is wrong here, and the
// schema deliberately has no unique index to enforce it. The second person naming
// the same cart is telling us something the first could not: that more than one
// customer wants it. Rejecting them throws that away and tells a helpful customer
// they did something wrong.
func TestSubmitDoesNotRejectARepeatMention(t *testing.T) {
	s, repo := newService()

	submit(t, s, "Sharma Juice Corner", "9876543210")
	submit(t, s, "Sharma juice", "9876543210")
	submit(t, s, "the juice guy near Gate 2", "9876543210")

	if len(repo.leads) != 3 {
		t.Fatalf("stored %d leads, want 3 — a repeat mention was swallowed", len(repo.leads))
	}
}

// TestListSurfacesMentionsAsPriority checks that the signal the schema preserves
// actually reaches whoever works the queue. Storing it and never showing it would
// be the same as deduplicating.
func TestListSurfacesMentionsAsPriority(t *testing.T) {
	s, _ := newService()
	submit(t, s, "Sharma Juice Corner", "9876543210")
	submit(t, s, "Sharma juice", "9876543210")
	submit(t, s, "Verma Chaat", "9812345678")

	got, err := s.List(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}

	byPhone := map[string]int{}
	for _, row := range got {
		byPhone[row.Lead.ContactPhone.String()] = row.Mentions
	}
	if byPhone["+919876543210"] != 2 {
		t.Errorf("mentions for the twice-named cart = %d, want 2", byPhone["+919876543210"])
	}
	if byPhone["+919812345678"] != 1 {
		t.Errorf("mentions for the once-named stall = %d, want 1", byPhone["+919812345678"])
	}
}

// TestListCountsOncePerNumber guards the N+1 from getting worse than advertised.
//
// The service documents itself as one count per DISTINCT number, not one per row.
// A queue holding six mentions of the same cart should cost one count, not six,
// and the difference only shows up under exactly the data this table is designed
// to collect.
func TestListCountsOncePerNumber(t *testing.T) {
	s, repo := newService()
	for range 6 {
		submit(t, s, "Sharma Juice Corner", "9876543210")
	}
	submit(t, s, "Verma Chaat", "9812345678")

	if _, err := s.List(context.Background(), "", 0, 0); err != nil {
		t.Fatalf("List: %v", err)
	}
	if repo.countCalls != 2 {
		t.Errorf("CountByPhone called %d times, want 2 (one per distinct number)", repo.countCalls)
	}
}

func TestListCapsThePageSize(t *testing.T) {
	// A caller asking for everything gets a page, not the table.
	s, _ := newService()
	for i := range 120 {
		submit(t, s, "Stall", "98765432"+string(rune('0'+i%10))+"0")
	}
	got, err := s.List(context.Background(), "", 10_000, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) > 100 {
		t.Errorf("returned %d rows, want at most 100", len(got))
	}
}

func TestListFiltersByStatus(t *testing.T) {
	s, repo := newService()
	submit(t, s, "Sharma Juice Corner", "9876543210")
	submit(t, s, "Verma Chaat", "9812345678")

	if _, err := s.MoveTo(context.Background(), repo.leads[0].ID, domain.StatusContacted); err != nil {
		t.Fatalf("MoveTo: %v", err)
	}

	got, err := s.List(context.Background(), domain.StatusNew, 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Lead.BusinessName != "Verma Chaat" {
		t.Fatalf("status filter returned %d rows, want just the untouched one", len(got))
	}
}

func TestDiscardRemovesALeadFromTheQueueWithoutRejectingIt(t *testing.T) {
	s, repo := newService()
	lead := submit(t, s, "asdfgh", "9876543210")

	if err := s.Discard(context.Background(), lead.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	got, err := s.List(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("discarded lead still in the queue")
	}
	if repo.leads[0].Status == domain.StatusRejected {
		t.Error("Discard set status to rejected — junk must not count as a rejection")
	}
}

func TestParseStatusRejectsAnInventedOne(t *testing.T) {
	if _, err := app.ParseStatus("thinking_about_it"); err == nil {
		t.Fatal("accepted an invented status")
	}
	// Empty means "no filter" and must not be an error, or the unfiltered queue
	// becomes unreachable.
	if got, err := app.ParseStatus(""); err != nil || got != "" {
		t.Errorf("ParseStatus(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestSubmitRejectsBadInputBeforeStoringAnything(t *testing.T) {
	s, repo := newService()
	_, err := s.Submit(context.Background(), app.SubmitInput{
		Kind: domain.KindReferral, BusinessName: "", ContactPhone: "9876543210",
		Area: "Hazratganj", VendorKind: domain.VendorJuiceCart,
	})
	if err == nil {
		t.Fatal("Submit accepted a blank business name")
	}
	if len(repo.leads) != 0 {
		t.Errorf("stored %d leads despite validation failing", len(repo.leads))
	}
}
