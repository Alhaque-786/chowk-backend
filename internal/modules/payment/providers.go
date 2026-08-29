package payment

import (
	"fmt"
	"sort"

	"github.com/zevoxa/chowk-backend/internal/config"
	paymentgateway "github.com/zevoxa/chowk-backend/internal/modules/payment/adapter/gateway"
	"github.com/zevoxa/chowk-backend/internal/modules/payment/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/resilience"
)

/*
	The payment providers this system knows how to talk to.

	A REGISTRY RATHER THAN A SWITCH, because the provider is expected to change and
	to multiply. Razorpay is the default for India today; Cashfree, PhonePe and
	Paytm are all plausible additions, and a marketplace usually ends up running
	more than one -- a second provider is the only real answer to the first being
	down, and split-settlement products differ enough that the choice may be made
	per merchant rather than once.

	Adding one is a constructor in adapter/gateway/ and one Register call below.
	Nothing else in the module changes, and nothing outside it knows which provider
	is in use: everything upstream depends on domain.Gateway.

	What each provider must supply is the interesting part of this shape. The
	signature header is per-provider (Razorpay signs X-Razorpay-Signature, Cashfree
	uses its own), and it has to travel WITH the adapter -- keeping it in a separate
	map is how a provider gets added and its webhooks silently fail verification.
*/

// Provider is one gateway this system can be configured to use.
type Provider struct {
	// Name is the value PAYMENT_PROVIDER must hold to select this provider.
	Name string

	// SignatureHeader is the HTTP header the provider signs its webhooks with.
	// Travels with the adapter so the two cannot drift apart.
	SignatureHeader string

	// NeedsCredentials marks a provider that cannot work without a key pair. The
	// mock does not; every real gateway does.
	NeedsCredentials bool

	// New builds the adapter. Credentials are already known to be present when
	// NeedsCredentials is set, so this does not have to re-check them.
	New func(cfg config.PaymentConfig, guards *resilience.Registry) domain.Gateway
}

var providers = map[string]Provider{}

// Register adds a provider. Panics on a duplicate name, which can only be a
// programming error at init time and is better found at startup than by
// discovering that payments went to whichever registration happened to run last.
func Register(p Provider) {
	if _, duplicate := providers[p.Name]; duplicate {
		panic(fmt.Sprintf("payment: provider %q registered twice", p.Name))
	}
	providers[p.Name] = p
}

func init() {
	Register(Provider{
		Name:             "razorpay",
		SignatureHeader:  "X-Razorpay-Signature",
		NeedsCredentials: true,
		New: func(cfg config.PaymentConfig, guards *resilience.Registry) domain.Gateway {
			return paymentgateway.NewRazorpay(cfg.KeyID, cfg.KeySecret, cfg.WebhookSecret, guards)
		},
	})

	// The mock exercises the entire payment path -- intent, signed webhook,
	// capture, order promotion -- with no gateway account. It verifies HMAC the
	// same way the real adapters do, so the signature path is genuinely tested
	// rather than bypassed.
	Register(Provider{
		Name:            "mock",
		SignatureHeader: "X-Mock-Signature",
		New: func(cfg config.PaymentConfig, _ *resilience.Registry) domain.Gateway {
			return paymentgateway.NewMock(cfg.WebhookSecret)
		},
	})
}

// knownProviders lists registered names, sorted, for error messages. A
// misconfigured PAYMENT_PROVIDER should say what the valid answers are rather
// than leaving somebody to grep for them.
func knownProviders() []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

/*
buildGateway selects the adapter for the configured provider.

Three failures it is deliberately shaped to prevent:

An UNKNOWN PROVIDER NAME is an error, never a silent fallback. A typo in
PAYMENT_PROVIDER that quietly selected the mock would mean an environment
taking real orders and collecting nothing.

INCOMPLETE CREDENTIALS choose the mock in development and fail in production.
This keyed off the secret alone once, so a half-filled .env -- secret pasted
in, key id still blank -- selected the real Razorpay adapter and failed
authentication on every call: no mock to develop against, and a gateway that
could not work either.

PRODUCTION NEVER GETS THE MOCK unless somebody names it explicitly, in which
case they have said something unambiguous and config validation has already
demanded the credentials a real provider needs.
*/
func buildGateway(cfg *config.Config, guards *resilience.Registry) (domain.Gateway, string, error) {
	name := cfg.Payment.Provider
	p, known := providers[name]
	if !known {
		return nil, "", fmt.Errorf(
			"payment: unknown PAYMENT_PROVIDER %q; known providers are %v",
			name, knownProviders())
	}

	if p.NeedsCredentials && (cfg.Payment.KeyID == "" || cfg.Payment.KeySecret == "") {
		if cfg.IsProduction() {
			return nil, "", fmt.Errorf(
				"payment: provider %q needs PAYMENT_KEY_ID and PAYMENT_KEY_SECRET", name)
		}
		mock := providers["mock"]
		return mock.New(cfg.Payment, guards), mock.SignatureHeader, nil
	}

	return p.New(cfg.Payment, guards), p.SignatureHeader, nil
}
