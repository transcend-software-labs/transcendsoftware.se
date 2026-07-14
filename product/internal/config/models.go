package config

// Model profiles: named, selectable model configs for the planner and the
// implementation agent, so a build can be run with any planner×impl
// combination from /admin to compare quality/cost. A "model" is really
// {provider, base URL, key, model id, effort}, so a profile bundles all of it;
// the credentials/URL are resolved from Config at use time (ResolveModel).
//
// Two provider families:
//   - anthropic: the native Anthropic Messages API (internal/llm.Anthropic for
//     the planner; opencode's anthropic provider for the impl). Needs
//     ANTHROPIC_API_KEY.
//   - zen: OpenAI-compatible via the OpenCode Zen Go gateway
//     (opencode.ai/zen/go/v1). Needs OPENCODE_GO_API_KEY.
//
// Gateway model slugs are env-overridable (MODEL_*) since the exact Zen catalog
// id may differ from the label.

type ModelProvider string

const (
	ProviderAnthropic ModelProvider = "anthropic"
	ProviderZen       ModelProvider = "zen"
)

// ModelProfile is one selectable model. In/Out prices are USD per 1M tokens,
// used only for the rough per-build cost shown in /admin.
type ModelProfile struct {
	Key      string
	Label    string
	Provider ModelProvider
	Model    string // provider model slug
	Effort   string // reasoning effort: "", low, medium, high, xhigh, max
	InPerM   float64
	OutPerM  float64
}

// allProfiles is the full catalog (independent of which keys are configured).
func allProfiles() []ModelProfile {
	return []ModelProfile{
		{"sonnet5", "Claude Sonnet 5", ProviderAnthropic, envOr("MODEL_SONNET5", "claude-sonnet-5"), "max", 2, 10},
		{"fable5", "Claude Fable 5", ProviderAnthropic, envOr("MODEL_FABLE5", "claude-fable-5"), "xhigh", 10, 50},
		{"opus48", "Claude Opus 4.8", ProviderAnthropic, envOr("MODEL_OPUS48", "claude-opus-4-8"), "high", 5, 25},
		{"kimi", "Kimi K2", ProviderZen, envOr("MODEL_KIMI", "kimi-k2.7-code"), "", 0.6, 2.5},
		{"glm", "GLM 5.2", ProviderZen, envOr("MODEL_GLM", "glm-5.2"), "", 0.6, 2.2},
		{"grok", "Grok 4.5", ProviderZen, envOr("MODEL_GROK", "grok-4.5"), "high", 3, 15},
		{"minimax", "MiniMax M3", ProviderZen, envOr("MODEL_MINIMAX", "minimax-m3"), "high", 0.5, 2},
		{"deepseek", "DeepSeek V4 Pro", ProviderZen, envOr("MODEL_DEEPSEEK", "deepseek-v4-pro"), "high", 0.6, 2.5},
	}
}

// ModelProfiles returns the profiles whose provider credentials are configured.
func (c Config) ModelProfiles() []ModelProfile {
	var out []ModelProfile
	for _, p := range allProfiles() {
		if c.profileEnabled(p) {
			out = append(out, p)
		}
	}
	return out
}

func (c Config) profileEnabled(p ModelProfile) bool {
	switch p.Provider {
	case ProviderAnthropic:
		return c.AnthropicAPIKey != ""
	case ProviderZen:
		return c.ZenAPIKey != ""
	}
	return false
}

// ModelProfileByKey looks up a profile by key (regardless of whether it's
// currently enabled).
func (c Config) ModelProfileByKey(key string) (ModelProfile, bool) {
	for _, p := range allProfiles() {
		if p.Key == key {
			return p, true
		}
	}
	return ModelProfile{}, false
}

// ResolvedModel is a profile plus the credentials/URL to actually use it.
// BaseURL is empty for anthropic (the Anthropic client carries its own host).
type ResolvedModel struct {
	ModelProfile
	BaseURL string
	APIKey  string
}

// ResolveModel resolves a profile key to a usable model, or ok=false when the
// key is unknown or its provider isn't configured (callers fall back to the
// global wiring).
func (c Config) ResolveModel(key string) (ResolvedModel, bool) {
	p, ok := c.ModelProfileByKey(key)
	if !ok || !c.profileEnabled(p) {
		return ResolvedModel{}, false
	}
	r := ResolvedModel{ModelProfile: p}
	switch p.Provider {
	case ProviderAnthropic:
		r.APIKey = c.AnthropicAPIKey
	case ProviderZen:
		r.BaseURL, r.APIKey = c.ZenBaseURL, c.ZenAPIKey
	}
	return r, true
}

// CostOre returns the rough cost in öre for an implementation session, given
// its input tokens and total tokens (output+reasoning billed at the out rate).
// USD prices are converted at a fixed ~10.5 SEK/USD — this is a ballpark for
// the /admin experiment table, not an invoice.
func (m ModelProfile) CostOre(tokensIn, tokensTotal int) int {
	if tokensTotal <= 0 {
		return 0
	}
	if tokensIn > tokensTotal {
		tokensIn = tokensTotal
	}
	out := tokensTotal - tokensIn
	usd := (float64(tokensIn)*m.InPerM + float64(out)*m.OutPerM) / 1_000_000
	return int(usd * 10.5 * 100) // USD → SEK → öre
}
