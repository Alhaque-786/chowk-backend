package database_test

import (
	"context"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
The two safety timeouts, asserted on a live connection.

Read from the live Supabase project: statement_timeout was 2min and
idle_in_transaction_session_timeout was 0 — DISABLED. The second is the dangerous
default: a transaction left open by a panicked handler holds its row locks until
something notices, and with SERIALIZABLE checkout every concurrent order for the
same kitchen queues behind it.

Set by the application as RuntimeParams so the same binary behaves identically
against a local container, Supabase and RDS. A managed provider's defaults are not
ours to assume, which is exactly why this is a test rather than a comment.
*/
func TestSafetyTimeoutsAreSetOnEveryConnection(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()

	for _, c := range []struct{ setting, want string }{
		{"idle_in_transaction_session_timeout", "30s"},
		{"statement_timeout", "1min"}, // Postgres normalises 60s
	} {
		var got string
		if err := db.Q(ctx).QueryRow(ctx, "SELECT current_setting($1)", c.setting).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", c.setting, err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q — an unset idle-in-transaction timeout lets a "+
				"leaked transaction hold its locks indefinitely", c.setting, got, c.want)
		}
	}
}
