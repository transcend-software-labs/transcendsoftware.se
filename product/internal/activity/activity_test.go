package activity

import (
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

func TestPageProgress(t *testing.T) {
	tr := NewTracker()
	tr.SetPages("p1", []project.PlanPage{
		{Slug: "start", Paths: []string{"index", "home"}, Names: map[string]string{"sv": "Startsidan", "en": "the home page"}},
		{Slug: "kontakt", Paths: []string{"kontakt", "contact"}, Names: map[string]string{"sv": "Kontakt"}},
	})

	// A line touching the home page's paths marks it building.
	tr.Observe("p1", "→ write /workspace/internal/web/templates/index.html")
	if pg, ok := tr.Building("p1"); !ok || pg.Slug != "start" {
		t.Fatalf("building = %+v ok=%v, want start", pg, ok)
	}
	if got := mustPage(t, tr, "start").Status; got != pageBuilding {
		t.Errorf("start status = %q", got)
	}

	// The agent's authoritative marker completes it.
	tr.Observe("p1", "FORGE_PAGE_DONE: start")
	if got := mustPage(t, tr, "start").Status; got != pageDone {
		t.Errorf("after marker start = %q, want done", got)
	}
	if _, ok := tr.Building("p1"); ok {
		t.Error("no page should be building after start completed")
	}

	// Names resolve with fallback: sv present, en+ru absent → slug.
	if got := mustPage(t, tr, "kontakt").Name("sv"); got != "Kontakt" {
		t.Errorf("kontakt sv name = %q", got)
	}
	if got := mustPage(t, tr, "kontakt").Name("ru"); got != "kontakt" {
		t.Errorf("kontakt ru name = %q, want slug fallback", got)
	}
}

func mustPage(t *testing.T, tr *Tracker, slug string) PageStatus {
	t.Helper()
	for _, p := range tr.Pages("p1") {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("page %q not found", slug)
	return PageStatus{}
}

func TestClassify(t *testing.T) {
	cases := map[string]Code{
		"→ bash: fly deploy --remote-only":                    Deploying,
		"→ bash: node scripts/audit.js":                       Reviewing,
		"Design audit: clean ✓":                               Reviewing,
		"→ bash: go test ./...":                               Testing,
		"→ bash: node scripts/flow.js flows/order.json":       Testing,
		"→ write /workspace/migrations/0002_products.sql":     Database,
		"→ write /workspace/internal/web/static/app.css":      Styling,
		"→ write /workspace/internal/web/templates/menu.html": Building,
		"→ edit /workspace/internal/web/handlers.go":          Building,
		"Preparing the Forge starter app…":                    Preparing,
		"Sandbox ready, starting the agent…":                  Preparing,
	}
	for line, want := range cases {
		got, ok := classify(line)
		if !ok || got != want {
			t.Errorf("classify(%q) = %q,%v want %q", line, got, ok, want)
		}
	}
	if _, ok := classify("some free text the agent wrote"); ok {
		t.Error("free text should not classify")
	}
}

func TestTrackerDebounce(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker()
	tr.now = func() time.Time { return now }

	tr.Observe("p1", "→ write internal/web/templates/index.html")
	if got := tr.Current("p1"); got != Building {
		t.Fatalf("first promote = %q, want building", got)
	}

	// A different code within the hold window must NOT replace it…
	now = now.Add(3 * time.Second)
	tr.Observe("p1", "→ write internal/web/static/app.css")
	if got := tr.Current("p1"); got != Building {
		t.Fatalf("promoted %q during hold, want building kept", got)
	}

	// …but after the hold it does.
	now = now.Add(minHold)
	tr.Observe("p1", "→ write internal/web/static/app.css")
	if got := tr.Current("p1"); got != Styling {
		t.Fatalf("after hold = %q, want styling", got)
	}
}

func TestTrackerStallAndClear(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker()
	tr.now = func() time.Time { return now }

	tr.Observe("p1", "→ bash: go test ./...")
	now = now.Add(stallAfter)
	if got := tr.Current("p1"); got != TakingLonger {
		t.Fatalf("silent build = %q, want taking_longer", got)
	}

	// Any event — even unclassified — restores liveness.
	tr.Observe("p1", "thinking…")
	if got := tr.Current("p1"); got != Testing {
		t.Fatalf("after liveness = %q, want testing", got)
	}

	tr.Clear("p1")
	if got := tr.Current("p1"); got != "" {
		t.Fatalf("after clear = %q, want empty", got)
	}
	if got := tr.Current("unknown"); got != "" {
		t.Fatalf("untracked = %q, want empty", got)
	}
}
