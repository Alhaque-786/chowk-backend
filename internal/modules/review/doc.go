// Package review is the ratings and reviews bounded context.
//
// Owns: order reviews, dish ratings, kitchen ratings, and the moderation queue.
//
// Consumes: order.delivered (unlock the ability to review).
// Publishes: review.submitted.
//
// Only a delivered order may be reviewed, and only once -- enforce both with a
// unique index on order_id, not in application code.
//
// Rating averages are denormalised onto kitchens and menu_items. Recompute them
// here on write; never AVG() over the reviews table to render a menu.
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
package review
