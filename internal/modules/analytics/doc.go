// Package analytics is the reporting bounded context, backing the admin dashboard.
//
// Owns: sales reports, food-cost and margin analysis, kitchen performance,
// customer cohorts, and live operational counters.
//
// Read-only. It consumes events and never mutates another module's data.
//
// This is the one module allowed to query across table boundaries, because a
// report legitimately joins orders, order_lines and ingredients. Keep it strictly
// read-only so that privilege cannot turn into a hidden write path.
//
// Run these queries against a read replica once traffic justifies it -- a heavy
// month-end report must never contend with checkout. Precompute daily rollups
// into their own tables rather than aggregating raw orders live; the dashboard
// must stay fast as history grows.
//
// NOT YET IMPLEMENTED. This file reserves the module boundary and records what
// belongs here, so whoever builds it does not have to re-derive the decomposition
// and does not accidentally scatter this logic across other modules.
//
// Follow the identity module exactly:
//
//	domain/            entities, value objects, repository interfaces. No I/O.
//	app/               use cases. Owns transaction boundaries.
//	adapter/postgres/  repository implementations. The only place with SQL.
//	adapter/http/      handlers + DTOs. The only place that knows about HTTP.
//	module.go          New(Deps), Routes(), RegisterSubscribers().
//
// The one hard rule: never import another module. Cross-module needs go through
// internal/contracts, injected by internal/server.
package analytics
