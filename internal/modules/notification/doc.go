// Package notification is the outbound messaging bounded context.
//
// Owns: templates, delivery preferences, device tokens, and the send log.
// Implements contracts.Notifier.
//
// Subscribes to nearly every event in the system and is the reason the event bus
// exists: ordering must not know that an SMS gets sent when an order is placed.
//
// Handlers here must be idempotent. Outbox delivery is at-least-once, so a
// retried order.placed must not send a second SMS -- that is what the DedupeKey
// on NotificationRequest is for.
//
// Put each channel (push/SMS/email/WhatsApp) behind its own Sender interface, and
// make a provider outage degrade rather than fail: a customer should still get
// the push if the SMS gateway is down.
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
package notification
