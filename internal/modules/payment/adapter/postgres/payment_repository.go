// Package postgres implements the payment module's repository ports.
package postgres

import (
	"context"
	"fmt"

	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

type PaymentRepository struct {
	db *database.DB
}

func NewPaymentRepository(db *database.DB) *PaymentRepository {
	return &PaymentRepository{db: db}
}

const paymentColumns = `
	id, order_id, user_id, amount_minor, currency, status, method,
	provider, provider_ref, provider_order_id, provider_payload,
	failure_code, failure_reason, idempotency_key,
	authorized_at, captured_at, failed_at, created_at, updated_at`

func (r *PaymentRepository) Create(ctx context.Context, p *domain.Payment) error {
	const q = `
		INSERT INTO payments (
			id, order_id, user_id, amount_minor, currency, status, method,
			provider, provider_ref, provider_order_id, provider_payload,
			idempotency_key, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`

	_, err := r.db.Q(ctx).Exec(ctx, q,
		p.ID, p.OrderID, p.UserID, p.Amount.Minor(), string(p.Amount.Currency()),
		string(p.Status), p.Method, p.Provider,
		nullString(p.ProviderRef), nullString(p.ProviderOrderID), nullBytes(p.ProviderPayload),
		nullString(p.IdempotencyKey), p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		// A duplicate idempotency key means a concurrent retry beat us to it. The
		// caller re-reads and returns the existing intent rather than creating a
		// second payment.
		if database.IsUniqueViolation(err, "payments_idempotency_key") {
			return apperr.Conflict("PAYMENT_ALREADY_EXISTS",
				"A payment for this request already exists.")
		}
		return apperr.Internal(fmt.Errorf("payment: create: %w", err))
	}
	return nil
}

func (r *PaymentRepository) Update(ctx context.Context, p *domain.Payment) error {
	const q = `
		UPDATE payments SET
			status = $2, provider_ref = $3, provider_order_id = $4, provider_payload = $5,
			failure_code = $6, failure_reason = $7,
			authorized_at = $8, captured_at = $9, failed_at = $10, updated_at = $11
		WHERE id = $1`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		p.ID, string(p.Status), nullString(p.ProviderRef), nullString(p.ProviderOrderID),
		nullBytes(p.ProviderPayload), p.FailureCode, p.FailureReason,
		p.AuthorizedAt, p.CapturedAt, p.FailedAt, p.UpdatedAt,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("payment: update %s: %w", p.ID, err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("PAYMENT_NOT_FOUND", "That payment could not be found.")
	}
	return nil
}

func (r *PaymentRepository) FindByID(ctx context.Context, paymentID id.ID) (*domain.Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE id = $1`
	return r.findOne(ctx, q, paymentID)
}

// FindByIDForUpdate row-locks the payment for the rest of the transaction, so two
// concurrent webhooks for the same payment cannot both apply.
func (r *PaymentRepository) FindByIDForUpdate(ctx context.Context, paymentID id.ID) (*domain.Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE id = $1 FOR UPDATE`
	return r.findOne(ctx, q, paymentID)
}

func (r *PaymentRepository) FindByProviderRef(ctx context.Context, provider, ref string) (*domain.Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE provider = $1 AND provider_ref = $2`
	return r.findOne(ctx, q, provider, ref)
}

// FindByOrderID returns the most recent payment for an order.
//
// Most recent, because a customer may retry after a decline and each attempt is
// its own row. The latest is the one that matters.
func (r *PaymentRepository) FindByOrderID(ctx context.Context, orderID id.ID) (*domain.Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE order_id = $1
		ORDER BY created_at DESC LIMIT 1`
	return r.findOne(ctx, q, orderID)
}

func (r *PaymentRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Payment, error) {
	q := `SELECT ` + paymentColumns + ` FROM payments WHERE idempotency_key = $1`
	return r.findOne(ctx, q, key)
}

func (r *PaymentRepository) findOne(ctx context.Context, q string, args ...any) (*domain.Payment, error) {
	p, err := scanPayment(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("PAYMENT_NOT_FOUND", "That payment could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("payment: find: %w", err))
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// Refunds
// ---------------------------------------------------------------------------

func (r *PaymentRepository) CreateRefund(ctx context.Context, rf *domain.Refund) error {
	const q = `
		INSERT INTO refunds (id, payment_id, order_id, amount_minor, reason, status,
		                     provider_ref, initiated_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		rf.ID, rf.PaymentID, rf.OrderID, rf.Amount.Minor(), rf.Reason, string(rf.Status),
		nullString(rf.ProviderRef), rf.InitiatedBy, rf.CreatedAt, rf.UpdatedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("payment: create refund: %w", err))
	}
	return nil
}

func (r *PaymentRepository) UpdateRefund(ctx context.Context, rf *domain.Refund) error {
	const q = `
		UPDATE refunds SET status = $2, provider_ref = $3, completed_at = $4, updated_at = $5
		WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q,
		rf.ID, string(rf.Status), nullString(rf.ProviderRef), rf.CompletedAt, rf.UpdatedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("payment: update refund: %w", err))
	}
	return nil
}

func (r *PaymentRepository) ListRefunds(ctx context.Context, paymentID id.ID) ([]*domain.Refund, error) {
	const q = `
		SELECT id, payment_id, order_id, amount_minor, reason, status, provider_ref,
		       initiated_by, completed_at, created_at, updated_at
		FROM refunds WHERE payment_id = $1 ORDER BY created_at`

	rows, err := r.db.Q(ctx).Query(ctx, q, paymentID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("payment: list refunds: %w", err))
	}
	defer rows.Close()

	var out []*domain.Refund
	for rows.Next() {
		var (
			rf     domain.Refund
			amount int64
			status string
			ref    *string
		)
		if err := rows.Scan(&rf.ID, &rf.PaymentID, &rf.OrderID, &amount, &rf.Reason,
			&status, &ref, &rf.InitiatedBy, &rf.CompletedAt, &rf.CreatedAt, &rf.UpdatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("payment: scan refund: %w", err))
		}
		rf.Amount = money.FromMinor(amount, money.INR)
		rf.Status = domain.RefundStatus(status)
		rf.ProviderRef = derefString(ref)
		out = append(out, &rf)
	}
	return out, rows.Err()
}

// TotalRefunded sums everything not failed.
//
// Excludes only 'failed': an initiated-but-not-yet-completed refund still counts
// against the balance, or two concurrent partial refunds could together exceed
// the captured amount.
func (r *PaymentRepository) TotalRefunded(ctx context.Context, paymentID id.ID) (money.Amount, error) {
	const q = `
		SELECT COALESCE(sum(amount_minor), 0) FROM refunds
		WHERE payment_id = $1 AND status <> 'failed'`

	var total int64
	if err := r.db.Q(ctx).QueryRow(ctx, q, paymentID).Scan(&total); err != nil {
		return money.Zero(money.INR), apperr.Internal(fmt.Errorf("payment: total refunded: %w", err))
	}
	return money.FromMinor(total, money.INR), nil
}

func scanPayment(row scanner) (*domain.Payment, error) {
	var (
		p                domain.Payment
		amount           int64
		currency, status string
		providerRef      *string
		providerOrderID  *string
		payload          []byte
		idempotencyKey   *string
	)
	if err := row.Scan(
		&p.ID, &p.OrderID, &p.UserID, &amount, &currency, &status, &p.Method,
		&p.Provider, &providerRef, &providerOrderID, &payload,
		&p.FailureCode, &p.FailureReason, &idempotencyKey,
		&p.AuthorizedAt, &p.CapturedAt, &p.FailedAt, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.Amount = money.FromMinor(amount, money.Currency(currency))
	p.Status = domain.Status(status)
	p.ProviderRef = derefString(providerRef)
	p.ProviderOrderID = derefString(providerOrderID)
	p.ProviderPayload = payload
	p.IdempotencyKey = derefString(idempotencyKey)
	return &p, nil
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

type WebhookRepository struct {
	db *database.DB
}

func NewWebhookRepository(db *database.DB) *WebhookRepository {
	return &WebhookRepository{db: db}
}

// Store persists an event, reporting whether it still needs processing.
//
// ON CONFLICT DO NOTHING plus a RETURNING check is the core idempotency
// mechanism. A read-then-write would have a race window where two concurrent
// deliveries of the same event both see "not present" — and that is a double
// capture. The unique index arbitrates.
//
// The subtlety: "already seen" is NOT the same as "already handled". If a
// previous delivery was recorded but its processing FAILED — the database was
// briefly down, a lock timed out — then skipping the retry loses that event
// forever. For payment.captured that means a customer who paid has an order
// stuck in pending_payment that nobody will ever cook.
//
// So a previously-failed event is reported as needing processing again, reusing
// the existing row id so the retry updates it in place rather than tripping the
// unique index. Only a CLEANLY processed event is skipped.
func (r *WebhookRepository) Store(ctx context.Context, e *domain.WebhookEvent) (bool, error) {
	const insert = `
		INSERT INTO webhook_events (id, provider, provider_event_id, event_type,
		                            payload, signature_valid, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider, provider_event_id) DO NOTHING
		RETURNING id`

	var inserted id.ID
	err := r.db.Q(ctx).QueryRow(ctx, insert,
		e.ID, e.Provider, e.ProviderEventID, e.EventType,
		e.Payload, e.SignatureValid, e.ReceivedAt,
	).Scan(&inserted)

	if err == nil {
		return true, nil
	}
	if !database.IsNotFound(err) {
		return false, apperr.Internal(fmt.Errorf("payment: store webhook: %w", err))
	}

	// The conflict fired. Decide whether the earlier attempt actually succeeded.
	const load = `
		SELECT id, processed_at IS NOT NULL AND process_error = '' AS handled
		FROM webhook_events
		WHERE provider = $1 AND provider_event_id = $2`

	var (
		existingID id.ID
		handled    bool
	)
	if err := r.db.Q(ctx).QueryRow(ctx, load, e.Provider, e.ProviderEventID).Scan(&existingID, &handled); err != nil {
		if database.IsNotFound(err) {
			// Vanished between the insert and this read (a cleanup job). Treat as
			// new; redoing the work is safer than dropping it.
			return true, nil
		}
		return false, apperr.Internal(fmt.Errorf("payment: load webhook: %w", err))
	}

	if handled {
		return false, nil
	}

	// A previous attempt failed. Adopt its row id so MarkProcessed updates the
	// same record, and let the caller process it again.
	e.ID = existingID
	return true, nil
}

func (r *WebhookRepository) MarkProcessed(ctx context.Context, eventID id.ID, processErr string) error {
	const q = `UPDATE webhook_events SET processed_at = now(), process_error = $2 WHERE id = $1`
	if _, err := r.db.Q(ctx).Exec(ctx, q, eventID, processErr); err != nil {
		return apperr.Internal(fmt.Errorf("payment: mark webhook processed: %w", err))
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

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

func nullBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

var (
	_ domain.PaymentRepository = (*PaymentRepository)(nil)
	_ domain.WebhookRepository = (*WebhookRepository)(nil)
)
