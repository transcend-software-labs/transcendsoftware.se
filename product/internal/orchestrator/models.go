package orchestrator

// Per-build model selection. An operator can pick the planner + implementation
// model per project from /admin (config.ModelProfile registry). When a project
// hasn't overridden them — or the chosen profile isn't configured — resolution
// falls back to the global wiring (o.planner / the builder's default env), so
// customer builds are unchanged.

import (
	"context"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// SetModelProfiles enables per-build model selection from the config registry.
func (o *Orchestrator) SetModelProfiles(cfg config.Config) { o.modelCfg = &cfg }

// SetProjectModels persists the operator's planner + implementation choice on a
// project WITHOUT building — the next build the project runs (a retry, a change,
// or a reiterate) resolves them. Empty keys mean "track Forge's global default"
// (so upgrading the global models still reaches every non-overridden project); a
// set key is an explicit per-project override that sticks. Synchronous: a quick
// store write.
func (o *Orchestrator) SetProjectModels(ctx context.Context, projectID, plannerProfile, implProfile string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	p.PlannerProfile, p.ImplProfile = plannerProfile, implProfile
	return o.save(ctx, p)
}

// plannerFor returns the planner client + a display label for a project's build,
// honoring its PlannerProfile (or the default), falling back to the global
// planner when profiles aren't wired or the key doesn't resolve.
func (o *Orchestrator) plannerFor(p *project.Project) (llm.Planner, string) {
	rm, ok := o.resolveProfile(p.PlannerProfile, plannerKind)
	if !ok {
		return o.planner, o.plannerModel
	}
	return llm.NewPlanner(string(rm.Provider), rm.BaseURL, rm.APIKey, rm.Model, rm.Effort), modelLabel(rm.Model, rm.Effort)
}

// implFor returns the implementation model override + a display label. A zero
// ModelSpec means "use the builder's configured default" (current behavior).
func (o *Orchestrator) implFor(p *project.Project) (builder.ModelSpec, string) {
	rm, ok := o.resolveProfile(p.ImplProfile, implKind)
	if !ok {
		return builder.ModelSpec{}, o.implModel
	}
	return builder.ModelSpec{
		Provider: string(rm.Provider), BaseURL: rm.BaseURL, APIKey: rm.APIKey,
		Model: rm.Model, Effort: rm.Effort,
	}, modelLabel(rm.Model, rm.Effort)
}

type profileKind int

const (
	plannerKind profileKind = iota
	implKind
)

// resolveProfile resolves a project's chosen profile key (or the configured
// default for that role) to a usable model.
func (o *Orchestrator) resolveProfile(key string, kind profileKind) (config.ResolvedModel, bool) {
	if o.modelCfg == nil {
		return config.ResolvedModel{}, false
	}
	def := o.modelCfg.DefaultPlannerProfile
	if kind == implKind {
		def = o.modelCfg.DefaultImplProfile
	}
	if key == "" { // no per-project override → track Forge's global default
		key = def
	}
	if rm, ok := o.modelCfg.ResolveModel(key); ok {
		return rm, true
	}
	// The stored override no longer resolves (a model we retired, or its provider
	// key was removed) → fall back to the current default so the project keeps
	// building instead of erroring on a stale choice.
	if key != def {
		if rm, ok := o.modelCfg.ResolveModel(def); ok {
			return rm, true
		}
	}
	return config.ResolvedModel{}, false
}

// modelLabel renders a model + effort for stamping on an iteration.
func modelLabel(model, effort string) string {
	if effort == "" {
		return model
	}
	return model + " · " + effort
}
