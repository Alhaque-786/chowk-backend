// Package payment is the payment bounded context.
//
// Owns: payments, refunds, gateway integration, and inbound webhooks.
// Implements contracts.PaymentInitiator.
//
// Three rules that are not negotiable, because each one is money:
//
//  1. Verify the webhook HMAC signature before trusting a single field. An
//     unverified webhook endpoint lets anyone mark any order as paid.
//  2. Persist the raw webhook to webhook_events BEFORE processing it, and make
//     provider_event_id unique. Gateways retry aggressively; without that index a
//     duplicate delivery becomes a duplicate capture.
//  3. Never derive payment state from the client. The gateway's webhook is the
//     only authority on whether money moved.
//
// Publishes: payment.authorized, payment.captured, payment.failed,
// refund.initiated, refund.completed.
//
// Put the gateway behind a Gateway interface in domain/. Razorpay is the default
// for India, but the interface is what lets you add a second provider for
// failover without touching the use cases.
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
package payment
