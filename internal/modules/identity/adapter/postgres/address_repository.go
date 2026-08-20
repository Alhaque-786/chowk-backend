package postgres

import (
	"context"
	"fmt"

	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

type AddressRepository struct {
	db *database.DB
}

func NewAddressRepository(db *database.DB) *AddressRepository {
	return &AddressRepository{db: db}
}

const addressColumns = `
	id, user_id, type, label, line1, line2, landmark, city, state, pincode,
	latitude, longitude, is_default, receiver_name, receiver_phone,
	created_at, updated_at`

func (r *AddressRepository) Create(ctx context.Context, a *domain.Address) error {
	const q = `
		INSERT INTO addresses (
			id, user_id, type, label, line1, line2, landmark, city, state, pincode,
			latitude, longitude, is_default, receiver_name, receiver_phone,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`

	_, err := r.db.Q(ctx).Exec(ctx, q,
		a.ID, a.UserID, string(a.Type), a.Label, a.Line1, a.Line2, a.Landmark,
		a.City, a.State, a.Pincode, a.Latitude, a.Longitude, a.IsDefault,
		a.ReceiverName, a.ReceiverPhone, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		// The partial unique index enforces one default per user. Hitting it means
		// the caller failed to clear the previous default first.
		if database.IsUniqueViolation(err, "addresses_one_default_per_user_idx") {
			return apperr.Conflict("DEFAULT_ADDRESS_EXISTS",
				"You already have a default address. Clear it before setting another.")
		}
		if database.IsCheckViolation(err) {
			return apperr.Invalid("INVALID_ADDRESS",
				"That address is not valid. Check the pincode and location.").WithCause(err)
		}
		return apperr.Internal(fmt.Errorf("identity: create address: %w", err))
	}
	return nil
}

func (r *AddressRepository) Update(ctx context.Context, a *domain.Address) error {
	const q = `
		UPDATE addresses SET
			type = $3, label = $4, line1 = $5, line2 = $6, landmark = $7,
			city = $8, state = $9, pincode = $10, latitude = $11, longitude = $12,
			is_default = $13, receiver_name = $14, receiver_phone = $15, updated_at = $16
		WHERE id = $1 AND user_id = $2`

	// user_id in the WHERE clause, not just id: it makes it impossible to update
	// someone else's address even if a caller passes a foreign id.
	tag, err := r.db.Q(ctx).Exec(ctx, q,
		a.ID, a.UserID, string(a.Type), a.Label, a.Line1, a.Line2, a.Landmark,
		a.City, a.State, a.Pincode, a.Latitude, a.Longitude, a.IsDefault,
		a.ReceiverName, a.ReceiverPhone, a.UpdatedAt,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("identity: update address: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("ADDRESS_NOT_FOUND", "That address could not be found.")
	}
	return nil
}

func (r *AddressRepository) Delete(ctx context.Context, userID, addressID id.ID) error {
	const q = `DELETE FROM addresses WHERE id = $1 AND user_id = $2`
	tag, err := r.db.Q(ctx).Exec(ctx, q, addressID, userID)
	if err != nil {
		return apperr.Internal(fmt.Errorf("identity: delete address: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("ADDRESS_NOT_FOUND", "That address could not be found.")
	}
	return nil
}

func (r *AddressRepository) FindByID(ctx context.Context, userID, addressID id.ID) (*domain.Address, error) {
	q := `SELECT ` + addressColumns + ` FROM addresses WHERE id = $1 AND user_id = $2`

	addr, err := scanAddress(r.db.Q(ctx).QueryRow(ctx, q, addressID, userID))
	if err != nil {
		if database.IsNotFound(err) {
			// "Not found" rather than "not yours": confirming an address exists but
			// belongs to someone else would let an attacker enumerate them.
			return nil, apperr.NotFound("ADDRESS_NOT_FOUND", "That address could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("identity: find address: %w", err))
	}
	return addr, nil
}

func (r *AddressRepository) ListForUser(ctx context.Context, userID id.ID) ([]*domain.Address, error) {
	q := `SELECT ` + addressColumns + ` FROM addresses WHERE user_id = $1
		ORDER BY is_default DESC, created_at DESC`

	rows, err := r.db.Q(ctx).Query(ctx, q, userID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("identity: list addresses: %w", err))
	}
	defer rows.Close()

	var out []*domain.Address
	for rows.Next() {
		addr, err := scanAddress(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("identity: scan address: %w", err))
		}
		out = append(out, addr)
	}
	return out, rows.Err()
}

// ClearDefault unsets the current default so a new one can be set inside the
// same transaction, satisfying the one-default-per-user index.
func (r *AddressRepository) ClearDefault(ctx context.Context, userID id.ID) error {
	const q = `UPDATE addresses SET is_default = false, updated_at = now()
		WHERE user_id = $1 AND is_default`
	if _, err := r.db.Q(ctx).Exec(ctx, q, userID); err != nil {
		return apperr.Internal(fmt.Errorf("identity: clear default address: %w", err))
	}
	return nil
}

func scanAddress(row scanner) (*domain.Address, error) {
	var (
		a        domain.Address
		addrType string
	)
	if err := row.Scan(
		&a.ID, &a.UserID, &addrType, &a.Label, &a.Line1, &a.Line2, &a.Landmark,
		&a.City, &a.State, &a.Pincode, &a.Latitude, &a.Longitude, &a.IsDefault,
		&a.ReceiverName, &a.ReceiverPhone, &a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.Type = domain.AddressType(addrType)
	return &a, nil
}

var _ domain.AddressRepository = (*AddressRepository)(nil)
