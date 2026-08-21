// Package notification is the module's assembly file and only public surface.
//
// This is the module the event bus was built for. It subscribes to nearly
// everything and nothing subscribes to it: ordering publishes `order.placed` and
// has no idea an SMS follows. Deleting this module would not change a line of
// ordering, payment or inventory code — which is the property that makes it
// independently deployable.
package notification

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/contracts"
	notificationhttp "github.com/zevoxa/chowk-backend/internal/modules/notification/adapter/http"
	notificationpg "github.com/zevoxa/chowk-backend/internal/modules/notification/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/notification/adapter/sender"
	"github.com/zevoxa/chowk-backend/internal/modules/notification/app"
	"github.com/zevoxa/chowk-backend/internal/modules/notification/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/platform/eventbus"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
)

type Deps struct {
	Config *config.Config
	DB     *database.DB
	Log    *slog.Logger
	Clock  clock.Clock
	Users  contracts.UserDirectory
}

type Module struct {
	deps Deps

	templates domain.TemplateRepository
	logs      domain.LogRepository
	devices   domain.DeviceRepository
	prefs     domain.PreferenceRepository

	service *app.NotificationService
	handler *notificationhttp.Handler
}

func New(deps Deps) *Module {
	templates := notificationpg.NewTemplateRepository(deps.DB)
	logs := notificationpg.NewLogRepository(deps.DB)
	devices := notificationpg.NewDeviceRepository(deps.DB)
	prefs := notificationpg.NewPreferenceRepository(deps.DB)

	// LogSender for every channel until real providers are configured. The whole
	// path — template lookup, rendering, preferences, dedupe, logging — runs for
	// real; only the final network call is replaced. Swapping in MSG91 or FCM is
	// one line here.
	senders := []domain.Sender{
		sender.NewLogSender(domain.ChannelPush, deps.Log),
		sender.NewLogSender(domain.ChannelSMS, deps.Log),
		sender.NewLogSender(domain.ChannelEmail, deps.Log),
	}

	service := app.NewNotificationService(templates, logs, devices, prefs, deps.Users, senders, deps.Clock)

	return &Module{
		deps:      deps,
		templates: templates,
		logs:      logs,
		devices:   devices,
		prefs:     prefs,
		service:   service,
		handler:   notificationhttp.NewHandler(service),
	}
}

func (m *Module) Name() string { return "notification" }

// CustomerRoutes: device registration, preferences, history.
func (m *Module) CustomerRoutes() http.Handler { return m.handler.CustomerRoutes() }

// RegisterSubscribers wires the order lifecycle to customer messaging.
//
// Every handler is idempotent via a dedupe key derived from the order id and the
// event name. Outbox delivery is at-least-once, so a redelivered order.placed
// WILL happen — and without the key the customer gets two SMS, which is both a
// cost and the single most common complaint about food apps.
//
// Handlers return nil on failure rather than propagating: a failed SMS must not
// make the relay retry the event forever and eventually dead-letter it. The send
// log records what happened, which is where the answer lives.
func (m *Module) RegisterSubscribers(sub eventbus.Subscriber) {
	// --- order placed: the one that matters most to a customer ---
	sub.Subscribe(contracts.EventOrderPlaced, "notification.order_placed",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.OrderPlacedPayload](e)
			if err != nil {
				return err
			}
			// SMS as well as push, because this is the confirmation a customer
			// checks for. Later status changes are push-only: an SMS per transition
			// is spam and burns money.
			return m.service.Send(ctx, contracts.NotificationRequest{
				UserID:     payload.UserID,
				Channels:   []contracts.NotificationChannel{contracts.ChannelPush, contracts.ChannelSMS},
				TemplateID: "order.placed",
				Params: map[string]string{
					"number":  payload.OrderNumber,
					"kitchen": fallback(payload.KitchenName, "The kitchen"),
					"total":   rupees(payload.TotalMinor),
					"eta":     "30",
				},
				DedupeKey: dedupe(e, payload.OrderID.String()),
			})
		})

	// --- status transitions: push only ---
	for _, wiring := range []struct {
		event    eventbus.Name
		handler  string
		template string
	}{
		{contracts.EventOrderConfirmed, "notification.order_confirmed", "order.confirmed"},
		{contracts.EventOrderPreparing, "notification.order_preparing", "order.preparing"},
		{contracts.EventOrderReady, "notification.order_ready", "order.ready"},
		{contracts.EventOrderDelivered, "notification.order_delivered", "order.delivered"},
	} {
		// Captured by value: the loop variable would otherwise be shared across
		// every closure and all four handlers would use the last template.
		wiring := wiring

		sub.Subscribe(wiring.event, wiring.handler,
			func(ctx context.Context, e eventbus.Event) error {
				payload, err := eventbus.Decode[contracts.OrderStatusChangedPayload](e)
				if err != nil {
					return err
				}
				return m.service.Send(ctx, contracts.NotificationRequest{
					UserID:     payload.UserID,
					Channels:   []contracts.NotificationChannel{contracts.ChannelPush},
					TemplateID: wiring.template,
					Params: map[string]string{
						"number":  payload.OrderNumber,
						"kitchen": fallback(payload.KitchenName, "The kitchen"),
					},
					DedupeKey: dedupe(e, payload.OrderID.String()),
				})
			})
	}

	// --- cancellation: SMS too, because it involves money ---
	sub.Subscribe(contracts.EventOrderCancelled, "notification.order_cancelled",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.OrderStatusChangedPayload](e)
			if err != nil {
				return err
			}
			reason := payload.Reason
			if reason == "" {
				reason = "The order could not be completed."
			}
			return m.service.Send(ctx, contracts.NotificationRequest{
				UserID:     payload.UserID,
				Channels:   []contracts.NotificationChannel{contracts.ChannelPush, contracts.ChannelSMS},
				TemplateID: "order.cancelled",
				Params: map[string]string{
					"number": payload.OrderNumber,
					"reason": reason,
				},
				DedupeKey: dedupe(e, payload.OrderID.String()),
			})
		})

	// --- rider assigned: the customer wants a name and a number ---
	sub.Subscribe(contracts.EventRiderAssigned, "notification.rider_assigned",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.RiderAssignedPayload](e)
			if err != nil {
				return err
			}
			// No user id on this payload, so it cannot be addressed yet. Returning
			// nil rather than erroring: the event is still useful to analytics, and
			// failing here would dead-letter it. Adding UserID to the payload is the
			// fix, and it belongs in contracts.
			_ = payload
			return nil
		})

	// --- payment failed: tell them nothing was charged ---
	sub.Subscribe(contracts.EventPaymentFailed, "notification.payment_failed",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.PaymentPayload](e)
			if err != nil {
				return err
			}
			return m.service.Send(ctx, contracts.NotificationRequest{
				UserID:     payload.UserID,
				Channels:   []contracts.NotificationChannel{contracts.ChannelSMS},
				TemplateID: "payment.failed",
				Params: map[string]string{
					// The order number is not on the payment payload; the order id is
					// enough for the customer to find it in the app.
					"number": payload.OrderID.String()[:8],
				},
				DedupeKey: dedupe(e, payload.PaymentID.String()),
			})
		})

	// --- stock low: staff, not customers ---
	sub.Subscribe(contracts.EventStockLow, "notification.stock_low",
		func(ctx context.Context, e eventbus.Event) error {
			payload, err := eventbus.Decode[contracts.StockLowPayload](e)
			if err != nil {
				return err
			}
			// Kitchen staff are not resolvable from a kitchen id through
			// UserDirectory today, so this is recorded rather than delivered. The
			// low-stock signal is already on the dashboard and in metrics; pushing it
			// needs a staff-roster lookup that belongs in identity.
			logger := m.deps.Log
			logger.Warn("stock low",
				"kitchen_id", payload.KitchenID, "ingredient", payload.Name,
				"on_hand", payload.OnHand, "reorder_level", payload.ReorderLevel)
			return nil
		})
}

// Notifier is the contract other modules can use for direct sends. Most
// messaging should go through events instead; this is for the rare case where a
// module genuinely needs to address a specific user synchronously.
func (m *Module) Notifier() contracts.Notifier { return m.service }

func (m *Module) Service() *app.NotificationService { return m.service }

// dedupe builds an idempotency key from the event name and its subject.
//
// Deliberately NOT the event id: a redelivery of the SAME event carries the same
// id, but a genuinely new event for the same order (a second cancellation
// attempt) would get a new id and should not be suppressed. Keying on
// name+subject means "we already told this customer this thing about this order".
func dedupe(e eventbus.Event, subject string) string {
	return string(e.Name) + ":" + subject
}

// rupees formats minor units for display in a message.
func rupees(minor int64) string {
	whole := minor / 100
	paise := minor % 100
	if paise == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return fmt.Sprintf("%d.%02d", whole, paise)
}

// fallback supplies a default when an event payload omits a value.
//
// Templates require every param to be present — rendering fails rather than
// sending a literal "{{kitchen}}" to a customer — so every param must resolve to
// something. A generic word is a poor message; a broken one is worse.
func fallback(value, def string) string {
	if value == "" {
		return def
	}
	return value
}
