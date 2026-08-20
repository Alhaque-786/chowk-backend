# Chowk — Backend

Go 1.26 · PostgreSQL 16 · Redis 7 · chi

A modular-monolith API for **Chowk**, a food-ordering platform: customer
ordering, kitchen operations, inventory, and the admin dashboard.

**Chowk** is the platform. **Khansama** is the first kitchen on it — structurally
just a row in `merchants` with `is_first_party = true`, with no special-casing
anywhere in the code. See [`docs/MARKETPLACE.md`](docs/MARKETPLACE.md).

**Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before writing code.** It
explains why the structure is what it is; this file only tells you how to run it.

---

## Quick start

```bash
cp .env.example .env
# Set a real secret — the app refuses to boot with one under 32 chars:
#   openssl rand -base64 48

make up        # Postgres + Redis via Docker, waits for health
make migrate   # apply schema
make run       # API on http://localhost:8080
make worker    # outbox relay + maintenance (separate terminal)
```

Verify:

```bash
curl localhost:8080/health   # liveness  — process up
curl localhost:8080/ready    # readiness — dependencies reachable
```

---

## Layout

```
cmd/
  api/         HTTP server
  worker/      outbox relay + periodic jobs
  migrate/     migration runner
internal/
  config/      all env parsing. Nothing else calls os.Getenv.
  shared/      money, id, apperr, paging, clock — the shared vocabulary
  platform/    infrastructure: db, cache, eventbus, httpx, token, password
  contracts/   the inter-module API: events + ports
  modules/     ten bounded contexts
  server/      the composition root — the only place that sees all modules
migrations/    numbered .sql, immutable once applied
docs/
```

## Module status

| Module | State |
|---|---|
| `identity` | **Built** — auth, sessions, OTP. The reference implementation. |
| `ordering` | Domain + state machine built. Cart and checkout saga pending. |
| `catalog`, `inventory`, `payment`, `delivery`, `promotion`, `review`, `notification`, `analytics` | Boundary + `doc.go` only |

Each unbuilt module's `doc.go` records its responsibilities, its events, and the
domain-specific traps. Read it before starting one.

Schema exists for identity, catalog, ordering, payments, and inventory —
migrations `0001`–`0004`.

---

## Commands

```bash
make check              # fmt + vet + test -race. Run before pushing.
make check-boundaries   # fails if one module imports another
make test               # tests with the race detector
make migration name=add_wallet
make migrate-status
make psql
make reset              # destroy data, recreate, re-migrate
```

## Endpoints

Implemented today:

```
GET  /health
GET  /ready

POST /api/v1/auth/register
POST /api/v1/auth/login
POST /api/v1/auth/otp/request
POST /api/v1/auth/otp/verify
POST /api/v1/auth/refresh
POST /api/v1/auth/logout        (bearer token)
POST /api/v1/auth/logout-all    (bearer token)

GET  /api/v1/admin/events       (platform_admin — live event-handler wiring)
```

Every response uses one envelope, so the frontend needs exactly one API client
and one error handler:

```json
{ "data":  { ... } }
{ "error": { "code": "PHONE_ALREADY_REGISTERED",
             "message": "…", "fields": {…}, "trace_id": "…" } }
```

`code` is stable and safe to branch on. `message` is user-presentable.
`trace_id` appears in server logs — quote it in bug reports.

---

## Conventions that are not optional

- **Never import another module.** Cross-module needs go through
  `internal/contracts`. `make check-boundaries` enforces it.
- **Money is `int64` paise via `shared/money`.** Never `float`.
- **Applied migrations are immutable.** The runner checksums them and refuses to
  start if one was edited. Add a new migration instead.
- **Event handlers must be idempotent.** Outbox delivery is at-least-once.
- **New env var → add it to `internal/config` and `.env.example`.** Nothing
  outside `config` reads the environment.
