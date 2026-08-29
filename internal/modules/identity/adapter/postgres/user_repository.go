// Package postgres implements the identity module's repository ports.
//
// This is an adapter: it is the only place in the module that knows SQL exists.
// Its job is translation in both directions -- domain types to columns, and
// driver errors to apperr kinds. A service must never see a *pgconn.PgError.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/paging"
	"github.com/zevoxa/chowk-backend/internal/shared/phone"
)

type UserRepository struct {
	db *database.DB
}

func NewUserRepository(db *database.DB) *UserRepository {
	return &UserRepository{db: db}
}

// userColumns is shared between every read so a new column cannot be added to
// one query and forgotten in another -- a bug that shows up as a field being
// mysteriously empty on only some endpoints.
const userColumns = `
	id, phone, email, full_name, password_hash, status, roles, kitchen_id,
	phone_verified_at, email_verified_at, last_login_at,
	referral_code, referred_by, created_at, updated_at`

func (r *UserRepository) Create(ctx context.Context, u *domain.User) error {
	const q = `
		INSERT INTO users (
			id, phone, email, full_name, password_hash, status, roles, kitchen_id,
			phone_verified_at, email_verified_at, referral_code, referred_by,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

	_, err := r.db.Q(ctx).Exec(ctx, q,
		u.ID, string(u.Phone), nullString(u.Email), u.FullName, nullString(u.PasswordHash),
		string(u.Status), rolesToStrings(u.Roles), nullID(u.KitchenID),
		u.PhoneVerifiedAt, u.EmailVerifiedAt, u.ReferralCode, nullString(u.ReferredBy),
		u.CreatedAt, u.UpdatedAt,
	)
	if err != nil {
		// Return the raw error for unique violations so the service can inspect
		// which constraint fired and phrase the message accordingly.
		if database.IsUniqueViolation(err, "") {
			return err
		}
		return apperr.Internal(fmt.Errorf("identity: create user: %w", err))
	}
	return nil
}

func (r *UserRepository) Update(ctx context.Context, u *domain.User) error {
	const q = `
		UPDATE users SET
			email = $2, full_name = $3, password_hash = $4, status = $5,
			roles = $6, kitchen_id = $7, phone_verified_at = $8,
			email_verified_at = $9, last_login_at = $10, updated_at = $11
		WHERE id = $1 AND status <> 'deleted'`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		u.ID, nullString(u.Email), u.FullName, nullString(u.PasswordHash), string(u.Status),
		rolesToStrings(u.Roles), nullID(u.KitchenID), u.PhoneVerifiedAt,
		u.EmailVerifiedAt, u.LastLoginAt, u.UpdatedAt,
	)
	if err != nil {
		if database.IsUniqueViolation(err, "") {
			return err
		}
		return apperr.Internal(fmt.Errorf("identity: update user %s: %w", u.ID, err))
	}
	// Zero rows means the id does not exist or was soft-deleted. Silently
	// succeeding here would let a caller believe a write landed when it did not.
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("USER_NOT_FOUND", "That account could not be found.")
	}
	return nil
}

func (r *UserRepository) FindByID(ctx context.Context, userID id.ID) (*domain.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE id = $1 AND status <> 'deleted'`
	return r.scanOne(ctx, q, userID)
}

func (r *UserRepository) FindByPhone(ctx context.Context, p phone.Phone) (*domain.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE phone = $1 AND status <> 'deleted'`
	return r.scanOne(ctx, q, string(p))
}

func (r *UserRepository) FindByEmail(ctx context.Context, email string) (*domain.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE email = $1 AND status <> 'deleted'`
	return r.scanOne(ctx, q, strings.ToLower(email))
}

func (r *UserRepository) FindByReferralCode(ctx context.Context, code string) (*domain.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE referral_code = $1 AND status <> 'deleted'`
	return r.scanOne(ctx, q, strings.ToUpper(code))
}

func (r *UserRepository) ExistsByPhone(ctx context.Context, p phone.Phone) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM users WHERE phone = $1 AND status <> 'deleted')`
	var exists bool
	if err := r.db.Q(ctx).QueryRow(ctx, q, string(p)).Scan(&exists); err != nil {
		return false, apperr.Internal(fmt.Errorf("identity: exists by phone: %w", err))
	}
	return exists, nil
}

// List is the admin user browser. Filters are built as parameterised fragments;
// no value is ever interpolated into the SQL string.
func (r *UserRepository) List(ctx context.Context, filter domain.UserFilter, page paging.Request) ([]*domain.User, error) {
	conds := []string{"status <> 'deleted'"}
	args := []any{}

	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if filter.Role != "" {
		args = append(args, string(filter.Role))
		conds = append(conds, fmt.Sprintf("$%d = ANY(roles)", len(args)))
	}
	if filter.KitchenID != id.Nil {
		args = append(args, filter.KitchenID)
		conds = append(conds, fmt.Sprintf("kitchen_id = $%d", len(args)))
	}
	if s := strings.TrimSpace(filter.Search); s != "" {
		args = append(args, "%"+s+"%")
		conds = append(conds, fmt.Sprintf(
			"(full_name ILIKE $%d OR phone ILIKE $%d OR email ILIKE $%d)",
			len(args), len(args), len(args)))
	}

	keyset, orderBy, keysetArgs := page.SQL("created_at", "id", len(args))
	if keyset != "" {
		conds = append(conds, keyset)
		args = append(args, keysetArgs...)
	}

	args = append(args, page.FetchLimit())
	q := fmt.Sprintf(`SELECT %s FROM users WHERE %s %s LIMIT $%d`,
		userColumns, strings.Join(conds, " AND "), orderBy, len(args))

	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("identity: list users: %w", err))
	}
	defer rows.Close()

	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("identity: scan user: %w", err))
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(fmt.Errorf("identity: iterate users: %w", err))
	}
	return out, nil
}

func (r *UserRepository) scanOne(ctx context.Context, q string, args ...any) (*domain.User, error) {
	u, err := scanUser(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("USER_NOT_FOUND", "That account could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("identity: find user: %w", err))
	}
	return u, nil
}

// scanner covers both pgx.Row and pgx.Rows, so one scan function serves both
// single-row and multi-row reads.
type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (*domain.User, error) {
	var (
		u          domain.User
		rawPhone   string
		email      *string
		pwHash     *string
		status     string
		roles      []string
		kitchenID  *id.ID
		referredBy *string
	)

	if err := row.Scan(
		&u.ID, &rawPhone, &email, &u.FullName, &pwHash, &status, &roles, &kitchenID,
		&u.PhoneVerifiedAt, &u.EmailVerifiedAt, &u.LastLoginAt,
		&u.ReferralCode, &referredBy, &u.CreatedAt, &u.UpdatedAt,
	); err != nil {
		return nil, err
	}

	u.Phone = phone.Phone(rawPhone)
	u.Status = domain.UserStatus(status)
	u.Email = derefString(email)
	u.PasswordHash = derefString(pwHash)
	u.ReferredBy = derefString(referredBy)
	if kitchenID != nil {
		u.KitchenID = *kitchenID
	}
	for _, role := range roles {
		u.Roles = append(u.Roles, httpx.Role(role))
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// Null helpers
//
// Postgres distinguishes NULL from ''. Writing '' into a column with a UNIQUE
// index breaks the second row that has no value, because '' = '' but NULL is
// never equal to NULL. So optional strings must be stored as NULL.
// ---------------------------------------------------------------------------

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullID(v id.ID) *id.ID {
	if v == id.Nil {
		return nil
	}
	return &v
}

func rolesToStrings(roles []httpx.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

// UserCursor derives a pagination cursor from a user, passed to paging.NewPage.
func UserCursor(u *domain.User) paging.Cursor {
	return paging.Cursor{CreatedAt: u.CreatedAt, ID: u.ID}
}

var (
	_ domain.UserRepository = (*UserRepository)(nil)
	_ time.Time             = time.Time{}
)
