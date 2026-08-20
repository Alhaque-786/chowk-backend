# Backend Architecture

Go 1.26 · PostgreSQL 16 · Redis 7 · chi

This document explains the decisions. The code explains the details.

---

## 1. Why these choices

### Go, not Rust

The workload is I/O-bound: HTTP in, SQL out, gateway calls, real-time order
tracking. Almost no CPU-bound work exists in a food-delivery backend. Rust's
compile-time memory safety buys very little against that shape of problem, and
costs a large multiple in development time — `async` lifetimes and trait
plumbing are real friction when you are still discovering the domain.

Go gives goroutines (one per request, no async colouring), a stdlib HTTP server
good enough for production, and a hiring pool. Revisit if a genuinely
CPU-bound component appears — a routing optimiser, say — and write *that* in
Rust as its own service.

### chi, not Gin / Fiber / Echo

The counter-intuitive one. A "full framework" is a liability at this scale:

- **Gin/Echo** own your handler signature (`func(c *gin.Context)`). Every piece
  of middleware, every test helper, every future gRPC gateway then needs an
  adapter. Migrating off is a full-codebase rewrite.
- **Fiber** is not `net/http` at all (it's fasthttp). You lose the entire stdlib
  middleware ecosystem, `httptest`, and standard `context` propagation.
- **chi** *is* `net/http`. Handlers are `http.HandlerFunc`, middleware is
  `func(http.Handler) http.Handler`. Anything written for the stdlib works
  unmodified: OpenTelemetry, `grpc-gateway`, `httptest`, `pprof`.

Framework lock-in is the thing that stops a codebase evolving. chi has none.

### PostgreSQL, not MongoDB

| Requirement | Why Postgres |
|---|---|
| Checkout | Touches `orders`, `order_lines`, `stock_reservations`, `payments`, `coupon_redemptions` — one ACID transaction. Mongo's multi-doc txns are a bolt-on with perf caveats. |
| Money | `CHECK` constraints catch a negative total *at write time*. Schemaless silently persists a typo'd field. |
| Inventory | Concurrent stock deduction needs `SELECT … FOR UPDATE`. The textbook relational case. |
| Reporting | The admin dashboard is aggregation-heavy. SQL wins decisively. |

Redis handles what it is genuinely good at: carts, OTPs, rate counters, cached
menus, live-tracking pub/sub. Mongo is not used; if CMS-style menu content or
high-volume audit logs appear later, add it *then*.

---

## 2. Modular monolith

**One deployable. Ten bounded contexts. Enforced seams.**

```
internal/modules/
  identity/    users, auth, sessions, addresses
  catalog/     kitchens, menus, items, modifiers, prices
  inventory/   ingredients, recipes, stock ledger, reservations, purchasing
  ordering/    cart, orders, order state machine   ← the spine
  payment/     gateway, webhooks, refunds
  delivery/    riders, assignment, live tracking
  promotion/   coupons, offers, referrals, wallet
  review/      ratings, reviews, moderation
  notification/ push, SMS, email, WhatsApp
  analytics/   admin dashboard reporting
```

Microservices from day one would be the wrong call: you cannot draw correct
service boundaries for a domain you have not built yet, and getting them wrong
means distributed transactions across the seam you drew in the wrong place.
A monolith with *enforced* seams gets you the option to split later, at the
point where you actually know where the boundaries belong.

### The one hard rule

> A module may import `internal/contracts` and `internal/shared`.
> A module may **never** import another module.

`make check-boundaries` fails the build on violation. Enforced mechanically,
because code review will not catch this reliably — and a single convenience
import is how a codebase quietly becomes untanglable.

### Layers inside a module

```
identity/
├── domain/            entities, value objects, repository interfaces. No I/O.
├── app/               use cases. Owns transaction boundaries.
├── adapter/postgres/  repository impls. The only place with SQL.
├── adapter/http/      handlers + DTOs. The only place that knows HTTP.
└── module.go          New(Deps), Routes(), RegisterSubscribers()
```

Dependencies point **inward**. `domain` declares the interfaces it needs;
`adapter/postgres` implements them. The domain never points at the adapter, so
business rules are testable with no database and portable if the module becomes
its own service.

`identity` is the complete reference implementation. Copy its shape exactly.

---

## 3. How modules communicate

### Asynchronously — events (preferred)

When an order is placed, `ordering` does **not** call
`notification.SendSMS()`. It publishes `order.placed`. `notification`
subscribes. Ordering does not know notification exists.

That inversion is what makes extraction mechanical instead of a rewrite.

### Synchronously — narrow ports

Some answers are needed before proceeding: checkout genuinely cannot continue
without knowing whether stock exists. Those go through interfaces in
`internal/contracts`, defined from the **consumer's** point of view and kept as
narrow as its actual need:

```go
type StockReserver interface {
    Reserve(ctx, orderID, kitchenID id.ID, lines []ReservationLine) error
    Release(ctx, orderID id.ID) error
    Consume(ctx, orderID id.ID) error
}
```

`ordering` depends on that. `internal/server` injects `inventory`'s
implementation. Swap in a gRPC client later and `ordering` recompiles untouched.

### The transactional outbox

Publishing an event and committing state must be atomic:

```go
db.InTx(ctx, func(ctx context.Context) error {
    orders.Create(ctx, order)          // state change
    inventory.Reserve(ctx, ...)        // cross-module, same tx
    events.Publish(ctx, orderPlaced)   // → outbox_events row, same tx
    return nil
})
```

"Insert the order, then publish" is two systems and no atomicity. Crash between
them and you get an order nobody was told about, or a notification for an order
that rolled back — both show up in production as *"customer charged but no
order."*

Writing the event as a row in the same transaction makes it all-or-nothing. The
relay (`cmd/worker`) then drains committed rows using
`FOR UPDATE SKIP LOCKED`, so it is safe to run on every replica with no
coordination.

**Delivery is at-least-once. Every handler must be idempotent.**

Replacing the relay's sink with Kafka/NATS is the microservices migration.
Publishing modules do not change.

---

## 4. Non-obvious decisions worth knowing

**Money is `int64` paise. Never float.** `0.1 + 0.2 != 0.3` in binary floating
point. A ledger drifting by a paise per order becomes unreconcilable. See
`shared/money` — `ApplyBasisPoints` (5% GST = 500 bps) and `SplitEvenly`
(distributes remainder so parts sum exactly to the whole).

**Order status is a data-driven state machine.** `ordering/domain/order.go`
declares every legal transition in one map. An illegal move is impossible by
construction. Scattered `if` statements eventually produce an order that is both
`delivered` and `cancelled`, with no way to reconstruct how.

**Prices are snapshotted onto the order, never joined live.** A kitchen raising
a price tomorrow must not alter yesterday's invoice. Correctness *and* GST
record-keeping.

**`orders_total_consistency_check`.** The database asserts
`total = subtotal + fees + tax + tip − discount`. Arithmetic divergence between
cart preview and charged amount is caught at write time, not in a reconciliation
report a month later.

**Cursor pagination only. No `OFFSET`.** `OFFSET 50000` makes Postgres walk and
discard 50k rows, and concurrent inserts cause rows to be skipped or duplicated
across pages. See `shared/paging`.

**Access tokens are stateless JWTs; refresh tokens are opaque and stored
hashed.** Authorizing a request needs no DB round trip. Refresh tokens must be
revocable (logout, stolen device), which a stateless token cannot be. Only the
SHA-256 is stored, so a DB leak yields no usable sessions.

**Refresh rotation with reuse detection.** Each refresh token is single-use.
Replay of a rotated token means duplicate-or-theft — and we cannot tell which —
so every session for that user is revoked. A stolen token becomes one extra
login instead of permanent silent access.

**Login timing is constant.** A non-existent phone is compared against
`password.DummyHash`, so "no such user" costs the same ~250ms as "wrong
password". Skipping this is a user-enumeration oracle measurable over the
network.

**Partial indexes throughout.** `orders_kitchen_active_idx` covers only live
statuses: a kitchen has a handful of live orders against a table that grows
forever. Same for `outbox_pending_idx` — without the predicate it would grow
without bound and the relay would slow as history accumulates.

**Idempotency keys on checkout.** Mobile clients retry on flaky networks. A
retried `POST /checkout` without this is a second order and a second charge.

**Stock is a ledger, not a number.** `stock_movements` is append-only;
`ingredients.on_hand` is a cache of its sum. "We should have 4kg, the shelf has
1kg" is otherwise unanswerable.

**Rate limiting keys on user before IP.** Behind carrier NAT, thousands of Jio
customers share one IP — an IP-only limit throttles a whole city because one
person was abusive. It also **fails open**: losing rate limiting beats losing
the service.

**`DisallowUnknownFields` on every request body.** A typo'd `"quantiy"` would
otherwise be silently ignored and default to zero. On a checkout request, that
is an order at the wrong price.

---

## 5. Running it

```bash
cp .env.example .env      # then set JWT_SECRET (openssl rand -base64 48)
make up                   # Postgres + Redis, waits for health
make migrate
make run                  # API on :8080
make worker               # outbox relay + maintenance, separate terminal
```

```bash
make check              # fmt + vet + test -race. Run before pushing.
make check-boundaries   # fail if a module imports another module
make migration name=add_wallet
```

Migrations are immutable once applied — the runner checksums them and refuses to
start if an applied file was edited, because that edit is already live elsewhere
and the environments have silently diverged. Add a new migration instead.

---

## 6. Build order from here

1. **catalog** — kitchens + menu. Nothing can be ordered until something exists
   to order. Implements `PriceQuoter`.
2. **ordering** — cart + checkout saga. The state machine already exists.
3. **payment** — Razorpay + webhooks. Verify the HMAC signature before trusting
   any field; an unverified webhook endpoint lets anyone mark any order paid.
4. **inventory** — reservations. Needed before real volume, or you oversell.
5. **notification**, then **delivery**, **promotion**, **review**, **analytics**.

Each module's `doc.go` records its responsibilities, the events it
publishes/consumes, and the specific traps in that domain. Read it before
starting one.
