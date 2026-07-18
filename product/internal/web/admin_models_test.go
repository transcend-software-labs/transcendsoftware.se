package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// TestAdminProjectModelPicker renders the operator project page with the model
// registry enabled (the path existing admin tests skip, since they configure no
// keys) — so the dropdowns, the save-models action, the per-iteration
// planner/impl labels and the ~cost all render without a template error.
func TestAdminProjectModelPicker(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	// Both keys set → all profiles enabled; XAI key → the build-agent picker.
	cfg := config.Config{AnthropicAPIKey: "sk", ZenAPIKey: "zk", ZenBaseURL: "https://zen", XAIAPIKey: "xai"}
	implModel, _ := cfg.ResolveModel("sonnet5")
	it := &project.Iteration{
		Number: 1, Status: project.StatusPreviewReady,
		ImplModel: "claude-sonnet-5 · max", PlannerModel: "claude-fable-5 · xhigh",
		Tokens: 124000, TokensInput: 100000,
	}
	view := adminProjectView{
		Item:           reviewItem{Project: &project.Project{ID: "exp1", Name: "Test Bakery"}},
		Iterations:     []adminBuildRow{{Iteration: it, CostStr: formatPrice(int64(implModel.CostOre(it.TokensInput, it.Tokens)), "sek")}},
		Profiles:       cfg.ModelProfiles(),
		PlannerProfile: "fable5",
		ImplProfile:    "sonnet5",
		ReviewProfile:  "fable5",
		BuildAgent:     "grok",
		GrokAvailable:  cfg.GrokBuildEnabled(),
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "admin_project", View{Title: "op", IsAdmin: true, CSRF: "x", Lang: "en", Data: view}); err != nil {
		t.Fatalf("render admin_project: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`/admin/projects/exp1/models`, // the save-only form action
		`name="planner_profile"`,
		`name="impl_profile"`,
		`name="review_profile"`, // the code-review picker (same profile set)
		`name="planner_custom"`, // free-form custom-model fields (any opencode model)
		`name="impl_custom"`,
		`name="review_custom"`,
		"Custom model format",   // the family reference
		`name="build_agent"`,    // agent picker (opencode | grok build)
		`value="grok" selected`, // the project's grok choice preselected
		"Grok Build (headless)",
		"— Forge default —",        // the track-the-global-default option
		"Claude Sonnet 5",          // a profile label
		"DeepSeek V4 Pro",          // a newly-added profile is in the dropdown
		`value="fable5" selected`,  // the current planner override preselected
		`value="sonnet5" selected`, // the current impl override preselected
		"claude-sonnet-5 · max",    // the iteration's impl model + effort
		"claude-fable-5 · xhigh",   // the iteration's planner model + effort
		"Save models",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin_project model picker missing %q", want)
		}
	}
	// The ~cost is rendered (124k tokens, 100k input, sonnet5 $2/$10 → ~4-5 kr).
	if !strings.Contains(out, " kr") {
		t.Errorf("expected a ~cost in kr; got:\n%s", out)
	}
}
