package resilience

import "sort"

// Registry makes the process's guards enumerable.
//
// WHY THIS EXISTS. `Guard.State()` carries the comment "exposes the breaker state
// for the health endpoint, so an operator can see which dependency is tripped
// without reading logs" -- and it was called from nowhere. The intention was
// written down and never wired up, because a guard is constructed deep inside the
// adapter that uses it (the Razorpay client builds its own) and nothing upstream
// held a reference to ask.
//
// That is the same shape of defect as an allowlist entry that matches no route: it
// reads as a working feature in review. The gap only shows up when an operator
// asks "why are payments failing" at 2am and the answer -- the breaker is open --
// is in a log line nobody is tailing.
//
// Deliberately a plain slice with no locking on read paths beyond registration.
// Guards are created once during composition, before the server starts serving, so
// registration is single-threaded by construction. Snapshot is called from an HTTP
// handler and only reads.
type Registry struct {
	guards []*Guard
}

func NewRegistry() *Registry { return &Registry{} }

// Register records a guard. Returns the guard so it can be used inline at the
// construction site: `guard: reg.Register(resilience.NewGuard(...))`.
//
// Nil-safe on the receiver, so a caller that has no registry -- a unit test
// building a gateway directly -- does not have to construct one.
func (r *Registry) Register(g *Guard) *Guard {
	if r == nil || g == nil {
		return g
	}
	r.guards = append(r.guards, g)
	return g
}

// GuardStatus is one dependency's protection state.
type GuardStatus struct {
	Name string `json:"name"`
	// "closed" (healthy), "half_open" (probing), or "open" (rejecting) — the exact
	// strings from breaker.State().String(), not a re-spelling of them.
	State string `json:"state"`
	// Calls currently occupying the bulkhead. A number pinned at the limit means
	// the dependency is slow rather than failing, which is a different fix.
	InFlight int `json:"in_flight"`
}

// Snapshot reports every registered guard, sorted by name so the output is stable
// between calls -- an operator diffing two responses should see only real changes.
func (r *Registry) Snapshot() []GuardStatus {
	if r == nil {
		return nil
	}
	out := make([]GuardStatus, 0, len(r.guards))
	for _, g := range r.guards {
		out = append(out, GuardStatus{Name: g.Name(), State: g.State(), InFlight: g.InFlight()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AnyOpen reports whether some dependency is currently being rejected outright.
//
// NOT used to fail readiness, and that is the point of having it separate. An open
// payment breaker means "payments are degraded", not "stop sending me traffic" --
// the API still serves menus, carts and order history perfectly well. Failing
// readiness there would pull the instance out of the load balancer and turn a
// partial degradation into a total outage.
func (r *Registry) AnyOpen() bool {
	if r == nil {
		return false
	}
	for _, g := range r.guards {
		if g.State() == "open" {
			return true
		}
	}
	return false
}
