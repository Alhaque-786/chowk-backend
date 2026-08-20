// Package catalog is the menu and kitchen bounded context.
//
// Owns: kitchens, opening hours, delivery radius, menu categories, menu items,
// modifier groups, and the authoritative price of everything.
//
// Implements contracts.PriceQuoter and contracts.KitchenAvailability. That first
// one is load-bearing: prices are resolved server-side at checkout and never
// trusted from the client, or someone orders a biryani for one rupee.
//
// Publishes: menu_item.created, menu_item.price_changed, kitchen.opened,
// kitchen.closed.
//
// Read-heavy and change-rarely, so the menu response is the first thing worth
// caching in Redis, keyed by kitchen and invalidated on any write here.
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
package catalog
