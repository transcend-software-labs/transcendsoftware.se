package config

// Model profiles: named, selectable model configs for the planner and the
// implementation agent, so a build can be run with any planner×impl
// combination from /admin to compare quality/cost. A "model" is really
// {provider, base URL, key, model id, effort}, so a profile bundles all of it;
// the credentials/URL are resolved from Config at use time (ResolveModel).
//
// Three provider families:
//   - anthropic: the native Anthropic Messages API (internal/llm.Anthropic for
//     the planner; opencode's anthropic provider for the impl). Needs
//     ANTHROPIC_API_KEY.
//   - zen: OpenAI-compatible via the OpenCode Zen Go gateway
//     (opencode.ai/zen/go/v1). Needs OPENCODE_GO_API_KEY.
//   - moonshot: Moonshot's own OpenAI-compatible API (api.moonshot.ai/v1),
//     first-party Kimi. Needs MOONSHOT_API_KEY. Same OpenAI-compatible path as
//     zen everywhere (planner shim + the sandbox entrypoint's generic provider
//     block) — only the base URL and key differ.
//
// Gateway model slugs are env-overridable (MODEL_*) since the exact Zen catalog
// id may differ from the label.

import "strings"

type ModelProvider string

const (
	ProviderAnthropic ModelProvider = "anthropic"
	ProviderZen       ModelProvider = "zen"
	ProviderMoonshot  ModelProvider = "moonshot"
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
	// Protocol is the API shape used by the planner/reviewer and the sandbox's
	// opencode provider. Empty means OpenAI-compatible chat/completions; Zen's
	// GPT models use responses and its Claude models use Anthropic messages.
	Protocol string
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

const (
	zenMainGateway  = "https://opencode.ai/zen/v1"
	moonshotGateway = "https://api.moonshot.ai/v1"
)

// allProfiles is the full catalog (independent of which keys are configured).
func allProfiles() []ModelProfile {
	return []ModelProfile{
		{Key: "sonnet5", Label: "Claude Sonnet 5", Provider: ProviderAnthropic, Model: envOr("MODEL_SONNET5", "claude-sonnet-5"), Effort: "max", InPerM: 2, OutPerM: 10},
		{Key: "fable5", Label: "Claude Fable 5", Provider: ProviderAnthropic, Model: envOr("MODEL_FABLE5", "claude-fable-5"), Effort: "xhigh", InPerM: 10, OutPerM: 50},
		{Key: "opus48", Label: "Claude Opus 4.8", Provider: ProviderAnthropic, Model: envOr("MODEL_OPUS48", "claude-opus-4-8"), Effort: "high", InPerM: 5, OutPerM: 25},
		{Key: "kimi", Label: "Kimi K2", Provider: ProviderZen, Model: envOr("MODEL_KIMI", "kimi-k2.7-code"), InPerM: 0.6, OutPerM: 2.5},
		// Kimi K3 twice, to compare routes: via OpenCode Go (native provider —
		// a brand-new model is safer on the full list than the lite shim) and
		// via Moonshot first-party. Same model, same list price ($3/$15); no
		// forced effort — K3 is a reasoning model, let it use its default.
		{Key: "kimi-k3", Label: "Kimi K3", Provider: ProviderZen, Model: envOr("MODEL_KIMI_K3", "kimi-k3"), InPerM: 3, OutPerM: 15, NativeGo: true},
		{Key: "kimi-k3-moonshot", Label: "Kimi K3 (Moonshot)", Provider: ProviderMoonshot, Model: envOr("MODEL_KIMI_K3_MOONSHOT", "kimi-k3"), InPerM: 3, OutPerM: 15},
		{Key: "glm", Label: "GLM 5.2", Provider: ProviderZen, Model: envOr("MODEL_GLM", "glm-5.2"), InPerM: 0.6, OutPerM: 2.2},
		{Key: "grok", Label: "Grok 4.5", Provider: ProviderZen, Model: envOr("MODEL_GROK", "grok-4.5"), Effort: "high", InPerM: 2, OutPerM: 6, BaseURL: envOr("MODEL_GROK_BASE", zenMainGateway)},
		// OpenCode Zen main-gateway profiles. The gateway exposes different wire
		// protocols per model family, hence the explicit Protocol rather than
		// treating the entire catalog as chat/completions-compatible.
		{Key: "gpt56sol", Label: "GPT 5.6 Sol (Zen)", Provider: ProviderZen, Model: envOr("MODEL_GPT56_SOL", "gpt-5.6-sol"), Effort: "high", InPerM: 5, OutPerM: 30, BaseURL: zenMainGateway, Protocol: "responses"},
		{Key: "gpt56terra", Label: "GPT 5.6 Terra (Zen)", Provider: ProviderZen, Model: envOr("MODEL_GPT56_TERRA", "gpt-5.6-terra"), Effort: "high", InPerM: 2.5, OutPerM: 15, BaseURL: zenMainGateway, Protocol: "responses"},
		{Key: "gpt56luna", Label: "GPT 5.6 Luna (Zen)", Provider: ProviderZen, Model: envOr("MODEL_GPT56_LUNA", "gpt-5.6-luna"), Effort: "high", InPerM: 1, OutPerM: 6, BaseURL: zenMainGateway, Protocol: "responses"},
		{Key: "grok-build-01", Label: "Grok Build 0.1 (Zen)", Provider: ProviderZen, Model: envOr("MODEL_GROK_BUILD_01", "grok-build-0.1"), Effort: "high", InPerM: 1, OutPerM: 2, BaseURL: zenMainGateway},
		{Key: "sonnet5-zen", Label: "Claude Sonnet 5 (Zen)", Provider: ProviderZen, Model: envOr("MODEL_SONNET5_ZEN", "claude-sonnet-5"), Effort: "max", InPerM: 2, OutPerM: 10, BaseURL: zenMainGateway, Protocol: "messages"},
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
	case ProviderMoonshot:
		return c.MoonshotAPIKey != ""
	}
	return false
}

// ModelProfileByKey looks up a profile by key (regardless of whether it's
// currently enabled). A key with the "custom:" prefix is an operator-typed
// ad-hoc model (see ParseCustomModel) rather than a catalog preset.
func (c Config) ModelProfileByKey(key string) (ModelProfile, bool) {
	if p, ok := ParseCustomModel(key); ok {
		return p, true
	}
	for _, p := range allProfiles() {
		if p.Key == key {
			return p, true
		}
	}
	return ModelProfile{}, false
}

// customPrefix marks an operator-typed model spec stored in a project's
// planner/impl/review profile column instead of a catalog preset key.
const customPrefix = "custom:"

// ParseCustomModel parses an operator-typed model spec into an ad-hoc profile,
// so any opencode-reachable model can be selected without a hardcoded catalog
// entry. Format: "custom:<family>/<model>[#effort]" — e.g.
// "custom:zen/grok-code-fast#high", "custom:anthropic/claude-opus-4-8#max",
// "custom:moonshot/kimi-k3". Families map to Forge's configured provider keys
// and the sandbox's opencode routing:
//   - anthropic → native Anthropic (ANTHROPIC_API_KEY)
//   - zen       → OpenCode Zen "go" gateway via opencode's NATIVE opencode-go
//     provider (the full model list: deepseek/minimax/kimi/glm/…)
//   - zen-shim  → the same go gateway via the generic openai-compatible shim
//     (lite list; a fallback for models the native provider mishandles)
//   - zen-main  → the MAIN Zen gateway (/zen/v1, e.g. the grok family)
//   - moonshot  → Moonshot first-party (MOONSHOT_API_KEY)
//
// Cost is left at zero (unknown) — the /admin cost column just shows blank.
// Returns ok=false for a malformed spec (unknown family, empty model); the
// caller then falls back to the default profile.
func ParseCustomModel(spec string) (ModelProfile, bool) {
	if !strings.HasPrefix(spec, customPrefix) {
		return ModelProfile{}, false
	}
	body := strings.TrimSpace(spec[len(customPrefix):])
	effort := ""
	if i := strings.LastIndex(body, "#"); i >= 0 {
		effort = strings.TrimSpace(body[i+1:])
		body = strings.TrimSpace(body[:i])
	}
	fam, model, ok := strings.Cut(body, "/")
	fam, model = strings.TrimSpace(fam), strings.TrimSpace(model)
	if !ok || fam == "" || model == "" {
		return ModelProfile{}, false
	}
	if effort != "" && !validEffort(effort) {
		effort = "" // ignore a bad effort rather than reject the whole spec
	}
	p := ModelProfile{Key: spec, Label: fam + "/" + model, Model: model, Effort: effort}
	switch fam {
	case "anthropic":
		p.Provider = ProviderAnthropic
	case "zen":
		p.Provider, p.NativeGo = ProviderZen, true // full go list via the native provider
	case "zen-shim":
		p.Provider = ProviderZen // go gateway, generic openai-compatible shim
	case "zen-main":
		p.Provider, p.BaseURL = ProviderZen, zenMainGateway // grok family etc.
	case "moonshot":
		p.Provider = ProviderMoonshot
	default:
		return ModelProfile{}, false
	}
	return p, true
}

// CustomModelKey turns an operator-typed spec ("<family>/<model>[#effort]")
// into the stored profile key by adding the custom: prefix (idempotent).
func CustomModelKey(typed string) string {
	typed = strings.TrimSpace(typed)
	if typed == "" || strings.HasPrefix(typed, customPrefix) {
		return typed
	}
	return customPrefix + typed
}

// CustomModelSpec returns the operator-typed spec for a stored custom key (for
// prefilling the /admin field), or "" when key is a preset/empty.
func CustomModelSpec(key string) string {
	if strings.HasPrefix(key, customPrefix) {
		return strings.TrimPrefix(key, customPrefix)
	}
	return ""
}

// validEffort reports whether s is a recognized reasoning-effort level.
func validEffort(s string) bool {
	switch s {
	case "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
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
	case ProviderMoonshot:
		r.APIKey = c.MoonshotAPIKey
		r.BaseURL = p.BaseURL
		if r.BaseURL == "" {
			r.BaseURL = moonshotGateway
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
