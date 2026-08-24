// Package postgres implements the ordering module's repository ports.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/modules/ordering/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/money"
	"github.com/zevoxa/chowk-backend/internal/shared/paging"
)

type OrderRepository struct {
	db *database.DB
}

func NewOrderRepository(db *database.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

const orderColumns = `
	id, number, user_id, kitchen_id, status,
	subtotal_minor, packaging_fee_minor, delivery_fee_minor, discount_minor,
	tax_minor, tip_minor, total_minor, currency,
	payment_method, payment_id, coupon_id, coupon_code,
	delivery_address_id, delivery_line1, delivery_line2, delivery_landmark,
	delivery_city, delivery_pincode, delivery_latitude, delivery_longitude,
	receiver_name, receiver_phone, rider_id,
	customer_note, kitchen_note,
	promised_at, confirmed_at, prepared_at, picked_up_at, delivered_at,
	cancelled_at, cancel_reason, cancelled_by,
	version, created_at, updated_at`

// Create inserts the order and all its lines.
//
// Must be called inside a transaction: an order without its lines is a corrupt
// invoice, and the caller is also reserving stock and writing an outbox event in
// the same commit.
func (r *OrderRepository) Create(ctx context.Context, o *domain.Order) error {
	const q = `
		INSERT INTO orders (
			id, number, user_id, kitchen_id, status,
			subtotal_minor, packaging_fee_minor, delivery_fee_minor, discount_minor,
			tax_minor, tip_minor, total_minor, currency,
			payment_method, payment_id, coupon_id, coupon_code,
			delivery_address_id, delivery_line1, delivery_line2, delivery_landmark,
			delivery_city, delivery_pincode, delivery_latitude, delivery_longitude,
			receiver_name, receiver_phone,
			customer_note, kitchen_note, promised_at, version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,
		          $18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,1,$31,$32)`

	_, err := r.db.Q(ctx).Exec(ctx, q,
		o.ID, o.Number, o.UserID, o.KitchenID, string(o.Status),
		o.Subtotal.Minor(), o.PackagingFee.Minor(), o.DeliveryFee.Minor(), o.Discount.Minor(),
		o.TaxAmount.Minor(), o.Tip.Minor(), o.Total.Minor(), string(o.Currency),
		string(o.PaymentMethod), o.PaymentID, o.CouponID, o.CouponCode,
		nullID(o.DeliveryAddressID), o.DeliveryLine1, o.DeliveryLine2, o.DeliveryLandmark,
		o.DeliveryCity, o.DeliveryPincode, o.DeliveryLatitude, o.DeliveryLongitude,
		o.ReceiverName, o.ReceiverPhone,
		o.CustomerNote, o.KitchenNote, o.PromisedAt, o.CreatedAt, o.UpdatedAt,
	)
	if err != nil {
		if database.IsCheckViolation(err) {
			// The DB re-checks total = subtotal + fees + tax + tip - discount.
			// Hitting this means the Go arithmetic and the SQL constraint disagree,
			// which is a bug worth surfacing loudly rather than a user error.
			return apperr.Internal(fmt.Errorf("ordering: order failed a DB invariant: %w", err))
		}
		if database.IsUniqueViolation(err, "orders_number_key") {
			return apperr.Conflict("ORDER_NUMBER_COLLISION", "Please try again.")
		}
		return apperr.Internal(fmt.Errorf("ordering: create order: %w", err))
	}

	return r.insertLines(ctx, o)
}

func (r *OrderRepository) insertLines(ctx context.Context, o *domain.Order) error {
	if len(o.Lines) == 0 {
		return nil
	}

	/*
		ONE STATEMENT FOR EVERY LINE, not one per line.

		This is on the checkout path, inside the SERIALIZABLE transaction, so each
		round trip saved is a round trip the transaction is not holding its locks
		for. A five-line order was five round trips; against a database in
		ap-south-1 measured at 29.7ms each that was ~150ms of pure waiting with the
		order row already locked.

		`unnest` rather than a multi-row VALUES list, for a reason that only shows up
		later: a VALUES list has a different placeholder count per line count, so the
		SQL TEXT changes with the size of the order. pgx caches prepared statements by
		text, so two-line and three-line orders would each prepare and cache their own
		statement, forever. Arrays keep the text constant and one plan serves every
		order size.
	*/
	const q = `
		INSERT INTO order_lines (
			id, order_id, menu_item_id, name, quantity,
			unit_price_minor, line_total_minor, tax_bps, tax_minor, modifiers, note
		)
		SELECT
			u.id, $2::uuid, u.menu_item_id, u.name, u.quantity,
			u.unit_price_minor, u.line_total_minor, u.tax_bps, u.tax_minor, u.modifiers, u.note
		FROM unnest(
			$1::uuid[], $3::uuid[], $4::text[], $5::int[],
			$6::bigint[], $7::bigint[], $8::int[], $9::bigint[], $10::jsonb[], $11::text[]
		) AS u(id, menu_item_id, name, quantity,
			unit_price_minor, line_total_minor, tax_bps, tax_minor, modifiers, note)`

	n := len(o.Lines)
	ids := make([]string, n)
	itemIDs := make([]string, n)
	names := make([]string, n)
	quantities := make([]int32, n)
	unitPrices := make([]int64, n)
	lineTotals := make([]int64, n)
	taxBps := make([]int32, n)
	taxMinors := make([]int64, n)
	modifiers := make([][]byte, n)
	notes := make([]string, n)

	for i := range o.Lines {
		l := &o.Lines[i]

		// Modifiers are denormalised as JSONB: they are never queried independently
		// of their line, and a child table would add a join to every order read for
		// no benefit.
		mods := l.Modifiers
		if mods == nil {
			mods = []domain.LineModifier{}
		}
		encoded, err := json.Marshal(mods)
		if err != nil {
			return apperr.Internal(fmt.Errorf("ordering: marshal modifiers: %w", err))
		}

		ids[i] = l.ID.String()
		itemIDs[i] = l.MenuItemID.String()
		names[i] = l.Name
		quantities[i] = int32(l.Quantity)
		unitPrices[i] = l.UnitPrice.Minor()
		lineTotals[i] = l.LineTotal.Minor()
		taxBps[i] = int32(l.TaxBps)
		taxMinors[i] = l.TaxAmount.Minor()
		modifiers[i] = encoded
		notes[i] = l.Note
	}

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		ids, o.ID, itemIDs, names, quantities,
		unitPrices, lineTotals, taxBps, taxMinors, modifiers, notes,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("ordering: insert %d order line(s): %w", n, err))
	}
	// A batched insert that silently writes fewer rows than it was given would
	// produce an order whose lines do not sum to its total. The loop version could
	// not do this; the batched version can, so it is checked.
	if tag.RowsAffected() != int64(n) {
		return apperr.Internal(fmt.Errorf(
			"ordering: inserted %d order lines, expected %d", tag.RowsAffected(), n))
	}
	return nil
}

// Update applies optimistic locking so two concurrent status changes cannot both
// win and lose one.
func (r *OrderRepository) Update(ctx context.Context, o *domain.Order) error {
	const q = `
		UPDATE orders SET
			status = $2, payment_id = $3, rider_id = $4, kitchen_note = $5,
			promised_at = $6, confirmed_at = $7, prepared_at = $8, picked_up_at = $9,
			delivered_at = $10, cancelled_at = $11, cancel_reason = $12, cancelled_by = $13,
			version = version + 1, updated_at = $14
		WHERE id = $1 AND version = $15`

	tag, err := r.db.Q(ctx).Exec(ctx, q,
		o.ID, string(o.Status), o.PaymentID, o.RiderID, o.KitchenNote,
		o.PromisedAt, o.ConfirmedAt, o.PreparedAt, o.PickedUpAt,
		o.DeliveredAt, o.CancelledAt, o.CancelReason, o.CancelledBy,
		o.UpdatedAt, o.Version,
	)
	if err != nil {
		return apperr.Internal(fmt.Errorf("ordering: update order %s: %w", o.ID, err))
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("ORDER_MODIFIED",
			"This order was just updated elsewhere. Please reload and try again.")
	}
	o.Version++
	return nil
}

func (r *OrderRepository) FindByID(ctx context.Context, orderID id.ID) (*domain.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders WHERE id = $1`
	return r.findOnePipelined(ctx, q, orderID)
}

func (r *OrderRepository) FindByNumber(ctx context.Context, number string) (*domain.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders WHERE number = $1`
	return r.findOne(ctx, q, strings.ToUpper(number))
}

// FindByIDForUpdate takes a row lock for the rest of the transaction.
//
// Required whenever the order's status is about to change: without it, a customer
// cancelling at the same moment the kitchen accepts produces a lost update, and
// the order ends up in a state nobody chose.
func (r *OrderRepository) FindByIDForUpdate(ctx context.Context, orderID id.ID) (*domain.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders WHERE id = $1 FOR UPDATE`
	return r.findOnePipelined(ctx, q, orderID)
}

/*
findOnePipelined fetches an order AND its lines in a single network round trip.

`findOne` below does the same work in two: read the order, then read its lines. The
second query only needs the order id — which the caller already had — so the two
are genuinely independent and can be pipelined. pgx sends both statements
back to back and reads both responses afterwards.

WHY THIS ONE AND NOT EVERYTHING. Pipelining requires statements that do not depend
on each other's results, and almost nothing in this codebase qualifies: checkout
reads, decides, then writes, and the idempotency Claim inspects the outcome of its
own INSERT before deciding what to do next. `FindByNumber` is the instructive case
— it looks identical to this, and it CANNOT be pipelined, because the lines query
needs the order id that the first query returns. Same shape, opposite answer.

Worth ~30ms per order view against a database in another city, ~1ms same-region.
The order-detail endpoint is polled while somebody waits for their food, so it is
one of the more frequently hit reads in the system.
*/
func (r *OrderRepository) findOnePipelined(ctx context.Context, q string, orderID id.ID) (*domain.Order, error) {
	batch := &pgx.Batch{}
	batch.Queue(q, orderID)
	batch.Queue(loadLinesQuery, []id.ID{orderID})

	results := r.db.Q(ctx).SendBatch(ctx, batch)
	// Closed exactly once. Leaving a BatchResults open returns its connection to the
	// pool mid-conversation, and the next borrower reads someone else's responses.
	defer results.Close()

	order, err := scanOrder(results.QueryRow())
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("ordering: find order: %w", err))
	}

	rows, err := results.Query()
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("ordering: load order lines: %w", err))
	}
	if err := scanLinesInto(rows, order); err != nil {
		return nil, err
	}
	return order, nil
}

func (r *OrderRepository) findOne(ctx context.Context, q string, args ...any) (*domain.Order, error) {
	order, err := scanOrder(r.db.Q(ctx).QueryRow(ctx, q, args...))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, apperr.NotFound("ORDER_NOT_FOUND", "That order could not be found.")
		}
		return nil, apperr.Internal(fmt.Errorf("ordering: find order: %w", err))
	}
	if err := r.loadLines(ctx, order); err != nil {
		return nil, err
	}
	return order, nil
}

// loadLinesQuery is shared by loadLines and findOnePipelined. One definition, so a
// column added to order_lines cannot be picked up by one path and missed by the
// other — which would be a scan error in whichever path the tests exercise less.
const loadLinesQuery = `
		SELECT order_id, id, menu_item_id, name, quantity,
		       unit_price_minor, line_total_minor, tax_bps, tax_minor, modifiers, note
		FROM order_lines WHERE order_id = ANY($1)`

func (r *OrderRepository) loadLines(ctx context.Context, orders ...*domain.Order) error {
	if len(orders) == 0 {
		return nil
	}

	ids := make([]id.ID, len(orders))
	for i, o := range orders {
		ids[i] = o.ID
	}

	rows, err := r.db.Q(ctx).Query(ctx, loadLinesQuery, ids)
	if err != nil {
		return apperr.Internal(fmt.Errorf("ordering: load order lines: %w", err))
	}
	return scanLinesInto(rows, orders...)
}

// scanLinesInto reads order_lines rows and attaches each to its order.
//
// Shared by loadLines and findOnePipelined so the two cannot drift: the money
// conversions and the modifiers JSON decode are easy to get subtly different in a
// second copy, and the difference would only show on whichever path the tests
// happen to exercise less.
func scanLinesInto(rows pgx.Rows, orders ...*domain.Order) error {
	defer rows.Close()

	byID := make(map[id.ID]*domain.Order, len(orders))
	for _, o := range orders {
		byID[o.ID] = o
	}

	for rows.Next() {
		var (
			orderID   id.ID
			l         domain.Line
			unitPrice int64
			lineTotal int64
			taxMinor  int64
			modifiers []byte
		)
		if err := rows.Scan(&orderID, &l.ID, &l.MenuItemID, &l.Name, &l.Quantity,
			&unitPrice, &lineTotal, &l.TaxBps, &taxMinor, &modifiers, &l.Note); err != nil {
			return apperr.Internal(fmt.Errorf("ordering: scan order line: %w", err))
		}

		l.OrderID = orderID
		l.UnitPrice = money.FromMinor(unitPrice, money.INR)
		l.LineTotal = money.FromMinor(lineTotal, money.INR)
		l.TaxAmount = money.FromMinor(taxMinor, money.INR)
		if len(modifiers) > 0 {
			if err := json.Unmarshal(modifiers, &l.Modifiers); err != nil {
				return apperr.Internal(fmt.Errorf("ordering: decode modifiers: %w", err))
			}
		}

		if o, ok := byID[orderID]; ok {
			o.Lines = append(o.Lines, l)
		}
	}
	return rows.Err()
}

func (r *OrderRepository) ListForUser(ctx context.Context, userID id.ID, filter domain.Filter, page paging.Request) ([]*domain.Order, error) {
	args := []any{userID}
	conds := []string{"user_id = $1"}

	if len(filter.Statuses) > 0 {
		args = append(args, statusStrings(filter.Statuses))
		conds = append(conds, fmt.Sprintf("status = ANY($%d)", len(args)))
	}

	keyset, orderBy, keysetArgs := page.SQL("created_at", "id", len(args))
	if keyset != "" {
		conds = append(conds, keyset)
		args = append(args, keysetArgs...)
	}
	args = append(args, page.FetchLimit())

	q := fmt.Sprintf(`SELECT %s FROM orders WHERE %s %s LIMIT $%d`,
		orderColumns, strings.Join(conds, " AND "), orderBy, len(args))

	return r.queryOrders(ctx, q, args...)
}

// ListForKitchen backs the staff queue. Oldest first so the queue is FIFO — a
// kitchen works through orders in the sequence they arrived.
func (r *OrderRepository) ListForKitchen(ctx context.Context, kitchenID id.ID, filter domain.Filter, page paging.Request) ([]*domain.Order, error) {
	args := []any{kitchenID}
	conds := []string{"kitchen_id = $1"}

	if len(filter.Statuses) > 0 {
		args = append(args, statusStrings(filter.Statuses))
		conds = append(conds, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	args = append(args, page.FetchLimit())

	q := fmt.Sprintf(`SELECT %s FROM orders WHERE %s ORDER BY created_at ASC LIMIT $%d`,
		orderColumns, strings.Join(conds, " AND "), len(args))

	return r.queryOrders(ctx, q, args...)
}

func (r *OrderRepository) queryOrders(ctx context.Context, q string, args ...any) ([]*domain.Order, error) {
	rows, err := r.db.Q(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("ordering: query orders: %w", err))
	}
	defer rows.Close()

	var out []*domain.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, apperr.Internal(fmt.Errorf("ordering: scan order: %w", err))
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}

	if err := r.loadLines(ctx, out...); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *OrderRepository) AppendStatusEvent(ctx context.Context, e domain.StatusEvent) error {
	const q = `
		INSERT INTO order_status_events (id, order_id, from_status, to_status, reason, actor_id, actor_role, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		e.ID, e.OrderID, string(e.From), string(e.To), e.Reason, e.ActorID, e.ActorRole, e.CreatedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("ordering: append status event: %w", err))
	}
	return nil
}

func (r *OrderRepository) ListStatusEvents(ctx context.Context, orderID id.ID) ([]domain.StatusEvent, error) {
	const q = `
		SELECT id, order_id, from_status, to_status, reason, actor_id, actor_role, created_at
		FROM order_status_events WHERE order_id = $1 ORDER BY created_at`

	rows, err := r.db.Q(ctx).Query(ctx, q, orderID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("ordering: list status events: %w", err))
	}
	defer rows.Close()

	var out []domain.StatusEvent
	for rows.Next() {
		var (
			e        domain.StatusEvent
			from, to string
		)
		if err := rows.Scan(&e.ID, &e.OrderID, &from, &to, &e.Reason, &e.ActorID, &e.ActorRole, &e.CreatedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("ordering: scan status event: %w", err))
		}
		e.From = domain.Status(from)
		e.To = domain.Status(to)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountActiveForUser backs the "too many orders in flight" abuse guard.
func (r *OrderRepository) CountActiveForUser(ctx context.Context, userID id.ID) (int, error) {
	const q = `
		SELECT count(*) FROM orders
		WHERE user_id = $1
		  AND status IN ('pending_payment','placed','confirmed','preparing','ready','picked_up')`

	var n int
	if err := r.db.Q(ctx).QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, apperr.Internal(fmt.Errorf("ordering: count active orders: %w", err))
	}
	return n, nil
}

// HasEverOrdered backs the first-order-only coupon rule.
//
// `EXISTS` with `LIMIT 1`, not `count(*)`: the question is boolean, and Postgres
// can stop at the first matching row instead of scanning every order the customer
// has ever placed. On a heavy user that is the difference between one index hit
// and hundreds.
//
// NO status filter, deliberately. Cancelled and rejected orders count as having
// ordered — otherwise the offer is farmable by placing an order and cancelling it,
// which is exactly the abuse a first-order restriction exists to prevent.
func (r *OrderRepository) HasEverOrdered(ctx context.Context, userID id.ID) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM orders WHERE user_id = $1 LIMIT 1)`

	var ordered bool
	if err := r.db.Q(ctx).QueryRow(ctx, q, userID).Scan(&ordered); err != nil {
		return false, apperr.Internal(fmt.Errorf("ordering: has ever ordered: %w", err))
	}
	return ordered, nil
}

// FindStale finds orders stuck in a status past a deadline, for the reaper.
//
// Its main job is releasing stock held by orders abandoned mid-payment. Without
// it, one customer opening checkout and walking away holds the last portion
// forever.
func (r *OrderRepository) FindStale(ctx context.Context, status domain.Status, olderThan time.Time, limit int) ([]*domain.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders
		WHERE status = $1 AND created_at < $2
		ORDER BY created_at LIMIT $3`
	return r.queryOrders(ctx, q, string(status), olderThan, limit)
}

func scanOrder(row scanner) (*domain.Order, error) {
	var (
		o                                                        domain.Order
		status, paymentMethod, currency                          string
		subtotal, packaging, delivery, discount, tax, tip, total int64
		deliveryAddressID                                        *id.ID
		lat, lng                                                 *float64
	)
	if err := row.Scan(
		&o.ID, &o.Number, &o.UserID, &o.KitchenID, &status,
		&subtotal, &packaging, &delivery, &discount, &tax, &tip, &total, &currency,
		&paymentMethod, &o.PaymentID, &o.CouponID, &o.CouponCode,
		&deliveryAddressID, &o.DeliveryLine1, &o.DeliveryLine2, &o.DeliveryLandmark,
		&o.DeliveryCity, &o.DeliveryPincode, &lat, &lng,
		&o.ReceiverName, &o.ReceiverPhone, &o.RiderID,
		&o.CustomerNote, &o.KitchenNote,
		&o.PromisedAt, &o.ConfirmedAt, &o.PreparedAt, &o.PickedUpAt, &o.DeliveredAt,
		&o.CancelledAt, &o.CancelReason, &o.CancelledBy,
		&o.Version, &o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		return nil, err
	}

	o.Status = domain.Status(status)
	o.PaymentMethod = domain.PaymentMethod(paymentMethod)
	o.Currency = money.Currency(currency)
	o.Subtotal = money.FromMinor(subtotal, o.Currency)
	o.PackagingFee = money.FromMinor(packaging, o.Currency)
	o.DeliveryFee = money.FromMinor(delivery, o.Currency)
	o.Discount = money.FromMinor(discount, o.Currency)
	o.TaxAmount = money.FromMinor(tax, o.Currency)
	o.Tip = money.FromMinor(tip, o.Currency)
	o.Total = money.FromMinor(total, o.Currency)

	if deliveryAddressID != nil {
		o.DeliveryAddressID = *deliveryAddressID
	}
	if lat != nil {
		o.DeliveryLatitude = *lat
	}
	if lng != nil {
		o.DeliveryLongitude = *lng
	}
	return &o, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func statusStrings(statuses []domain.Status) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = string(s)
	}
	return out
}

func nullID(v id.ID) *id.ID {
	if id.IsNil(v) {
		return nil
	}
	return &v
}

// OrderCursor derives a pagination cursor for paging.NewPage.
func OrderCursor(o *domain.Order) paging.Cursor {
	return paging.Cursor{CreatedAt: o.CreatedAt, ID: o.ID}
}

var _ domain.OrderRepository = (*OrderRepository)(nil)
