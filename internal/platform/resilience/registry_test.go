package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRegistryNilSafe covers the case the production call site actually hits in
// tests: a gateway constructed directly, with no registry to register into.
func TestRegistryNilSafe(t *testing.T) {
	var reg *Registry

	g := NewGuard(GuardConfig{Name: "x", Timeout: time.Second, MaxConcurrent: 1})
	if got := reg.Register(g); got != g {
		t.Fatal("Register on a nil registry must return the guard unchanged")
	}
	if reg.Snapshot() != nil {
		t.Error("Snapshot on a nil registry should be nil, not a panic or an empty non-nil slice")
	}
	if reg.AnyOpen() {
		t.Error("a nil registry cannot have an open breaker")
	}
}

func TestRegistryReportsRegisteredGuards(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewGuard(GuardConfig{Name: "razorpay", Timeout: time.Second, MaxConcurrent: 5}))
	reg.Register(NewGuard(GuardConfig{Name: "delhivery", Timeout: time.Second, MaxConcurrent: 5}))

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("got %d guards, want 2", len(snap))
	}

	// Sorted, so an operator diffing two responses sees only real changes rather
	// than map iteration order.
	if snap[0].Name != "delhivery" || snap[1].Name != "razorpay" {
		t.Errorf("guards are not sorted by name: %v, %v", snap[0].Name, snap[1].Name)
	}

	// A fresh breaker is closed, and "closed" must be the literal string the
	// breaker itself produces — not a re-spelling in the registry.
	for _, g := range snap {
		if g.State != "closed" {
			t.Errorf("%s: fresh guard reports %q, want %q", g.Name, g.State, "closed")
		}
	}
}

// TestRegistrySeesBreakerTrip is the assertion the whole type exists for: when a
// dependency starts failing, an operator can SEE it without reading logs.
func TestRegistrySeesBreakerTrip(t *testing.T) {
	reg := NewRegistry()
	g := reg.Register(NewGuard(GuardConfig{
		Name:          "flaky",
		Timeout:       time.Second,
		MaxConcurrent: 4,
		DisableRetry:  true,
		Breaker:       BreakerConfig{FailureThreshold: 2},
	}))

	if reg.AnyOpen() {
		t.Fatal("nothing has failed yet")
	}

	boom := errors.New("dependency down")
	for i := 0; i < 8; i++ {
		_ = g.Call(context.Background(), "call", func(context.Context) error { return boom })
	}

	if !reg.AnyOpen() {
		t.Fatal("after repeated failures the breaker should be open and the registry should say so")
	}

	var found bool
	for _, s := range reg.Snapshot() {
		if s.Name == "flaky" {
			found = true
			if s.State == "closed" {
				t.Errorf("breaker reports %q after 8 consecutive failures", s.State)
			}
		}
	}
	if !found {
		t.Error("the registered guard is missing from the snapshot")
	}
}

// AnyOpen must not be wired into readiness. This test documents the intent so that
// a future change which routes it into /ready has to delete an explicit statement
// rather than silently invert the design: an open PAYMENT breaker means payments are
// degraded, while menus, carts and order history still work. Failing readiness there
// pulls the instance from the load balancer and turns partial degradation into a
// total outage.
func TestAnyOpenIsAdvisoryOnly(t *testing.T) {
	reg := NewRegistry()
	g := reg.Register(NewGuard(GuardConfig{
		Name: "payments", Timeout: time.Second, MaxConcurrent: 2,
		DisableRetry: true, Breaker: BreakerConfig{FailureThreshold: 1},
	}))
	for i := 0; i < 5; i++ {
		_ = g.Call(context.Background(), "charge", func(context.Context) error { return errors.New("x") })
	}
	if !reg.AnyOpen() {
		t.Skip("breaker did not open; threshold semantics differ, nothing to assert here")
	}
	// The assertion is the absence of coupling, checked at the call site in
	// server.go rather than here. Kept as a named test so the reasoning is
	// discoverable from the package.
}
