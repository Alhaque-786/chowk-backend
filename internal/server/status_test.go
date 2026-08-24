package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatusReportsWhatIsDegraded covers the endpoint that exists because
// Guard.State() was documented as feeding a health endpoint and called from
// nowhere. The shape matters: an operator reading this at 2am needs to see WHICH
// dependency is tripped, not a boolean.
func TestStatusReportsWhatIsDegraded(t *testing.T) {
	h := harness(t)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/status returned %d, want 200 (is OBSERV_METRICS_ENABLED off?)", rec.Code)
	}

	// A probe served from a proxy cache is worse than no probe.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control is %q, want %q", got, "no-store")
	}

	// httpx.JSON wraps every response in a {"data": ...} envelope — the codebase's
	// convention, and easy to forget when hand-decoding in a test.
	var envelope struct {
		Data struct {
			Env   string `json:"env"`
			Ready struct {
				Ready  bool              `json:"ready"`
				Checks map[string]string `json:"checks"`
			} `json:"ready"`
			Breakers []struct {
				Name     string `json:"name"`
				State    string `json:"state"`
				InFlight int    `json:"in_flight"`
			} `json:"breakers"`
			Outbox struct {
				Pending    int64   `json:"pending"`
				DeadLetter int64   `json:"dead_letter"`
				LagSeconds float64 `json:"lag_seconds"`
			} `json:"outbox"`
			OutboxSampled bool             `json:"outbox_sampled"`
			DBPool        map[string]int32 `json:"db_pool"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode /status: %v", err)
	}
	body := envelope.Data

	for _, dep := range []string{"postgres", "redis"} {
		if _, ok := body.Ready.Checks[dep]; !ok {
			t.Errorf("/status does not report %q", dep)
		}
	}

	if body.DBPool["max"] == 0 {
		t.Error("db_pool.max is 0; pool stats are not being read")
	}

	/*
		A breaker is only registered when the LIVE gateway is built, and local config
		selects the mock (no credentials). So an empty list here is correct rather
		than a failure — but it must be an empty list, not a missing key, or a
		dashboard rendering this cannot tell "no breakers" from "field absent".
	*/
	if body.Breakers == nil {
		t.Error("breakers must serialise as [] rather than null, or a dashboard " +
			"cannot tell \"no breakers registered\" from \"field absent\"")
	}
	for _, b := range body.Breakers {
		switch b.State {
		case "closed", "half_open", "open":
		default:
			t.Errorf("breaker %q reports unknown state %q", b.Name, b.State)
		}
	}

	// The relay samples every 15s, so a freshly-built harness has almost certainly
	// not sampled yet. The flag must say so honestly: zeros from "never measured"
	// and zeros from "nothing pending" are different facts.
	if !body.OutboxSampled && (body.Outbox.Pending != 0 || body.Outbox.LagSeconds != 0) {
		t.Error("outbox reports values while claiming it has never sampled")
	}
}

// TestReadinessIsMemoised guards the amplification fix. /ready sits outside rate
// limiting by design, so without a cache each anonymous request costs a Postgres
// ping and a Redis ping — a public endpoint that loads the database it is reporting
// on, hardest at exactly the moment it is being polled hardest.
func TestReadinessIsMemoised(t *testing.T) {
	h := harness(t)

	first := readyBody(t, h)
	second := readyBody(t, h)

	// Within the TTL both must be byte-identical, which is only true if the second
	// call returned the memoised value rather than re-running the checks.
	if first != second {
		t.Errorf("two rapid /ready calls differ:\n  %s\n  %s", first, second)
	}
}

// TestReadinessNeverLeaksInternals: this endpoint is public and unauthenticated, so
// the response must name the failing dependency and nothing else. A driver error can
// carry a DSN, a hostname, or a pool internal.
func TestReadinessNeverLeaksInternals(t *testing.T) {
	h := harness(t)
	body := readyBody(t, h)

	for _, leak := range []string{"postgres://", "password", "5432", "6379", "localhost", "dial tcp"} {
		if contains(body, leak) {
			t.Errorf("/ready body contains %q, which should never be exposed:\n%s", leak, body)
		}
	}
}

func readyBody(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/ready returned %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("/ready Cache-Control is %q, want no-store", got)
	}
	return rec.Body.String()
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
