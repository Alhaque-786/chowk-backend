package database

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

/*
The transaction-pooler guard.

Worth a test because the bug it prevents is invisible locally and hostile in
production: pgx caches prepared statements per connection, a transaction-mode
pooler reassigns connections per transaction, and the result is "prepared
statement does not exist" appearing only under concurrency, only sometimes, and
looking like database corruption. Every local test passes, because locally there
is no pooler.
*/

func TestSessionModePortsAreAccepted(t *testing.T) {
	for _, port := range []uint16{5432, 5433, 25060} {
		if err := rejectTransactionPooler(port, pgx.QueryExecModeCacheStatement); err != nil {
			t.Errorf("port %d should be accepted: %v", port, err)
		}
	}
}

func TestTransactionPoolerPortsAreRefused(t *testing.T) {
	// 6543 is Supabase's Supavisor transaction mode; 6432 is the PgBouncer
	// convention. Both are the wrong choice for a long-lived service.
	for _, port := range []uint16{6543, 6432} {
		err := rejectTransactionPooler(port, pgx.QueryExecModeCacheStatement)
		if err == nil {
			t.Fatalf("port %d is a transaction pooler and must be refused", port)
		}
		// The message has to name the fix, not just the problem. Somebody reads
		// this at the end of a deploy.
		for _, want := range []string{"5432", "default_query_exec_mode=exec", "prepared statement"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("port %d: error should mention %q; got: %v", port, want, err)
			}
		}
	}
}

func TestExplicitExecModeOptsOut(t *testing.T) {
	/*
		The escape hatch has to be real, or the comment describing it is a lie.
		`?default_query_exec_mode=exec` makes pgx send statements unprepared, which
		is precisely what makes transaction pooling safe — so somebody who set it
		has already made this decision and should not be blocked.
	*/
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		if err := rejectTransactionPooler(6543, mode); err != nil {
			t.Errorf("exec mode %v deliberately opts out and must be allowed: %v", mode, err)
		}
	}
}

func TestPreparedModesStillRefused(t *testing.T) {
	// The modes that DO prepare must still be caught. Only exec and simple
	// protocol avoid the statement cache.
	for _, mode := range []pgx.QueryExecMode{
		pgx.QueryExecModeCacheStatement,
		pgx.QueryExecModeCacheDescribe,
		pgx.QueryExecModeDescribeExec,
	} {
		if err := rejectTransactionPooler(6543, mode); err == nil {
			t.Errorf("mode %v prepares statements and must still be refused on a transaction pooler", mode)
		}
	}
}
