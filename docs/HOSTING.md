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
