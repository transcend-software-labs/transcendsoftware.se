package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/notify"
	"github.com/transcend-software-labs/rasmus-ai/internal/oauth"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithConfig(t, config.Config{AdminEmail: "admin@example.com"})
}

func newTestServerWithConfig(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()
	srv, _, _ := newTestServerAuth(t, cfg, nil)
	return srv
}

// recNotifier records magic-link emails for tests.
type recNotifier struct{ bodies []string }

func (n *recNotifier) Send(_ context.Context, _, _, body string) error {
	n.bodies = append(n.bodies, body)
	return nil
}

// newTestServerAuth builds a server, optionally wiring a notifier for
// magic-link tests, and returns the httptest server plus the backing store
// (for tests that seed projects directly).
func newTestServerAuth(t *testing.T, cfg config.Config, notifier *recNotifier) (*httptest.Server, store.Store, *recNotifier) {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var reg *oauth.Registry
	if cfg.GoogleClientID != "" {
		reg = oauth.NewRegistry(oauth.Google(cfg.GoogleClientID, cfg.GoogleClientSecret))
	}
	if notifier != nil || reg != nil {
		var n notify.Notifier
		if notifier != nil {
			n = notifier
		}
		srv.SetAuth(reg, n)
	}
	return httptest.NewServer(srv.Handler()), st, notifier
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func csrfToken(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	resp, err := c.Get(base + "/projects/new")
	if err != nil {
		t.Fatalf("get new project: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := csrfRe.FindSubmatch(body)
	if m == nil {
		t.Fatal("no csrf token in form")
	}
	return string(m[1])
}

func signedInClient(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/signup", url.Values{
		"email": {"neighbour@example.com"}, "password": {"apples12345"},
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signup status = %d", resp.StatusCode)
	}
	return c
}

func TestQuota_DailyProjectCap(t *testing.T) {
	srv := newTestServerWithConfig(t, config.Config{AdminEmail: "admin@example.com", MaxProjectsPerDay: 1})
	defer srv.Close()
	c := signedInClient(t, srv.URL)

	tok := csrfToken(t, c, srv.URL)
	resp, err := c.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"A brochure site for an apple farm selling juice locally"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	projURL := resp.Request.URL.String() // followed the redirect to the project page
	resp.Body.Close()

	// Wait for the project to settle in needs_input (intake questions shown),
	// so the second create hits the daily cap, not the one-at-a-time rule.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := c.Get(projURL)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(b), "A few quick questions") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	tok2 := csrfToken(t, c, srv.URL)
	resp2, err := c.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"Another site for the very same apple farm please"}, "csrf_token": {tok2},
	})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	b, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(b), "Daily limit reached") {
		t.Fatalf("expected daily-cap rejection, got status %d", resp2.StatusCode)
	}
}

func TestFullFlow_IntakeToPreview(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)

	// Create a project (carries a valid CSRF token).
	tok := csrfToken(t, c, srv.URL)
	resp, err := c.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"A brochure site for an apple farm selling juice locally"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	pid := path.Base(resp.Request.URL.Path)

	// Intake should be asking clarifying questions.
	if !strings.Contains(string(body), "A few quick questions") {
		t.Fatal("expected clarifying questions after create")
	}

	// Answer them, picking one of the suggested design directions (the fake
	// intake suggests "Clean & minimal").
	resp, err = c.PostForm(srv.URL+"/projects/"+pid+"/answer", url.Values{
		"answers":       {"brochure only; I have photos; Swedish"},
		"design_choice": {"Clean & minimal"},
		"csrf_token":    {tok},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	resp.Body.Close()

	// Poll the status endpoint. While building it returns the status fragment;
	// once the build leaves the polling state it asks HTMX to reload the page
	// (HX-Refresh), which is our signal the flow finished.
	deadline := time.Now().Add(8 * time.Second)
	var done bool
	for time.Now().Before(deadline) {
		r, _ := c.Get(srv.URL + "/projects/" + pid + "/status")
		refresh := r.Header.Get("HX-Refresh")
		r.Body.Close()
		if refresh == "true" {
			done = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !done {
		t.Fatal("project never left the building state")
	}

	// The full project page should now show the ready preview and the design
	// direction that was picked.
	r, _ := c.Get(srv.URL + "/projects/" + pid)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	page := string(b)
	if !strings.Contains(page, "Preview ready") {
		t.Fatal("project page does not show preview ready after build")
	}
	if !strings.Contains(page, "Design direction") || !strings.Contains(page, "Clean &amp; minimal") {
		t.Fatal("project page does not show the chosen design direction")
	}
}

func TestLoginPage_MethodGating(t *testing.T) {
	cases := []struct {
		name              string
		cfg               config.Config
		wantGoogle        bool
		wantMagic         bool
		wantVisiblePwForm bool // password form shown directly (not in <details>)
	}{
		{
			name:              "google on, magic off",
			cfg:               config.Config{GoogleClientID: "gid", GoogleClientSecret: "gsec"},
			wantGoogle:        true,
			wantMagic:         false,
			wantVisiblePwForm: true,
		},
		{
			name:       "google on, magic on",
			cfg:        config.Config{GoogleClientID: "gid", GoogleClientSecret: "gsec", MagicLinkEnabled: true},
			wantGoogle: true,
			wantMagic:  true,
		},
		{
			name:       "no google, magic on",
			cfg:        config.Config{MagicLinkEnabled: true},
			wantGoogle: false,
			wantMagic:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWithConfig(t, tc.cfg)
			defer srv.Close()
			resp, err := http.Get(srv.URL + "/login")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := string(b)
			if got := strings.Contains(body, "Continue with Google"); got != tc.wantGoogle {
				t.Errorf("Google button present=%v, want %v", got, tc.wantGoogle)
			}
			if got := strings.Contains(body, "Email me a login link"); got != tc.wantMagic {
				t.Errorf("magic-link form present=%v, want %v", got, tc.wantMagic)
			}
			if tc.wantVisiblePwForm && strings.Contains(body, "password-login") {
				t.Error("password form should be shown directly when magic-link is off, not tucked in <details>")
			}
		})
	}
}

// TestProjectStatus_BuildingStreamsInline guards the fix for "had to refresh
// for live build streaming to start": the live-log element must live inside the
// polled #status fragment (so the 2s poll swaps it in the moment a build
// starts, no manual refresh), carry hx-preserve (so the SSE connection + logs
// survive later polls), and NOT appear for other statuses.
func TestProjectStatus_BuildingStreamsInline(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, err := st.UserByEmail(ctx, "neighbour@example.com")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	seed := func(status project.Status) string {
		p := &project.Project{ID: "proj-" + string(status), UserID: u.ID, Name: "Test", Status: status}
		if err := st.CreateProject(ctx, p); err != nil {
			t.Fatalf("create %s: %v", status, err)
		}
		return p.ID
	}
	fetch := func(path string) string {
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	get := func(id string) string { return fetch("/projects/" + id + "/status") }
	getPage := func(id string) string { return fetch("/projects/" + id) }

	buildingID := seed(project.StatusBuilding)
	// The live log is DECOUPLED from the polled status fragment: it lives as a
	// stable sibling on the project page so the status poll can't recreate it
	// (which caused flicker + scroll-reset). The /status fragment must not carry
	// it...
	if frag := get(buildingID); strings.Contains(frag, "livelog") {
		t.Errorf("status fragment must not carry the live log (it is a stable sibling now):\n%s", frag)
	}
	// ...and the full project page while building must render it once.
	page := getPage(buildingID)
	for _, want := range []string{
		`sse-connect="/projects/` + buildingID + `/stream"`,
		`id="livelog"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("building project page missing %q:\n%s", want, page)
		}
	}
	// A finished project page (delivered) must not stream a live log.
	if strings.Contains(getPage(seed(project.StatusDelivered)), "livelog") {
		t.Errorf("a finished project page should not stream a live log")
	}
}

// TestRetry_FailedBuildRerun guards the recovery path: a build left in the
// terminal failed state (e.g. interrupted by a deploy) can be retried by the
// customer and runs to a live preview again.
func TestRetry_FailedBuildRerun(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, err := st.UserByEmail(ctx, "neighbour@example.com")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{
		ID: "proj-retry", UserID: u.ID, Name: "Retry me",
		Brief:        "A brochure site for an apple farm selling juice locally",
		Plan:         "Static brochure site with a hero and contact section.",
		Status:       project.StatusFailed,
		RejectReason: "The build was interrupted by a server restart.",
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A non-failed project can't be retried (gating): make a preview_ready one.
	ready := &project.Project{ID: "proj-ready", UserID: u.ID, Name: "Ready", Status: project.StatusPreviewReady}
	_ = st.CreateProject(ctx, ready)

	tok := csrfToken(t, c, srv.URL)
	post := func(id string) {
		resp, err := c.PostForm(srv.URL+"/projects/"+id+"/retry", url.Values{"csrf_token": {tok}})
		if err != nil {
			t.Fatalf("retry post: %v", err)
		}
		resp.Body.Close()
	}

	// Retrying the ready project is a no-op.
	post(ready.ID)
	if got, _ := st.ProjectByID(ctx, ready.ID); got.Status != project.StatusPreviewReady {
		t.Fatalf("non-failed retry should be a no-op, status = %q", got.Status)
	}

	// Retrying the failed project re-runs the build to a live preview.
	post(p.ID)
	deadline := time.Now().Add(8 * time.Second)
	var final project.Status
	for time.Now().Before(deadline) {
		if got, err := st.ProjectByID(ctx, p.ID); err == nil {
			final = got.Status
		}
		if final == project.StatusPreviewReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if final != project.StatusPreviewReady {
		t.Fatalf("retry did not reach preview_ready, got %q", final)
	}
}

func TestMagicLink_RequestConsumeLogsIn(t *testing.T) {
	rec := &recNotifier{}
	srv, _, _ := newTestServerAuth(t, config.Config{BaseURL: "http://app.example"}, rec)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	// Request a magic link.
	resp, err := c.PostForm(srv.URL+"/auth/magic", url.Values{"email": {"newuser@example.com"}})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if len(rec.bodies) != 1 {
		t.Fatalf("expected one email, got %d", len(rec.bodies))
	}

	// Extract the link and follow it (points at BaseURL; hit our test server).
	m := regexp.MustCompile(`/auth/magic\?token=[a-f0-9]+`).FindString(rec.bodies[0])
	if m == "" {
		t.Fatalf("no magic link in email: %q", rec.bodies[0])
	}
	resp, err = c.Get(srv.URL + m)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	final := resp.Request.URL.Path
	resp.Body.Close()
	if final != "/dashboard" {
		t.Fatalf("magic link should log in and land on /dashboard, got %s", final)
	}

	// A second use of the same link must fail (single-use).
	resp, _ = c.Get(srv.URL + m)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "invalid or has expired") {
		t.Error("magic link should be single-use")
	}
}

func TestCSRF_BlocksTokenlessPost(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)

	resp, err := c.PostForm(srv.URL+"/projects", url.Values{"brief": {"a site for an apple farm here"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 without CSRF token, got %d", resp.StatusCode)
	}
}

func TestAuth_DashboardRedirectsAnonymous(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect for anonymous, got %d", resp.StatusCode)
	}
}

func TestAdmin_ForbiddenForNonAdmin(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL) // neighbour@example.com, not the admin
	resp, err := c.Get(srv.URL + "/admin")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for non-admin /admin, got %d", resp.StatusCode)
	}
}
