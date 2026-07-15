package orchestrator

import (
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
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
