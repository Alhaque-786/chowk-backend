// Package delivery is the rider and dispatch bounded context.
//
// Owns: riders, shift availability, order assignment, live location, and proof of
// delivery. Implements contracts.RiderDispatcher.
//
// Consumes: order.ready (request a rider).
// Publishes: rider.assigned, rider.location_updated.
//
// Two things to get right early. Location updates arrive every few seconds per
// active rider -- write them to Redis and publish over pub/sub, and only persist
// a decimated track to Postgres, or the write volume will dominate the database
// for data nobody queries. And assignment should start as nearest-available with
// a manual override; batching and route optimisation are a later problem that
// needs real volume to tune.
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
package delivery
