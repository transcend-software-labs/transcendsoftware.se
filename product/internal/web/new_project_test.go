package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
)

// TestNewProjectModelPicker: the model picker renders on the new-project form
// for an admin (so models can be chosen for the FIRST build), preselecting the
// configured defaults — and is absent for a customer.
func TestNewProjectModelPicker(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	cfg := config.Config{AnthropicAPIKey: "sk", ZenAPIKey: "zk", ZenBaseURL: "https://zen"}
	data := newProjectView{Profiles: cfg.ModelProfiles(), PlannerProfile: "glm", ImplProfile: "kimi"}

	// Admin: picker present, defaults preselected, form posts the fields.
	var admin bytes.Buffer
	if err := tmpl.ExecuteTemplate(&admin, "new_project", View{Lang: "en", IsAdmin: true, CSRF: "x", Data: data}); err != nil {
		t.Fatalf("render new_project (admin): %v", err)
	}
	out := admin.String()
	for _, want := range []string{
		`name="planner_profile"`,
		`name="impl_profile"`,
		"Claude Fable 5",
		"DeepSeek V4 Pro",
		`value="glm" selected`,
		`value="kimi" selected`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin new_project missing %q", want)
		}
	}

	// Customer: no picker, and a zero-value Data must not error.
	var cust bytes.Buffer
	if err := tmpl.ExecuteTemplate(&cust, "new_project", View{Lang: "en", IsAdmin: false, CSRF: "x", Data: newProjectView{}}); err != nil {
		t.Fatalf("render new_project (customer): %v", err)
	}
	if strings.Contains(cust.String(), `name="planner_profile"`) {
		t.Errorf("customers must not see the model picker")
	}
}
