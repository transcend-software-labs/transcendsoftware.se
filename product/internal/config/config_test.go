package config

import "testing"

func TestGlesysEnabledAndDomainGates(t *testing.T) {
	// GleSYS needs both the project key and the API key.
	if (Config{GlesysProjectID: "CL1"}).GlesysEnabled() {
		t.Error("GleSYS should be off with only a project id")
	}
	if !(Config{GlesysProjectID: "CL1", GlesysAPIKey: "k"}).GlesysEnabled() {
		t.Error("GleSYS should be on with both creds")
	}

	// DomainsEnabled is true if any registrar is configured.
	if (Config{}).DomainsEnabled() {
		t.Error("no registrar → domains off")
	}
	if !(Config{GlesysProjectID: "CL1", GlesysAPIKey: "k"}).DomainsEnabled() {
		t.Error("GleSYS configured → domains on")
	}
	if !(Config{HostupAPIToken: "t"}).DomainsEnabled() {
		t.Error("Hostup configured → domains on")
	}

	// Buying needs Stripe live so the registration cost can be invoiced.
	glesys := Config{GlesysProjectID: "CL1", GlesysAPIKey: "k"}
	if glesys.DomainBuyEnabled() {
		t.Error("buy should be off without Stripe")
	}
	glesys.StripeSecretKey, glesys.StripePriceID, glesys.StripeWebhookSecret = "sk", "price", "whsec"
	if !glesys.DomainBuyEnabled() {
		t.Error("buy should be on with a registrar + Stripe")
	}
}

func TestRegistrantComplete(t *testing.T) {
	full := Registrant{
		Organization: "Transcend Software", NationalID: 5566778899,
		Address: "Storgatan 1", City: "Stockholm", ZipCode: "11122",
		Country: "SE", Email: "rasmus@transcendsoftware.se",
	}
	if !full.Complete() {
		t.Error("a fully populated registrant should be complete")
	}
	if (Registrant{Organization: "Transcend"}).Complete() {
		t.Error("a registrant missing the national id/address should be incomplete")
	}
}
