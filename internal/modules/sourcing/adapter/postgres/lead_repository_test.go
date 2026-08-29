package postgres_test

import (
	"context"
	"testing"
	"time"

	sourcingpg "github.com/zevoxa/chowk-backend/internal/modules/sourcing/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
	The lead SQL, tested where the SQL lives.

	The service tests use a fake repository, which is right for them — nothing in
	the service has a transaction or a cross-row invariant. But a fake cannot tell
	you whether the INSERT column order matches the scan, whether the partial index
	predicate agrees with the WHERE clause, or whether the phone CHECK fires. Those
	are exactly the mistakes that compile, pass every unit test, and fail on the
	first real submission.
*/

func repo(t *testing.T) (*sourcingpg.LeadRepository, context.Context) {
	t.Helper()
	return sourcingpg.NewLeadRepository(testutil.DB(t)), context.Background()
}

// cleanup deactivates a lead so tests do not accumulate rows in the shared
// development database. Soft, not a DELETE -- make check-soft-delete forbids the
// latter, and a test that violated the policy it is meant to protect would be an
// odd thing to leave behind.
func cleanup(t *testing.T, r *sourcingpg.LeadRepository, ctx context.Context, l *domain.Lead) {
	t.Helper()
	t.Cleanup(func() {
		l.Discard(time.Now().UTC())
		_ = r.Update(ctx, l)
	})
}

func lead(t *testing.T, business, phoneNum string) *domain.Lead {
	t.Helper()
	l, err := domain.NewLead(domain.KindReferral, business, "", phoneNum, "Hazratganj",
		domain.VendorJuiceCart, "", nil, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatalf("NewLead: %v", err)
	}
	return l
}

// TestCreateAndReadBackEveryField is the column-order test.
//
// An INSERT and a SELECT that disagree about column order is the classic failure
// here, and it is invisible when the mismatched columns are both text — the area
// silently becomes the business name and nothing errors.
func TestCreateAndReadBackEveryField(t *testing.T) {
	r, ctx := repo(t)

	want := lead(t, "Sharma Juice Corner", "9876543210")
	want.Note = "best nimbu paani near the crossing"
	want.ContactName = "Sharma ji"
	if err := r.Create(ctx, want); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cleanup(t, r, ctx, want)

	got, err := r.FindByID(ctx, want.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}

	if got.BusinessName != want.BusinessName {
		t.Errorf("BusinessName = %q, want %q", got.BusinessName, want.BusinessName)
	}
	if got.ContactName != want.ContactName {
		t.Errorf("ContactName = %q, want %q", got.ContactName, want.ContactName)
	}
	if got.ContactPhone != want.ContactPhone {
		t.Errorf("ContactPhone = %q, want %q", got.ContactPhone, want.ContactPhone)
	}
	if got.Area != want.Area {
		t.Errorf("Area = %q, want %q", got.Area, want.Area)
	}
	if got.Note != want.Note {
		t.Errorf("Note = %q, want %q", got.Note, want.Note)
	}
	if got.Kind != domain.KindReferral {
		t.Errorf("Kind = %q, want referral", got.Kind)
	}
	if got.VendorKind != domain.VendorJuiceCart {
		t.Errorf("VendorKind = %q, want juice_cart", got.VendorKind)
	}
	if got.Status != domain.StatusNew {
		t.Errorf("Status = %q, want new", got.Status)
	}
	if !got.IsActive {
		t.Error("IsActive = false on a freshly created lead")
	}
	if got.ReferredBy != nil {
		t.Errorf("ReferredBy = %v, want nil for an anonymous submission", got.ReferredBy)
	}
}

// TestTheDatabaseAcceptsRepeatMentions proves the missing unique index.
//
// This is the schema decision the whole module turns on, and it can only be
// checked against a real database — a fake repository will accept anything.
func TestTheDatabaseAcceptsRepeatMentions(t *testing.T) {
	r, ctx := repo(t)

	first := lead(t, "Sharma Juice Corner", "9811100011")
	second := lead(t, "the juice cart near Gate 2", "9811100011")

	if err := r.Create(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	cleanup(t, r, ctx, first)

	if err := r.Create(ctx, second); err != nil {
		t.Fatalf("the database rejected a second mention of the same number: %v\n"+
			"a unique index has crept onto contact_phone; see migration 0018", err)
	}
	cleanup(t, r, ctx, second)

	n, err := r.CountByPhone(ctx, first.ContactPhone)
	if err != nil {
		t.Fatalf("CountByPhone: %v", err)
	}
	if n != 2 {
		t.Errorf("CountByPhone = %d, want 2", n)
	}
}

// TestDiscardedLeadsLeaveTheQueueButStayCounted checks that the partial indexes
// and the WHERE clauses agree on what "active" means.
func TestDiscardedLeadsLeaveTheQueue(t *testing.T) {
	r, ctx := repo(t)

	junk := lead(t, "asdfgh", "9811100022")
	if err := r.Create(ctx, junk); err != nil {
		t.Fatalf("Create: %v", err)
	}

	junk.Discard(time.Now().UTC())
	if err := r.Update(ctx, junk); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, err := r.FindByID(ctx, junk.ID); err == nil {
		t.Error("FindByID returned a discarded lead")
	} else if apperr.CodeOf(err) != "LEAD_NOT_FOUND" {
		t.Errorf("code = %q, want LEAD_NOT_FOUND", apperr.CodeOf(err))
	}

	n, err := r.CountByPhone(ctx, junk.ContactPhone)
	if err != nil {
		t.Fatalf("CountByPhone: %v", err)
	}
	if n != 0 {
		t.Errorf("CountByPhone = %d for a discarded lead, want 0 — junk must not "+
			"inflate the signal the queue sorts on", n)
	}
}

// TestListFiltersAndOrders checks the dynamic WHERE/LIMIT/OFFSET assembly, which
// is built with string concatenation and argument counting — easy to get subtly
// wrong in a way that still runs.
func TestListFiltersAndOrders(t *testing.T) {
	r, ctx := repo(t)

	older := lead(t, "First stall", "9811100033")
	newer := lead(t, "Second stall", "9811100044")
	newer.CreatedAt = older.CreatedAt.Add(time.Hour)

	for _, l := range []*domain.Lead{older, newer} {
		if err := r.Create(ctx, l); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cleanup(t, r, ctx, l)
	}

	got, err := r.List(ctx, domain.LeadFilter{Status: domain.StatusNew, Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Oldest first: a queue worked newest-first starves the leads going cold.
	var seenOlder, seenNewer int
	for i, l := range got {
		switch l.ID {
		case older.ID:
			seenOlder = i + 1
		case newer.ID:
			seenNewer = i + 1
		}
	}
	if seenOlder == 0 || seenNewer == 0 {
		t.Fatalf("List did not return both leads (older=%d newer=%d)", seenOlder, seenNewer)
	}
	if seenOlder > seenNewer {
		t.Error("List returned newest first; the oldest lead must come first")
	}

	// A status nothing is in must return nothing, not everything — the classic
	// bug when a filter is appended to a query but the argument index is off.
	none, err := r.List(ctx, domain.LeadFilter{Status: domain.StatusConverted, Limit: 50})
	if err != nil {
		t.Fatalf("List converted: %v", err)
	}
	for _, l := range none {
		if l.ID == older.ID || l.ID == newer.ID {
			t.Error("the status filter did not apply; a new lead came back as converted")
		}
	}
}

// TestCreateRejectsWhatTheDomainWouldHaveCaught proves the database is a real
// second line rather than decoration. Reaching it means a caller built a Lead
// without going through NewLead.
func TestCreateRejectsAMalformedPhone(t *testing.T) {
	r, ctx := repo(t)

	bad := lead(t, "Sharma Juice Corner", "9876543210")
	bad.ContactPhone = "98765" // bypassing the value object on purpose

	err := r.Create(ctx, bad)
	if err == nil {
		t.Fatal("the database accepted a malformed phone number")
	}
	if apperr.CodeOf(err) != "INVALID_LEAD" {
		t.Errorf("code = %q, want INVALID_LEAD", apperr.CodeOf(err))
	}
}
