package payment

import (
	"strings"
	"testing"

	"github.com/zevoxa/chowk-backend/internal/config"
)

/*
	Provider selection, tested because every mistake here is a production incident
	of a particular kind: the server comes up, takes orders, and collects nothing.
	None of these failures is loud on its own.
*/

func cfgWith(env config.Env, provider, keyID, keySecret string) *config.Config {
	c := &config.Config{}
	c.Env = env
	c.Payment = config.PaymentConfig{
		Provider: provider, KeyID: keyID, KeySecret: keySecret, WebhookSecret: "whsec",
	}
	return c
}

func TestUnknownProviderRefusesToBoot(t *testing.T) {
	// A typo in PAYMENT_PROVIDER must never resolve to something that runs. Silently
	// selecting the mock would be an environment taking real orders for free.
	_, _, err := buildGateway(cfgWith(config.EnvLocal, "razorpey", "id", "secret"), nil)
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if !strings.Contains(err.Error(), "razorpey") {
		t.Errorf("error does not name the bad value: %v", err)
	}
	// The message should say what the valid answers are, rather than leaving
	// somebody to grep the source for them.
	if !strings.Contains(err.Error(), "mock") || !strings.Contains(err.Error(), "razorpay") {
		t.Errorf("error does not list the known providers: %v", err)
	}
}

func TestHalfConfiguredCredentialsGiveTheMockInDevelopment(t *testing.T) {
	/*
		The regression this file exists for. Selection keyed off the secret alone,
		so a .env with a secret pasted in and the key id still blank chose the REAL
		Razorpay adapter, which then failed authentication on every call -- no mock
		to develop against, and a gateway that could not work.
	*/
	for _, tc := range []struct{ name, keyID, keySecret string }{
		{"no key id", "", "secret"},
		{"no secret", "id", ""},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, header, err := buildGateway(cfgWith(config.EnvLocal, "razorpay", tc.keyID, tc.keySecret), nil)
			if err != nil {
				t.Fatalf("buildGateway: %v", err)
			}
			if gw.Name() != "mock" {
				t.Errorf("gateway = %q, want mock", gw.Name())
			}
			// The header must follow the adapter. A mock gateway verifying against
			// Razorpay's header would reject every webhook it was sent.
			if header != "X-Mock-Signature" {
				t.Errorf("signature header = %q, want X-Mock-Signature", header)
			}
		})
	}
}

func TestProductionRefusesToBootOnIncompleteCredentials(t *testing.T) {
	// Falling back to the mock here would mean a live site collecting nothing.
	if _, _, err := buildGateway(cfgWith(config.EnvProduction, "razorpay", "", "secret"), nil); err == nil {
		t.Fatal("production booted with no PAYMENT_KEY_ID")
	}
}

func TestCompleteCredentialsSelectTheRealAdapter(t *testing.T) {
	gw, header, err := buildGateway(cfgWith(config.EnvLocal, "razorpay", "rzp_test_x", "secret"), nil)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	if gw.Name() != "razorpay" {
		t.Errorf("gateway = %q, want razorpay", gw.Name())
	}
	if header != "X-Razorpay-Signature" {
		t.Errorf("signature header = %q, want X-Razorpay-Signature", header)
	}
}

func TestNamingTheMockExplicitlyIsHonoured(t *testing.T) {
	// Even with full credentials present: somebody who says "mock" means it.
	gw, _, err := buildGateway(cfgWith(config.EnvLocal, "mock", "rzp_test_x", "secret"), nil)
	if err != nil {
		t.Fatalf("buildGateway: %v", err)
	}
	if gw.Name() != "mock" {
		t.Errorf("gateway = %q, want mock", gw.Name())
	}
}

func TestEveryRegisteredProviderCarriesASignatureHeader(t *testing.T) {
	// A provider added without one would accept unsigned webhooks, or reject every
	// signed one, depending on which side of the comparison went blank.
	for name, p := range providers {
		if p.SignatureHeader == "" {
			t.Errorf("provider %q has no signature header", name)
		}
		if p.New == nil {
			t.Errorf("provider %q has no constructor", name)
		}
		if p.Name != name {
			t.Errorf("provider registered as %q calls itself %q", name, p.Name)
		}
	}
}
