# Request Flow

How a request actually travels through this backend, end to end.

---

## 1. The middleware pipeline

Every request passes through this, outermost first. Order is deliberate.

```
TCP → net/http.Server                ReadHeaderTimeout 10s (slowloris guard)
  │
  ├─ RequestID          reuse inbound X-Request-ID, else mint UUIDv7
  ├─ SecurityHeaders    nosniff, DENY, restrictive CSP
  ├─ Observe            OTel span opens, metrics start, in-flight++
  ├─ RequestLogger      attaches request-scoped logger carrying trace_id
  ├─ Recoverer          panic → 500, process survives
  ├─ CORS               allowlist from config, never "*"
  ├─ Timeout            bounds handler duration
  │
  ├─ [rate limit: global 300/min]
  ├─ [rate limit: auth 10/min on /auth]
  │
  ├─ Authenticate       Bearer JWT → Principal in context (no DB hit)
  ├─ RequireRole        authorize
  ├─ Idempotency        replay on repeated Idempotency-Key
  │
  └─ Handler
```

**Why `Observe` sits outside `Recoverer`:** a panicking handler must still be
counted as a 5xx. Inside, the panic would be swallowed before the metric fired
and the failure would be invisible on the dashboard.

**Why `Authenticate` does no database lookup:** the JWT is self-contained
(user id, roles, kitchen, session). Authorizing a request is pure computation,
so the auth check never becomes the bottleneck under load.

---

## 2. Inside a handler — the four layers

Take `POST /api/v1/auth/register`:

```
adapter/http/auth_handler.go        TRANSPORT
   httpx.Decode → registerRequest       reject unknown fields
   req.Validate()                       shape validation
   ↓ calls app with a plain input struct
app/auth.go                         USE CASE
   domain.NewPhone(raw)                 normalise + validate
   password.ValidateStrength()
   users.ExistsByPhone()                cheap precheck
   hasher.Hash()                        ~250ms bcrypt
   db.InTx(func(ctx) {                  ← TRANSACTION BOUNDARY
       users.Create(ctx, user)
       issueTokens(ctx, ...)            writes session row
       events.Publish(ctx, evt)         → outbox_events row
   })
   ↓
adapter/postgres/user_repository.go ADAPTER
   db.Q(ctx)                            resolves to the ambient tx
   translate pgconn.PgError → apperr
   ↓
domain/user.go                       DOMAIN
   Phone value object, User entity, business rules. No I/O.
```

Each layer only knows the one below it. The domain knows nothing about HTTP, SQL,
or Redis — which is what makes it unit-testable with no infrastructure.

---

## 3. Error flow

```
domain    apperr.Conflict("PHONE_ALREADY_REGISTERED", "...")
   ↑
app       returns it unchanged, or wraps a driver error
   ↑
adapter   pgconn.PgError → apperr.Internal (cause retained for logs)
   ↑
handler   httpx.Error(w, r, err)
             ├─ RecordError()         → chowk_errors_total{kind,code}
             ├─ tracing.RecordError() → span marked failed
             ├─ log (Error if 5xx, Info if 4xx)
             └─ statusFor(kind)       → the ONLY kind→HTTP mapping
```

`statusFor` existing in exactly one place is why every module stays
transport-agnostic. A module says "this conflicts"; `httpx` decides that means
409.

Internal errors return a generic message. Leaking
`duplicate key value violates unique constraint users_phone_key` tells an
attacker your schema.

---

## 4. Checkout — the critical path

**Not yet built.** This is the design; `ordering/domain` already has the state
machine and `contracts` already has every port it needs.

```
POST /api/v1/orders/checkout
  Idempotency-Key: <client uuid>          ← without this, a retry = second charge

 1. Idempotency middleware claims the key
       INSERT ... ON CONFLICT DO NOTHING  ← the DB is the mutex
       already completed? → replay stored response, STOP

 2. Validate cart, resolve delivery address (contracts.UserDirectory)

 3. db.InTx {                             ← ONE transaction, all of it
      a. PriceQuoter.QuotePrices()        server-side prices. NEVER trust client
                                          prices, or biryani costs ₹1
      b. KitchenAvailability.IsAcceptingOrders()
      c. StockReserver.Reserve()          SELECT..FOR UPDATE on ingredients
                                          → conflict if the last portion is gone
      d. DiscountCalculator.Calculate() + Redeem()
      e. order.RecalculateTotals()        per-line GST, discount cap
      f. orders.Create()                  DB re-checks total arithmetic
      g. events.Publish(order.placed)     → outbox, same tx
    }
    any step fails → everything rolls back. No orphaned reservation,
    no burned coupon, no phantom order.

 4. PaymentInitiator.CreateIntent()       OUTSIDE the tx — never hold a DB
                                          transaction open across a network call
                                          to a third party

 5. Return { order, payment_intent }; middleware stores the response

 6. Relay drains order.placed →
       notification: "order received" SMS/push
       analytics:    counters
       kitchen:      live dashboard push
```

**Why payment is outside the transaction:** holding a transaction open across a
call to Razorpay means a 30-second gateway hang holds a database connection and a
row lock for 30 seconds. Under load that exhausts the pool and takes down
checkout for everyone.

**Consequence:** the order exists in `pending_payment` before payment resolves.
The `payment.captured` webhook moves it to `placed`; a reaper releases stock from
orders abandoned in `pending_payment`. That is the correct trade — a
briefly-inconsistent order is recoverable; an exhausted pool is an outage.

---

## 5. Event flow

```
WRITE PATH (synchronous, transactional)
  use case → Outbox.Publish → INSERT outbox_events   [same tx as state change]
  commit ─────────────────────────────────────────►  both or neither

READ PATH (asynchronous, at-least-once)
  Relay (every 1s, in cmd/worker)
    UPDATE ... WHERE status='pending' AND next_attempt_at <= now()
      FOR UPDATE SKIP LOCKED              ← safe on every replica, no coordination
    → Dispatcher.Dispatch(event)
        fans out to all subscribers CONCURRENTLY
        each in its own goroutine, own timeout, own panic recovery
    all succeeded → status='processed'
    any failed    → exponential backoff, retry
    8 attempts    → status='dead_letter'  ← needs a human
```

Handlers run independently because they are genuinely separate concerns — notify
the customer / decrement stock / bump analytics. Coupling their fates would mean
a flaky SMS provider blocks stock deduction.

**Every handler must be idempotent.** At-least-once means the same event will
sometimes arrive twice. That is what `NotificationRequest.DedupeKey` and the
unique index on `ledger_entries (order_id, entry_type)` are for.

---

## 6. Marketplace flow (dormant today)

Currently one first-party merchant, `commission_bps = 0`. When partners onboard:

```
order.delivered
   ↓
ledger_entries (idempotent via unique index on order_id+entry_type)
   + order_revenue   gross owed to merchant
   − commission      gross × merchant.commission_bps
   − gateway_fee     if merchant absorbs it
   ↓
settlement job (weekly, distributed lock so one replica runs it)
   SELECT unsettled entries WHERE settlement_id IS NULL
   → settlement row, arithmetic verified by CHECK constraint
   → payout via provider
   → link entries to settlement
```

The balance is `SUM(amount_minor)`, never an `UPDATE`-able column. A balance you
can update is a balance a bug can silently corrupt with no way to find out when.

---

## 7. Where time goes

Rough local numbers, to make the trade-offs concrete.

| Step | Typical | Note |
|---|---|---|
| Middleware chain | < 1 ms | pure computation |
| JWT verify | ~ 50 µs | HMAC, no DB |
| Rate limit | ~ 0.5 ms | one Redis round trip |
| bcrypt (login/register) | **~ 250 ms** | intentional; the cost of security |
| Simple indexed query | 1–5 ms | |
| Checkout transaction | 20–50 ms | multi-table + row locks |
| Payment intent | **200–2000 ms** | third party — hence the Guard |
| Cached menu (hit) | ~ 1 ms | |
| Cached menu (miss) | 10–30 ms | singleflight collapses concurrent misses |

The two slow things are bcrypt (deliberate) and the payment gateway (external).
Everything else is single-digit milliseconds, which is why the connection pool —
not CPU — is the resource that runs out first, and why every external call is
wrapped in a timeout and a bulkhead.
