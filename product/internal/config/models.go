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
	// BaseURL overrides the gateway for a zen profile ("" → the default
	// OpenCode Zen Go gateway). Grok lives on the MAIN zen gateway (/zen/v1),
	// not the "opencode go" one (/go/v1) that hosts kimi/glm/minimax — sending
	// grok-4.5 to /go/v1 fails with "Model grok-4.5 is not supported".
	BaseURL string
	// NativeGo routes the IMPL through opencode's built-in "opencode-go" provider
	// instead of the generic openai-compatible shim. The shim only reaches go's
	// "lite" model list (kimi/glm); the full list (deepseek/minimax) — and the
	// per-model chat-completions-vs-messages routing — needs the native provider.
	NativeGo bool
}

const zenMainGateway = "https://opencode.ai/zen/v1"

// allProfiles is the full catalog (independent of which keys are configured).
func allProfiles() []ModelProfile {
	return []ModelProfile{
		{Key: "sonnet5", Label: "Claude Sonnet 5", Provider: ProviderAnthropic, Model: envOr("MODEL_SONNET5", "claude-sonnet-5"), Effort: "max", InPerM: 2, OutPerM: 10},
		{Key: "fable5", Label: "Claude Fable 5", Provider: ProviderAnthropic, Model: envOr("MODEL_FABLE5", "claude-fable-5"), Effort: "xhigh", InPerM: 10, OutPerM: 50},
		{Key: "opus48", Label: "Claude Opus 4.8", Provider: ProviderAnthropic, Model: envOr("MODEL_OPUS48", "claude-opus-4-8"), Effort: "high", InPerM: 5, OutPerM: 25},
		{Key: "kimi", Label: "Kimi K2", Provider: ProviderZen, Model: envOr("MODEL_KIMI", "kimi-k2.7-code"), InPerM: 0.6, OutPerM: 2.5},
		{Key: "glm", Label: "GLM 5.2", Provider: ProviderZen, Model: envOr("MODEL_GLM", "glm-5.2"), InPerM: 0.6, OutPerM: 2.2},
		{Key: "grok", Label: "Grok 4.5", Provider: ProviderZen, Model: envOr("MODEL_GROK", "grok-4.5"), Effort: "high", InPerM: 2, OutPerM: 6, BaseURL: envOr("MODEL_GROK_BASE", zenMainGateway)},
		{Key: "minimax", Label: "MiniMax M3", Provider: ProviderZen, Model: envOr("MODEL_MINIMAX", "minimax-m3"), Effort: "high", InPerM: 0.5, OutPerM: 2, NativeGo: true},
		{Key: "deepseek", Label: "DeepSeek V4 Pro", Provider: ProviderZen, Model: envOr("MODEL_DEEPSEEK", "deepseek-v4-pro"), Effort: "high", InPerM: 0.6, OutPerM: 2.5, NativeGo: true},
		{Key: "deepseek-flash", Label: "DeepSeek V4 Flash", Provider: ProviderZen, Model: envOr("MODEL_DEEPSEEK_FLASH", "deepseek-v4-flash"), Effort: "high", InPerM: 0.3, OutPerM: 1.2, NativeGo: true},
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
		r.APIKey = c.ZenAPIKey
		r.BaseURL = p.BaseURL // per-profile gateway override (e.g. grok on /zen/v1)
		if r.BaseURL == "" {
			r.BaseURL = c.ZenBaseURL
		}
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
