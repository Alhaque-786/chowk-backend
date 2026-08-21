// Command erdgen writes docs/ERD.md from the LIVE database catalog.
//
// The point of generating rather than hand-writing it: a schema document written
// from memory is wrong within a week and nobody notices, because nothing checks
// it. This reads information_schema and pg_constraint, so the tables, columns,
// foreign keys, delete behaviour and index counts cannot drift from reality.
//
// The prose -- why a delete rule is what it is, which concurrency mechanism
// guards which invariant -- is hand-written and lives in narrative.go. That part
// is judgement and cannot be derived from the catalog.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

// module ownership. Every table belongs to exactly one, and this map is the
// source of truth for the boundary the architecture claims to have.
var owner = map[string]string{
	"users": "identity", "sessions": "identity", "addresses": "identity",

	"kitchens": "catalog", "kitchen_hours": "catalog", "menu_categories": "catalog",
	"menu_items": "catalog", "modifier_groups": "catalog", "modifiers": "catalog",
	"menu_item_modifier_groups": "catalog",

	"orders": "ordering", "order_lines": "ordering", "order_status_events": "ordering",

	"payments": "payment", "refunds": "payment", "webhook_events": "payment",

	"ingredients": "inventory", "recipes": "inventory", "stock_movements": "inventory",
	"stock_reservations": "inventory", "purchase_orders": "inventory",
	"purchase_order_lines": "inventory",

	"deliveries": "delivery",

	"coupons": "promotion", "coupon_redemptions": "promotion",

	"notification_templates": "notification", "notification_log": "notification",
	"device_tokens": "notification", "notification_preferences": "notification",

	"merchants": "marketplace", "merchant_users": "marketplace",
	"ledger_entries": "marketplace", "settlements": "marketplace",
	"merchant_payout_holds": "marketplace",

	"outbox_events": "platform", "idempotency_keys": "platform",
	"audit_log": "platform", "schema_migrations": "platform",
}

type column struct {
	Name, Type string
	NotNull    bool
}

type fk struct {
	Child, Column, Parent, OnDelete string
}

type index struct {
	Name    string
	Unique  bool
	Partial bool
}

type schema struct {
	Columns   map[string][]column
	PK        map[string][]string
	FKs       []fk
	Indexes   map[string][]index
	Migration string
	FKColumns map[string]map[string]bool
	// oneToOne holds FKs whose column has a TOTAL unique index. A PARTIAL unique
	// index does not count: `UNIQUE (user_id) WHERE is_default` permits many
	// addresses per user, so reading it as 1:1 would draw the wrong cardinality.
	OneToOne map[string]bool
}

func main() {
	_ = godotenv.Load()

	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		fatal("POSTGRES_DSN is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fatal("connect: %v", err)
	}
	defer conn.Close(ctx)

	s, err := load(ctx, conn)
	if err != nil {
		fatal("%v", err)
	}

	out := render(s)
	if err := os.WriteFile("docs/ERD.md", []byte(out), 0o644); err != nil {
		fatal("write docs/ERD.md: %v", err)
	}

	structural, actor, internal := classify(s)
	fmt.Printf("docs/ERD.md: %d tables, %d FKs (%d structural seams, %d audit refs, %d internal), %d indexes\n",
		len(s.Columns), len(s.FKs), len(structural), len(actor), len(internal), countIndexes(s))
}

func load(ctx context.Context, conn *pgx.Conn) (*schema, error) {
	s := &schema{
		Columns:   map[string][]column{},
		PK:        map[string][]string{},
		Indexes:   map[string][]index{},
		FKColumns: map[string]map[string]bool{},
		OneToOne:  map[string]bool{},
	}

	rows, err := conn.Query(ctx, `
		SELECT c.table_name, c.column_name,
		       CASE
		         WHEN c.data_type='timestamp with time zone' THEN 'timestamptz'
		         WHEN c.data_type='character varying'        THEN 'varchar'
		         WHEN c.data_type='time without time zone'   THEN 'time'
		         WHEN c.data_type='ARRAY'                    THEN ltrim(c.udt_name,'_')||'_array'
		         WHEN c.data_type='double precision'         THEN 'float8'
		         ELSE c.data_type
		       END,
		       c.is_nullable = 'NO'
		FROM information_schema.columns c
		JOIN pg_tables t ON t.tablename = c.table_name AND t.schemaname='public'
		WHERE c.table_schema='public'
		ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	for rows.Next() {
		var tbl string
		var col column
		if err := rows.Scan(&tbl, &col.Name, &col.Type, &col.NotNull); err != nil {
			return nil, err
		}
		s.Columns[tbl] = append(s.Columns[tbl], col)
	}
	rows.Close()

	if err := conn.QueryRow(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&s.Migration); err != nil {
		s.Migration = "unknown"
	}

	rows, err = conn.Query(ctx, `
		SELECT c.conrelid::regclass::text,
		       (SELECT string_agg(a.attname, ',' ORDER BY x.ord)
		        FROM unnest(c.conkey) WITH ORDINALITY x(att, ord)
		        JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=x.att)
		FROM pg_constraint c
		WHERE c.contype='p' AND c.connamespace='public'::regnamespace`)
	if err != nil {
		return nil, fmt.Errorf("read primary keys: %w", err)
	}
	for rows.Next() {
		var tbl, cols string
		if err := rows.Scan(&tbl, &cols); err != nil {
			return nil, err
		}
		s.PK[tbl] = strings.Split(cols, ",")
	}
	rows.Close()

	rows, err = conn.Query(ctx, `
		SELECT c.conrelid::regclass::text,
		       (SELECT string_agg(a.attname, ',')
		        FROM unnest(c.conkey) x(att)
		        JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=x.att),
		       c.confrelid::regclass::text,
		       CASE c.confdeltype WHEN 'c' THEN 'CASCADE' WHEN 'r' THEN 'RESTRICT'
		            WHEN 'n' THEN 'SET NULL' ELSE 'NO ACTION' END
		FROM pg_constraint c
		WHERE c.contype='f' AND c.connamespace='public'::regnamespace
		ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	for rows.Next() {
		var f fk
		if err := rows.Scan(&f.Child, &f.Column, &f.Parent, &f.OnDelete); err != nil {
			return nil, err
		}
		s.FKs = append(s.FKs, f)
		if s.FKColumns[f.Child] == nil {
			s.FKColumns[f.Child] = map[string]bool{}
		}
		s.FKColumns[f.Child][f.Column] = true
	}
	rows.Close()

	rows, err = conn.Query(ctx, `
		SELECT tablename, indexname,
		       indexdef LIKE '%UNIQUE%', indexdef LIKE '% WHERE %'
		FROM pg_indexes WHERE schemaname='public' ORDER BY tablename, indexname`)
	if err != nil {
		return nil, fmt.Errorf("read indexes: %w", err)
	}
	for rows.Next() {
		var tbl string
		var ix index
		if err := rows.Scan(&tbl, &ix.Name, &ix.Unique, &ix.Partial); err != nil {
			return nil, err
		}
		s.Indexes[tbl] = append(s.Indexes[tbl], ix)
	}
	rows.Close()

	// Resolve true 1:1 relationships: a TOTAL unique index whose only column is
	// the FK column. Asking Postgres directly rather than guessing from the index
	// name, and excluding partial indexes, which was the bug in the first pass.
	rows, err = conn.Query(ctx, `
		SELECT c.conrelid::regclass::text || '.' || a.attname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum = c.conkey[1]
		JOIN pg_index i ON i.indrelid = c.conrelid
		WHERE c.contype='f' AND c.connamespace='public'::regnamespace
		  AND array_length(c.conkey,1) = 1
		  AND i.indisunique
		  AND i.indpred IS NULL              -- NOT partial
		  AND i.indnatts = 1
		  AND i.indkey[0] = c.conkey[1]`)
	if err != nil {
		return nil, fmt.Errorf("read one-to-one: %w", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		s.OneToOne[key] = true
	}
	rows.Close()

	return s, nil
}

// classify splits FKs three ways. The distinction matters: an audit column
// pointing at users with SET NULL is not architectural coupling, and counting it
// as a seam overstates how tangled the schema is.
func classify(s *schema) (structural, actorRefs, internal []fk) {
	actorCols := map[string]bool{
		"actor_id": true, "created_by": true, "cancelled_by": true,
		"initiated_by": true, "placed_by": true, "released_by": true,
		"verified_by": true, "invited_by": true,
	}
	for _, f := range s.FKs {
		switch {
		case owner[f.Child] == owner[f.Parent]:
			internal = append(internal, f)
		case f.Parent == "users" && f.OnDelete == "SET NULL" && actorCols[f.Column]:
			actorRefs = append(actorRefs, f)
		default:
			structural = append(structural, f)
		}
	}
	return
}

func countIndexes(s *schema) int {
	n := 0
	for _, ix := range s.Indexes {
		n += len(ix)
	}
	return n
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "erdgen: "+format+"\n", args...)
	os.Exit(1)
}
