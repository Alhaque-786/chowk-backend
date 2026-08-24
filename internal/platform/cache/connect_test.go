package cache

import (
	"strings"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/config"
)

/*
Connection-string handling, tested because it cannot be exercised locally.

Neither branch below is reachable from a laptop with a local container: a TLS URL
needs a hosted provider, and the plaintext refusal only triggers against a
non-loopback host. Without these, "does the app work against Upstash" is answered
by deploying and reading a crash.
*/

func TestLocalConfigNeedsNoURL(t *testing.T) {
	opts, err := options(config.RedisConfig{Addr: "localhost:6379", DB: 0})
	if err != nil {
		t.Fatalf("plain local config rejected: %v", err)
	}
	if opts.Addr != "localhost:6379" {
		t.Errorf("addr = %q", opts.Addr)
	}
	if opts.TLSConfig != nil {
		t.Error("local development should not require TLS")
	}
	if opts.PoolSize == 0 || opts.DialTimeout == 0 {
		t.Error("timeouts and pool size must be applied on this branch too; they were " +
			"previously set inline in Connect and are easy to lose when refactoring")
	}
}

func TestRedissURLEnablesTLS(t *testing.T) {
	// The shape Upstash hands you. TLS must come from the SCHEME, with no separate
	// boolean that could disagree with it.
	const url = "rediss://default:someSecret@nearby-mongoose-12345.upstash.io:6379"

	opts, err := options(config.RedisConfig{URL: url})
	if err != nil {
		t.Fatalf("rediss:// rejected: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("rediss:// must produce a TLS config; without it the password goes in the clear")
	}
	if opts.Password != "someSecret" {
		t.Errorf("password not parsed from the URL")
	}
	if opts.Addr != "nearby-mongoose-12345.upstash.io:6379" {
		t.Errorf("addr = %q", opts.Addr)
	}
	if opts.DialTimeout == 0 {
		t.Error("timeouts must be applied to the URL branch as well")
	}
}

func TestURLWinsOverDiscreteFields(t *testing.T) {
	// Both set is a realistic accident: REDIS_ADDR left in a .env from local work
	// while REDIS_URL is added for staging. One must clearly win, or the app
	// connects somewhere nobody intended.
	opts, err := options(config.RedisConfig{
		Addr:     "localhost:6379",
		Password: "stale-local-value",
		URL:      "rediss://default:real@host.upstash.io:6379",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Addr != "host.upstash.io:6379" || opts.Password != "real" {
		t.Errorf("URL must take precedence; got addr=%q password=%q", opts.Addr, opts.Password)
	}
}

func TestPlaintextToRemoteHostIsRefused(t *testing.T) {
	_, err := options(config.RedisConfig{URL: "redis://default:secret@host.upstash.io:6379"})
	if err == nil {
		t.Fatal("plaintext redis:// to a remote host must be refused; the password " +
			"is sent in the clear on the first command")
	}
	// And the refusal must not quote the credential back into a log line.
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error message leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "rediss://") {
		t.Errorf("error should say what to do instead: %v", err)
	}
}

func TestPlaintextToLoopbackIsAllowed(t *testing.T) {
	// The local container. Requiring certificates here would make development
	// worse for no security gain — there is no network to intercept.
	for _, url := range []string{
		"redis://localhost:6379",
		"redis://127.0.0.1:6379",
		"redis://[::1]:6379",
	} {
		if _, err := options(config.RedisConfig{URL: url}); err != nil {
			t.Errorf("%s should be allowed: %v", url, err)
		}
	}
}

func TestMalformedURLDoesNotLeakTheSecret(t *testing.T) {
	_, err := options(config.RedisConfig{URL: "not-a-url://default:topsecret@host:6379"})
	if err == nil {
		t.Fatal("a malformed URL must be rejected")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Errorf("parse error echoes the connection string, including the password: %v", err)
	}
}
