package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// TestAdminRendersFindings verifies the /admin template renders the impeccable
// design-audit checklist for accepted projects and the compact flag on previews.
// It parses the real template set exactly as NewServer does, so a missing field
// or bad pipeline would fail here (html/template errors on unknown struct fields).
func TestAdminRendersFindings(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	flagged := reviewItem{Project: &project.Project{ID: "p1", Name: "Bakery", Findings: []project.Finding{
		{Antipattern: "ai-color-palette", Name: "AI color palette", Severity: "warning",
			Description: "Purple gradients are a tell.", File: "internal/web/static/app.css", Line: 3,
			Snippet: "Purple/violet accent colors detected"},
	}}}
	clean := reviewItem{Project: &project.Project{ID: "p2", Name: "Clean Co", Findings: []project.Finding{}}}
	preview := reviewItem{Project: &project.Project{ID: "p3", Name: "Prev", PreviewURL: "https://x.fly.dev",
		Findings: []project.Finding{{Name: "Cramped padding", Severity: "warning"}}}}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "admin", View{Title: "Operator review", IsAdmin: true, CSRF: "x",
		Data: adminView{Accepted: []reviewItem{flagged, clean}, Previews: []reviewItem{preview}}}); err != nil {
		t.Fatalf("render admin: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Design audit",                          // section label on the flagged card
		"AI color palette",                      // the finding name
		"Purple/violet accent colors detected",  // the snippet
		"internal/web/static/app.css:3",         // file:line
		"Design audit: clean ✓",                 // the clean card
		"⚑ 1",                                   // the preview flag
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin render missing %q", want)
		}
	}
}
