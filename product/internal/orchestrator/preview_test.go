package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// storeCheckVerifier mimics the real reverse proxy's full rejection contract
// (web.servePreview): a branded URL is only "reachable" if the preview host
// resolves in the store AND the project's PreviewURL is already persisted —
// servePreview 404s an unknown host and 410s a project with an empty PreviewURL.
// Both must be true BEFORE verification, or a first preview falls back to the
// fly.dev URL.
type storeCheckVerifier struct {
	st     store.Store
	domain string
}

func (v *storeCheckVerifier) Verify(ctx context.Context, rawURL string) error {
	label := strings.TrimSuffix(strings.TrimPrefix(rawURL, "https://"), "."+v.domain)
	p, err := v.st.ProjectByPreviewHost(ctx, label)
	if err != nil {
		return errors.New("preview host not resolvable in store yet (proxy 404): " + label)
	}
	if p.PreviewURL == "" {
		return errors.New("preview URL not persisted yet (proxy 410): " + label)
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

// TestBrandedPreviewURL_PersistsHostBeforeVerify guards the fix for the first-
// preview fallback bug: both the host AND the PreviewURL must be in the store
// when we verify the branded URL, because our own proxy resolves the host that
// way and 410s a project with an empty PreviewURL. With storeCheckVerifier
// (which models both proxy rejections) this only passes if brandedPreviewURL
// saved host+url before probing.
func TestBrandedPreviewURL_PersistsHostBeforeVerify(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, &storeCheckVerifier{st: st, domain: "forge.example.se"})
	orch.SetPreviewDomain("forge.example.se")
	p := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")

	got := orch.brandedPreviewURL(context.Background(), p, "https://forge-aaaabbbbcccc.fly.dev")
	want := "https://bageriet-aaaabb.forge.example.se"
	if got != want {
		t.Fatalf("branded url = %q, want %q — host+url must be persisted before verify so the proxy serves it", got, want)
	}
}

// TestBrandedPreviewURL_FirstBuildSelfProbe reproduces the production first-
// build fallback end-to-end through the real self-probe HTTP path. The probe
// server enforces web.servePreview's contract against the live store: resolve
// the host, then 410 Gone any project whose PreviewURL is still empty. Before
// the fix, brandedPreviewURL persisted only the host, so the very first probe
// 410'd — every first preview fell back to the fly.dev URL and emailed the
// operator "branded preview host failed" (the "status 410 via self-probe" seen
// live for liljebaren-pizzeria-cd553a).
func TestBrandedPreviewURL_FirstBuildSelfProbe(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetPreviewDomain("forge.example.se")

	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := strings.TrimSuffix(r.Host, ".forge.example.se")
		p, err := st.ProjectByPreviewHost(r.Context(), label)
		if err != nil {
			t.Errorf("self-probe: host %q not resolvable (proxy would 404)", label)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The bug: probing before PreviewURL is persisted. Fail loudly, but still
		// answer 200 so the test doesn't wait out the 45s retry window.
		if p.PreviewURL == "" {
			t.Errorf("self-probe ran before PreviewURL persisted — proxy 410s (first-build race)")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()
	orch.SetPreviewSelfProbe(probe.URL)

	// A genuine first preview: a fresh project with no host and no URL yet.
	p := seedPreviewProject(t, st, "cd553a7cb4fa0f2fe220f30ae4ab068f", "Liljebaren Pizzeria")
	direct := "https://forge-cd553a7cb4fa.fly.dev"
	got := orch.brandedPreviewURL(context.Background(), p, direct)

	want := "https://liljebaren-pizzeria-cd553a.forge.example.se"
	if got != want {
		t.Fatalf("first-build branded probe = %q; want %q (410-race regression)", got, want)
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

// The healer upgrades a preview stuck on its direct fly.dev URL (the 45s
// build-time probe lost the race) to the branded host once it answers —
// without waiting for a restart's backfill. A host that still fails, and
// projects without a preview, stay untouched.
func TestHealBrandedPreviews_UpgradesWhenHostAnswers(t *testing.T) {
	st := store.NewMemory()
	v := &urlVerifier{}
	orch, _ := newTestOrchWithVerifier(st, v)
	orch.SetPreviewDomain("forge.example.se")
	ctx := context.Background()

	stuck := seedPreviewProject(t, st, "aaaabbbbccccdddd0000111122223333", "Bageriet")
	stuck.PreviewHost = "bageriet-aaaabb"
	stuck.PreviewURL = "https://forge-aaaabbbbcccc.fly.dev"
	_ = st.UpdateProject(ctx, stuck)

	broken := seedPreviewProject(t, st, "eeeeffff00001111222233334444aaaa", "Salongen")
	broken.PreviewHost = "salongen-eeeeff"
	broken.PreviewURL = "https://forge-eeeeffff0000.fly.dev"
	_ = st.UpdateProject(ctx, broken)

	v.failSubstr = "salongen" // the salon's branded host still doesn't answer

	orch.healBrandedPreviews(ctx)

	got, _ := st.ProjectByID(ctx, stuck.ID)
	if got.PreviewURL != "https://bageriet-aaaabb.forge.example.se" {
		t.Errorf("stuck preview should be upgraded, got %q", got.PreviewURL)
	}
	still, _ := st.ProjectByID(ctx, broken.ID)
	if still.PreviewURL != "https://forge-eeeeffff0000.fly.dev" {
		t.Errorf("a failing host must not be flipped, got %q", still.PreviewURL)
	}

	// Second tick: the already-branded project is not re-probed.
	v.seen = nil
	orch.healBrandedPreviews(ctx)
	for _, u := range v.seen {
		if strings.Contains(u, "bageriet") {
			t.Errorf("healed project should not be probed again, saw %v", v.seen)
		}
	}
}

// With a self-probe configured, branded verification asks OUR listener with
// the branded Host header — proving row → proxy → backend without hairpinning
// through the public edge (the vantage point that produced false alarms on
// real first-previews). A redirect (e.g. a CRM's / → /login) counts as
// serving; a proxy 404 (host not resolvable) does not.
func TestVerifyBranded_SelfProbeUsesHostHeader(t *testing.T) {
	var gotHost string
	status := http.StatusSeeOther // a CRM-style redirect must PASS
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		if status == http.StatusSeeOther {
			w.Header().Set("Location", "https://"+r.Host+"/login")
		}
		w.WriteHeader(status)
	}))
	defer web.Close()

	st := store.NewMemory()
	v := &urlVerifier{failSubstr: "https://"} // the public path must never be used
	orch, _ := newTestOrchWithVerifier(st, v)
	orch.SetPreviewDomain("forge.example.se")
	orch.SetPreviewSelfProbe(web.URL)

	if err := orch.verifyBranded(context.Background(), "bageriet-aaaabb"); err != nil {
		t.Fatalf("redirect via self-probe should verify, got %v", err)
	}
	if gotHost != "bageriet-aaaabb.forge.example.se" {
		t.Errorf("probe Host = %q, want the branded host", gotHost)
	}
	if len(v.seen) != 0 {
		t.Errorf("public verifier must not be used when self-probe is set, saw %v", v.seen)
	}

	// A proxy miss (404) fails within the ctx window.
	status = http.StatusNotFound
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := orch.verifyBranded(ctx, "unknown-host"); err == nil {
		t.Fatal("a 404 from the proxy must fail verification")
	}
}
