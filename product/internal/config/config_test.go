package config

import "testing"

func TestModelProfiles(t *testing.T) {
	// No keys → nothing enabled.
	if ps := (Config{}).ModelProfiles(); len(ps) != 0 {
		t.Errorf("no keys → no profiles, got %d", len(ps))
	}
	// Anthropic key enables only the anthropic profiles.
	anth := Config{AnthropicAPIKey: "sk"}
	for _, p := range anth.ModelProfiles() {
		if p.Provider != ProviderAnthropic {
			t.Errorf("only anthropic profiles should enable with just ANTHROPIC key, got %s", p.Key)
		}
	}
	if _, ok := anth.ResolveModel("sonnet5"); !ok {
		t.Error("sonnet5 should resolve with an anthropic key")
	}
	if _, ok := anth.ResolveModel("kimi"); ok {
		t.Error("zen profile must not resolve without a zen key")
	}
	// Zen key enables the gateway profiles with the zen base+key.
	zen := Config{ZenAPIKey: "zk", ZenBaseURL: "https://zen"}
	r, ok := zen.ResolveModel("kimi") // a standard go-gateway profile
	if !ok || r.BaseURL != "https://zen" || r.APIKey != "zk" || r.Model == "" {
		t.Errorf("kimi resolve = %+v ok=%v", r, ok)
	}
	// Grok lives on the MAIN zen gateway, not the go gateway — its profile
	// overrides the base URL (else "Model grok-4.5 is not supported").
	if g, ok := zen.ResolveModel("grok"); !ok || g.BaseURL != "https://opencode.ai/zen/v1" || g.APIKey != "zk" {
		t.Errorf("grok resolve = %+v ok=%v (want the main zen gateway)", g, ok)
	}
}

func TestModelProfileGateways(t *testing.T) {
	byKey := map[string]ModelProfile{}
	for _, p := range allProfiles() {
		byKey[p.Key] = p
	}
	// deepseek/minimax need opencode's native provider (full list); kimi/glm/grok don't.
	wantNative := map[string]bool{"deepseek": true, "minimax": true}
	for _, k := range []string{"kimi", "glm", "grok", "deepseek", "minimax"} {
		if byKey[k].NativeGo != wantNative[k] {
			t.Errorf("%s NativeGo = %v, want %v", k, byKey[k].NativeGo, wantNative[k])
		}
	}
	// grok routes to the main zen gateway (its own base URL); the others don't.
	if byKey["grok"].BaseURL == "" {
		t.Error("grok should override BaseURL to the main zen gateway")
	}
	if byKey["kimi"].BaseURL != "" {
		t.Error("kimi should use the default go gateway (no base override)")
	}
}

func TestModelProfileCost(t *testing.T) {
	// Sonnet-5-ish: $2/M in, $10/M out. 100k in + 24k out(total 124k).
	p := ModelProfile{InPerM: 2, OutPerM: 10}
	// input 100k×2 + output 24k×10 = 0.2 + 0.24 = $0.44 → ×10.5 SEK ×100 = 462 öre.
	if got := p.CostOre(100_000, 124_000); got < 400 || got > 520 {
		t.Errorf("cost öre = %d, want ~462", got)
	}
	if p.CostOre(0, 0) != 0 {
		t.Error("zero tokens → zero cost")
	}
}

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
