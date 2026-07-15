package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// TestAdminDashboardFailedSection: a failed project is surfaced on /admin with a
// link to its operator page — so it's reachable to retry / change models instead
// of being orphaned (the bug: failed fell into no status bucket).
func TestAdminDashboardFailedSection(t *testing.T) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	view := adminView{
		Failed: []waitingItem{{
			ID: "f1", Name: "Slateline CRM", Status: project.StatusFailed,
			OwnerEmail: "rasmus@transcendsoftware.se", Since: time.Unix(1_700_000_000, 0).UTC(),
		}},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "admin", View{Lang: "en", IsAdmin: true, CSRF: "x", Data: view}); err != nil {
		t.Fatalf("render admin: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Failed",             // the section heading
		"Slateline CRM",      // the failed project's name
		`/admin/projects/f1`, // navigable link to its operator page
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin dashboard Failed section missing %q", want)
		}
	}

	// When nothing has failed, the section is omitted entirely (no empty panel).
	var empty bytes.Buffer
	if err := tmpl.ExecuteTemplate(&empty, "admin", View{Lang: "en", IsAdmin: true, CSRF: "x", Data: adminView{}}); err != nil {
		t.Fatalf("render admin (no failures): %v", err)
	}
	if strings.Contains(empty.String(), "<h2>Failed</h2>") {
		t.Errorf("Failed section should be hidden when there are no failures")
	}
}
