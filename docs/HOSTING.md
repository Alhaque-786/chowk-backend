# Hosting the datastores

Written down because two of the decisions below are counter-intuitive and one of
them is a trap that only fails in production.

## Region is the whole decision

This is an India-only product. Everything else is secondary to that.

Measured from a Lucknow connection:

| target | TCP connect (best of 3) |
| --- | --- |
| AWS Mumbai `ap-south-1` | **29 ms** |
| AWS Singapore `ap-southeast-1` | **97 ms** |

Roughly 3x. From a Mumbai-hosted API into a Mumbai datastore the figure is closer
to 1-2 ms, so the real comparison for a cross-region database is 1 ms against
~60 ms per round trip.

That matters here more than it would for most apps, because `CheckoutService`
runs about ten sequential statements inside ONE `SERIALIZABLE` transaction and
retries the whole transaction on a serialization failure. At 60 ms per round trip
that is ~600 ms of pure network per attempt, doubled on a retry, on the one
request in the app where the customer is watching a spinner and holding a card.

**So: `ap-south-1` for both, or self-hosted beside the API.**

## Postgres

**Neon was considered and rejected.** Not on features — Neon is real Postgres and
its branching model is genuinely good, a copy-on-write branch per pull request is
better than anything self-hosting offers. It has **no India region**; the nearest
is Singapore. The branching is not worth 60 ms on every statement of a checkout.

**Supabase, region South Asia (Mumbai) `ap-south-1`.** It is plain Postgres: a
normal connection string, `pgx`, raw SQL, `SERIALIZABLE`, `SELECT FOR UPDATE`,
and the same migrations. PostgREST, auth and storage are additional layers this
backend simply does not use. Free tier is 500 MB, which is a great deal of menu
and order data, and projects pause after a week of inactivity — fine for staging,
not for production.

### Use the session-mode port. This is the trap.

Supabase offers three ways in:

| | port | use |
| --- | --- | --- |
| Direct connection | 5432 | IPv6-only without the paid IPv4 add-on |
| Supavisor **session** mode | 5432 | **this one** — persistent clients, IPv4 |
| Supavisor **transaction** mode | 6543 | serverless functions only |

`pgx` uses the extended protocol and caches prepared statements per connection. A
transaction-mode pooler hands a different backend connection to each
*transaction*, so a statement prepared on one is unknown on the next. The symptom
is `prepared statement "lrupsc_1_0" does not exist`, appearing only under
concurrency, only sometimes, and looking like database corruption rather than a
config mistake. Every local test passes, because locally there is no pooler.

`database.Connect` refuses to boot on ports 6543 and 6432 for exactly this
reason, with the fix in the error message. The deliberate opt-out is
`?default_query_exec_mode=exec`, which tells pgx to stop preparing statements.

    POSTGRES_DSN=postgres://postgres.<ref>:<password>@aws-0-ap-south-1.pooler.supabase.com:5432/postgres?sslmode=require

## Redis

**Upstash, region `ap-south-1`.** TLS is mandatory, so use the `rediss://` URL:

    REDIS_URL=rediss://default:<token>@<name>.upstash.io:6379

### Watch the command count, not the storage

The free tier is 256 MB and **500,000 commands per month**. Storage is not the
constraint; the command count is. This backend touches Redis on *every* request
for rate limiting, plus cache reads and writes, so budget several commands per
request:

    500,000 / ~4 commands per request  ≈  125,000 requests/month  ≈  4,000/day

Ample for development. Nothing at all for launch.

### For production, self-host Redis beside the API

Contrary to the Postgres advice, and for two specific reasons:

1. **Latency lands on every request.** Rate limiting runs before the handler, so
   a Redis round trip is added to every call, including ones that never touch
   Postgres. A cache that is 60 ms away is slower than the query it replaces.
2. **Nothing in it is precious.** It holds cache entries, rate-limit counters,
   distributed locks and OTP throttles. Losing all of it costs a cold cache and
   reset counters. There is no durability requirement to pay someone else for —
   which is exactly the opposite of the orders table.

Managed Postgres, self-hosted Redis. The asymmetry is the point: pay for
durability, not for latency.

## Do not run a keep-alive cron

Tempting on a tier that sleeps when idle, and it backfires. Neon's free
allowance, as an example of the shape: 100 CU-hours/month, which their own docs
describe as 400 hours at 0.25 CU. A month is 730 hours, so staying awake
continuously exhausts the allowance around day 17 and compute is then suspended
until the next billing period — trading a one-second cold start for a fortnight
of downtime.

The mechanism does not matter, either. A GitHub Actions cron, an uptime pinger, a
goroutine in this service: all burn the same budget, because staying awake *is*
the cost. And a cold start only affects the first request after idle — with
traffic you are never idle, and without traffic nobody is waiting.

## Measured: where the time actually goes

Run against the live Supabase project in `ap-south-1` from a Lucknow connection,
and against the local container for reference. Client figures are the median of 7;
server figures come from `EXPLAIN (ANALYZE, BUFFERS, TIMING)`, which is the only
way to separate Postgres's own CPU from the trip to reach it.

### Opening a connection

| phase | Supabase Mumbai | local |
| --- | --- | --- |
| TCP handshake | 32.6 ms | 2.1 ms |
| Postgres `SSLRequest` + TLS | +58.1 ms | n/a |
| startup, SASL auth, parameters | +113.5 ms | +10.4 ms |
| **full new connection** | **202.9 ms** | **12.5 ms** |

Auth is the expensive part, not TLS. This is the number that makes the pool
worth having: `POSTGRES_MIN_CONNS=2` keeps two connections warm so a request
never pays 200 ms to say hello.

### Running a query

| query | client | server plan | server exec | network+protocol |
| --- | --- | --- | --- | --- |
| `SELECT 1` (floor) | 29.7 ms | — | — | 29.7 ms |
| indexed primary-key lookup | 30.8 ms | 0.07 ms | 0.03 ms | 30.7 ms (**100%**) |
| indexed FK lookup (menu by kitchen) | 29.8 ms | 0.08 ms | 0.04 ms | 29.7 ms (**100%**) |
| forced sequential scan | 29.3 ms | 0.08 ms | 0.11 ms | 29.1 ms (99%) |
| 5M-row aggregate (pure CPU) | 940.6 ms | 0.05 ms | 933.9 ms | 6.7 ms (1%) |

Same queries, local:

| query | client | server exec |
| --- | --- | --- |
| indexed primary-key lookup | 0.17 ms | 0.01 ms |
| indexed FK lookup | 0.22 ms | 0.01 ms |
| forced sequential scan | 0.21 ms | 0.02 ms |
| 5M-row aggregate | 417.8 ms | 431.5 ms |

Three conclusions, and the first is the one that matters:

1. **For ordinary queries the network is one hundred percent of the cost.**
   Supabase's execution time (0.03-0.11 ms) is indistinguishable from local. The
   database is not slow; the distance is.

2. **Indexed and sequential scans are the same speed from here** — 29.8 ms against
   29.3 ms. Not because the index is useless, but because 0.04 ms against 0.11 ms
   is invisible behind 29.7 ms of travel. The index will matter when the tables
   are large; it cannot help with latency.

3. **Free-tier CPU is roughly 2.2x slower than this laptop** on real work: 933.9 ms
   against 431.5 ms for the identical 5M-row aggregate. Irrelevant for OLTP, where
   nothing is CPU-bound, and worth knowing before running a report.

A methodology caveat, since one figure looks impossible: the local aggregate shows
NEGATIVE network overhead (-13.9 ms) because `EXPLAIN ANALYZE` adds per-row timing
instrumentation, so the measured server time exceeds the uninstrumented run. It
distorts long queries and is negligible for short ones.

### What this means for checkout

`CheckoutService` runs about ten statements inside one `SERIALIZABLE` transaction.
At 29.7 ms per round trip that is **~300 ms of pure network per attempt**, doubled
if the transaction retries on a serialization conflict — from a laptop in Lucknow.

From an API hosted in `ap-south-1` the same ten statements cost single-digit
milliseconds. This is the entire argument for co-locating the API with the
database, and it is why Singapore was rejected: it would have made this ~600 ms
instead.

### Server limits worth knowing

Read from the live project:

| setting | value | note |
| --- | --- | --- |
| `max_connections` | 60 | 3 reserved for superuser, 16 already in use |
| `shared_buffers` | 224 MB | |
| `work_mem` | 2184 kB | |
| `statement_timeout` | 2 min | a runaway query dies rather than hanging forever |
| `idle_in_transaction_session_timeout` | 0 | **disabled** — a leaked transaction holds its locks indefinitely |
| `default_transaction_isolation` | read committed | checkout sets SERIALIZABLE per transaction, so this is fine |

**`POSTGRES_MAX_CONNS=25` fits one instance and not two.** Two replicas would want
50 of the ~41 actually available, and the failure arrives as connection errors
under load rather than at startup. Lower it before scaling out horizontally.

`idle_in_transaction_session_timeout=0` is worth changing. A transaction left open
by a crashed handler holds its row locks until the connection dies, and with
`SERIALIZABLE` checkout that means every concurrent order on the same kitchen
blocks.

### Version skew

Supabase runs **PostgreSQL 17.6**. CI and docker-compose pin **16.4-alpine**.
Worth aligning: testing on 16 and deploying on 17 means the planner, and any
behaviour that changed between majors, is untested.
