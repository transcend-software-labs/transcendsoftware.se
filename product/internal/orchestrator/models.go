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

// RebuildWithModels is the operator's experiment action: set the project's
// planner + implementation profiles and re-run the whole pipeline (re-plan with
// the chosen planner, then build with the chosen impl), bypassing the customer
// approval gate. Async — a rebuild takes minutes. Intended for test projects;
// it replaces the current plan + preview.
func (o *Orchestrator) RebuildWithModels(projectID, plannerProfile, implProfile string) {
	go func() {
		ctx := context.Background()
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			o.log.Error("rebuild with models: load", "project", projectID, "err", err)
			return
		}
		p.PlannerProfile, p.ImplProfile = plannerProfile, implProfile
		if err := o.save(ctx, p); err != nil {
			o.log.Error("rebuild with models: save profiles", "project", projectID, "err", err)
			return
		}
		if err := o.runPlanGateBuild(ctx, projectID); err != nil {
			o.log.Error("rebuild with models: plan", "project", projectID, "err", err)
			return
		}
		// Plan+gate stop at awaiting_approval on allow; the operator is the
		// approver here, so kick the build.
		if p2, err := o.store.ProjectByID(ctx, projectID); err == nil && p2.Status == project.StatusAwaitingApproval {
			o.ApprovePlan(projectID)
		}
	}()
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
	if key == "" {
		if kind == plannerKind {
			key = o.modelCfg.DefaultPlannerProfile
		} else {
			key = o.modelCfg.DefaultImplProfile
		}
	}
	return o.modelCfg.ResolveModel(key)
}

// modelLabel renders a model + effort for stamping on an iteration.
func modelLabel(model, effort string) string {
	if effort == "" {
		return model
	}
	return model + " · " + effort
}
