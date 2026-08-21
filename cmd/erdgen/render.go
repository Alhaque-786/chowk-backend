package main

import (
	"fmt"
	"sort"
	"strings"
)

// moduleOrder is the sequence sections appear in: the order flow first, because
// that is the path a reader actually follows, then the reactive modules, then
// infrastructure.
var moduleOrder = []struct {
	Name, Role string
}{
	{"identity", "Accounts, sessions, saved addresses. Phone is the primary credential; email is optional."},
	{"catalog", "Kitchens (outlets), opening hours, the menu tree — and the authoritative price of everything."},
	{"ordering", "The spine. The only module permitted to create an order, and the publisher of the `order.*` events every other module reacts to."},
	{"payment", "Gateway integration. The gateway is the only authority on whether money moved."},
	{"inventory", "Stock as an append-only ledger plus a cached balance. Solves the overselling problem."},
	{"delivery", "Deliberately thin: a per-drop adapter over a logistics provider, or the kitchen's own rider."},
	{"promotion", "Coupons and redemptions. Preview never consumes a use."},
	{"notification", "Subscribes to nine events; nothing subscribes to it."},
	{"marketplace", "Dormant today: one first-party merchant at 0% commission. Money movement is an immutable ledger."},
	{"platform", "Infrastructure owned by no business module."},
}

// growth annotates what makes each table grow. This is the input to any
// partitioning decision later, so it is worth stating explicitly rather than
// leaving a reader to infer it from column names.
var growth = map[string]string{
	"users": "signups", "sessions": "logins", "addresses": "signups",
	"kitchens": "outlets", "kitchen_hours": "outlets", "menu_categories": "menu size",
	"menu_items": "menu size", "modifier_groups": "menu size", "modifiers": "menu size",
	"menu_item_modifier_groups": "menu size",
	"orders":                    "**every order**", "order_lines": "**every order**",
	"order_status_events": "**every transition**",
	"payments":            "**every prepaid order**", "refunds": "disputes",
	"webhook_events": "**every gateway event**",
	"ingredients":    "pantry size", "recipes": "menu size",
	"stock_movements": "**every cook**", "stock_reservations": "**every order**",
	"purchase_orders": "procurement", "purchase_order_lines": "procurement",
	"deliveries": "**every order**",
	"coupons":    "campaigns", "coupon_redemptions": "coupon usage",
	"notification_templates": "copy changes", "notification_log": "**every message**",
	"device_tokens": "installs", "notification_preferences": "opt-outs",
	"merchants": "partners", "merchant_users": "partner staff",
	"ledger_entries": "**every delivered order**", "settlements": "payout cycles",
	"merchant_payout_holds": "investigations",
	"outbox_events":         "**every state change**", "idempotency_keys": "**every write**",
	"audit_log": "admin actions", "schema_migrations": "migrations",
}

func render(s *schema) string {
	var b strings.Builder
	w := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	structural, actorRefs, internal := classify(s)

	partial, unique := 0, 0
	for _, ixs := range s.Indexes {
		for _, ix := range ixs {
			if ix.Partial {
				partial++
			}
			if ix.Unique {
				unique++
			}
		}
	}

	w("# Entity-Relationship Diagram")
	w("")
	w("<!-- GENERATED FILE. Run `make erd` to regenerate. Prose lives in cmd/erdgen. -->")
	w("")
	w("Read from the live catalog at migration `%s`. Tables, columns, foreign keys,", s.Migration)
	w("delete behaviour and index counts come from `information_schema` and")
	w("`pg_constraint` — never from memory, so this file cannot quietly drift from the")
	w("database it describes.")
	w("")
	w("**%d tables · %d foreign keys · %d indexes (%d unique, %d partial) · %d modules**",
		len(s.Columns), len(s.FKs), countIndexes(s), unique, partial, len(moduleOrder))
	w("")
	w("---")
	w("")
	w("## How to read this")
	w("")
	w("Every table has exactly one **owning module**. Nothing outside a module writes to")
	w("its tables; cross-module reads go through an interface in `internal/contracts`.")
	w("`make check-boundaries` fails the build if that is violated, which is what makes")
	w("the boundary a compile-time fact rather than a convention.")
	w("")
	w("Diagrams are split per module. One diagram of %d tables and %d edges is a picture", len(s.Columns), len(s.FKs))
	w("of a schema, not a map of one — so the edges that leave a module are listed")
	w("instead, under [Cross-module seams](#cross-module-seams). Those are the cut lines")
	w("a service extraction would follow.")
	w("")
	w("Each module diagram shows primary keys, foreign keys and the first few business")
	w("columns. The [complete column reference](#complete-column-reference) has all %d.", countColumns(s))
	w("")
	w("### Conventions")
	w("")
	w("| Convention | Reason |")
	w("|---|---|")
	w("| `uuid` primary keys, UUIDv7 generated in Go | Time-ordered, so inserts append to the right edge of the index instead of scattering across it |")
	w("| Money as `bigint` in paise | Floats drift: `0.1 + 0.2 != 0.3`. A ledger off by a paise per order is unreconcilable |")
	w("| Quantities as `numeric(14,3)` | Ingredient amounts are genuinely fractional, with no natural smallest unit across flour, milk and eggs |")
	w("| `timestamptz` everywhere | `timestamp` silently reinterprets under a server timezone change, corrupting every order history |")
	w("| `CHECK` constraints, not `ENUM` | A value cannot be removed from a Postgres enum; a check is a one-line migration |")
	w("| %d of %d indexes are partial | History costs nothing to skip when the index only covers the hot subset |", partial, countIndexes(s))
	w("| Soft delete on `users` | Orders reference them and financial records must survive account closure |")
	w("")
	w("---")
	w("")
	w("## Module dependencies")
	w("")
	w("Solid arrows are synchronous calls through a `contracts` interface. Dashed arrows")
	w("are events — the seams that survive a service split, because the publisher does")
	w("not know who listens.")
	w("")
	w("```mermaid")
	w("flowchart LR")
	w("  ord[ordering]")
	w("  cat[catalog]")
	w("  inv[inventory]")
	w("  pay[payment]")
	w("  promo[promotion]")
	w("  del[delivery]")
	w("  ident[identity]")
	w("  notif[notification]")
	w("  market[marketplace]")
	w("")
	w("  ord -->|PriceQuoter| cat")
	w("  ord -->|KitchenAvailability| cat")
	w("  ord -->|StockReserver| inv")
	w("  ord -->|DiscountCalculator| promo")
	w("  ord -->|PaymentInitiator| pay")
	w("  ord -->|RiderDispatcher| del")
	w("  ord -->|UserDirectory| ident")
	w("  pay -->|UserDirectory| ident")
	w("  notif -->|UserDirectory| ident")
	w("")
	w("  ord -.->|order.placed| notif")
	w("  ord -.->|order.preparing| inv")
	w("  ord -.->|order.delivered| market")
	w("  pay -.->|payment.captured| ord")
	w("  pay -.->|payment.failed| notif")
	w("  inv -.->|menu_item.sold_out| cat")
	w("  inv -.->|stock.low| notif")
	w("```")
	w("")
	w("Note the shape: **ordering depends on six modules, and nothing depends on")
	w("ordering except through events.** That asymmetry is deliberate — the spine")
	w("orchestrates, and everything downstream reacts.")
	w("")
	w("---")
	w("")
	w("## Order lifecycle")
	w("")
	w("The state machine is declared as data in `ordering/domain/order.go`, so an")
	w("illegal transition is impossible by construction rather than prevented by")
	w("scattered `if` statements. `orders.status` carries a `CHECK` constraint listing")
	w("the same states.")
	w("")
	w("```mermaid")
	w("stateDiagram-v2")
	w("  [*] --> draft")
	w("  draft --> pending_payment : checkout")
	w("  pending_payment --> placed : payment.captured / COD")
	w("  pending_payment --> cancelled : payment.failed, reaper")
	w("  placed --> confirmed : kitchen accepts")
	w("  placed --> rejected : kitchen refuses")
	w("  confirmed --> preparing : cooking starts (stock consumed)")
	w("  preparing --> ready : packed")
	w("  ready --> picked_up : rider collects")
	w("  picked_up --> delivered")
	w("  placed --> cancelled")
	w("  confirmed --> cancelled")
	w("  preparing --> cancelled : support only")
	w("  ready --> cancelled : support only")
	w("  delivered --> refunded")
	w("  cancelled --> refunded")
	w("  rejected --> refunded")
	w("  delivered --> [*]")
	w("  refunded --> [*]")
	w("```")
	w("")
	w("Two transitions that are absent on purpose:")
	w("")
	w("- **`picked_up` cannot be cancelled.** The food is with the rider; a failed")
	w("  delivery is resolved as a refund on the delivered order, not by rewinding.")
	w("- **`pending_payment` cannot reach `confirmed`.** Money must land before the")
	w("  kitchen sees the order, or a failed UPI payment means food cooked for free.")
	w("")
	w("---")
	w("")

	// ---- per-module sections ----
	for _, mod := range moduleOrder {
		tables := tablesFor(s, mod.Name)
		if len(tables) == 0 {
			continue
		}

		w("## `%s`", mod.Name)
		w("")
		w("%s", mod.Role)
		w("")
		w("```mermaid")
		w("erDiagram")
		for _, t := range tables {
			w("  %s {", t)
			shown := 0
			for _, c := range s.Columns[t] {
				tag := ""
				if contains(s.PK[t], c.Name) {
					tag = "PK"
				} else if s.FKColumns[t][c.Name] {
					tag = "FK"
				}
				if tag == "" && shown >= 4 {
					continue
				}
				w("    %s %s %s", mermaidType(c.Type), c.Name, tag)
				shown++
			}
			w("  }")
		}
		for _, f := range s.FKs {
			if owner[f.Child] != mod.Name || owner[f.Parent] != mod.Name {
				continue
			}
			card := "||--o{"
			if s.OneToOne[f.Child+"."+f.Column] {
				card = "||--o|"
			}
			w("  %s %s %s : \"%s\"", f.Parent, card, f.Child, f.Column)
		}
		w("```")
		w("")
		w("| Table | Cols | Indexes | Grows with |")
		w("|---|---|---|---|")
		for _, t := range tables {
			u, p := 0, 0
			for _, ix := range s.Indexes[t] {
				if ix.Unique {
					u++
				}
				if ix.Partial {
					p++
				}
			}
			detail := fmt.Sprintf("%d", len(s.Indexes[t]))
			var bits []string
			if u > 0 {
				bits = append(bits, fmt.Sprintf("%d unique", u))
			}
			if p > 0 {
				bits = append(bits, fmt.Sprintf("%d partial", p))
			}
			if len(bits) > 0 {
				detail += " (" + strings.Join(bits, ", ") + ")"
			}
			g := growth[t]
			if g == "" {
				g = "—"
			}
			w("| `%s` | %d | %s | %s |", t, len(s.Columns[t]), detail, g)
		}
		w("")
	}

	// ---- seams ----
	w("---")
	w("")
	w("## Cross-module seams")
	w("")
	w("Of %d foreign keys, **%d are structural seams** — a real data dependency crossing", len(s.FKs), len(structural))
	w("a module boundary. Extracting a module means replacing each with an API call, and")
	w("the delete behaviour is the guarantee that extraction must preserve.")
	w("")
	w("| Child | → Parent | Column | On delete | Cardinality | Crosses |")
	w("|---|---|---|---|---|---|")
	sort.Slice(structural, func(i, j int) bool {
		if owner[structural[i].Child] != owner[structural[j].Child] {
			return owner[structural[i].Child] < owner[structural[j].Child]
		}
		return structural[i].Child+structural[i].Column < structural[j].Child+structural[j].Column
	})
	for _, f := range structural {
		// A total unique index on the FK column makes the relationship 1:1, and
		// that is load-bearing information: `deliveries.order_id` being unique is
		// what stops a second rider being booked and paid for one drop.
		card := "many-to-one"
		if s.OneToOne[f.Child+"."+f.Column] {
			card = "**one-to-one**"
		}
		w("| `%s` | `%s` | `%s` | `%s` | %s | %s → %s |",
			f.Child, f.Parent, f.Column, f.OnDelete, card, owner[f.Child], owner[f.Parent])
	}
	w("")
	w("Three of those are **one-to-one**, enforced by a total unique index rather than")
	w("by application logic: `deliveries.order_id` (a second booking means paying twice")
	w("for one drop), `coupon_redemptions.order_id` (makes redemption idempotent under a")
	w("retried checkout), and `notification_preferences.user_id`.")
	w("")
	w("### Why the count is lower than it looks")
	w("")
	w("A further **%d foreign keys point at `users` with `ON DELETE SET NULL`** —", len(actorRefs))
	w("audit columns (`actor_id`, `created_by`, `cancelled_by`) recording *who* acted.")
	w("Deliberately not counted as seams:")
	w("")
	w("- No data dependency. The row is fully meaningful with the reference nulled.")
	w("- Users are soft-deleted, so the `SET NULL` path never fires in practice.")
	w("- After a split they become an opaque id, not a join. Nothing breaks.")
	w("")
	w("The remaining **%d are internal to one module** — the ratio the architecture", len(internal))
	w("aims for: relationships stay inside a bounded context, and the ones that escape")
	w("are few enough to fit on one screen.")
	w("")
	w("### The seams that carry a lesson")
	w("")
	w("**`stock_reservations.order_id → orders` (CASCADE).** Caught a real bug: checkout")
	w("reserved stock *before* inserting the order, so every checkout failed on this")
	w("constraint. A mocked `StockReserver` has no foreign key, so no amount of unit")
	w("testing would have found it — only a real database and a real order did.")
	w("")
	w("**`orders.payment_id → payments` (SET NULL) with `payments.order_id → orders`")
	w("(RESTRICT).** A deliberate cycle: an order points at its current payment, the")
	w("payment points back. One direction must be broken before either row can be")
	w("deleted — exactly the friction financial records should have.")
	w("")
	w("**`recipes.menu_item_id → menu_items` (CASCADE).** The one place a module reaches")
	w("another's table through a foreign key rather than an interface. A bill of")
	w("materials has no meaning without its dish, so the coupling is intrinsic rather")
	w("than accidental.")
	w("")
	w("**`kitchens.merchant_id → merchants` (RESTRICT), NOT NULL.** Mandatory on")
	w("purpose. An outlet with no merchant is unscopeable data, which is precisely the")
	w("leak the tenant guard exists to prevent.")
	w("")
	w("---")
	w("")

	// ---- concurrency ----
	w("## Where concurrency is decided")
	w("")
	w("The non-obvious part of the schema. Each mechanism was chosen because the")
	w("cheaper option is *wrong* for that specific invariant — not for consistency.")
	w("")
	w("| Invariant | Table | Mechanism | Why not something cheaper |")
	w("|---|---|---|---|")
	w("| Two customers cannot buy the last portion | `ingredients` | `FOR UPDATE` `ORDER BY id` | The ordering is load-bearing: without it two checkouts locking rice and chicken in opposite orders deadlock, and checkout fails intermittently under exactly the load where it must not |")
	w("| A capped coupon cannot be over-redeemed | `coupon_redemptions` | `SERIALIZABLE` + retry | `COUNT(*) < 100` is a predicate over rows that do not exist yet, so no row lock protects it. Locking the coupon row would serialise every checkout behind one lock during the flash sale the cap exists for |")
	w("| A retried checkout cannot double-charge | `idempotency_keys` | `INSERT … ON CONFLICT` | Read-then-write has a window where two concurrent retries both see \"absent\" — the exact double order the mechanism prevents. The primary key arbitrates |")
	w("| A redelivered webhook cannot double-capture | `webhook_events` | `UNIQUE (provider, provider_event_id)` | Gateways retry aggressively. The index makes redelivery a no-op instead of a second capture, and the raw body is retained so a processing bug can be replayed |")
	w("| A redelivered event cannot double-send an SMS | `notification_log` | `UNIQUE (dedupe_key)` | Keyed on event + subject + channel, not event id: a redelivery carries the same id, but a genuinely new event for the same order must not be suppressed |")
	w("| Two staff editing one order cannot silently overwrite | `orders`, `menu_items`, `ingredients`, `kitchens` | `version` column | A row lock cannot span the ninety seconds a human spends deciding. Optimistic locking turns a lost update into a visible conflict, so the loser is told rather than ignored |")
	w("| Every replica can drain the outbox safely | `outbox_events` | `FOR UPDATE SKIP LOCKED` | Each relay claims a disjoint set with no coordination and no leader election. Nothing is delivered twice concurrently |")
	w("| One redemption per order | `coupon_redemptions` | `UNIQUE (order_id)` | Makes redemption idempotent under a retried checkout without any application check |")
	w("| One delivery per order | `deliveries` | `UNIQUE (order_id)` | A second booking means paying twice for one drop |")
	w("| The charged total equals its parts | `orders` | `CHECK` constraint | Arithmetic divergence between cart preview and charged amount is invisible in testing. The database catches it at write time, not in a reconciliation report a month later |")
	w("")
	w("---")
	w("")

	// ---- complete column reference ----
	w("## Complete column reference")
	w("")
	w("All %d columns. `PK` primary key · `FK` foreign key · `NN` not null.", countColumns(s))
	w("")
	for _, mod := range moduleOrder {
		tables := tablesFor(s, mod.Name)
		if len(tables) == 0 {
			continue
		}
		w("### %s", mod.Name)
		w("")
		for _, t := range tables {
			w("<details>")
			w("<summary><code>%s</code> — %d columns</summary>", t, len(s.Columns[t]))
			w("")
			w("| Column | Type | | References |")
			w("|---|---|---|---|")
			for _, c := range s.Columns[t] {
				flags := []string{}
				if contains(s.PK[t], c.Name) {
					flags = append(flags, "PK")
				}
				if s.FKColumns[t][c.Name] {
					flags = append(flags, "FK")
				}
				if c.NotNull {
					flags = append(flags, "NN")
				}
				ref := ""
				for _, f := range s.FKs {
					if f.Child == t && f.Column == c.Name {
						ref = fmt.Sprintf("`%s` `%s`", f.Parent, f.OnDelete)
						break
					}
				}
				w("| `%s` | %s | %s | %s |", c.Name, c.Type, strings.Join(flags, " "), ref)
			}
			w("")
			w("</details>")
			w("")
		}
	}

	w("---")
	w("")
	w("## Known gaps")
	w("")
	w("What this schema does not yet handle, ordered by when it starts to hurt.")
	w("")
	w("| Gap | Becomes a problem when |")
	w("|---|---|")
	w("| No archival for processed `outbox_events` | Soonest. The partial index keeps reads fast, but the table grows without bound |")
	w("| `orders` and `outbox_events` unpartitioned | Around 50M rows. `created_at` is already the natural monthly key, so it is a migration rather than a redesign |")
	w("| Row-Level Security not enabled | Third-party merchants onboard. The application tenant guard is the first layer; RLS is the one that survives a forgotten `WHERE` |")
	w("| Bounding-box geo search, not PostGIS | Past roughly 10k outlets, or when delivery zones stop being circles — a river makes somewhere 2 km away unreachable |")
	w("| Trigram search, not full-text | Past roughly 100k rows, or when relevance ranking matters more than matching |")
	w("| No read-replica routing | Month-end reports begin contending with checkout |")
	w("| `RiderAssignedPayload` carries no `user_id` | Blocks the \"your rider is here\" notification; the fix belongs in `contracts` |")
	w("")

	return b.String()
}

func tablesFor(s *schema, mod string) []string {
	var out []string
	for t := range s.Columns {
		if owner[t] == mod {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func countColumns(s *schema) int {
	n := 0
	for _, c := range s.Columns {
		n += len(c)
	}
	return n
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// mermaidType sanitises a Postgres type for mermaid's attribute grammar, which
// rejects brackets and spaces.
func mermaidType(t string) string {
	r := strings.NewReplacer(" ", "_", "[", "", "]", "", "(", "_", ")", "", ",", "_")
	return r.Replace(t)
}
