// Package ordering is the order bounded context -- the spine of the system.
//
// Owns: carts, orders, order lines, the status audit trail, and the checkout
// saga. It is the only module permitted to create an order.
//
// domain/order.go is already written: the aggregate plus the state machine. Read
// it before adding anything here, because every legal status transition is
// declared there as data and nothing should bypass it.
//
// Still to build: the cart, and the checkout use case. Checkout is a saga that
// must, in ONE transaction: quote prices (PriceQuoter), reserve stock
// (StockReserver), apply the coupon (DiscountCalculator), write the order, and
// publish order.placed to the outbox. Any step failing must roll all of it back.
//
// Consumes: payment.captured (-> placed), payment.failed (-> cancelled).
// Publishes: the whole order.* family.
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
package ordering
