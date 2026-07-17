package orchestrator

import (
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// TestResolveProfile covers the per-project model resolution: empty = track the
// global default, an explicit key = override, and a retired/unknown key falls
// back to the current default so an old project never breaks on a stale choice.
func TestResolveProfile(t *testing.T) {
	cfg := config.Config{
		AnthropicAPIKey: "sk", ZenAPIKey: "zk", ZenBaseURL: "https://zen",
		DefaultPlannerProfile: "glm", DefaultImplProfile: "kimi",
	}
	o := &Orchestrator{modelCfg: &cfg}

	cases := []struct {
		name string
		key  string
		kind profileKind
		want string // resolved profile key, "" = expect ok==false
	}{
		{"empty planner → default", "", plannerKind, "glm"},
		{"empty impl → default", "", implKind, "kimi"},
		{"explicit override", "fable5", implKind, "fable5"},
		{"retired override → default", "was-removed", implKind, "kimi"},
	}
	for _, c := range cases {
		rm, ok := o.resolveProfile(c.key, c.kind)
		if !ok || rm.Key != c.want {
			t.Errorf("%s: got %q ok=%v, want %q", c.name, rm.Key, ok, c.want)
		}
	}

	// No registry wired → not ok (caller falls back to the global client).
	if _, ok := (&Orchestrator{}).resolveProfile("glm", implKind); ok {
		t.Error("nil modelCfg should resolve ok=false")
	}
}

// The intake follows the SAME planner profile as the plan step: the questions
// and design options come from the model that will plan the site. With no
// registry wired (dev), both fall back to the global client.
func TestIntakeFor_TracksPlannerProfile(t *testing.T) {
	cfg := config.Config{
		AnthropicAPIKey: "sk", ZenAPIKey: "zk", ZenBaseURL: "https://zen",
		DefaultPlannerProfile: "glm", DefaultImplProfile: "kimi",
	}
	def := llm.NewFake()
	o := &Orchestrator{modelCfg: &cfg, intake: def}

	if got := o.intakeFor(&project.Project{PlannerProfile: "fable5"}); got == llm.Intake(def) {
		t.Error("a resolvable profile should build a per-project intake client, not the default")
	}

	// No registry wired → the global intake client.
	bare := &Orchestrator{intake: def}
	if got := bare.intakeFor(&project.Project{PlannerProfile: "fable5"}); got != llm.Intake(def) {
		t.Error("without a profile registry the default intake client must be used")
	}
}
