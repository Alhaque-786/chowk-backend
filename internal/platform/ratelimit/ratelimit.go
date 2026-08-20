// Package ratelimit implements a distributed fixed-window rate limiter on top
// of the cache.
//
// A fixed window is used rather than a token bucket because it needs one atomic
// INCR per request instead of a read-modify-write, so it stays correct across
// many API replicas without a lock. The tradeoff is burst tolerance at a window
// boundary -- acceptable for abuse prevention, which is what this is for.
package ratelimit

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/cache"
	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
)

// Rule is one limit: N requests per window.
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// Common rules. OTP and login are far tighter than general traffic because they
// are the endpoints worth attacking: one guards SMS spend, the other guards
// credentials.
var (
	RuleGlobal   = Rule{Name: "global", Limit: 300, Window: time.Minute}
	RuleAuth     = Rule{Name: "auth", Limit: 10, Window: time.Minute}
	RuleOTP      = Rule{Name: "otp", Limit: 5, Window: 10 * time.Minute}
	RuleCheckout = Rule{Name: "checkout", Limit: 20, Window: time.Minute}
)

type Limiter struct {
	cache cache.Cache
}

func New(c cache.Cache) *Limiter { return &Limiter{cache: c} }

// Middleware enforces a rule, keyed by user id when authenticated and by IP
// otherwise.
//
// Keying by user first matters behind carrier NAT: thousands of Jio customers
// can share one IP, and an IP-only limit would throttle a whole city because one
// person was abusive.
func (l *Limiter) Middleware(rule Rule) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := fmt.Sprintf("rl:%s:%s", rule.Name, subjectOf(r))

			count, err := l.cache.Incr(r.Context(), key, rule.Window)
			if err != nil {
				// Fail open. A Redis outage must not take the whole API down --
				// losing rate limiting is far less bad than losing the service.
				logger.From(r.Context()).Error("rate limiter unavailable, allowing request",
					"rule", rule.Name, "error", err)
				next.ServeHTTP(w, r)
				return
			}

			remaining := rule.Limit - count
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(rule.Limit, 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))

			if count > rule.Limit {
				w.Header().Set("Retry-After", strconv.Itoa(int(rule.Window.Seconds())))
				httpx.Error(w, r, apperr.RateLimited("RATE_LIMITED",
					"Too many requests. Please slow down and try again shortly."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// subjectOf identifies who to bill the request against.
func subjectOf(r *http.Request) string {
	if p, ok := httpx.PrincipalFrom(r.Context()); ok {
		return "u:" + p.UserID.String()
	}
	// RemoteAddr is used rather than X-Forwarded-For because that header is
	// client-controlled: trusting it would let an attacker rotate a fake IP per
	// request and bypass the limit entirely. The real client IP should be
	// resolved by trusted-proxy middleware before this runs.
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", fmt.Errorf("no port in %q", addr)
}
