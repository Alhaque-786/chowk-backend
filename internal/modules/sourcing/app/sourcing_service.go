// Package app holds the sourcing module's use cases.
package app

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// maxPageSize caps the admin list. A queue nobody can page through is a queue
// nobody works.
const maxPageSize = 100

type SourcingService struct {
	leads domain.LeadRepository
	clock clock.Clock
}

func NewSourcingService(leads domain.LeadRepository, c clock.Clock) *SourcingService {
	return &SourcingService{leads: leads, clock: c}
}

// SubmitInput is a lead as the public form sends it.
type SubmitInput struct {
	Kind         domain.Kind
	BusinessName string
	ContactName  string
	ContactPhone string
	Area         string
	VendorKind   domain.VendorKind
	Note         string
	// ReferredBy is set when the submitter happened to be signed in. Nil is the
	// normal case and must stay supported.
	ReferredBy *id.ID
}

// Submit records a lead.
//
// NO DUPLICATE CHECK, on purpose. The instinct is to look up the phone number and
// reject a second submission, and that instinct is wrong here: the second person
// naming the same cart is telling us something the first could not, which is that
// more than one customer wants it. The count is surfaced to whoever works the
// queue instead. Abuse is the rate limiter's problem, one layer up.
func (s *SourcingService) Submit(ctx context.Context, in SubmitInput) (*domain.Lead, error) {
	lead, err := domain.NewLead(
		in.Kind, in.BusinessName, in.ContactName, in.ContactPhone, in.Area,
		in.VendorKind, in.Note, in.ReferredBy, s.clock.Now(),
	)
	if err != nil {
		return nil, err
	}
	if err := s.leads.Create(ctx, lead); err != nil {
		return nil, err
	}
	return lead, nil
}

// LeadWithSignal is a lead plus how many people have named the same number.
type LeadWithSignal struct {
	Lead *domain.Lead
	// Mentions counts active leads sharing this phone number, this one included.
	// One is the ordinary case; anything above it is the queue's own priority
	// order, arrived at by customers rather than by us guessing.
	Mentions int
}

// List returns the admin queue, oldest first, each row carrying its mention count.
//
// The counts are fetched per lead rather than as a join. That is N+1 and it is a
// deliberate trade at this size: the page is capped at 100 rows, the query is a
// single indexed count, and the alternative is a windowed aggregate that is much
// harder to read for a table that will hold thousands of rows, not millions. If
// this ever pages through more than a few hundred, replace it with a join and say
// so here.
func (s *SourcingService) List(ctx context.Context, status domain.Status, limit, offset int) ([]LeadWithSignal, error) {
	if limit <= 0 || limit > maxPageSize {
		limit = maxPageSize
	}
	if offset < 0 {
		offset = 0
	}

	leads, err := s.leads.List(ctx, domain.LeadFilter{Status: status, Limit: limit, Offset: offset})
	if err != nil {
		return nil, err
	}

	// Counted once per distinct number, not once per row: a queue holding six
	// mentions of the same cart should cost one count, not six.
	counts := make(map[string]int, len(leads))
	out := make([]LeadWithSignal, 0, len(leads))
	for _, l := range leads {
		key := string(l.ContactPhone)
		if _, seen := counts[key]; !seen {
			n, err := s.leads.CountByPhone(ctx, l.ContactPhone)
			if err != nil {
				return nil, err
			}
			counts[key] = n
		}
		out = append(out, LeadWithSignal{Lead: l, Mentions: counts[key]})
	}
	return out, nil
}

// MoveTo advances a lead's status.
func (s *SourcingService) MoveTo(ctx context.Context, leadID id.ID, next domain.Status) (*domain.Lead, error) {
	lead, err := s.leads.FindByID(ctx, leadID)
	if err != nil {
		return nil, err
	}
	if err := lead.MoveTo(next, s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.leads.Update(ctx, lead); err != nil {
		return nil, err
	}
	return lead, nil
}

// Reopen puts a closed lead back in the queue.
func (s *SourcingService) Reopen(ctx context.Context, leadID id.ID) (*domain.Lead, error) {
	lead, err := s.leads.FindByID(ctx, leadID)
	if err != nil {
		return nil, err
	}
	if err := lead.Reopen(s.clock.Now()); err != nil {
		return nil, err
	}
	if err := s.leads.Update(ctx, lead); err != nil {
		return nil, err
	}
	return lead, nil
}

// Discard marks a lead as junk, which is not the same as rejecting it.
func (s *SourcingService) Discard(ctx context.Context, leadID id.ID) error {
	lead, err := s.leads.FindByID(ctx, leadID)
	if err != nil {
		return err
	}
	lead.Discard(s.clock.Now())
	return s.leads.Update(ctx, lead)
}

// ErrUnknownStatus is returned for a status string that is not a status.
func ParseStatus(raw string) (domain.Status, error) {
	if raw == "" {
		return "", nil // no filter
	}
	switch domain.Status(raw) {
	case domain.StatusNew, domain.StatusContacted, domain.StatusVisiting,
		domain.StatusOnboarding, domain.StatusConverted, domain.StatusRejected:
		return domain.Status(raw), nil
	}
	return "", apperr.Invalid("INVALID_LEAD_STATUS", "That is not a status a lead can be in.").
		WithField("status", "invalid")
}
