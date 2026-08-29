// Package sourcing is the supply-acquisition bounded context.
//
// Owns: vendor leads — who somebody thinks should be selling on this platform,
// and how far along the conversation with them has got.
//
// It exists because of the gap assisted vendors opened up. A cart selling fresh
// nariyal pani is exactly the supply this platform wants and exactly the supply
// that cannot onboard itself: no dashboard, often no smartphone workflow, and no
// reason to trust a web form. The lead therefore arrives from somebody else — a
// customer who buys there every morning, or our own team after walking a market.
//
// Deliberately NOT part of catalog. Catalog owns kitchens that exist; this owns
// vendors that do not exist yet and mostly never will. Their lifecycles have
// nothing in common: a lead is a conversation with a person, a kitchen is a
// tenant with menus and orders. Merging them would put a sales funnel inside the
// module that serves every customer request.
//
// Structure follows identity exactly:
//
//	domain/            entities, value objects, repository interfaces. No I/O.
//	app/               use cases. Owns transaction boundaries.
//	adapter/postgres/  repository implementations. The only place with SQL.
//	adapter/http/      handlers + DTOs. The only place that knows about HTTP.
//	module.go          New(Deps), Routes().
//
// The one hard rule: never import another module. Cross-module needs go through
// internal/contracts, injected by internal/server.
package sourcing
