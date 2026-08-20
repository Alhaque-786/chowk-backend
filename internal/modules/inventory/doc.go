// Package inventory is the stock and recipe bounded context.
//
// Owns: ingredients, recipes (the bill of materials per dish), the append-only
// stock movement ledger, reservations, and purchase orders.
//
// Implements contracts.StockReserver. This is the hardest concurrency problem in
// the system: two customers buying the last portion at the same instant must not
// both succeed. Reserve must take a row lock (SELECT ... FOR UPDATE on the
// ingredient) inside the caller's transaction. Optimistic checking races.
//
// Consumes: order.placed (reserve), order.preparing (consume),
// order.cancelled (release).
// Publishes: stock.reserved, stock.released, stock.consumed, stock.low,
// menu_item.sold_out.
//
// The sold-out signal is derived, not manual: recipes let the system compute how
// many servings remain, so a dish disappears from the menu before it is oversold
// rather than after a customer complains.
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
package inventory
