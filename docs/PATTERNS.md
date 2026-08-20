# Pattern Catalog

Every design pattern used in this codebase, what problem it solves, where it
lives, and — where it matters — why the obvious alternative was rejected.

Patterns marked **PLANNED** are not built yet; they are recorded here so the
decision is made before someone improvises a different one.

---

## 1. Architectural patterns

### Modular Monolith
**Where:** `internal/modules/*` · **Status:** built

One deployable, ten bounded contexts, enforced seams. Microservices from day one
would mean drawing service boundaries for a domain you have not built — and the
cost of a wrong boundary is a distributed transaction across it.

**Enforcement:** `make check-boundaries` fails the build if one module imports
another. Convention without enforcement decays.

### Hexagonal / Ports & Adapters
**Where:** every module's `domain/` (ports) → `adapter/` (adapters)

The domain declares interfaces it needs; adapters implement them. Dependencies
point inward, so business rules are testable with no database and portable if the
module becomes a service.

### Dependency Inversion
**Where:** `internal/contracts/ports.go`

`ordering` depends on `contracts.StockReserver`, not on `inventory`. The
composition root injects the implementation. Swap it for a gRPC client and
`ordering` recompiles untouched — that is the microservices migration.

### Composition Root
**Where:** `internal/server/server.go`

The single place that knows every module. All wiring happens here; nothing else
constructs a dependency. If you want to inject something in two places, you have
two composition roots and no longer know what the object graph is.

### Shared Kernel
**Where:** `internal/shared/*`

`money`, `id`, `apperr`, `paging`, `clock`. Deliberately tiny. A shared kernel
that grows becomes a distributed dependency every module must agree on before
changing — the coupling the module boundaries exist to prevent.

### Anti-Corruption Layer
**Where:** `adapter/postgres` error translation · **PLANNED** for payment gateway

Repositories translate `*pgconn.PgError` into `apperr` kinds. A service never
sees a driver type. Same discipline applies to Razorpay's response shapes: they
get mapped at the adapter, so a provider changing its JSON does not ripple into
domain logic.

---

## 2. Domain patterns

| Pattern | Where | Note |
|---|---|---|
| **Aggregate Root** | `ordering/domain.Order` | Order owns its Lines. Nothing mutates a Line except through the Order — that is what makes `RecalculateTotals` authoritative. |
| **Entity** | `identity/domain.User`, `Session` | Identity by ID, mutable over time. |
| **Value Object** | `identity/domain.Phone`, `shared/money.Amount` | Immutable, identity by value, self-validating. |
| **Repository** | `domain.*Repository` interfaces | Collection-like persistence abstraction. Declared by the domain, implemented by the adapter. |
| **Domain Event** | `contracts/events.go` | Past-tense facts. `order.placed`, not `place_order`. |
| **Domain Service** | **PLANNED** `ordering/app.CheckoutService` | Logic spanning aggregates that belongs to no single one. |
| **Specification** | `domain.UserFilter`, `ordering.Filter` | Query criteria as data, translated to SQL by the adapter. |
| **Factory** | `money.FromMinor`, `domain.NewPhone` | Construction with validation. The only way to obtain the type. |

### Value Object, specifically
`Phone` is a distinct type, not a `string`. This makes an unvalidated number
*unrepresentable* — the only constructor validates and normalises. It also
eliminated a real bug class: `"9876543210"`, `"+919876543210"` and
`"09876543210"` registering as three separate accounts.

### State Machine as Data
**Where:** `ordering/domain/order.go` — the `transitions` map

Every legal transition in one map. An illegal move is impossible by
construction. Scattered `if` statements eventually produce an order that is both
`delivered` and `cancelled`, with no way to reconstruct how.

---

## 3. Data patterns

### Transactional Outbox
**Where:** `platform/eventbus/outbox.go` · `outbox_events` table

**Problem:** "insert the order, then publish the event" is two systems with no
atomicity. Crash between them → an order nobody was told about, or a
notification for an order that rolled back. In production this reads as
*"customer charged but no order."*

**Solution:** the event is a row written in the same transaction. All-or-nothing.

**Consequence:** delivery becomes at-least-once, so **every handler must be
idempotent.**

### Competing Consumers
**Where:** `Relay.drainBatch` — `FOR UPDATE SKIP LOCKED`

Every replica runs a relay. Each claims a disjoint row set with no coordination,
no leader election, and no event delivered twice concurrently.

### Unit of Work
**Where:** `platform/database.DB.InTx`

The transaction lives in the `context`; `db.Q(ctx)` resolves to the ambient tx or
the pool. A repository never knows whether it is in a transaction, so a use case
can wrap five repositories from three modules in one commit without any of them
taking a `*pgx.Tx`.

### Optimistic Locking
**Where:** `version` columns (migration 0005)

Two staff open the same order on two tablets. Both read version 3. One marks it
preparing, the other cancelled. Last-write-wins loses one action *silently*.
With `AND version = $expected`, the second writer matches zero rows and gets a
conflict — the user is told instead of lied to.

**vs. pessimistic (`FOR UPDATE`):** row locks serialise writers inside one short
transaction (used for stock). Optimistic locking covers the long window where a
human holds stale data on screen — you cannot hold a DB lock for the 90 seconds
someone spends deciding.

### Event Sourcing (partial)
**Where:** `order_status_events`, `stock_movements`, `ledger_entries`

Append-only ledgers alongside current-state tables. Not full event sourcing —
current state is stored, not replayed. The ledger exists because *"we should have
4kg, the shelf has 1kg"* and *"who cancelled this order and why"* must be
answerable months later.

`ingredients.on_hand` is a **cached projection** of `stock_movements`. Always
re-derivable, which is what makes reconciliation possible.

### Keyset (Cursor) Pagination
**Where:** `shared/paging`

`OFFSET 50000` makes Postgres walk and discard 50k rows, and concurrent inserts
cause rows to be skipped or duplicated across pages. Row-value comparison
`(created_at, id) < ($1, $2)` gives a total order — O(log n) per page, stable
under writes.

### Snapshot / Denormalisation
**Where:** `orders.delivery_*`, `order_lines.name`, `menu_items.rating_average`

Order-time prices and addresses are **copied, not joined**. A price change
tomorrow must not alter yesterday's invoice — correctness *and* a GST
record-keeping requirement.

Rating averages are denormalised because a menu listing must not `AVG()` over
every review.

### Soft Delete
**Where:** `users.status = 'deleted'`

Orders reference users and financial records must stay intact. Every query
filters `status <> 'deleted'`.

### Multi-Tenancy: Shared Schema + Discriminator
**Where:** `platform/tenant`, `merchants` / `kitchens.merchant_id`

One database, `merchant_id` on every owned row. Schema-per-tenant was rejected:
hundreds of restaurants means hundreds of schemas to migrate in lockstep.

The `Scope` type is the enforcement — it has only unexported fields and named
constructors, so a repository cannot fabricate one. Authorisation is carried by
the type system, and it **fails closed**: an unscoped non-platform Scope emits
`FALSE`, returning nothing rather than everything.

---

## 4. Resilience patterns

**Where:** `platform/resilience` · all built and tested

| Pattern | Solves |
|---|---|
| **Circuit Breaker** | Stop calling a known-broken dependency. Failing in 1µs beats failing in 30s. |
| **Retry + Exponential Backoff + Jitter** | Transient blips. Jitter is not optional: without it every client retries at the same instant and re-flattens a recovering service. |
| **Bulkhead** | Cap concurrency per dependency. A slow SMS provider gets ≤N goroutines and cannot starve checkout. |
| **Timeout** | Bound every outbound call. An unbounded call is a held connection. |
| **Fail Fast** | Open circuit rejects without invoking. |
| **Fail Open** | Rate limiter allows traffic if Redis is down. Losing rate limiting beats losing the service. |
| **Graceful Degradation** | Cache miss on Redis outage → hit origin. Slow beats broken. |
| **Dead Letter Queue** | `outbox_events.status = 'dead_letter'`. A failed `payment.captured` is money; it needs a human, not silent deletion. |

### Composition order matters
```
bulkhead → breaker → timeout → retry → the call
```
1. Bulkhead outermost — reject a flood before it consumes anything.
2. Breaker next — a known-down dependency fails in microseconds.
3. Timeout **inside** the breaker — a hang becomes a countable failure that can
   actually trip it. Outside, hangs would never be recorded.
4. Retry innermost — each attempt gets a fresh timeout, rather than all attempts
   sharing one deadline the first slow call already consumed.

### Saga with Compensation
**PLANNED** — `ordering/app.Checkout`

Today checkout is one local ACID transaction, which is strictly better than a
saga and should stay that way as long as it can. Once `payment` or `inventory`
becomes a separate service, an atomic transaction is impossible and each step
needs a compensating action: release stock, release the coupon redemption, void
the payment intent. `contracts.StockReserver.Release` and
`DiscountCalculator.ReleaseRedemption` already exist for exactly that.

---

## 5. Caching patterns

| Pattern | Where |
|---|---|
| **Cache-Aside** | `cache.GetOrLoad` |
| **Stampede Protection (singleflight)** | `cache/loader.go` — of N concurrent misses on one key, exactly one runs the loader |
| **TTL Jitter** | ±10%, so keys populated together do not expire together |
| **Explicit Invalidation** | `Loader.Invalidate` on write. A price change must be visible immediately, not after a TTL |
| **Negative Caching** | **PLANNED** — cache "not found" too, or a 404-scanner hits the DB every time |

**The stampede scenario:** a popular menu's cache key expires at 12:30, mid lunch
rush. 2,000 concurrent requests miss and all run the same expensive query. The
DB saturates and the pool exhausts — at the busiest moment of the day.
singleflight collapses N queries to 1 per process.

---

## 6. API / transport patterns

| Pattern | Where |
|---|---|
| **Response Envelope** | `httpx.Envelope` — `{data}` or `{error:{code,message,fields,trace_id}}`. Frontend writes one client, one error handler. |
| **DTO / Projection** | `adapter/http` request+response structs. Separate from domain entities so adding an internal field cannot leak it — that is how password hashes end up in API responses. |
| **Idempotency Key** | `httpx.Idempotency` + `idempotency_keys`. Mobile clients retry; without this a retried checkout is a second charge. |
| **Middleware Chain** | `server.routes()`. Order is deliberate and documented at the call site. |
| **Rate Limiting** | Fixed window on Redis, keyed user-before-IP (carrier NAT would otherwise throttle a whole city). |
| **API Versioning** | `/api/v1` from day one. Shipped mobile apps cannot be forced to update. |
| **Health vs Readiness** | `/health` = process alive (no deps). `/ready` = deps reachable. Conflating them makes a Redis blip restart healthy pods. |
| **Graceful Shutdown** | Drain in-flight → stop relay → close pools. SIGTERM-aware. |
| **BFF** | **PLANNED** — Next.js route handlers for cookie handling and webhook proxying. |

### Idempotency contract
- Same key + same body → replay the stored response verbatim.
- Same key + **different** body → 422. A client bug; silently serving the old
  response would hide it while producing a baffling result.
- Key in flight → 409, retry shortly.
- Only 2xx is stored. Caching a 500 would make a transient failure permanent.

---

## 7. Security patterns

| Pattern | Where | Note |
|---|---|---|
| **Stateless access + revocable refresh** | `platform/token` | JWT needs no DB round trip; refresh must be revocable, which stateless cannot be. |
| **Refresh Rotation + Reuse Detection** | `app.AuthService.Refresh` | Replay of a rotated token = duplicate-or-theft, indistinguishable. Revoke every session. A stolen token becomes one extra login, not permanent silent access. |
| **Token Hashing** | SHA-256 of refresh tokens | A DB leak yields no usable sessions. Plain SHA-256 is correct here — 256 bits of entropy has nothing to brute-force, and a fast hash allows an indexed lookup. |
| **Constant-Time Comparison** | `password.DummyHash`, `CompareOTP` | A missing user must cost the same ~250ms as a wrong password, or response timing enumerates registered numbers. |
| **Algorithm Pinning** | `VerifyAccessToken` | Pinned to HS256. Without it, `alg=none` bypasses verification entirely. |
| **Least Privilege** | `httpx.RequireRole` + `tenant.Scope` | |
| **Fail Closed** | `tenant.Scope.SQL` returns `FALSE` when unscoped | A bug shows an empty dashboard (obvious, reported) not the whole platform's data (silent). |
| **Defence in Depth** | App validation **and** DB `CHECK` constraints | |
| **Audit Log** | `audit_log` (migration 0005) | Actor role snapshotted — a later promotion must not rewrite history. |
| **Webhook Signature Verification** | **PLANNED** `payment` | Non-negotiable: an unverified webhook endpoint lets anyone mark any order paid. |
| **IDOR Prevention** | `Scope.RequireOutlet` | Returns "not found", never "not yours" — confirming an id exists but belongs to someone else lets an attacker enumerate merchants. |

---

## 8. Observability patterns

| Pattern | Where |
|---|---|
| **Structured Logging** | `platform/logger` — `slog`, JSON in prod |
| **Correlation ID** | `httpx.RequestID` — propagates inbound `X-Request-ID` across API → worker |
| **Distributed Tracing** | `platform/tracing` — OTel, W3C `traceparent`, `ParentBased` sampling keeps traces whole |
| **RED metrics** | Rate, Errors, Duration per route |
| **USE metrics** | Pool utilisation, bulkhead occupancy, in-flight requests |
| **Business metrics** | `ck_business_orders_total`. A deploy that breaks checkout often shows a healthy 200 rate while orders quietly stop. |
| **Bounded Cardinality** | Route *patterns* as labels, never concrete paths. `/orders/{id}` not `/orders/abc-123`. |
| **Queue Depth + Age** | `outbox_pending_events` and `oldest_pending_age_seconds`. 10 events stuck an hour is worse than 10,000 that arrived a second ago. |

**The single most important gauge:** `ck_outbox_pending_events`. If the relay
stalls, the API stays perfectly healthy while every downstream side effect
silently stops. Alert on sustained growth.

---

## 9. Concurrency patterns

| Pattern | Where |
|---|---|
| **Context Propagation** | Every function takes `ctx` first |
| **Worker Pool** | Relay batch dispatch |
| **Semaphore** | `resilience.Bulkhead` — buffered channel |
| **Distributed Lock** | `platform/lock` — Redis `SETNX` + ownership token |
| **Singleflight** | `cache/loader.go` |
| **Pessimistic Row Lock** | `FindByIDForUpdate`, stock reservation |
| **Serializable + Retry** | `db.RetrySerializable` — for invariants over rows that *do not exist yet* ("first 100 orders only"), which row locks cannot cover |
| **Graceful Cancellation** | Detached contexts for rollback/release, so a cancelled request still cleans up |

### Lock ownership token
`Lock.Release` verifies the token before deleting. If our TTL lapsed mid-work,
another holder owns the key — an unconditional `DEL` would release *their* mutex
and let a third party into the critical section.

**Honest limitation:** documented in `lock.go` — a single-instance Redis lock is
not safe across failover. Fine for "one replica runs the nightly job"; **not**
fine for anything moving money. Use a DB transaction there.

---

## 10. Anti-patterns deliberately avoided

| Anti-pattern | Why rejected |
|---|---|
| **Float money** | `0.1+0.2 != 0.3`. Test `TestFloatWouldBeWrong` documents it. |
| **Anemic domain model** | Rules live on entities (`CanCustomerCancel`, `TransitionTo`), not in a service layer of getters/setters. |
| **Offset pagination** | See keyset above. |
| **Shared mutable global state** | Private Prometheus registry, no package-level vars holding services. |
| **Fat framework** | chi is `net/http`. Gin/Fiber own your handler signature. |
| **God module / shared "common"** | `shared/` is 5 tiny packages, guarded by boundary checks. |
| **Distributed monolith** | Modules communicate by events, not synchronous chains. |
| **Chatty N+1** | Repositories batch; `ANY($1)` over arrays keeps the plan cached. |
| **Silent failure** | `RowsAffected() == 0` → `NotFound`, never a silent success. |
| **Leaky abstraction** | `pgconn.PgError` never escapes the adapter. |
| **Premature microservices** | See §1. |
| **`SELECT *`** | Explicit `userColumns` const shared across reads, so a new column cannot be added to one query and forgotten in another. |
| **Postgres `ENUM`** | Cannot remove a value; adding one historically needed an exclusive lock. `CHECK` constraints instead. |
| **Timestamps without zone** | `TIMESTAMPTZ` always. `TIMESTAMP` silently reinterprets under a server tz change. |

---

## 11. Still missing

Honest list. Nothing here is built.

**Before real traffic**
- Integration tests against real Postgres (testcontainers) — CI has the services;
  the tests don't exist yet
- HTTP-level handler tests (`httptest`)
- Webhook signature verification (`payment`)
- Seed/fixture data for local dev
- OpenAPI spec + generated TypeScript client

**Before scale**
- Read replica routing for analytics
- Negative caching
- Load shedding / adaptive concurrency
- Partitioning `orders` and `outbox_events` by month
- Outbox archival job

**Before onboarding merchants**
- Row-Level Security as a second layer behind `tenant.Scope`
- Per-merchant rate limits
- Settlement reconciliation job against gateway statements
- Merchant-facing API keys + webhooks

**Operational**
- Alert rules (the metrics exist; nothing alerts on them)
- Runbooks (dead-letter drain, stuck settlement, breaker stuck open)
- Backup/restore and PITR verification — *untested backups are not backups*
- Load test establishing actual capacity
