// Package postgres implements the notification module's repository ports.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/notification/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

type TemplateRepository struct{ db *database.DB }

func NewTemplateRepository(db *database.DB) *TemplateRepository {
	return &TemplateRepository{db: db}
}

const templateColumns = `
	id, key, channel, locale, subject, body, is_transactional, dlt_template_id, is_active`

func (r *TemplateRepository) Find(ctx context.Context, key string, channel domain.Channel, locale string) (*domain.Template, error) {
	if locale == "" {
		locale = "en"
	}
	q := `SELECT ` + templateColumns + ` FROM notification_templates
		WHERE key = $1 AND channel = $2 AND locale = $3 AND is_active`

	t, err := scanTemplate(r.db.Q(ctx).QueryRow(ctx, q, key, string(channel), locale))
	if err != nil {
		if database.IsNotFound(err) {
			// Not an error the caller should shout about: many keys deliberately
			// have a push template but no SMS one, because an SMS per status change
			// is spam.
			return nil, apperr.NotFound("TEMPLATE_NOT_FOUND", "No template for that key and channel.")
		}
		return nil, apperr.Internal(fmt.Errorf("notification: find template: %w", err))
	}
	return t, nil
}

func (r *TemplateRepository) FindForKey(ctx context.Context, key, locale string) ([]*domain.Template, error) {
	if locale == "" {
		locale = "en"
	}
	q := `SELECT ` + templateColumns + ` FROM notification_templates
		WHERE key = $1 AND locale = $2 AND is_active ORDER BY channel`

	rows, err := r.db.Q(ctx).Query(ctx, q, key, locale)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("notification: find templates: %w", err))
	}
	defer rows.Close()

	var out []*domain.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("notification: scan template: %w", err))
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TemplateRepository) Upsert(ctx context.Context, t *domain.Template) error {
	const q = `
		INSERT INTO notification_templates (id, key, channel, locale, subject, body,
		                                    is_transactional, dlt_template_id, is_active,
		                                    created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),now())
		ON CONFLICT (key, channel, locale) DO UPDATE SET
			subject = $5, body = $6, is_transactional = $7,
			dlt_template_id = $8, is_active = $9, updated_at = now()`

	tID := t.ID
	if id.IsNil(tID) {
		tID = id.New()
	}
	locale := t.Locale
	if locale == "" {
		locale = "en"
	}
	if _, err := r.db.Q(ctx).Exec(ctx, q, tID, t.Key, string(t.Channel), locale,
		t.Subject, t.Body, t.IsTransactional, t.DLTTemplateID, t.IsActive); err != nil {
		return apperr.Internal(fmt.Errorf("notification: upsert template: %w", err))
	}
	return nil
}

func scanTemplate(row scanner) (*domain.Template, error) {
	var (
		t       domain.Template
		channel string
	)
	if err := row.Scan(&t.ID, &t.Key, &channel, &t.Locale, &t.Subject, &t.Body,
		&t.IsTransactional, &t.DLTTemplateID, &t.IsActive); err != nil {
		return nil, err
	}
	t.Channel = domain.Channel(channel)
	return &t, nil
}

// ---------------------------------------------------------------------------
// Send log
// ---------------------------------------------------------------------------

type LogRepository struct{ db *database.DB }

func NewLogRepository(db *database.DB) *LogRepository { return &LogRepository{db: db} }

// Claim reserves a dedupe key, reporting false if it was already used.
//
// INSERT ... ON CONFLICT DO NOTHING plus a RETURNING check is the whole
// idempotency mechanism. A read-then-write has a window where two concurrent
// deliveries of the same event both see "not sent" — and the customer gets two
// SMS. The unique index arbitrates.
func (r *LogRepository) Claim(ctx context.Context, entry *domain.LogEntry) (bool, error) {
	const q = `
		INSERT INTO notification_log (id, user_id, channel, template_key, destination,
		                              rendered_body, dedupe_key, status, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (dedupe_key) DO NOTHING
		RETURNING id`

	var inserted id.ID
	err := r.db.Q(ctx).QueryRow(ctx, q,
		entry.ID, entry.UserID, string(entry.Channel), entry.TemplateKey,
		entry.Destination, entry.RenderedBody, entry.DedupeKey,
		string(entry.Status), entry.CreatedAt,
	).Scan(&inserted)

	if err == nil {
		return true, nil
	}
	if database.IsNotFound(err) {
		// The conflict fired: this notification was already sent.
		return false, nil
	}
	return false, apperr.Internal(fmt.Errorf("notification: claim dedupe key: %w", err))
}

func (r *LogRepository) MarkSent(ctx context.Context, entryID id.ID, provider, providerRef string, at time.Time) error {
	const q = `
		UPDATE notification_log
		SET status = 'sent', provider = $2, provider_ref = NULLIF($3,''),
		    sent_at = $4, attempts = attempts + 1
		WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, entryID, provider, providerRef, at); err != nil {
		return apperr.Internal(fmt.Errorf("notification: mark sent: %w", err))
	}
	return nil
}

func (r *LogRepository) MarkFailed(ctx context.Context, entryID id.ID, reason string) error {
	const q = `
		UPDATE notification_log
		SET status = 'failed', failure_reason = $2, attempts = attempts + 1
		WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, entryID, truncate(reason, 500)); err != nil {
		return apperr.Internal(fmt.Errorf("notification: mark failed: %w", err))
	}
	return nil
}

// MarkSuppressed records a deliberate non-send.
//
// Recorded rather than silently skipped, because "the customer says they were
// never told" needs an answer, and "we chose not to, here is why" is one.
func (r *LogRepository) MarkSuppressed(ctx context.Context, entryID id.ID, reason string) error {
	const q = `UPDATE notification_log SET status = 'suppressed', failure_reason = $2 WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, entryID, truncate(reason, 500)); err != nil {
		return apperr.Internal(fmt.Errorf("notification: mark suppressed: %w", err))
	}
	return nil
}

func (r *LogRepository) ListForUser(ctx context.Context, userID id.ID, limit int) ([]*domain.LogEntry, error) {
	const q = `
		SELECT id, user_id, channel, template_key, destination, rendered_body,
		       dedupe_key, status, provider, provider_ref, failure_reason, attempts,
		       sent_at, created_at
		FROM notification_log WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`

	rows, err := r.db.Q(ctx).Query(ctx, q, userID, limit)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("notification: list log: %w", err))
	}
	defer rows.Close()

	var out []*domain.LogEntry
	for rows.Next() {
		var (
			e           domain.LogEntry
			channel     string
			status      string
			providerRef *string
		)
		if err := rows.Scan(&e.ID, &e.UserID, &channel, &e.TemplateKey, &e.Destination,
			&e.RenderedBody, &e.DedupeKey, &status, &e.Provider, &providerRef,
			&e.FailureReason, &e.Attempts, &e.SentAt, &e.CreatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("notification: scan log: %w", err))
		}
		e.Channel = domain.Channel(channel)
		e.Status = domain.Status(status)
		if providerRef != nil {
			e.ProviderRef = *providerRef
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Device tokens
// ---------------------------------------------------------------------------

type DeviceRepository struct{ db *database.DB }

func NewDeviceRepository(db *database.DB) *DeviceRepository { return &DeviceRepository{db: db} }

// Register upserts a token.
//
// ON CONFLICT on the token, not on (user, token): a token identifies a device
// INSTALLATION globally, so reinstalling after switching accounts must move the
// token to the new user rather than leaving it pointing at the old one and
// pushing someone else's order updates to their phone.
func (r *DeviceRepository) Register(ctx context.Context, t *domain.DeviceToken) error {
	const q = `
		INSERT INTO device_tokens (id, user_id, token, platform, device_name,
		                           is_active, last_used_at, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,true,$6,now(),now())
		ON CONFLICT (token) DO UPDATE SET
			user_id = $2, platform = $4, device_name = $5,
			is_active = true, failure_count = 0, last_used_at = $6, updated_at = now()`

	if _, err := r.db.Q(ctx).Exec(ctx, q, t.ID, t.UserID, t.Token, t.Platform,
		t.DeviceName, t.LastUsedAt); err != nil {
		return apperr.Internal(fmt.Errorf("notification: register device: %w", err))
	}
	return nil
}

func (r *DeviceRepository) ListActiveForUser(ctx context.Context, userID id.ID) ([]*domain.DeviceToken, error) {
	const q = `
		SELECT id, user_id, token, platform, device_name, is_active, failure_count, last_used_at
		FROM device_tokens WHERE user_id = $1 AND is_active`

	rows, err := r.db.Q(ctx).Query(ctx, q, userID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("notification: list devices: %w", err))
	}
	defer rows.Close()

	var out []*domain.DeviceToken
	for rows.Next() {
		var t domain.DeviceToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Token, &t.Platform, &t.DeviceName,
			&t.IsActive, &t.FailureCount, &t.LastUsedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("notification: scan device: %w", err))
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RecordFailure increments the counter and retires the token once it is clearly
// dead. Providers penalise senders who keep pushing to uninstalled apps.
func (r *DeviceRepository) RecordFailure(ctx context.Context, token string) error {
	const q = `
		UPDATE device_tokens
		SET failure_count = failure_count + 1,
		    is_active = (failure_count + 1) < 5,
		    updated_at = now()
		WHERE token = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, token); err != nil {
		return apperr.Internal(fmt.Errorf("notification: record device failure: %w", err))
	}
	return nil
}

func (r *DeviceRepository) Deactivate(ctx context.Context, token string) error {
	const q = `UPDATE device_tokens SET is_active = false, updated_at = now() WHERE token = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, token); err != nil {
		return apperr.Internal(fmt.Errorf("notification: deactivate device: %w", err))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

type PreferenceRepository struct{ db *database.DB }

func NewPreferenceRepository(db *database.DB) *PreferenceRepository {
	return &PreferenceRepository{db: db}
}

// Get returns stored preferences, or defaults when no row exists.
//
// Absence means "all defaults on", so a new user needs no row and opting in is
// not a prerequisite for receiving order updates.
func (r *PreferenceRepository) Get(ctx context.Context, userID id.ID) (domain.Preferences, error) {
	const q = `
		SELECT promotional_push, promotional_sms, promotional_email,
		       quiet_hours_start, quiet_hours_end
		FROM notification_preferences WHERE user_id = $1`

	prefs := domain.DefaultPreferences(userID)
	err := r.db.Q(ctx).QueryRow(ctx, q, userID).Scan(
		&prefs.PromotionalPush, &prefs.PromotionalSMS, &prefs.PromotionalEmail,
		&prefs.QuietHoursStart, &prefs.QuietHoursEnd,
	)
	if err != nil {
		if database.IsNotFound(err) {
			return domain.DefaultPreferences(userID), nil
		}
		return prefs, apperr.Internal(fmt.Errorf("notification: get preferences: %w", err))
	}
	return prefs, nil
}

func (r *PreferenceRepository) Save(ctx context.Context, prefs domain.Preferences) error {
	const q = `
		INSERT INTO notification_preferences (user_id, promotional_push, promotional_sms,
		                                      promotional_email, quiet_hours_start,
		                                      quiet_hours_end, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (user_id) DO UPDATE SET
			promotional_push = $2, promotional_sms = $3, promotional_email = $4,
			quiet_hours_start = $5, quiet_hours_end = $6, updated_at = now()`

	if _, err := r.db.Q(ctx).Exec(ctx, q, prefs.UserID, prefs.PromotionalPush,
		prefs.PromotionalSMS, prefs.PromotionalEmail,
		prefs.QuietHoursStart, prefs.QuietHoursEnd); err != nil {
		return apperr.Internal(fmt.Errorf("notification: save preferences: %w", err))
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

var (
	_ domain.TemplateRepository   = (*TemplateRepository)(nil)
	_ domain.LogRepository        = (*LogRepository)(nil)
	_ domain.DeviceRepository     = (*DeviceRepository)(nil)
	_ domain.PreferenceRepository = (*PreferenceRepository)(nil)
)
