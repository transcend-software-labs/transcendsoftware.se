package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

func TestSlugifyHost(t *testing.T) {
	cases := map[string]string{
		"Bageriet":          "bageriet",
		"Mitt Lilla Bageri": "mitt-lilla-bageri",
		"Kärlekens Ö":       "karlekens-o",
		"  Trim -- me  ":    "trim-me",
		"日本語":               "", // nothing usable → caller falls back
		"Café 24/7!":        "caf-247",
		"already-a-slug-99": "already-a-slug-99",
	}
	for in, want := range cases {
		if got := slugifyHost(in); got != want {
			t.Errorf("slugifyHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreviewHost(t *testing.T) {
	id := "a1fa8116e9e51b08f80a6e919122eefe"
	if got := previewHost("Bageriet", id, 6); got != "bageriet-a1fa81" {
		t.Errorf("previewHost = %q", got)
	}
	// Unusable name falls back to "site"; long names are capped.
	if got := previewHost("日本語", id, 6); got != "site-a1fa81" {
		t.Errorf("fallback host = %q", got)
	}
	long := previewHost(strings.Repeat("long-name-", 10), id, 6)
	if len(long) > 40 || !strings.HasSuffix(long, "-a1fa81") {
		t.Errorf("long name not capped sanely: %q (len %d)", long, len(long))
	}
}

// urlVerifier fails exactly the URLs matching failSubstr, recording every call.
type urlVerifier struct {
	failSubstr string
	seen       []string
}

func (v *urlVerifier) Verify(_ context.Context, url string) error {
	v.seen = append(v.seen, url)
	if v.failSubstr != "" && strings.Contains(url, v.failSubstr) {
		return errors.New("host not reachable")
	}
	return nil
}

func seedPreviewProject(t *testing.T, st store.Store, id, name string) *project.Project {
	t.Helper()
	ctx := context.Background()
	_ = st.CreateUser(ctx, &user.User{ID: "u-" + id, Email: id + "@example.com", CreatedAt: time.Now().UTC()})
	p := &project.Project{
		ID: id, UserID: "u-" + id, Name: name, Status: project.StatusPreviewReady,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return p
}

func TestBrandedPreviewURL_OffByDefault(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	p := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")

	got := orch.brandedPreviewURL(context.Background(), p, "https://forge-aaaabbbbcccc.fly.dev")
	if got != "https://forge-aaaabbbbcccc.fly.dev" || p.PreviewHost != "" {
		t.Fatalf("feature off must be a no-op: url=%q host=%q", got, p.PreviewHost)
	}
}

func TestBrandedPreviewURL_AssignsHostAndBrands(t *testing.T) {
	st := store.NewMemory()
	v := &urlVerifier{}
	orch, _ := newTestOrchWithVerifier(st, v)
	orch.SetPreviewDomain("forge.example.se")
	p := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")

	got := orch.brandedPreviewURL(context.Background(), p, "https://forge-aaaabbbbcccc.fly.dev")
	want := "https://bageriet-aaaabb.forge.example.se"
	if got != want {
		t.Fatalf("branded url = %q, want %q", got, want)
	}
	if p.PreviewHost != "bageriet-aaaabb" {
		t.Fatalf("preview host = %q", p.PreviewHost)
	}
	if len(v.seen) != 1 || v.seen[0] != want {
		t.Fatalf("should verify the branded url, saw %v", v.seen)
	}

	// Stable across rebuilds — the host is never regenerated.
	p.Name = "Renamed Bakery"
	if got := orch.brandedPreviewURL(context.Background(), p, "https://forge-aaaabbbbcccc.fly.dev"); got != want {
		t.Fatalf("host must be stable after rename, got %q", got)
	}
}

func TestBrandedPreviewURL_FallsBackAndAlerts(t *testing.T) {
	st := store.NewMemory()
	v := &urlVerifier{failSubstr: "forge.example.se"} // branded host fails, direct works
	orch, _ := newTestOrchWithVerifier(st, v)
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	orch.SetPreviewDomain("forge.example.se")
	p := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")

	direct := "https://forge-aaaabbbbcccc.fly.dev"
	if got := orch.brandedPreviewURL(context.Background(), p, direct); got != direct {
		t.Fatalf("must fall back to the direct url, got %q", got)
	}
	if !sentTo(rec, "rasmus@example.com", "branded preview host failed") {
		t.Errorf("operator should be alerted; sent %+v", rec.all())
	}
}

func TestBrandedPreviewURL_HostCollisionExtends(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetPreviewDomain("forge.example.se")

	// Another project already owns "bageriet-aaaabb".
	other := seedPreviewProject(t, st, "ffffbbbbccccdddd0000111122223333", "Other")
	other.PreviewHost = "bageriet-aaaabb"
	if err := st.UpdateProject(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	p := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")
	got := orch.brandedPreviewURL(context.Background(), p, "https://x.fly.dev")
	if p.PreviewHost != "bageriet-aaaabbbbcc" { // extended to id[:10]
		t.Fatalf("collision should extend the id suffix, got host %q (url %q)", p.PreviewHost, got)
	}
}

func TestBackfillPreviewHosts(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetPreviewDomain("forge.example.se")
	ctx := context.Background()

	// A legacy project on a direct URL, one already branded, one with no build.
	legacy := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")
	legacy.PreviewURL = "https://forge-aaaabbbbcccc.fly.dev"
	_ = st.UpdateProject(ctx, legacy)
	branded := seedPreviewProject(t, st, "bbbbccccddddeeee0000111122223333", "Kiosk")
	branded.PreviewHost, branded.PreviewURL = "kiosk-bbbbcc", "https://kiosk-bbbbcc.forge.example.se"
	_ = st.UpdateProject(ctx, branded)
	unbuilt := seedPreviewProject(t, st, "ccccddddeeeeffff0000111122223333", "Empty")

	orch.BackfillPreviewHosts(ctx)

	got, _ := st.ProjectByID(ctx, legacy.ID)
	if got.PreviewHost != "bageriet-aaaabb" || got.PreviewURL != "https://bageriet-aaaabb.forge.example.se" {
		t.Fatalf("legacy not backfilled: host=%q url=%q", got.PreviewHost, got.PreviewURL)
	}
	if got, _ := st.ProjectByID(ctx, branded.ID); got.PreviewURL != "https://kiosk-bbbbcc.forge.example.se" {
		t.Errorf("already-branded project must be untouched, got %q", got.PreviewURL)
	}
	if got, _ := st.ProjectByID(ctx, unbuilt.ID); got.PreviewHost != "" || got.PreviewURL != "" {
		t.Errorf("project without a build must be untouched, got host=%q url=%q", got.PreviewHost, got.PreviewURL)
	}
}

func TestNormalizeCustomerHostname_RejectsPreviewDomain(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetPreviewDomain("forge.example.se")

	if _, ok := orch.normalizeCustomerHostname("bageriet-a1.forge.example.se"); ok {
		t.Error("a host under the preview domain must be rejected")
	}
	if _, ok := orch.normalizeCustomerHostname("forge.example.se"); ok {
		t.Error("the preview domain itself must be rejected")
	}
	if _, ok := orch.normalizeCustomerHostname("acme.se"); !ok {
		t.Error("a normal customer domain must pass")
	}
	if _, ok := orch.normalizeCustomerHostname("x.fly.dev"); ok {
		t.Error("fly.dev must still be rejected")
	}
}
