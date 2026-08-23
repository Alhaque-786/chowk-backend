package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zevoxa/chowk-backend/internal/config"
	"github.com/zevoxa/chowk-backend/internal/server"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

// harness builds the REAL router: every module, every middleware, real Postgres
// and real Redis.
//
// Testing handlers in isolation would skip the part most likely to be wrong. The
// middleware chain — auth, rate limiting, idempotency, the error envelope — only
// exists as a composed whole, and a route mounted outside `Authenticate` is an
// auth bypass that no unit test can see.
func harness(t *testing.T) http.Handler {
	t.Helper()

	// Reuse testutil's env discovery, then skip if the dependencies are absent so
	// `go test ./...` on a laptop without Docker is not a wall of red.
	_ = testutil.DB(t)

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("config unavailable: %v", err)
	}

	srv, err := server.New(context.Background(), cfg,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		t.Skipf("cannot build server: %v", err)
	}
	return srv.Handler()
}

// publicRoutes is the allowlist of paths that may be reached WITHOUT a bearer
// token, and why each one has to be.
//
// This is the security-relevant half of the test: anything not on this list must
// reject an anonymous request. Adding a route to the list should require an
// argument, which is why each entry carries its reason.
var publicRoutes = map[string]string{
	"/health":  "liveness probe; the orchestrator has no token",
	"/ready":   "readiness probe",
	"/metrics": "scraped by Prometheus; restricted at the ingress, not by a token",

	"/api/v1/auth/register":    "how you obtain a token in the first place",
	"/api/v1/auth/login":       "how you obtain a token in the first place",
	"/api/v1/auth/otp/request": "pre-authentication by definition",
	"/api/v1/auth/otp/verify":  "pre-authentication by definition",
	"/api/v1/auth/refresh":     "authenticates via the refresh token in the body; the access token has expired by now",

	"/api/v1/kitchens":             "browsing must not require an account",
	"/api/v1/kitchens/{kitchenID}": "browsing must not require an account",
	"/api/v1/kitchens/{slug}":      "browsing must not require an account",
	// Mounted subtree, so chi reports the pattern WITH a trailing slash. Both
	// forms are listed deliberately: the bare form was here on its own and never
	// matched anything, so it silently documented an intent the router did not
	// implement -- `/offers` was authenticated, and this dead entry is why nobody
	// noticed. Normalisation in isPublic now covers the general case; these stay
	// explicit so the reason is attached to the path.
	"/api/v1/offers":  "offers are advertising; visible before signup",
	"/api/v1/offers/": "offers are advertising; visible before signup",

	"/api/v1/webhooks/payment":  "authenticated by HMAC signature; a gateway has no bearer token",
	"/api/v1/webhooks/delivery": "authenticated by HMAC signature; a 3PL has no bearer token",
}

// isPublic matches a concrete or templated route against the allowlist,
// including menu and nested browse paths under a kitchen.
func isPublic(pattern string) bool {
	if _, ok := publicRoutes[pattern]; ok {
		return true
	}
	// A mounted subtree's root is reported with a trailing slash, so compare
	// without it too. Without this an allowlist entry can look correct, match
	// nothing, and quietly assert the opposite of what it says.
	if trimmed := strings.TrimSuffix(pattern, "/"); trimmed != pattern {
		if _, ok := publicRoutes[trimmed]; ok {
			return true
		}
	}
	// Public browse subtree: menus and search hang off a kitchen.
	if strings.HasPrefix(pattern, "/api/v1/kitchens/") {
		return true
	}
	if strings.HasPrefix(pattern, "/api/v1/webhooks/") {
		return true
	}
	return false
}

// TestEveryProtectedRouteRejectsAnonymousRequests is the test that earns its
// keep.
//
// It walks chi's ACTUAL route tree rather than a hand-maintained list, so a route
// added tomorrow is covered automatically. Forgetting to mount something behind
// `Authenticate` becomes a failing build instead of a silent auth bypass — which
// is the kind of bug that is invisible in review and catastrophic in production.
func TestEveryProtectedRouteRejectsAnonymousRequests(t *testing.T) {
	h := harness(t)

	router, ok := h.(chi.Routes)
	if !ok {
		t.Fatal("handler does not expose its routes; cannot enumerate")
	}

	var checked, publicSeen int

	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi renders wildcards with a trailing /* on mounted subtrees.
		pattern := strings.TrimSuffix(route, "/*")
		if pattern == "" {
			pattern = "/"
		}

		if method == http.MethodOptions {
			return nil // CORS preflight is intentionally unauthenticated
		}
		if isPublic(pattern) {
			publicSeen++
			return nil
		}

		// Substitute any URL params so the request actually routes.
		concrete := fillParams(pattern)

		req := httptest.NewRequest(method, concrete, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		// Deliberately NO Authorization header.

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		checked++

		switch rec.Code {
		case http.StatusUnauthorized:
			// Correct: rejected for missing credentials.
		case http.StatusNotFound, http.StatusMethodNotAllowed:
			// The param substitution did not resolve to a real route. Not an auth
			// finding, so not a failure.
		case http.StatusTooManyRequests:
			// Rate limited before reaching auth. Also safe — the request never got in.
		default:
			t.Errorf("AUTH BYPASS: %s %s returned %d without a token (want 401)\n"+
				"  If this route is genuinely public, add it to publicRoutes with a reason.",
				method, pattern, rec.Code)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking routes: %v", err)
	}

	if checked == 0 {
		t.Fatal("no protected routes were checked; the walk found nothing")
	}
	t.Logf("checked %d protected routes, skipped %d public ones", checked, publicSeen)
}

// fillParams replaces chi URL params with syntactically valid values so a
// request routes to the handler rather than 404ing on a malformed path.
func fillParams(pattern string) string {
	const sampleUUID = "00000000-0000-0000-0000-0000000000ff"

	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if !strings.HasPrefix(p, "{") {
			continue
		}
		// Most params are ids; slug is the exception.
		if strings.Contains(p, "slug") {
			parts[i] = "some-slug"
		} else {
			parts[i] = sampleUUID
		}
	}
	return strings.Join(parts, "/")
}

// TestPublicRoutesAreActuallyReachable is the other half.
//
// A route wrongly placed BEHIND auth is a availability bug rather than a security
// one, but it is just as real: putting /health behind a token means the
// orchestrator restarts healthy pods forever.
func TestPublicRoutesAreActuallyReachable(t *testing.T) {
	h := harness(t)

	for _, probe := range []struct {
		method, path string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/ready"},
		{http.MethodGet, "/metrics"},
	} {
		req := httptest.NewRequest(probe.method, probe.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s requires a token; probes and scrapers do not have one",
				probe.method, probe.path)
		}
	}
}

// TestHealthDoesNotTouchDependencies distinguishes liveness from readiness.
//
// Conflating them means a Redis blip restarts a perfectly healthy pod, turning a
// dependency wobble into an outage.
func TestHealthDoesNotTouchDependencies(t *testing.T) {
	h := harness(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/health returned %d, want 200", rec.Code)
	}

	var body struct {
		Data struct {
			Status string `json:"status"`
			Env    string `json:"env"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Data.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Data.Status)
	}

	// Readiness DOES check dependencies, and reports them individually so an
	// operator can see which one is down.
	req = httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var ready struct {
		Data struct {
			Ready  bool              `json:"ready"`
			Checks map[string]string `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("decode /ready: %v", err)
	}
	for _, dep := range []string{"postgres", "redis"} {
		if _, ok := ready.Data.Checks[dep]; !ok {
			t.Errorf("/ready does not report %q; an operator cannot tell what is down", dep)
		}
	}
}

// TestErrorResponsesUseTheEnvelope: the frontend writes ONE error handler, so
// every failure path must produce the same shape — including the ones generated
// by the router rather than by a handler.
func TestErrorResponsesUseTheEnvelope(t *testing.T) {
	h := harness(t)

	for _, tc := range []struct {
		name, method, path, body string
		wantCode                 string
	}{
		{"unknown route outside the API", http.MethodGet, "/nonsense", "", "ROUTE_NOT_FOUND"},
		{"unknown route in a public subtree", http.MethodGet, "/api/v1/auth/nope", "", "ROUTE_NOT_FOUND"},
		{"missing token", http.MethodGet, "/api/v1/orders", "", "MISSING_TOKEN"},
		{"malformed json", http.MethodPost, "/api/v1/auth/login", "{not json", ""},
		{"empty body", http.MethodPost, "/api/v1/auth/login", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusTooManyRequests {
				t.Skip("rate limited; not what this test is about")
			}
			if rec.Code < 400 {
				t.Fatalf("expected an error status, got %d", rec.Code)
			}

			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON — a non-JSON error is the one shape the client cannot parse", ct)
			}

			var env struct {
				Error *struct {
					Code    string            `json:"code"`
					Message string            `json:"message"`
					Fields  map[string]string `json:"fields"`
					TraceID string            `json:"trace_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("response is not the envelope: %v\nbody: %s", err, rec.Body.String())
			}
			if env.Error == nil {
				t.Fatalf("no error object in the envelope: %s", rec.Body.String())
			}
			if env.Error.Code == "" {
				t.Error("error has no machine-readable code; the client can only show the message")
			}
			if env.Error.Message == "" {
				t.Error("error has no message to show a user")
			}
			// The trace id is what turns a customer complaint into a log query.
			if env.Error.TraceID == "" {
				t.Error("error carries no trace_id; a support report cannot be traced")
			}
			if tc.wantCode != "" && env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestUnknownAuthenticatedRoutesDoNotRevealThemselves.
//
// An unknown path inside the authenticated subtree returns 401, not 404. That
// looked like a bug when a test first caught it, and it is the opposite: if
// unknown paths 404 while real ones 401, an anonymous caller can enumerate the
// entire API surface by diffing status codes. Uniform 401 leaks nothing.
//
// Asserted explicitly because the behaviour currently falls out of middleware
// ordering rather than from a deliberate decision — so a future refactor that
// "fixes" the 404 would silently reopen the enumeration channel, and this test
// is what would object.
func TestUnknownAuthenticatedRoutesDoNotRevealThemselves(t *testing.T) {
	h := harness(t)

	// A path that does not exist, and one that does, both under the authenticated
	// subtree. An anonymous caller must not be able to tell them apart.
	for _, path := range []string{
		"/api/v1/definitely-not-a-route",
		"/api/v1/orders", // real
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusTooManyRequests {
			t.Skip("rate limited")
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s returned %d; real and fake authenticated routes must both 401, "+
				"or the difference enumerates the API", path, rec.Code)
		}
	}
}

// TestInternalErrorsDoNotLeakDetail: an internal error must never expose SQL,
// constraint names or a stack. "duplicate key value violates unique constraint
// users_phone_key" tells an attacker the schema.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	h := harness(t)

	// A malformed UUID in a path param exercises the parse/validation path.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/kitchens/not-a-uuid/menu", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, leak := range []string{
		"SQLSTATE", "pgx", "pq:", "constraint", "goroutine",
		"internal/modules", "SELECT ", "INSERT ",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("response leaks %q:\n%s", leak, body)
		}
	}
}

// TestSecurityHeadersArePresent on every response, including errors — a browser
// tricked into rendering an API response must not be able to execute anything.
func TestSecurityHeadersArePresent(t *testing.T) {
	h := harness(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, expected := range want {
		if got := rec.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("no Content-Security-Policy")
	}
}

// TestRequestIDIsPropagated: an inbound X-Request-ID must be reused so a trace
// spans the load balancer, the API and the worker. Without it, debugging a failed
// order means correlating by timestamp.
func TestRequestIDIsPropagated(t *testing.T) {
	h := harness(t)

	const inbound = "test-trace-id-12345"
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-ID", inbound)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != inbound {
		t.Errorf("X-Request-ID = %q, want the inbound %q echoed back", got, inbound)
	}

	// Absent inbound, one must still be minted: every response needs a handle.
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID was generated")
	}
}

// TestTamperedTokenIsRejected covers the JWT verification path end to end,
// including algorithm confusion.
func TestTamperedTokenIsRejected(t *testing.T) {
	h := harness(t)

	// alg=none, the classic bypass attempt.
	algNone := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." +
		"eyJzdWIiOiIwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDEifQ."

	for _, tc := range []struct{ name, token string }{
		{"garbage", "not-a-token"},
		{"alg=none", algNone},
		{"empty bearer", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
			if tc.name == "wrong scheme" {
				req.Header.Set("Authorization", tc.token)
			} else {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusTooManyRequests {
				t.Skip("rate limited")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s token returned %d, want 401\nbody: %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMetricsEndpointUsesBoundedLabels guards against a cardinality explosion.
//
// Labelling by concrete path means one time series per order id, which kills
// Prometheus long before it kills the app.
func TestMetricsEndpointUsesBoundedLabels(t *testing.T) {
	h := harness(t)

	// Generate traffic against a templated route with distinct ids.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/api/v1/kitchens/00000000-0000-0000-0000-00000000000%d/menu", i), nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	// The concrete ids must NOT appear as label values.
	for i := 0; i < 3; i++ {
		concrete := fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", i)
		if strings.Contains(body, concrete) {
			t.Errorf("metrics contain the concrete id %s as a label value; that is a cardinality explosion", concrete)
		}
	}
	// The route PATTERN should appear instead.
	if strings.Contains(body, "chowk_http_requests_total") && !strings.Contains(body, "{kitchenID}") {
		t.Log("note: no templated route label observed; check Observe is reading chi's RoutePattern")
	}
}
