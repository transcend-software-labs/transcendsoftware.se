package llm

// NewPlanner builds a Planner client from a resolved model spec, so the
// orchestrator can run the plan step on a per-build-chosen model. provider is
// "anthropic" (native Messages API) or anything else (OpenAI-compatible, e.g.
// the OpenCode Zen gateway). effort is best-effort for the OpenAI-compatible
// path and exact (output_config.effort) for Anthropic.
//
// Kept a plain-string signature (not config types) so internal/llm stays free
// of an import cycle; callers pass values off config.ResolvedModel.
func NewPlanner(provider, baseURL, apiKey, model, effort string) Planner {
	if provider == "anthropic" {
		return NewAnthropic(apiKey, model, effort)
	}
	return NewOpenAICompat(baseURL, apiKey, model).WithEffort(effort)
}

// NewIntake builds an Intake client from the same resolved model spec. The
// clarifying questions and design options should come from the SAME model
// that will plan the site — they shape what it plans, and a mismatched pair
// means the intake asks for things the planner then ignores (or vice versa).
func NewIntake(provider, baseURL, apiKey, model, effort string) Intake {
	if provider == "anthropic" {
		return NewAnthropic(apiKey, model, effort)
	}
	return NewOpenAICompat(baseURL, apiKey, model).WithEffort(effort)
}
