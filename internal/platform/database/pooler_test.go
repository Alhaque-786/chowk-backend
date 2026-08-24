package database

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

/*
TLS handling, tested against the values pgx actually produces rather than the
values the sslmode names suggest. Measured with pgx v5.10:

	(no sslmode)         TLSConfig set, InsecureSkipVerify=true
	sslmode=disable      TLSConfig nil
	sslmode=prefer       TLSConfig set, InsecureSkipVerify=true
	sslmode=require      TLSConfig set, InsecureSkipVerify=true
	sslmode=verify-full  TLSConfig set, InsecureSkipVerify=false

The row that matters is `require`: it means encrypted, NOT verified.
*/

func TestPlaintextToRemoteHostIsRefused(t *testing.T) {
	err := requireTLSToRemoteHost("db.aaqznhtaohltycisemlr.supabase.co", nil)
	if err == nil {
		t.Fatal("sslmode=disable to a hosted database must be refused; the password " +
			"and every row would cross the internet in clear text")
	}
	for _, want := range []string{"clear text", "sslmode=require", "verify-full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %v", want, err)
		}
	}
}

func TestPlaintextToLoopbackIsAllowed(t *testing.T) {
	// The local container has no certificates and no network to intercept.
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "/tmp", ""} {
		if err := requireTLSToRemoteHost(host, nil); err != nil {
			t.Errorf("host %q should be allowed without TLS: %v", host, err)
		}
	}
}

func TestEncryptedRemoteHostIsAllowed(t *testing.T) {
	if err := requireTLSToRemoteHost("db.example.supabase.co", &tls.Config{}); err != nil {
		t.Errorf("TLS-enabled remote connection should be allowed: %v", err)
	}
}

func TestUnverifiedTLSIsFlaggedButNotFatal(t *testing.T) {
	/*
		`require` is the trap. It sets InsecureSkipVerify, so any host that can answer
		for the name may present its own certificate and read or rewrite the whole
		session. It is a WARNING rather than a refusal because Supabase signs its
		Postgres certificates with its own CA — verified by handshake: the chain is
		self-signed and does not validate against the system trust store — so
		verify-full needs their CA file, and refusing without it would block a working
		deployment on a setting the operator may not be able to satisfy yet.
	*/
	if !unverifiedTLS("db.example.supabase.co", &tls.Config{InsecureSkipVerify: true}) {
		t.Error("sslmode=require to a remote host must be flagged as unverified")
	}
	if unverifiedTLS("db.example.supabase.co", &tls.Config{InsecureSkipVerify: false}) {
		t.Error("verify-full must NOT be flagged")
	}
	// Local development must stay quiet, or the warning becomes noise nobody reads.
	if unverifiedTLS("localhost", &tls.Config{InsecureSkipVerify: true}) {
		t.Error("loopback must not be flagged")
	}
	if unverifiedTLS("db.example.supabase.co", nil) {
		t.Error("a nil TLSConfig is the refusal case, not the warning case")
	}
}

func TestSslmodeValuesProduceTheExpectedTLSConfig(t *testing.T) {
	// Pins pgx's actual behaviour, so a driver upgrade that changes what `require`
	// means shows up here rather than in production.
	cases := []struct {
		suffix       string
		wantTLS      bool
		wantInsecure bool
	}{
		{"", true, true},
		{"?sslmode=disable", false, false},
		{"?sslmode=prefer", true, true},
		{"?sslmode=require", true, true},
		{"?sslmode=verify-full", true, false},
	}
	for _, c := range cases {
		cfg, err := pgxpool.ParseConfig("postgres://u:p@db.example.supabase.co:5432/postgres" + c.suffix)
		if err != nil {
			t.Fatalf("%q: %v", c.suffix, err)
		}
		gotTLS := cfg.ConnConfig.TLSConfig != nil
		if gotTLS != c.wantTLS {
			t.Errorf("%q: TLSConfig present = %v, want %v", c.suffix, gotTLS, c.wantTLS)
			continue
		}
		if gotTLS && cfg.ConnConfig.TLSConfig.InsecureSkipVerify != c.wantInsecure {
			t.Errorf("%q: InsecureSkipVerify = %v, want %v",
				c.suffix, cfg.ConnConfig.TLSConfig.InsecureSkipVerify, c.wantInsecure)
		}
	}
}
