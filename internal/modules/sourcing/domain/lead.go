// Package domain holds the sourcing module's entities and ports.
//
// The interesting decision here is what counts as a duplicate. See Lead.
package domain

import (
	"context"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/phone"
)

// Kind is who is telling us about the vendor.
//
// Load-bearing rather than decorative: the two convert at very different rates
// and the outreach script is not the same. A referral means somebody already buys
// there and vouches for the food; a self-signup means the vendor found us, which
// is rarer and warmer but says nothing about whether anyone likes their food.
type Kind string

const (
	KindReferral   Kind = "referral"
	KindSelfSignup Kind = "self_signup"
)

// VendorKind is the sort of business, which decides how it would operate.
//
// Not cosmetic: a juice cart is almost certainly an assisted vendor in the sense
// of migration 0017, while a restaurant almost certainly runs its own dashboard.
// The category is the first guess at which onboarding path to offer.
type VendorKind string

const (
	VendorJuiceCart    VendorKind = "juice_cart"
	VendorTiffin       VendorKind = "tiffin"
	VendorCloudKitchen VendorKind = "cloud_kitchen"
	VendorRestaurant   VendorKind = "restaurant"
	VendorSweetShop    VendorKind = "sweet_shop"
	VendorOther        VendorKind = "other"
)

// Status is how far the conversation has got.
type Status string

const (
	StatusNew        Status = "new"
	StatusContacted  Status = "contacted"
	StatusVisiting   Status = "visiting"
	StatusOnboarding Status = "onboarding"
	StatusConverted  Status = "converted"
	// StatusRejected is a real vendor we decided against. It is NOT the same as
	// an inactive lead, which is junk. Keeping them apart is what stops the
	// rejection rate becoming meaningless the first week somebody submits "asdfgh".
	StatusRejected Status = "rejected"
)

// terminal statuses end the conversation. Reopening one is a deliberate act, not
// a side effect of somebody clicking through a list.
var terminal = map[Status]bool{StatusConverted: true, StatusRejected: true}

// Lead is a vendor somebody wants on the platform.
//
// THERE IS NO UNIQUENESS ON PHONE, and that is the central modelling decision.
// The obvious design rejects a second submission for the same number as a
// duplicate. It would be throwing away the best signal this type carries: three
// separate customers naming the same cart in Hazratganj is not noise, it is the
// strongest argument available for going there first. Repeat mentions are counted,
// not collapsed. Spam is the rate limiter's job.
type Lead struct {
	ID           id.ID
	Kind         Kind
	BusinessName string
	ContactName  string
	ContactPhone phone.Phone
	Area         string
	VendorKind   VendorKind
	Note         string
	Status       Status
	// ReferredBy is the signed-in customer who submitted, when there was one. The
	// form works signed out on purpose — requiring an account to tell us about a
	// juice cart would lose most of the leads this exists to collect.
	ReferredBy *id.ID
	IsActive   bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// NewLead validates and constructs a lead from submitted input.
//
// Everything is trimmed before validation, because a field containing only spaces
// is empty as far as a human is concerned and the database CHECK agrees.
func NewLead(
	kind Kind,
	businessName, contactName, rawPhone, area string,
	vendorKind VendorKind,
	note string,
	referredBy *id.ID,
	now time.Time,
) (*Lead, error) {
	if kind != KindReferral && kind != KindSelfSignup {
		return nil, apperr.Invalid("INVALID_LEAD_KIND", "Tell us whether this is your own business or one you know.").
			WithField("kind", "invalid")
	}
	if !validVendorKind(vendorKind) {
		return nil, apperr.Invalid("INVALID_VENDOR_KIND", "Pick the closest match for the kind of business.").
			WithField("vendor_kind", "invalid")
	}

	businessName = strings.TrimSpace(businessName)
	if businessName == "" {
		return nil, apperr.Invalid("MISSING_BUSINESS_NAME", "What is the shop or cart called?").
			WithField("business_name", "required")
	}
	if len(businessName) > 120 {
		return nil, apperr.Invalid("BUSINESS_NAME_TOO_LONG", "Keep the name under 120 characters.").
			WithField("business_name", "too_long")
	}

	area = strings.TrimSpace(area)
	if area == "" {
		return nil, apperr.Invalid("MISSING_AREA", "Where is it? A locality is enough.").
			WithField("area", "required")
	}
	if len(area) > 120 {
		return nil, apperr.Invalid("AREA_TOO_LONG", "Keep the locality under 120 characters.").
			WithField("area", "too_long")
	}

	// A cap rather than validation: notes are free text and long ones are usually
	// useful, but unbounded text from an unauthenticated endpoint is a storage
	// problem waiting to happen.
	note = strings.TrimSpace(note)
	if len(note) > 1000 {
		return nil, apperr.Invalid("NOTE_TOO_LONG", "Keep the note under 1000 characters.").
			WithField("note", "too_long")
	}

	p, err := phone.New(rawPhone)
	if err != nil {
		return nil, err
	}

	return &Lead{
		ID:           id.New(),
		Kind:         kind,
		BusinessName: businessName,
		// Blank is normal and allowed: somebody referring the cart outside their
		// office knows the stall, not the owner's name.
		ContactName:  strings.TrimSpace(contactName),
		ContactPhone: p,
		Area:         area,
		VendorKind:   vendorKind,
		Note:         note,
		Status:       StatusNew,
		ReferredBy:   referredBy,
		IsActive:     true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// MoveTo advances the conversation.
//
// The only rule enforced is that a terminal status does not silently change. A
// converted vendor quietly becoming "contacted" again would corrupt the only
// number this table exists to produce.
func (l *Lead) MoveTo(next Status, now time.Time) error {
	if !validStatus(next) {
		return apperr.Invalid("INVALID_LEAD_STATUS", "That is not a status a lead can be in.").
			WithField("status", "invalid")
	}
	if l.Status == next {
		return nil // idempotent: clicking twice is not an error
	}
	if terminal[l.Status] {
		return apperr.Conflict("LEAD_ALREADY_CLOSED",
			"This lead was already closed. Reopen it before changing the status.")
	}
	l.Status = next
	l.UpdatedAt = now
	return nil
}

// Reopen puts a closed lead back in the queue.
//
// Separate from MoveTo so that reopening is always a deliberate act. Vendors say
// no in February and yes in June, and that is worth supporting — but not by
// accident.
func (l *Lead) Reopen(now time.Time) error {
	if !terminal[l.Status] {
		return apperr.Conflict("LEAD_NOT_CLOSED", "This lead is already open.")
	}
	l.Status = StatusContacted
	l.UpdatedAt = now
	return nil
}

// Discard marks a lead as junk.
//
// Deliberately not the same as rejecting it — see the Status comment. Junk never
// was a lead and must not be counted as one.
func (l *Lead) Discard(now time.Time) {
	l.IsActive = false
	l.UpdatedAt = now
}

func validStatus(s Status) bool {
	switch s {
	case StatusNew, StatusContacted, StatusVisiting, StatusOnboarding, StatusConverted, StatusRejected:
		return true
	}
	return false
}

func validVendorKind(v VendorKind) bool {
	switch v {
	case VendorJuiceCart, VendorTiffin, VendorCloudKitchen, VendorRestaurant, VendorSweetShop, VendorOther:
		return true
	}
	return false
}

// LeadFilter narrows the admin queue.
type LeadFilter struct {
	Status Status // empty means every status
	Limit  int
	Offset int
}

// LeadRepository is the port. Implemented in adapter/postgres.
type LeadRepository interface {
	Create(ctx context.Context, l *Lead) error
	FindByID(ctx context.Context, leadID id.ID) (*Lead, error)
	Update(ctx context.Context, l *Lead) error
	List(ctx context.Context, f LeadFilter) ([]*Lead, error)
	// CountByPhone answers "how many people have named this number", which is the
	// signal the schema refuses to deduplicate away.
	CountByPhone(ctx context.Context, p phone.Phone) (int, error)
}
