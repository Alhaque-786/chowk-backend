// Package postgres implements the sourcing module's repository ports.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/sourcing/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/phone"
)

type LeadRepository struct{ db *database.DB }

func NewLeadRepository(db *database.DB) *LeadRepository { return &LeadRepository{db: db} }

const leadColumns = `
	id, kind, business_name, contact_name, contact_phone, area,
	vendor_kind, note, status, referred_by, is_active, created_at, updated_at`

func (r *LeadRepository) Create(ctx context.Context, l *domain.Lead) error {
	const q = `
		INSERT INTO vendor_leads (
			id, kind, business_name, contact_name, contact_phone, area,
			vendor_kind, note, status, referred_by, is_active, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		l.ID, string(l.Kind), l.BusinessName, l.ContactName, string(l.ContactPhone),
		l.Area, string(l.VendorKind), l.Note, string(l.Status), l.ReferredBy,
		l.IsActive, l.CreatedAt, l.UpdatedAt,
	); err != nil {
		if database.IsCheckViolation(err) {
			// The DB enforces the same rules the domain does. Reaching here means a
			// caller built a Lead without going through NewLead.
			return apperr.Invalid("INVALID_LEAD", "Those details are not valid.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("sourcing: create lead: %w", err))
	}
	return nil
}

func (r *LeadRepository) FindByID(ctx context.Context, leadID id.ID) (*domain.Lead, error) {
	q := `SELECT ` + leadColumns + ` FROM vendor_leads WHERE id = $1 AND is_active`
	l, err := r.scanOne(ctx, q, leadID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.NotFound("LEAD_NOT_FOUND", "That lead could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("sourcing: find lead: %w", err))
	}
	return l, nil
}

func (r *LeadRepository) Update(ctx context.Context, l *domain.Lead) error {
	// status and is_active only. Nothing a submitter typed is editable here: the
	// record of what somebody actually told us is the point of keeping it, and an
	// admin quietly correcting a business name destroys the ability to tell a bad
	// lead from a mistyped one.
	const q = `
		UPDATE vendor_leads SET status = $2, is_active = $3, updated_at = $4
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q, l.ID, string(l.Status), l.IsActive, l.UpdatedAt)
	if err != nil {
		return apperr.Internal(fmt.Errorf("sourcing: update lead: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("LEAD_NOT_FOUND", "That lead could not be found.")
	}
	return nil
}

func (r *LeadRepository) List(ctx context.Context, f domain.LeadFilter) ([]*domain.Lead, error) {
	// Oldest first. A queue worked newest-first starves the leads that have been
	// waiting longest, which are exactly the ones going cold.
	q := `SELECT ` + leadColumns + ` FROM vendor_leads WHERE is_active`
	args := []any{}

	if f.Status != "" {
		args = append(args, string(f.Status))
		q += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, f.Limit, f.Offset)
	q += fmt.Sprintf(" ORDER BY created_at ASC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("sourcing: list leads: %w", err))
	}
	defer rows.Close()

	leads := []*domain.Lead{}
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("sourcing: scan lead: %w", err))
		}
		leads = append(leads, l)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(fmt.Errorf("sourcing: iterate leads: %w", err))
	}
	return leads, nil
}

// CountByPhone counts every active lead naming this number, including the one
// being looked at.
//
// This is the query the schema's missing unique index exists to make possible:
// repeat mentions of the same cart are the signal, not a duplicate to suppress.
func (r *LeadRepository) CountByPhone(ctx context.Context, p phone.Phone) (int, error) {
	const q = `SELECT count(*) FROM vendor_leads WHERE contact_phone = $1 AND is_active`
	var n int
	if err := r.db.Q(ctx).QueryRow(ctx, q, string(p)).Scan(&n); err != nil {
		return 0, apperr.Internal(fmt.Errorf("sourcing: count by phone: %w", err))
	}
	return n, nil
}

type scanner interface{ Scan(dest ...any) error }

func (r *LeadRepository) scanOne(ctx context.Context, q string, args ...any) (*domain.Lead, error) {
	return scanLead(r.db.Q(ctx).QueryRow(ctx, q, args...))
}

func scanLead(row scanner) (*domain.Lead, error) {
	var (
		l          domain.Lead
		kind       string
		rawPhone   string
		vendorKind string
		status     string
	)
	if err := row.Scan(
		&l.ID, &kind, &l.BusinessName, &l.ContactName, &rawPhone, &l.Area,
		&vendorKind, &l.Note, &status, &l.ReferredBy, &l.IsActive,
		&l.CreatedAt, &l.UpdatedAt,
	); err != nil {
		return nil, err
	}
	l.Kind = domain.Kind(kind)
	l.ContactPhone = phone.Phone(rawPhone)
	l.VendorKind = domain.VendorKind(vendorKind)
	l.Status = domain.Status(status)
	return &l, nil
}
