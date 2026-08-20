# Algorithms & Scaling Strategy

The algorithmic decisions in a food-delivery platform, each with the naive
version, the version to actually ship, and the trigger for upgrading.

The recurring principle: **ship the simplest thing that is correct, and know in
advance what replaces it.** Premature optimisation of rider assignment when you
have four riders is wasted work; having no plan when you have four hundred is
worse.

---

## 1. "Kitchens near me" — geospatial search

**Problem:** given a customer at (lat, lng), find open kitchens that deliver
there, ordered by distance.

### Stage 1 — bounding box + Haversine (ship this)
**Status:** index exists — `kitchens_location_idx (latitude, longitude)`

```
1. Compute a lat/lng box around the customer:
      Δlat = radius_km / 111.0                       -- 1° lat ≈ 111 km everywhere
      Δlng = radius_km / (111.0 × cos(latitude))     -- longitude narrows toward poles
2. Index scan: WHERE latitude BETWEEN ... AND longitude BETWEEN ...
3. Filter exactly in Go with Haversine, discarding box corners
4. Sort by distance
```

The `cos(latitude)` correction is the part people get wrong. At Mumbai's latitude
(19°N) a degree of longitude is ~105 km, not 111. Omitting it makes the box 6%
too narrow east-west and silently drops kitchens at the edge of the radius.

**Complexity:** O(log n + k). Fine to ~10k kitchens.

### Stage 2 — PostGIS + GiST
**Trigger:** > 10k outlets, or when you need true polygon delivery zones rather
than a radius.

```sql
CREATE EXTENSION postgis;
ALTER TABLE kitchens ADD COLUMN location geography(Point, 4326);
CREATE INDEX kitchens_geo_idx ON kitchens USING GIST (location);

SELECT * FROM kitchens
WHERE ST_DWithin(location, ST_MakePoint($lng,$lat)::geography, delivery_radius_m)
ORDER BY location <-> ST_MakePoint($lng,$lat)::geography
LIMIT 20;
```

`<->` is a KNN index-assisted distance operator — genuinely fast, not a sort over
a filtered set. Polygons matter because real delivery zones are not circles: a
river or a highway makes somewhere 2 km away unreachable.

### Stage 3 — road distance / isochrones
**Trigger:** delivery cost accuracy becomes a margin problem.

Straight-line distance under-estimates real travel by 20–40% in dense cities.
Cache OSRM/Mapbox matrix results in Redis keyed by rounded coordinate pairs —
routing APIs are billed per call, so an uncached lookup on every menu view is a
line item you will notice.

**Do not start here.** Stage 1 is correct enough to launch.

---

## 2. Rider assignment

**Problem:** N ready orders, M available riders. Assign to minimise delivery time.

This is an **assignment problem** and it is where the real algorithmic depth is.

### Stage 1 — greedy nearest available (ship this)
```
for each ready order (oldest first):
    candidates = riders within 5 km, idle, not at capacity
    pick argmin(distance + current_load × penalty)
```
O(N×M). Correct, explainable, debuggable. With < 50 riders it is within a few
percent of optimal, and "why did rider X get this order" has a one-sentence
answer — which matters when a rider complains.

### Stage 2 — batched Hungarian algorithm
**Trigger:** > 50 concurrent riders, or measurable idle-rider time.

Greedy is locally optimal and globally poor: assigning rider A to the nearest
order can force rider B into a much worse one. Batch every 30 seconds and solve
the whole matrix optimally.

**Hungarian (Kuhn–Munkres):** O(n³) — for n=100 that is ~10⁶ operations, a few
milliseconds. Entirely affordable.

```
cost[i][j] = pickup_distance(rider_i, order_j)
           + w1 × order_j.waiting_time      -- fairness: don't starve old orders
           + w2 × rider_i.active_deliveries -- load balance
           + w3 × direction_mismatch        -- prefer riders already heading that way
```
The 30-second batching window is the trade: slightly delayed assignment for
materially better global assignment.

### Stage 3 — order batching (multi-drop)
**Trigger:** high order density in a small radius.

Two orders from the same kitchen going 400 m apart should be one trip. This is a
capacitated vehicle-routing problem — NP-hard, so use a heuristic:

```
cluster by (kitchen, destination proximity, ready-time window)
accept a batch only if:
    detour_time < 8 min AND both orders stay within their promised ETA
```
Constrain by promise, not by efficiency. Batching that makes one customer late to
save a rider five minutes costs more in refunds and churn than it saves.

---

## 3. ETA prediction

**Problem:** the number shown at checkout. Under-promise and customers leave;
over-promise and they get angry. It is a trust metric, not a display field.

### Stage 1 — additive with a percentile buffer (ship this)
```
ETA = kitchen_queue_delay        -- pending orders × avg prep / parallel capacity
    + max(item.prep_minutes)     -- MAX not SUM; a kitchen cooks in parallel
    + rider_pickup_time
    + travel_time
    + P75 buffer
```
Two details that matter:
- **MAX, not SUM.** A biryani (25 min) and a coke (0 min) is 25 minutes, not 25.
  Summing prep times inflates every multi-item ETA.
- **P75, not mean.** Optimising for the average means being late half the time.
  Quote a percentile you can actually hit.

`metrics.OrderPrepDuration` already records real prep time per kitchen — feed the
observed P75 back in rather than trusting the configured `avg_prep_minutes`.

### Stage 2 — gradient-boosted regression
**Trigger:** enough delivered orders (~50k+) to train on.

Features: kitchen, hour-of-week, item count, current queue depth, rider supply,
weather, rain flag, distance, historical kitchen P75.

Train on `confirmed_at → delivered_at`. Optimise **quantile loss at P75**, not
MSE — you are predicting a promise you must keep, not a central tendency.

Serve from the `analytics` module; fall back to Stage 1 whenever the model is
unavailable. An ETA is on the checkout path, and a model outage must not become a
checkout outage.

---

## 4. Menu ranking

**Problem:** which dishes appear first.

### Stage 1 — explicit `display_order` (ship this)
The kitchen decides. Starters before biryani. Alphabetical would be wrong, and
"most popular" on day one is noise over a handful of orders.

### Stage 2 — popularity with time decay
```
score = w1 × normalized_order_count_30d
      + w2 × (rating_average − 3.0)      -- centred; 3★ is neutral, not positive
      + w3 × margin_bps                  -- push profitable items
      + w4 × recency_boost               -- new items need exposure to be discovered
      − w5 × sold_out_frequency          -- stop promoting what runs out
```
`order_count` is denormalised on `menu_items` precisely so this never aggregates
`order_lines` on a page view.

**Cold-start:** a new dish has no orders, so pure popularity ranking buries it
forever and it never gets the exposure needed to earn a rank. The recency boost
is not a nicety — without it the menu ossifies.

### Stage 3 — personalised
Collaborative filtering on (user, item) order history; blend with the global
score. Precompute nightly into a table. Never compute at request time.

**Diversity constraint:** cap items per category in the top N, or the whole first
screen is biryani and a customer who wants something else sees nothing.

---

## 5. Inventory depletion — BOM explosion

**Problem:** given a cart, how much of each ingredient is consumed, and is there
enough?

```
for each order_line:
    for each recipe row of line.menu_item:
        needed = recipe.quantity
               × line.quantity
               × (1 + recipe.wastage_bps/10000)
        required[ingredient] += needed        -- AGGREGATE across lines first
```

**Aggregate before checking.** Two biryanis on two separate lines both need rice;
checking each line independently passes twice against the same stock and
oversells. This is the single most common inventory bug.

**Then, in one transaction:**
```sql
SELECT id, on_hand, reserved FROM ingredients
WHERE id = ANY($1) ORDER BY id            -- ORDER BY prevents deadlock
FOR UPDATE;
```
`ORDER BY id` is load-bearing. Two concurrent orders locking the same ingredients
in different orders deadlock; a consistent order makes one simply wait.

**Servings remaining** (drives automatic sold-out):
```
servings = MIN over required ingredients of
             floor((on_hand − reserved) / qty_per_serving)
           -- skipping is_optional ingredients: a missing garnish must not
           -- make the dish unorderable
```

---

## 6. Rate limiting

| Stage | Algorithm | Trade-off |
|---|---|---|
| **1 (shipped)** | Fixed window, Redis `INCR` + `EXPIRE NX` | One atomic op, correct across replicas. Allows a 2× burst at a window boundary. |
| **2** | Sliding window log (sorted set) | Exact, but O(log n) memory per key — expensive at millions of keys. |
| **3** | Token bucket in a Lua script | Smooth, allows controlled bursts, one round trip. The right long-term answer. |

**Keying:** user id when authenticated, IP otherwise. Behind carrier NAT
(Jio/Airtel) thousands of users share one IP, so IP-only throttles a whole city
because one person was abusive.

**Fails open.** A Redis outage allows traffic through. Losing rate limiting is
far better than losing the service.

---

## 7. Search

| Stage | Approach | Trigger |
|---|---|---|
| **1 (shipped)** | `pg_trgm` GIN index, `ILIKE '%term%'` | Good to ~100k rows. Handles typos reasonably. |
| **2** | Postgres full-text (`tsvector` + weighted `ts_rank`) | Need relevance ranking, stemming, multi-field weighting |
| **3** | OpenSearch / Typesense | Need fuzzy matching, synonyms ("veg" → "vegetarian"), per-user boosting, faceting |

Do not reach for Elasticsearch first. It is a second datastore to keep in sync,
and sync bugs produce a search index that confidently returns deleted dishes.

---

## 8. Idempotency & concurrency control

| Problem | Mechanism | Why |
|---|---|---|
| Retried checkout | `INSERT ... ON CONFLICT DO NOTHING` on `idempotency_keys` | The DB is the mutex. Read-then-write has a race window that produces the exact double order it was meant to prevent. |
| Duplicate webhook | Unique `(provider, provider_event_id)` | Gateways retry aggressively; the index makes redelivery a no-op instead of a double capture. |
| Concurrent stock | `SELECT … FOR UPDATE ORDER BY id` | Serialises writers; ordered locking avoids deadlock. |
| Stale UI writes | `version` column | Row locks cannot span the 90 seconds a human spends deciding. |
| Global coupon cap | `SERIALIZABLE` + retry | The invariant is over rows that do not exist yet; row locks cannot cover that. |
| Duplicate ledger entry | Unique `(order_id, entry_type)` | Makes at-least-once event delivery safe for money. |
| One replica runs a job | Redis lock with ownership token | No row to lock. |

---

## 9. The scaling ladder

Each rung, with its trigger. **Do not climb early** — every rung adds operational
surface, and the rung you add before you need it is the one you maintain for
nothing.

| Rung | Trigger | Action |
|---|---|---|
| 0 | today | 1 API + 1 worker + Postgres + Redis |
| 1 | CPU > 60% | Horizontally scale the API. Already stateless; `SKIP LOCKED` makes the relay replica-safe. |
| 2 | read latency rises | Read replica. Route analytics and reports there — a month-end report must never contend with checkout. |
| 3 | cache misses hurt | Redis Cluster; per-tenant cache keys |
| 4 | `orders` > ~50M rows | Partition `orders` and `outbox_events` by month. `created_at` is already the natural key. Archive cold partitions. |
| 5 | one module dominates load | Extract it as a service. The seams already exist: swap the injected `contracts` implementation for a gRPC client. Start with `notification` — lowest risk, no transactional coupling. |
| 6 | in-process events insufficient | Point the outbox relay at Kafka/NATS. Publishing modules do not change. |
| 7 | multi-city | Shard by city. Orders never cross city boundaries, so it is a clean shard key — the rare property that makes sharding tractable. |

### Which module to extract first, and last
- **First: `notification`.** Purely reactive, no transactional coupling, an outage
  is degradation not failure.
- **Then `analytics`.** Read-only, already wants its own replica.
- **Last: `ordering` + `inventory`.** They share a transaction boundary. Splitting
  them turns one ACID checkout into a saga with compensations — a genuine loss of
  correctness guarantees. Only do it when a single Postgres truly cannot cope,
  and expect it to be the hardest migration you do.

---

## 10. Capacity reality check

Back-of-envelope, so the ladder above is grounded in numbers rather than vibes.

```
1,000 orders/day
  ≈ 42/hour average, but food traffic is violently peaked:
  ~40% of the day's orders land in two lunch and dinner hours
  → peak ≈ 200 orders/hour ≈ 3.3/minute

Each order ≈ 30 API calls (browse, menu, cart edits, checkout, 20× status poll)
  → peak ≈ 100 req/min ≈ 2 req/s
```

**2 requests per second.** A single Go process handles that with roughly 1% of one
core. One Postgres instance on modest hardware handles hundreds of times this.

The honest conclusion: **for the first year, correctness and observability matter
far more than performance.** The architecture is built for the seams and the
guarantees, not because 2 req/s is hard. Do not let the scaling ladder tempt you
into climbing it before the traffic exists.

Replace status polling with SSE or WebSockets before anything else, though — 20
polls per order is two-thirds of all traffic, and it is pure waste.
