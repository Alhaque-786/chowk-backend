package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/cloud-kitchen-backend/internal/modules/identity/domain"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/database"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

type SessionRepository struct {
	db *database.DB
}

func NewSessionRepository(db *database.DB) *SessionRepository {
	return &SessionRepository{db: db}
}

const sessionColumns = `
	id, user_id, refresh_token_hash, user_agent, ip_address, device_name,
	expires_at, revoked_at, rotated_to, created_at, last_used_at`

func (r *SessionRepository) Create(ctx context.Context, s *domain.Session) error {
	const q = `
		INSERT INTO sessions (
			id, user_id, refresh_token_hash, user_agent, ip_address, device_name,
			expires_at, created_at, last_used_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`

	_, err := r.db.Q(ctx).Exec(ctx, q,
		s.ID, s.UserID, s.RefreshTokenHash, nullString(s.UserAgent),
		nullString(s.IPAddress), nullString(s.DeviceName),
		s.ExpiresAt, s.CreatedAt, s.LastUsedAt,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("identity: create session: %w", err))
	}
	return nil
}

func (r *SessionRepository) FindByTokenHash(ctx context.Context, hash string) (*domain.Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE refresh_token_hash = $1`

	s, err := scanSession(r.db.Q(ctx).QueryRow(ctx, q, hash))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("SESSION_NOT_FOUND", "Please sign in again.")
		}
		return nil, apperr.Internal(fmt.Errorf("identity: find session: %w", err))
	}
	return s, nil
}

// Revoke ends one session. It is idempotent -- revoking an already-revoked
// session is a success, because a double logout tap is not an error.
func (r *SessionRepository) Revoke(ctx context.Context, sessionID id.ID, now time.Time) error {
	const q = `UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`
	if _, err := r.db.Q(ctx).Exec(ctx, q, sessionID, now); err != nil {
		return apperr.Internal(fmt.Errorf("identity: revoke session: %w", err))
	}
	return nil
}

func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID id.ID, now time.Time) error {
	const q = `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`
	if _, err := r.db.Q(ctx).Exec(ctx, q, userID, now); err != nil {
		return apperr.Internal(fmt.Errorf("identity: revoke all sessions: %w", err))
	}
	return nil
}

// MarkRotated links a consumed session to its replacement, which is what makes
// refresh-token reuse detectable.
func (r *SessionRepository) MarkRotated(ctx context.Context, oldID, newID id.ID, now time.Time) error {
	const q = `
		UPDATE sessions
		SET rotated_to = $2, revoked_at = COALESCE(revoked_at, $3)
		WHERE id = $1`
	tag, err := r.db.Q(ctx).Exec(ctx, q, oldID, newID, now)
	if err != nil {
		return apperr.Internal(fmt.Errorf("identity: mark session rotated: %w", err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("SESSION_NOT_FOUND", "Please sign in again.")
	}
	return nil
}

func (r *SessionRepository) ListActiveForUser(ctx context.Context, userID id.ID, now time.Time) ([]*domain.Session, error) {
	q := `SELECT ` + sessionColumns + `
		FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY last_used_at DESC`

	rows, err := r.db.Q(ctx).Query(ctx, q, userID, now)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("identity: list sessions: %w", err))
	}
	defer rows.Close()

	var out []*domain.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("identity: scan session: %w", err))
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteExpired prunes long-dead sessions. Called by the cleanup worker rather
// than on the request path -- an unbounded sessions table degrades the token
// lookup that every refresh depends on.
func (r *SessionRepository) DeleteExpired(ctx context.Context, before time.Time) (int64, error) {
	const q = `DELETE FROM sessions WHERE expires_at < $1`
	tag, err := r.db.Q(ctx).Exec(ctx, q, before)
	if err != nil {
		return 0, apperr.Internal(fmt.Errorf("identity: delete expired sessions: %w", err))
	}
	return tag.RowsAffected(), nil
}

func scanSession(row scanner) (*domain.Session, error) {
	var (
		s          domain.Session
		userAgent  *string
		ipAddress  *string
		deviceName *string
	)
	if err := row.Scan(
		&s.ID, &s.UserID, &s.RefreshTokenHash, &userAgent, &ipAddress, &deviceName,
		&s.ExpiresAt, &s.RevokedAt, &s.RotatedTo, &s.CreatedAt, &s.LastUsedAt,
	); err != nil {
		return nil, err
	}
	s.UserAgent = derefString(userAgent)
	s.IPAddress = derefString(ipAddress)
	s.DeviceName = derefString(deviceName)
	return &s, nil
}

var _ domain.SessionRepository = (*SessionRepository)(nil)
