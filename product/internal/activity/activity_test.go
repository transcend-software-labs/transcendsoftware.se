package activity

import (
	"testing"
	"time"
)

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
