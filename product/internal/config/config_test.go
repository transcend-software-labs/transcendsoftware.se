package config

import (
	"testing"
	"time"
)

func TestProductionValidationRejectsFakeFallbacks(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	c := Config{BaseURL: "https://forge.example", SecureCookie: true, FlyAppName: "forge"}
	if err := c.Validate(); err == nil {
		t.Fatal("partial production configuration must fail")
	}
	c.DatabaseURL = "postgres://db"
	c.AdminEmail = "admin@example.com"
	c.ResendAPIKey = "re"
	c.EmailFrom = "Transcend Forge <hello@forge.example>"
	c.EmailReplyTo = "support@example.com"
	c.LLMAPIKey = "llm"
	c.FlyAPIToken = "fly"
	c.FlySandboxApp = "sandbox"
	c.FlySandboxImage = "image"
	c.StorageEndpoint, c.StorageAccessKey, c.StorageSecretKey, c.StorageBucket = "s3", "ak", "sk", "assets"
	c.BackupEndpoint, c.BackupAccessKey, c.BackupSecretKey, c.BackupBucket = "s3", "ak", "sk", "backups"
	c.StripeSecretKey, c.StripePriceID, c.StripeWebhookSecret = "stripe", "price", "whsec"
	c.MaxProjectsPerDay, c.MaxConcurrentBuilds, c.MaxBuildsPerDay = 3, 1, 10
	c.ChangesPerMonth, c.OverageOre, c.PreviewTTL = 3, 4900, 14*24*time.Hour
	if err := c.Validate(); err != nil {
		t.Fatalf("complete production config rejected: %v", err)
	}
}

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
	// Kimi K3 has two routes: via the go gateway (zen key) and first-party
	// Moonshot (its own key + base URL). Each resolves only with its own key.
	if k3, ok := zen.ResolveModel("kimi-k3"); !ok || k3.BaseURL != "https://zen" || k3.APIKey != "zk" {
		t.Errorf("kimi-k3 resolve = %+v ok=%v (want the go gateway)", k3, ok)
	}
	if _, ok := zen.ResolveModel("kimi-k3-moonshot"); ok {
		t.Error("moonshot profile must not resolve without a moonshot key")
	}
	moon := Config{MoonshotAPIKey: "mk"}
	m, ok := moon.ResolveModel("kimi-k3-moonshot")
	if !ok || m.BaseURL != "https://api.moonshot.ai/v1" || m.APIKey != "mk" || m.Model == "" {
		t.Errorf("kimi-k3-moonshot resolve = %+v ok=%v", m, ok)
	}
	if _, ok := moon.ResolveModel("kimi-k3"); ok {
		t.Error("zen profile must not resolve with only a moonshot key")
	}
}

func TestParseCustomModel(t *testing.T) {
	cases := []struct {
		spec     string
		provider ModelProvider
		model    string
		effort   string
		native   bool
		base     string
		ok       bool
	}{
		{"custom:zen/deepseek-v4-pro#high", ProviderZen, "deepseek-v4-pro", "high", true, "", true},
		{"custom:zen-main/grok-code-fast", ProviderZen, "grok-code-fast", "", false, zenMainGateway, true},
		{"custom:zen-shim/kimi-k2.7-code", ProviderZen, "kimi-k2.7-code", "", false, "", true},
		{"custom:anthropic/claude-opus-4-8#max", ProviderAnthropic, "claude-opus-4-8", "max", false, "", true},
		{"custom:moonshot/kimi-k3", ProviderMoonshot, "kimi-k3", "", false, "", true},
		{"custom:zen/x#bogus", ProviderZen, "x", "", true, "", true}, // bad effort dropped, spec kept
		{"custom:unknownfam/x", "", "", "", false, "", false},
		{"custom:zen/", "", "", "", false, "", false},   // empty model
		{"custom:/model", "", "", "", false, "", false}, // empty family
		{"sonnet5", "", "", "", false, "", false},       // not a custom spec
	}
	for _, c := range cases {
		p, ok := ParseCustomModel(c.spec)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.spec, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if p.Provider != c.provider || p.Model != c.model || p.Effort != c.effort || p.NativeGo != c.native || p.BaseURL != c.base {
			t.Errorf("%q → %+v (want provider=%s model=%s effort=%s native=%v base=%s)",
				c.spec, p, c.provider, c.model, c.effort, c.native, c.base)
		}
		if p.Key != c.spec {
			t.Errorf("%q: Key = %q, want the spec verbatim (round-trips through the DB)", c.spec, p.Key)
		}
	}
}

func TestResolveCustomModel(t *testing.T) {
	zen := Config{ZenAPIKey: "zk", ZenBaseURL: "https://go-gw"}
	// A custom go-gateway model resolves with the zen key + go base.
	r, ok := zen.ResolveModel("custom:zen/some-new-model#high")
	if !ok || r.APIKey != "zk" || r.BaseURL != "https://go-gw" || r.Model != "some-new-model" || r.Effort != "high" || !r.NativeGo {
		t.Errorf("custom zen resolve = %+v ok=%v", r, ok)
	}
	// A custom main-gateway (grok family) model overrides the base URL.
	if g, ok := zen.ResolveModel("custom:zen-main/grok-9"); !ok || g.BaseURL != zenMainGateway {
		t.Errorf("custom zen-main resolve = %+v ok=%v (want main gateway)", g, ok)
	}
	// A custom model whose family key isn't configured must not resolve (caller
	// falls back to the default), so an unconfigured provider can't leak.
	if _, ok := zen.ResolveModel("custom:anthropic/claude-x"); ok {
		t.Error("custom anthropic model must not resolve without an anthropic key")
	}
	if _, ok := (Config{AnthropicAPIKey: "sk"}).ResolveModel("custom:anthropic/claude-x#max"); !ok {
		t.Error("custom anthropic model should resolve with an anthropic key")
	}
}

func TestModelProfileGateways(t *testing.T) {
	byKey := map[string]ModelProfile{}
	for _, p := range allProfiles() {
		byKey[p.Key] = p
	}
	// deepseek/minimax/kimi-k3 need opencode's native provider (full list);
	// kimi/glm/grok — and kimi-k3-moonshot (not a go-gateway model) — don't.
	wantNative := map[string]bool{"deepseek": true, "deepseek-flash": true, "minimax": true, "kimi-k3": true}
	for _, k := range []string{"kimi", "glm", "grok", "deepseek", "deepseek-flash", "minimax", "kimi-k3", "kimi-k3-moonshot"} {
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

	// These presets are the exact IDs and API shapes published in OpenCode's
	// Zen catalog. All use the main gateway, even when the older profile set is
	// configured with the separate Go gateway as its default.
	zenMain := map[string]struct {
		model    string
		protocol string
	}{
		"gpt56sol":      {"gpt-5.6-sol", "responses"},
		"gpt56terra":    {"gpt-5.6-terra", "responses"},
		"gpt56luna":     {"gpt-5.6-luna", "responses"},
		"grok-build-01": {"grok-build-0.1", ""},
		"sonnet5-zen":   {"claude-sonnet-5", "messages"},
	}
	for key, want := range zenMain {
		p, ok := byKey[key]
		if !ok {
			t.Errorf("missing Zen profile %s", key)
			continue
		}
		if p.Provider != ProviderZen || p.BaseURL != zenMainGateway || p.Model != want.model || p.Protocol != want.protocol {
			t.Errorf("%s = %+v, want model=%s protocol=%s on main Zen gateway", key, p, want.model, want.protocol)
		}
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

func TestNameComEnabledAndDomainGates(t *testing.T) {
	// name.com needs both the username and the API token.
	if (Config{NameComUsername: "forge"}).NameComEnabled() {
		t.Error("name.com should be off with only a username")
	}
	if !(Config{NameComUsername: "forge", NameComAPIKey: "k"}).NameComEnabled() {
		t.Error("name.com should be on with both creds")
	}

	// DomainsEnabled mirrors the registrar being configured.
	if (Config{}).DomainsEnabled() {
		t.Error("no registrar → domains off")
	}
	if !(Config{NameComUsername: "forge", NameComAPIKey: "k"}).DomainsEnabled() {
		t.Error("name.com configured → domains on")
	}

	// Buying needs Stripe live so the registration cost can be invoiced.
	nc := Config{NameComUsername: "forge", NameComAPIKey: "k"}
	if nc.DomainBuyEnabled() {
		t.Error("buy should be off without Stripe")
	}
	nc.StripeSecretKey, nc.StripePriceID, nc.StripeWebhookSecret = "sk", "price", "whsec"
	if !nc.DomainBuyEnabled() {
		t.Error("buy should be on with a registrar + Stripe")
	}
}
