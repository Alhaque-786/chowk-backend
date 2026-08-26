// Package app holds the payment module's use cases.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
)

// PaymentService creates intents, processes webhooks and issues refunds.
type PaymentService struct {
	db       *database.DB
	payments domain.PaymentRepository
	webhooks domain.WebhookRepository
	gateway  domain.Gateway
	users    contracts.UserDirectory
	events   eventbus.Publisher
	clock    clock.Clock
}

func NewPaymentService(
	db *database.DB,
	payments domain.PaymentRepository,
	webhooks domain.WebhookRepository,
	gateway domain.Gateway,
	users contracts.UserDirectory,
	events eventbus.Publisher,
	clk clock.Clock,
) *PaymentService {
	return &PaymentService{
		db: db, payments: payments, webhooks: webhooks,
		gateway: gateway, users: users, events: events, clock: clk,
	}
}

// CreateIntent implements contracts.PaymentInitiator.
//
// Called by ordering during checkout, OUTSIDE its transaction. Two-phase on
// purpose: persist our row first, then call the gateway. If the gateway call
// fails we still have a record of the attempt, which is what makes
// reconciliation possible. Calling the gateway first and persisting after means a
// crash in between leaves money moving with nothing on our side pointing at it.
func (s *PaymentService) CreateIntent(ctx context.Context, req contracts.PaymentIntentRequest) (contracts.PaymentIntent, error) {
	now := s.clock.Now()

	// Idempotency first. A retried checkout must return the SAME intent, not open
	// a second payment for one order.
	if req.IdempotencyKey != "" {
		existing, err := s.payments.FindByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
			return contracts.PaymentIntent{}, err
		}
		if existing != nil {
			return contracts.PaymentIntent{
				PaymentID:      existing.ID,
				ProviderRef:    existing.ProviderRef,
				Status:         string(existing.Status),
				RequiresAction: !existing.Status.IsTerminal(),
			}, nil
		}
	}

	if !req.Amount.IsPositive() {
		return contracts.PaymentIntent{}, apperr.Invalid("INVALID_AMOUNT",
			"A payment amount must be positive.")
	}

	payment := &domain.Payment{
		ID:             id.New(),
		OrderID:        req.OrderID,
		UserID:         req.UserID,
		Amount:         req.Amount,
		Status:         domain.StatusCreated,
		Method:         req.Method,
		Provider:       s.gateway.Name(),
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.payments.Create(ctx, payment); err != nil {
		return contracts.PaymentIntent{}, err
	}

	// Customer contact is passed to the gateway so it can send its own
	// confirmations and so disputes are traceable.
	user, err := s.users.GetUser(ctx, req.UserID)
	if err != nil {
		return contracts.PaymentIntent{}, err
	}

	intent, err := s.gateway.CreateIntent(ctx, domain.IntentRequest{
		OrderID:        req.OrderID,
		OrderNumber:    req.OrderID.String(),
		UserID:         req.UserID,
		Amount:         req.Amount,
		Method:         req.Method,
		CustomerPhone:  user.Phone,
		CustomerEmail:  user.Email,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		// Record the failure rather than leaving the row in `created` forever, so
		// the reconciliation report can tell "never attempted" from "attempted and
		// rejected".
		payment.Status = domain.StatusFailed
		payment.FailureReason = err.Error()
		payment.FailedAt = &now
		payment.UpdatedAt = now
		if updateErr := s.payments.Update(ctx, payment); updateErr != nil {
			logger.From(ctx).Error("recording payment failure failed",
				"payment_id", payment.ID, "error", updateErr)
		}
		return contracts.PaymentIntent{}, err
	}

	payment.ProviderRef = intent.ProviderRef
	payment.ProviderOrderID = intent.ProviderOrderID
	payment.ProviderPayload = intent.RawPayload
	payment.Status = intent.Status
	payment.UpdatedAt = s.clock.Now()
	if err := s.payments.Update(ctx, payment); err != nil {
		return contracts.PaymentIntent{}, err
	}

	return contracts.PaymentIntent{
		PaymentID:      payment.ID,
		ProviderRef:    intent.ProviderRef,
		ClientSecret:   intent.ClientSecret,
		Status:         string(intent.Status),
		RequiresAction: intent.RequiresAction,
	}, nil
}

// ProcessWebhook is the only path by which a payment becomes captured.
//
// The order of operations is the whole design, and each step exists for a reason:
//
//  1. VERIFY THE SIGNATURE FIRST. Nothing in the body is trusted before this. The
//     webhook URL is public, so the signature is the only thing preventing anyone
//     from marking any order paid.
//  2. PERSIST THE RAW EVENT. The unique (provider, event_id) index makes
//     redelivery a no-op instead of a double capture, and keeps the raw body for
//     replay if our processing has a bug.
//  3. Only then apply it, under a row lock, through the state machine.
func (s *PaymentService) ProcessWebhook(ctx context.Context, rawBody []byte, signature string) error {
	// --- 1. Authenticate ---
	payload, err := s.gateway.VerifyWebhook(rawBody, signature)
	if err != nil {
		// Deliberately NOT stored. An unverified event is an attack or a
		// misconfiguration, and persisting attacker-controlled bodies would let
		// anyone fill the table.
		return err
	}

	// --- 2. Record, and detect duplicates ---
	event := &domain.WebhookEvent{
		ID:              id.New(),
		Provider:        s.gateway.Name(),
		ProviderEventID: payload.EventID,
		EventType:       payload.EventType,
		Payload:         payload.RawPayload,
		SignatureValid:  true,
		ReceivedAt:      s.clock.Now(),
	}

	isNew, err := s.webhooks.Store(ctx, event)
	if err != nil {
		return err
	}
	if !isNew {
		// Already handled. Return SUCCESS, not an error: an error makes the
		// gateway retry forever, and we have genuinely already done the work.
		logger.From(ctx).Info("duplicate webhook ignored",
			"event_id", payload.EventID, "type", payload.EventType)
		return nil
	}

	// --- 3. Apply ---
	processErr := s.applyWebhook(ctx, payload)

	// Mark processed either way, recording the error. A stuck event with a
	// recorded reason is debuggable; one that silently vanished is not.
	msg := ""
	if processErr != nil {
		msg = processErr.Error()
	}
	if err := s.webhooks.MarkProcessed(ctx, event.ID, msg); err != nil {
		logger.From(ctx).Error("marking webhook processed failed", "event_id", event.ID, "error", err)
	}
	return processErr
}

func (s *PaymentService) applyWebhook(ctx context.Context, payload domain.WebhookPayload) error {
	// An informational event with no status implication: recorded above, nothing
	// to do. Not an error — providers add event types over time.
	if payload.Status == "" {
		return nil
	}

	payment, err := s.payments.FindByProviderRef(ctx, s.gateway.Name(), payload.ProviderRef)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// A webhook for a payment we have no record of. Logged loudly rather
			// than erroring: retrying will not conjure the row, and it usually
			// means a test payment from the provider dashboard.
			logger.From(ctx).Warn("webhook for unknown payment",
				"provider_ref", payload.ProviderRef, "event", payload.EventType)
			return nil
		}
		return err
	}

	now := s.clock.Now()

	return s.db.InTx(ctx, func(ctx context.Context) error {
		// Row lock: two webhooks for the same payment (authorized then captured,
		// arriving together) must serialise, or one overwrites the other.
		locked, err := s.payments.FindByIDForUpdate(ctx, payment.ID)
		if err != nil {
			return err
		}

		// AMOUNT CHECK. If the gateway reports a different amount than we asked
		// for, something is badly wrong — a tampered request, a currency mix-up,
		// or the wrong payment matched. Refuse rather than capture the difference.
		if payload.Status == domain.StatusCaptured && !payload.Amount.IsZero() {
			if payload.Amount.Minor() != locked.Amount.Minor() {
				return apperr.Conflict("PAYMENT_AMOUNT_MISMATCH",
					fmt.Sprintf("Gateway reported %s but the order is %s.",
						payload.Amount, locked.Amount))
			}
		}

		if err := locked.TransitionTo(payload.Status, now); err != nil {
			// Same-status redelivery is the expected case under at-least-once
			// delivery, so it is a success, not a failure.
			if errors.Is(err, domain.ErrAlreadyInStatus) {
				return nil
			}
			return err
		}

		locked.ProviderPayload = payload.RawPayload
		locked.FailureCode = payload.FailureCode
		locked.FailureReason = payload.FailureReason
		if err := s.payments.Update(ctx, locked); err != nil {
			return err
		}

		return s.publishPaymentEvent(ctx, locked, payload)
	})
}

// publishPaymentEvent tells ordering what happened.
//
// Ordering subscribes and promotes or cancels the order. Payment does NOT call
// ordering — that inversion is what keeps the dependency one-directional and lets
// either module move to its own service.
func (s *PaymentService) publishPaymentEvent(ctx context.Context, p *domain.Payment, payload domain.WebhookPayload) error {
	var name eventbus.Name
	switch p.Status {
	case domain.StatusCaptured:
		name = contracts.EventPaymentCaptured
	case domain.StatusAuthorized:
		name = contracts.EventPaymentAuthorized
	case domain.StatusFailed:
		name = contracts.EventPaymentFailed
	case domain.StatusRefunded, domain.StatusPartiallyRefunded:
		name = contracts.EventRefundCompleted
	default:
		return nil
	}

	evt, err := eventbus.New(name, p.OrderID, contracts.PaymentPayload{
		PaymentID:     p.ID,
		OrderID:       p.OrderID,
		UserID:        p.UserID,
		AmountMinor:   p.Amount.Minor(),
		Currency:      string(p.Amount.Currency()),
		Provider:      p.Provider,
		ProviderRef:   p.ProviderRef,
		Method:        p.Method,
		FailureReason: p.FailureReason,
		OccurredAt:    s.clock.Now(),
	})
	if err != nil {
		return err
	}
	evt.TraceID = httpx.TraceIDFrom(ctx)
	return s.events.Publish(ctx, evt)
}

// Refund implements contracts.PaymentInitiator.
//
// Called by ordering on cancellation or rejection.
/*
Capture takes funds that were only authorised.

This is the second half of the assisted-vendor flow. A cart selling fresh nariyal
pani has no inventory system, so a meaningful share of orders cannot be fulfilled
and nobody finds out until somebody phones him. Authorising first places a hold;
this call converts it once he commits to making the food.

A NO-OP FOR EVERYTHING ELSE, and that is what makes it safe to call on the accept
path for every order. Cash orders have no payment row worth capturing, immediate
captures are already captured, and a failed payment has nothing held. All three
return nil rather than an error, because the caller — a kitchen accepting an order
— must not be blocked by a payment concern that does not apply to it.

IDEMPOTENT, because staff double-tap accept and the gateway is the wrong place to
discover that. The status check does the work: a payment already captured returns
early, so a second call cannot take the money twice.
*/
func (s *PaymentService) Capture(ctx context.Context, orderID id.ID) error {
	payment, err := s.payments.FindByOrderID(ctx, orderID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// A cash order. Nothing was ever held.
			return nil
		}
		return err
	}

	// Only an authorised payment has anything to take. Anything else — created,
	// captured, failed, refunded — is either not ready or already resolved.
	if payment.Status != domain.StatusAuthorized {
		return nil
	}

	now := s.clock.Now()

	// The gateway call comes FIRST here, which is the opposite of CreateIntent and
	// deliberately so. On intent creation our row must exist before the money moves
	// so a crash leaves something for reconciliation to find. Here the money is
	// already held and the row already exists: what we must not do is record a
	// capture that did not happen, because that promotes an order the customer was
	// never charged for.
	if err := s.gateway.Capture(ctx, payment.ProviderRef, payment.Amount); err != nil {
		return err
	}

	return s.db.InTx(ctx, func(ctx context.Context) error {
		locked, err := s.payments.FindByIDForUpdate(ctx, payment.ID)
		if err != nil {
			return err
		}
		// Re-checked under the lock. Two staff accepting at once both passed the
		// check above; only one may transition.
		if locked.Status != domain.StatusAuthorized {
			return nil
		}
		if err := locked.TransitionTo(domain.StatusCaptured, now); err != nil {
			if errors.Is(err, domain.ErrAlreadyInStatus) {
				return nil
			}
			return err
		}
		if err := s.payments.Update(ctx, locked); err != nil {
			return err
		}
		return s.publishPaymentEvent(ctx, locked, domain.WebhookPayload{
			// Synthesised rather than webhook-derived: this capture was initiated by
			// us, so there is no provider event behind it. Ordering only needs to
			// know the payment reached captured.
			Status:      domain.StatusCaptured,
			ProviderRef: locked.ProviderRef,
			Amount:      locked.Amount,
		})
	})
}

func (s *PaymentService) Refund(ctx context.Context, orderID id.ID, amount money.Amount, reason string) error {
	payment, err := s.payments.FindByOrderID(ctx, orderID)
	if err != nil {
		return err
	}
	// Nothing was collected, so there is nothing to return. Not an error: COD and
	// abandoned orders both land here legitimately.
	if !payment.Status.IsSettled() {
		return nil
	}

	now := s.clock.Now()
	refund := &domain.Refund{
		ID:        id.New(),
		PaymentID: payment.ID,
		OrderID:   orderID,
		Amount:    amount,
		Reason:    reason,
		Status:    domain.RefundInitiated,
		CreatedAt: now,
		UpdatedAt: now,
	}

	err = s.db.InTx(ctx, func(ctx context.Context) error {
		// Over-refund guard, inside a transaction so two concurrent partial
		// refunds cannot together exceed what was captured.
		alreadyRefunded, err := s.payments.TotalRefunded(ctx, payment.ID)
		if err != nil {
			return err
		}
		proposed, err := alreadyRefunded.Add(amount)
		if err != nil {
			return apperr.Internal(err)
		}
		if proposed.GreaterThan(payment.Amount) {
			return apperr.Conflict("REFUND_EXCEEDS_PAYMENT",
				fmt.Sprintf("Refunding %s would exceed the %s captured.", amount, payment.Amount))
		}
		return s.payments.CreateRefund(ctx, refund)
	})
	if err != nil {
		return err
	}

	// The gateway call is outside the transaction, same rule as checkout.
	resp, err := s.gateway.Refund(ctx, domain.RefundRequest{
		ProviderRef: payment.ProviderRef,
		Amount:      amount,
		Reason:      reason,
		// The refund's own id as the key: retrying this refund can never pay twice.
		IdempotencyKey: refund.ID.String(),
	})
	if err != nil {
		refund.Status = domain.RefundFailed
		refund.UpdatedAt = s.clock.Now()
		if updateErr := s.payments.UpdateRefund(ctx, refund); updateErr != nil {
			logger.From(ctx).Error("recording refund failure failed", "refund_id", refund.ID, "error", updateErr)
		}
		// Surfaced so it can be retried. A silently failed refund is money owed to
		// a customer that nobody knows about.
		return err
	}

	refund.ProviderRef = resp.ProviderRef
	refund.Status = resp.Status
	refund.UpdatedAt = s.clock.Now()
	if resp.Status == domain.RefundCompleted {
		completed := s.clock.Now()
		refund.CompletedAt = &completed
	}
	if err := s.payments.UpdateRefund(ctx, refund); err != nil {
		return err
	}

	// Reflect the refund on the payment itself, so a reconciliation query does
	// not have to aggregate refunds to know a payment's state.
	return s.db.InTx(ctx, func(ctx context.Context) error {
		locked, err := s.payments.FindByIDForUpdate(ctx, payment.ID)
		if err != nil {
			return err
		}
		total, err := s.payments.TotalRefunded(ctx, payment.ID)
		if err != nil {
			return err
		}

		next := domain.StatusPartiallyRefunded
		if total.Minor() >= locked.Amount.Minor() {
			next = domain.StatusRefunded
		}
		if err := locked.TransitionTo(next, s.clock.Now()); err != nil {
			if errors.Is(err, domain.ErrAlreadyInStatus) {
				return nil
			}
			return err
		}
		return s.payments.Update(ctx, locked)
	})
}

// GetForOrder returns a payment for the customer's order view.
func (s *PaymentService) GetForOrder(ctx context.Context, orderID id.ID) (*domain.Payment, error) {
	return s.payments.FindByOrderID(ctx, orderID)
}

// ReconcilePending finds payments stuck in a non-terminal state.
//
// Money that was authorized but never captured is money at risk, and a payment
// stuck in `pending` for hours means a webhook was missed. This is the job that
// catches what the event stream lost.
func (s *PaymentService) ReconcilePending(ctx context.Context, olderThan time.Duration) (int, error) {
	// Intentionally read-only for now: it reports, it does not act. Auto-capturing
	// or auto-failing based on our own guess is exactly the kind of automation
	// that loses money quietly. Querying the gateway for truth comes next.
	cutoff := s.clock.Now().Add(-olderThan)

	const q = `
		SELECT count(*) FROM payments
		WHERE status IN ('created','pending','authorized') AND created_at < $1`

	var n int
	if err := s.db.Q(ctx).QueryRow(ctx, q, cutoff).Scan(&n); err != nil {
		return 0, apperr.Internal(fmt.Errorf("payment: reconcile query: %w", err))
	}
	if n > 0 {
		logger.From(ctx).Warn("payments stuck in a non-terminal state", "count", n, "older_than", olderThan)
	}
	return n, nil
}

var _ contracts.PaymentInitiator = (*PaymentService)(nil)
