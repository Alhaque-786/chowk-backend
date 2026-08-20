// Package promotion is the offers and loyalty bounded context.
//
// Owns: coupons, offer rules, redemption records, referrals, and wallet credit.
// Implements contracts.DiscountCalculator.
//
// Note the deliberate split in that interface: Calculate is a read-only "what
// would this be worth", Redeem is the transactional "consume one of its uses".
// Merging them would burn a redemption every time a customer merely looked at
// the offers page.
//
// A global usage cap ("first 100 orders only") cannot be enforced with row locks
// alone -- use db.RetrySerializable, because the invariant is over rows that do
// not exist yet and only SERIALIZABLE catches that.
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
package promotion
