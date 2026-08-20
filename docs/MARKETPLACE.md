# Single-Tenant → Marketplace

The plan for going from "our own cloud kitchen" to "a platform other restaurants
sell on at a lower commission than Zomato/Swiggy."

**Today:** one first-party merchant, `commission_bps = 0`, marketplace machinery
present but dormant.

---

## Why this is already in the schema

Because tenant isolation is the one thing you cannot safely retrofit.

With one kitchen, a query missing `WHERE merchant_id = ?` returns slightly wrong
data — a bug. With paying partners, that same omission shows one restaurant its
competitor's order volume, margins, and customer list. It stops being a bug and
becomes a breach that ends the contract and probably the business.

The failure mode is what makes it dangerous: **nothing errors.** No exception, no
5xx, no alert. The data is just wrong, in a direction nobody notices until a
merchant does. And by then it has been wrong for months.

Adding the guard at 40 queries is nearly free. Adding it at 400 is a quarter of
engineering time and you will still miss one.

So migration 0006 exists now, and `platform/tenant` exists now, while the cost of
being careful is close to zero.

---

## The data model

```
merchant                      the business: contract, PAN, bank account, gets paid
  ├── outlet (kitchens row)   one physical location: own menu, own stock, own hours
  ├── outlet
  └── merchant_users          who may act for this merchant, and at which outlet
```

**Two levels, not one.** A brand with three locations needs ONE settlement
account and THREE independent inventories. Modelling outlets as separate
merchants breaks payouts; collapsing them into one breaks stock.

| Table | Purpose |
|---|---|
| `merchants` | Business entity, KYC, commission terms, payout details |
| `kitchens.merchant_id` | Every outlet has an owner — no unscopeable rows |
| `kitchens.is_first_party` | Separates our own margin from platform take-rate |
| `merchant_users` | Owner (all outlets) vs staff (one outlet) |
| `ledger_entries` | Immutable money movement |
| `settlements` | Batch payouts |
| `merchant_payout_holds` | Freeze money pending investigation |

---

## Tenant isolation

`platform/tenant.Scope` carries an authorisation decision in the type system.

```go
scope, err := tenant.FromPrincipal(principal, merchantID)
// platform_admin → Platform()          unrestricted
// support        → Platform()          cross-merchant reads, audit-logged
// kitchen_admin  → ForMerchant(id)     all of their outlets
// kitchen_staff  → ForOutlet(id)       one outlet only
// customer       → error               no tenant scope at all
```

Three properties make it hold:

1. **Unforgeable.** All fields unexported, only named constructors. A repository
   cannot fabricate a Scope, so it cannot run unscoped by accident.
2. **Fails closed.** An unscoped non-platform Scope emits `FALSE`, not `TRUE`. A
   bug shows a merchant an empty dashboard — obvious, immediately reported —
   rather than the whole platform's data.
3. **One filter implementation.** `Scope.SQL()` builds the predicate, so it cannot
   be subtly different in the one query someone wrote in a hurry.

**IDOR:** `Scope.RequireOutlet` returns *"not found"*, never *"not yours."*
Confirming an id exists but belongs to someone else lets an attacker enumerate
every merchant on the platform.

**Second layer, later:** Postgres Row-Level Security, so even a raw `psql` session
or a forgotten `Scope` cannot cross tenants. Defence in depth — but the
application guard comes first, because RLS is easy to bypass accidentally with a
superuser connection.

---

## The money model

This is the part that decides whether the business works.

### Ledger, not a balance column

```
order.delivered
   ↓
ledger_entries      (signed, from the MERCHANT's perspective)
   + order_revenue    gross owed to the merchant
   − commission       gross × merchant.commission_bps
   − gateway_fee      if the merchant absorbs it
   ↓
settlement job      weekly, under a distributed lock (one replica)
   SELECT entries WHERE settlement_id IS NULL
   → settlement row (arithmetic verified by CHECK constraint)
   → payout via provider
   → link entries to the settlement
```

**Balance = `SUM(amount_minor)`, never an `UPDATE`-able column.** A balance you
can update is a balance a bug can silently corrupt, with no way to reconstruct
when or why. A ledger of immutable entries can always be re-derived and
reconciled against the gateway's own statement.

**Idempotent by index:** `UNIQUE (order_id, entry_type)`. At-least-once event
delivery means `order.delivered` will sometimes arrive twice; the index makes the
second one a no-op instead of double-crediting a merchant.

**Signed single column** rather than separate debit/credit columns, so the balance
is a plain `SUM` with no `CASE` that a newly added `entry_type` could silently
fall outside of.

### Commission

Stored per merchant, in basis points, because it is negotiated per merchant.

```
Zomato / Swiggy:   ~18–30%
This platform:      12% default (1200 bps), negotiable per merchant
```

Both models supported from day one — `commission_bps` and
`commission_flat_minor` — so a merchant negotiating a flat per-order fee does not
require a schema change mid-conversation.

**Also a real lever: settlement speed.** The incumbents run 7–14 day cycles, and
slow payouts are one of the loudest merchant complaints. `settlement_cycle`
supports daily. For a restaurant with thin working capital, daily settlement is
worth more than two points of commission — and it costs you float, not margin.

### TDS

`settlements.tds_minor` — Indian tax deducted at source on marketplace payouts.
Not optional, and getting it wrong is the platform's liability, not the
merchant's.

---

## Onboarding

```
draft → documents_pending → under_review → active
                                  ↓
                              rejected
```

A merchant cannot reach `active` without FSSAI and verification — enforced by
`merchants_active_requires_kyc_check`, so no code path can skip it. Taking money
on behalf of an unverified food business is both a compliance problem and a fraud
vector.

Validated in the schema: GSTIN length 15, PAN format `AAAAA9999A`, FSSAI required
before going live. A malformed GSTIN discovered at settlement time is the worst
possible moment.

`merchants_fssai_expiry_idx` drives the compliance alert. An expired FSSAI means
the outlet must stop selling **immediately** — not at the end of the month.

---

## What still needs building

Ordered by when you will need it.

**Before the first partner**
- Merchant module: onboarding API, document upload, admin review queue
- Apply `tenant.Scope` to every merchant-facing repository method
- Merchant auth: `merchant_users` → `Principal.KitchenID` / merchant resolution
- Ledger writes on `order.delivered` / `order.refunded`
- Merchant dashboard: today's orders, payouts, menu, stock

**Before the fifth partner**
- Settlement job + payout provider integration (RazorpayX / Cashfree)
- Merchant-facing settlement statements (PDF)
- Per-merchant rate limits — one partner's integration bug must not degrade others
- Onboarding self-service flow

**Before scale**
- Postgres RLS as the second isolation layer
- Reconciliation job: ledger vs gateway statement, alert on drift
- Merchant API keys + outbound webhooks (their POS integration)
- Commission experiments (per-category, promotional rates)
- Merchant SLA tracking → `penalty` ledger entries

---

## Things that will hurt, stated plainly

**Tenant leakage is the existential risk.** Not downtime. One merchant seeing
another's data is unrecoverable reputationally. Every new merchant-facing query
needs a `Scope`, and that should be a review checklist item, not a hope.

**Payout bugs are the expensive risk.** Underpaying a merchant loses the account;
overpaying is money you will not get back. This is why the ledger is immutable and
why `settlements_arithmetic_check` exists in the database rather than only in Go.

**Commission is not the only competitive axis, and probably not the best one.**
Undercutting on rate alone invites a price war with companies that can absorb it
far longer than you can. Settlement speed, lower customer-facing delivery fees,
honest ETAs, and not running your own private label against your own partners are
all defensible in ways a percentage is not.

**First-party and third-party conflict is structural.** You run kitchens *and* a
marketplace those kitchens compete in. Zomato's private-label push was
sufficiently resented to matter commercially. `is_first_party` exists so ranking
and reporting can be audited for self-preferencing — decide the policy
deliberately, and be able to prove it.
