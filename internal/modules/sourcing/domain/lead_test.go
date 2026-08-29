package domain_test

import (
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

var at = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

func newValid(t *testing.T) *domain.Lead {
	t.Helper()
	l, err := domain.NewLead(
		domain.KindReferral, "Sharma Juice Corner", "", "9876543210",
		"Hazratganj", domain.VendorJuiceCart, "", nil, at,
	)
	if err != nil {
		t.Fatalf("NewLead returned error: %v", err)
	}
	return l
}

func TestNewLeadNormalisesThePhone(t *testing.T) {
	// The value object does the work; this pins that the lead actually uses it
	// rather than storing whatever was typed. Two rows for the same cart written
	// as "09876543210" and "+919876543210" would not group, and grouping is the
	// entire point of this table.
	for _, raw := range []string{"9876543210", "09876543210", "+91 98765 43210"} {
		l, err := domain.NewLead(domain.KindReferral, "X", "", raw, "Y", domain.VendorOther, "", nil, at)
		if err != nil {
			t.Fatalf("NewLead(%q) returned error: %v", raw, err)
		}
		if got := l.ContactPhone.String(); got != "+919876543210" {
			t.Errorf("NewLead(%q) phone = %q, want +919876543210", raw, got)
		}
	}
}

func TestNewLeadRejectsWhatCannotBeActedOn(t *testing.T) {
	tests := []struct {
		name, business, phone, area string
		kind                        domain.Kind
		vendorKind                  domain.VendorKind
		wantCode                    string
	}{
		{"blank business", "", "9876543210", "Hazratganj", domain.KindReferral, domain.VendorJuiceCart, "MISSING_BUSINESS_NAME"},
		// Whitespace-only is empty to a human, and the database CHECK agrees. A
		// domain that disagreed would surface as a 500 instead of a field error.
		{"whitespace business", "   ", "9876543210", "Hazratganj", domain.KindReferral, domain.VendorJuiceCart, "MISSING_BUSINESS_NAME"},
		{"blank area", "Sharma", "9876543210", "", domain.KindReferral, domain.VendorJuiceCart, "MISSING_AREA"},
		{"whitespace area", "Sharma", "9876543210", "  ", domain.KindReferral, domain.VendorJuiceCart, "MISSING_AREA"},
		{"bad phone", "Sharma", "12345", "Hazratganj", domain.KindReferral, domain.VendorJuiceCart, "INVALID_PHONE"},
		{"unknown kind", "Sharma", "9876543210", "Hazratganj", "walk_in", domain.VendorJuiceCart, "INVALID_LEAD_KIND"},
		{"unknown vendor kind", "Sharma", "9876543210", "Hazratganj", domain.KindReferral, "food_truck", "INVALID_VENDOR_KIND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.NewLead(tt.kind, tt.business, "", tt.phone, tt.area, tt.vendorKind, "", nil, at)
			if err == nil {
				t.Fatalf("NewLead accepted %s", tt.name)
			}
			if got := apperr.CodeOf(err); got != tt.wantCode {
				t.Errorf("code = %q, want %q", got, tt.wantCode)
			}
		})
	}
}

func TestNewLeadAcceptsAMissingContactName(t *testing.T) {
	// Deliberate: somebody referring the cart outside their office knows the
	// stall, not the owner's name. Demanding it would cost more leads than the
	// field is worth.
	l, err := domain.NewLead(domain.KindReferral, "The cart by Gate 2", "", "9876543210",
		"Gomti Nagar", domain.VendorJuiceCart, "", nil, at)
	if err != nil {
		t.Fatalf("NewLead rejected a blank contact name: %v", err)
	}
	if l.ContactName != "" {
		t.Errorf("ContactName = %q, want empty", l.ContactName)
	}
}

func TestNewLeadStartsNewAndActive(t *testing.T) {
	l := newValid(t)
	if l.Status != domain.StatusNew {
		t.Errorf("Status = %q, want new", l.Status)
	}
	if !l.IsActive {
		t.Error("IsActive = false, want true")
	}
}

func TestMoveToIsIdempotent(t *testing.T) {
	// Clicking "contacted" twice is not an error. Treating it as one would make
	// the queue punish double-clicks, which is a thing people do.
	l := newValid(t)
	if err := l.MoveTo(domain.StatusContacted, at); err != nil {
		t.Fatalf("first move: %v", err)
	}
	if err := l.MoveTo(domain.StatusContacted, at); err != nil {
		t.Fatalf("repeat move should be a no-op, got: %v", err)
	}
}

func TestMoveToRefusesToReopenSilently(t *testing.T) {
	// A converted vendor quietly becoming "contacted" again would corrupt the one
	// number this table exists to produce. Reopening has to be deliberate.
	for _, closed := range []domain.Status{domain.StatusConverted, domain.StatusRejected} {
		l := newValid(t)
		if err := l.MoveTo(closed, at); err != nil {
			t.Fatalf("move to %s: %v", closed, err)
		}
		err := l.MoveTo(domain.StatusContacted, at)
		if err == nil {
			t.Fatalf("moving out of %s silently succeeded", closed)
		}
		if got := apperr.CodeOf(err); got != "LEAD_ALREADY_CLOSED" {
			t.Errorf("code = %q, want LEAD_ALREADY_CLOSED", got)
		}
	}
}

func TestMoveToRejectsAStatusThatIsNotAStatus(t *testing.T) {
	l := newValid(t)
	if err := l.MoveTo("thinking_about_it", at); err == nil {
		t.Fatal("accepted an invented status")
	}
}

func TestReopenOnlyAppliesToAClosedLead(t *testing.T) {
	l := newValid(t)
	if err := l.Reopen(at); err == nil {
		t.Fatal("reopened a lead that was never closed")
	}

	if err := l.MoveTo(domain.StatusRejected, at); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := l.Reopen(at); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if l.Status != domain.StatusContacted {
		t.Errorf("Status after reopen = %q, want contacted", l.Status)
	}
}

func TestDiscardIsNotRejection(t *testing.T) {
	/*
		The distinction migration 0018 exists to preserve. Junk never was a lead and
		must not be counted as one; a rejection is a real vendor we said no to. If
		Discard also set the status to rejected, the rejection rate would become
		meaningless the first week somebody submitted "asdfgh".
	*/
	l := newValid(t)
	l.Discard(at)

	if l.IsActive {
		t.Error("IsActive = true after Discard")
	}
	if l.Status != domain.StatusNew {
		t.Errorf("Status = %q after Discard, want it untouched at new", l.Status)
	}
}

func TestNoteIsCapped(t *testing.T) {
	// Unbounded text from an unauthenticated endpoint is a storage problem
	// waiting to happen.
	long := make([]byte, 1001)
	for i := range long {
		long[i] = 'a'
	}
	_, err := domain.NewLead(domain.KindReferral, "X", "", "9876543210", "Y",
		domain.VendorOther, string(long), nil, at)
	if err == nil {
		t.Fatal("accepted a 1001-character note")
	}
	if got := apperr.CodeOf(err); got != "NOTE_TOO_LONG" {
		t.Errorf("code = %q, want NOTE_TOO_LONG", got)
	}
}
