package web_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/imagegen"
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
	return newTestServerAuthImageGen(t, cfg, notifier, nil)
}

func newTestServerAuthImageGen(t *testing.T, cfg config.Config, notifier *recNotifier, images *imagegen.Client) (*httptest.Server, store.Store, *recNotifier) {
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
	if images != nil {
		srv.SetImageGen(images)
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
	ts := httptest.NewServer(srv.Handler())
	testStores.Store(ts.URL, st) // let sign-in helpers confirm the user's email
	return ts, st, notifier
}

// testStores maps a running test server's URL to its backing store, so the
// sign-in helpers can mark the freshly-signed-up account verified without
// threading the store through every call site. (Email verification is exercised
// on its own in the TestVerify_* tests.)
var testStores sync.Map // base URL → store.Store

func verifyTestUser(base, email string) {
	if v, ok := testStores.Load(base); ok {
		st := v.(store.Store)
		_ = st.MarkUserVerified(context.Background(), email)
		if u, err := st.UserByEmail(context.Background(), email); err == nil {
			_ = st.MarkUserApproved(context.Background(), u.ID, time.Now().UTC())
		}
	}
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
	verifyTestUser(base, "neighbour@example.com")
	return c
}

// signedInAdminClient signs up the configured operator (AdminEmail) and
// returns a client whose session passes requireAdmin.
func signedInAdminClient(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/signup", url.Values{
		"email": {"admin@example.com"}, "password": {"apples12345"},
	})
	if err != nil {
		t.Fatalf("admin signup: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin signup status = %d", resp.StatusCode)
	}
	verifyTestUser(base, "admin@example.com")
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

	// Answer them per question (one "answer" field each, in order — the fake
	// intake asks three), picking a suggested design direction ("Clean & minimal").
	resp, err = c.PostForm(srv.URL+"/projects/"+pid+"/answer", url.Values{
		"answer":        {"brochure only", "I have my own photos", "Swedish"},
		"design_choice": {"Clean & minimal"},
		"csrf_token":    {tok},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	resp.Body.Close()

	// Planning now stops at the approval gate: the customer must approve the
	// scope card before the build spends money. Wait for it, then approve.
	deadline := time.Now().Add(8 * time.Second)
	var approved bool
	for time.Now().Before(deadline) {
		r, _ := c.Get(srv.URL + "/projects/" + pid)
		pg, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(pg), "/approve-plan") {
			pr, err := c.PostForm(srv.URL+"/projects/"+pid+"/approve-plan", url.Values{"csrf_token": {tok}})
			if err != nil {
				t.Fatalf("approve-plan: %v", err)
			}
			pr.Body.Close()
			approved = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !approved {
		t.Fatal("project never reached the plan-approval gate")
	}

	// Now poll the status endpoint. Once the build leaves the polling state it
	// asks HTMX to reload the page (HX-Refresh) — our signal the flow finished.
	deadline = time.Now().Add(8 * time.Second)
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

// TestProjectStatus_BuildingStreamsInline guards the customer/operator split:
// the customer project page never shows the raw live log (it reads as a
// devtool, not "my tech guy is building my site") — that stream lives on the
// operator page /admin/projects/{id}. The customer's polled #status fragment
// stays log-free too.
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
	// Neither the status fragment nor the customer page may carry the raw log.
	if frag := get(buildingID); strings.Contains(frag, `id="livelog"`) {
		t.Errorf("status fragment must not carry the live log:\n%s", frag)
	}
	if page := getPage(buildingID); strings.Contains(page, `id="livelog"`) {
		t.Errorf("customer project page must not carry the live log (operator-only now):\n%s", page)
	}
	// The operator page streams it while the build runs.
	adm := signedInAdminClient(t, srv.URL)
	resp, err := adm.Get(srv.URL + "/admin/projects/" + buildingID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(b)
	for _, want := range []string{
		`sse-connect="/projects/` + buildingID + `/stream"`,
		`id="livelog"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("admin project page missing %q:\n%s", want, page)
		}
	}
	// A finished project shows no live log anywhere.
	if strings.Contains(getPage(seed(project.StatusDelivered)), `id="livelog"`) {
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

// signupRaw signs up without confirming the email (unlike signedInClient), so
// the account stays unverified — for exercising the verification gate.
func signupRaw(t *testing.T, base, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/signup", url.Values{"email": {email}, "password": {"apples12345"}})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	resp.Body.Close()
	return c
}

func TestVerify_UnverifiedBlockedFromCreate(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := signupRaw(t, srv.URL, "unverified@example.com")

	// The dashboard nudges them to confirm their email.
	dash, _ := c.Get(srv.URL + "/dashboard")
	db, _ := io.ReadAll(dash.Body)
	dash.Body.Close()
	if !strings.Contains(string(db), "confirm your email") {
		t.Error("expected the verify banner on the dashboard for an unverified user")
	}

	// Creating a project is refused until the email is confirmed.
	tok := csrfToken(t, c, srv.URL)
	resp, err := c.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"A brochure site for an apple farm selling juice"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unverified create should be 403, got %d", resp.StatusCode)
	}
}

func TestVerify_ConfirmLinkUnlocksFirstProjectSubmission(t *testing.T) {
	rec := &recNotifier{}
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com", BaseURL: "http://app.example"}, rec)
	defer srv.Close()
	c := signupRaw(t, srv.URL, "newbie@example.com")

	// Signup sent a verification email carrying a /verify link.
	if len(rec.bodies) == 0 {
		t.Fatal("no verification email sent on signup")
	}
	m := regexp.MustCompile(`/verify\?token=[a-f0-9]+`).FindString(rec.bodies[len(rec.bodies)-1])
	if m == "" {
		t.Fatalf("no verify link in email: %q", rec.bodies)
	}

	// Follow the confirmation link → the account is now verified.
	resp, err := c.Get(srv.URL + m)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	resp.Body.Close()
	u, err := st.UserByEmail(context.Background(), "newbie@example.com")
	if err != nil || !u.Verified {
		t.Fatalf("account should be verified after the link, got %+v (err %v)", u, err)
	}

	// The verified customer may submit a project, but no AI work starts until
	// the operator approves this first brief.
	tok := csrfToken(t, c, srv.URL)
	resp, err = c.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"A brochure site for an apple farm selling juice"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatalf("create after verify: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Waiting for Rasmus’s approval") {
		t.Fatalf("verified first project should wait for approval, got %d", resp.StatusCode)
	}
	pid := path.Base(resp.Request.URL.Path)
	p, err := st.ProjectByID(context.Background(), pid)
	if err != nil || p.Status != project.StatusPendingAccessApproval || len(p.Questions) != 0 {
		t.Fatalf("project should be resting before intake, got %+v (err %v)", p, err)
	}
	u, _ = st.UserByEmail(context.Background(), "newbie@example.com")
	if u.Approved() {
		t.Fatal("email verification must not implicitly approve project access")
	}
}

func TestFirstProject_AdminApprovalStartsIntakeAndApprovesUser(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	ctx := context.Background()

	// Sign up and verify a new customer without using signedInClient: that
	// helper represents an established approved account for the older tests.
	customer := signupRaw(t, srv.URL, "first-project@example.com")
	if err := st.MarkUserVerified(ctx, "first-project@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := csrfToken(t, customer, srv.URL)
	resp, err := customer.PostForm(srv.URL+"/projects", url.Values{
		"name": {"First Bakery"}, "brief": {"A welcoming website for a neighbourhood bakery with opening hours and a menu"},
		"csrf_token": {tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	pid := path.Base(resp.Request.URL.Path)
	if !strings.Contains(string(body), "Rasmus will review it before any work starts") {
		t.Fatalf("customer approval explanation missing: %s", body)
	}
	p, _ := st.ProjectByID(ctx, pid)
	if p.Status != project.StatusPendingAccessApproval || p.Plan != "" || len(p.Questions) != 0 {
		t.Fatalf("AI pipeline ran before approval: %+v", p)
	}

	// A second pending brief is refused and the durable queue stays at one.
	tok = csrfToken(t, customer, srv.URL)
	second, err := customer.PostForm(srv.URL+"/projects", url.Values{
		"brief": {"A second website that must not create another pending approval"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second pending project status = %d, want 429", second.StatusCode)
	}
	projects, _ := st.ProjectsByUser(ctx, p.UserID)
	if len(projects) != 1 {
		t.Fatalf("created %d pending projects, want one", len(projects))
	}

	admin := signedInAdminClient(t, srv.URL)
	adminPage, _ := admin.Get(srv.URL + "/admin")
	adminBody, _ := io.ReadAll(adminPage.Body)
	adminPage.Body.Close()
	if !strings.Contains(string(adminBody), "first-project@example.com") ||
		!strings.Contains(string(adminBody), "Approve customer &amp; start") {
		t.Fatalf("pending customer missing from admin queue: %s", adminBody)
	}
	adminTok := csrfToken(t, admin, srv.URL)
	approved, err := admin.PostForm(srv.URL+"/admin/projects/"+pid+"/approve-access", url.Values{
		"csrf_token": {adminTok},
	})
	if err != nil {
		t.Fatal(err)
	}
	approved.Body.Close()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, _ = st.ProjectByID(ctx, pid)
		if p.Status == project.StatusNeedsInput {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if p.Status != project.StatusNeedsInput {
		t.Fatalf("approved project did not start intake, status %q", p.Status)
	}
	u, _ := st.UserByEmail(ctx, "first-project@example.com")
	if !u.Approved() {
		t.Fatal("admin approval did not permanently approve the customer")
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

// TestContentAnswer_TextSlotSavedFileSlotRejected verifies the fix for
// text-shaped content needs: a text-kind slot (a contact email) is saved and
// shown as filled, while a file-kind slot cannot be satisfied by typed text.
func TestContentAnswer_TextSlotSavedFileSlotRejected(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, err := st.UserByEmail(ctx, "neighbour@example.com")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{
		ID: "proj-content", UserID: u.ID, Name: "Nimbus", Status: project.StatusPreviewReady,
		Spec: project.PlanSpec{ContentNeeded: []project.ContentItem{
			{Slug: "contact_email", Names: map[string]string{"en": "Contact email"}, Kind: "text"},
			{Slug: "logo", Names: map[string]string{"en": "Logo"}, Kind: "file"},
		}},
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)

	// A text slot accepts a typed value.
	post := func(slot, value string) {
		r, err := c.PostForm(srv.URL+"/projects/"+p.ID+"/content",
			url.Values{"slot": {slot}, "value": {value}, "csrf_token": {tok}})
		if err != nil {
			t.Fatalf("post %s: %v", slot, err)
		}
		r.Body.Close()
	}
	post("contact_email", "hello@nimbusair.example")
	got, _ := st.ProjectByID(ctx, p.ID)
	if got.ContentAnswers["contact_email"] != "hello@nimbusair.example" {
		t.Errorf("text slot not saved: %v", got.ContentAnswers)
	}

	// A file slot must NOT be satisfiable by typed text.
	post("logo", "some text pretending to be a logo")
	got, _ = st.ProjectByID(ctx, p.ID)
	if _, ok := got.ContentAnswers["logo"]; ok {
		t.Errorf("file slot should reject a typed answer, got %v", got.ContentAnswers)
	}

	// The project page shows the text slot as filled with its value.
	r, _ := c.Get(srv.URL + "/projects/" + p.ID)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "hello@nimbusair.example") {
		t.Error("saved contact email not shown on the project page")
	}
}

// TestAdmin_PaidGateAndDelivery covers the manual-payment flow end to end: an
// accepted project shows as unpaid, delivery is refused until it's marked paid,
// the admin toggle settles it, and delivery then goes through.
func TestAdmin_PaidGateAndDelivery(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	adm := signedInAdminClient(t, srv.URL)
	ctx := context.Background()
	u, err := st.UserByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{ID: "pay1", UserID: u.ID, Name: "Bakery",
		Status: project.StatusAccepted, PreviewURL: "https://forge-pay1.fly.dev"}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, adm, srv.URL)

	// The dashboard shows it unpaid, with a Mark-paid control and no deliver button.
	dash, _ := adm.Get(srv.URL + "/admin")
	db, _ := io.ReadAll(dash.Body)
	dash.Body.Close()
	if !strings.Contains(string(db), "Unpaid") || !strings.Contains(string(db), "mark-paid") {
		t.Error("dashboard should show the unpaid project with a Mark-paid control")
	}
	if strings.Contains(string(db), "/admin/projects/pay1/deliver") {
		t.Error("deliver button must be hidden until the project is paid")
	}

	// Delivering while unpaid is refused and bounces back with the notice.
	resp, _ := adm.PostForm(srv.URL+"/admin/projects/pay1/deliver", url.Values{"csrf_token": {tok}})
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, _ := st.ProjectByID(ctx, "pay1"); got.Status == project.StatusDelivered {
		t.Fatal("unpaid project must not be delivered")
	}
	if !strings.Contains(string(rb), "mark it paid") {
		t.Error("expected the unpaid notice after a blocked delivery")
	}

	// Mark it paid → state recorded with provenance.
	resp, _ = adm.PostForm(srv.URL+"/admin/projects/pay1/mark-paid", url.Values{"csrf_token": {tok}})
	resp.Body.Close()
	got, _ := st.ProjectByID(ctx, "pay1")
	if !got.Paid || got.PaidVia != "manual" || got.PaidAt.IsZero() {
		t.Fatalf("mark-paid did not settle: %+v", got)
	}

	// The operator detail page renders the Payment panel (exercises paid-at
	// formatting) and reads as paid.
	detail, _ := adm.Get(srv.URL + "/admin/projects/pay1")
	dtb, _ := io.ReadAll(detail.Body)
	detail.Body.Close()
	if !strings.Contains(string(dtb), "Payment") || !strings.Contains(string(dtb), "Paid") {
		t.Error("admin project page should render the Payment panel as paid")
	}

	// Delivery now goes through.
	resp, _ = adm.PostForm(srv.URL+"/admin/projects/pay1/deliver", url.Values{"csrf_token": {tok}})
	resp.Body.Close()
	if got, _ := st.ProjectByID(ctx, "pay1"); got.Status != project.StatusDelivered {
		t.Fatalf("paid project should deliver, got %q", got.Status)
	}
}

// TestApplyContent_ButtonAppearsAndRebuilds covers content added after a build:
// it flags the project, surfaces the "update my site" button, and the button
// triggers a rebuild that consumes a change and clears the flag.
func TestApplyContent_ButtonAppearsAndRebuilds(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	// A project that already has one build (a preview the customer has seen).
	p := &project.Project{
		ID: "proj-apply", UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady,
		IterationsUsed: 1, PreviewURL: "https://forge-proj-apply.fly.dev",
		Spec: project.PlanSpec{ContentNeeded: []project.ContentItem{
			{Slug: "tagline", Names: map[string]string{"en": "Tagline"}, Kind: "text"},
		}},
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)

	page := func() string {
		r, _ := c.Get(srv.URL + "/projects/" + p.ID)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return string(b)
	}
	// No content added yet → no update button.
	if strings.Contains(page(), "Update my site with it") {
		t.Error("apply-content button should not show before new content is added")
	}

	// Add content after the build → flag set, button appears.
	r, _ := c.PostForm(srv.URL+"/projects/"+p.ID+"/content",
		url.Values{"slot": {"tagline"}, "value": {"Fresh sourdough daily"}, "csrf_token": {tok}})
	r.Body.Close()
	if got, _ := st.ProjectByID(ctx, p.ID); !got.ContentPending {
		t.Fatal("adding content after a build should set ContentPending")
	}
	if !strings.Contains(page(), "Update my site with it") {
		t.Error("apply-content button should appear once content is pending")
	}

	// Click it → a rebuild runs (a 2nd iteration), and the flag clears.
	r, _ = c.PostForm(srv.URL+"/projects/"+p.ID+"/apply-content", url.Values{"csrf_token": {tok}})
	r.Body.Close()
	deadline := time.Now().Add(8 * time.Second)
	var got *project.Project
	for time.Now().Before(deadline) {
		got, _ = st.ProjectByID(ctx, p.ID)
		if got.Status == project.StatusPreviewReady && got.IterationsUsed == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.IterationsUsed != 2 {
		t.Fatalf("apply-content should rebuild as a change, IterationsUsed=%d", got.IterationsUsed)
	}
	if got.ContentPending {
		t.Error("ContentPending should be cleared by the rebuild")
	}
}

// TestCaptionAsset_LabelsPhotoForPairing covers the recipe-photo pairing fix: a
// files-slot upload gets a per-photo caption field, and labelling it stores the
// caption (so the build pairs it) and flags a rebuild.
func TestCaptionAsset_LabelsPhotoForPairing(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: "proj-cap", UserID: u.ID, Name: "Tiny Hands", Status: project.StatusPreviewReady, IterationsUsed: 1,
		Spec: project.PlanSpec{ContentNeeded: []project.ContentItem{
			{Slug: "photos", Names: map[string]string{"en": "Recipe photos"}, Kind: "files"},
		}},
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := &project.Asset{ID: "asset-1", ProjectID: p.ID, Key: "projects/proj-cap/assets/cake.jpg",
		Filename: "cake.jpg", ContentType: "image/jpeg", Slot: "photos", CreatedAt: time.Now().UTC()}
	if err := st.CreateAsset(ctx, a); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)

	// The project page offers a per-photo caption form.
	r, _ := c.Get(srv.URL + "/projects/" + p.ID)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), "/projects/proj-cap/assets/asset-1/caption") {
		t.Error("expected a per-photo caption form for the uploaded file")
	}

	// Labelling stores the caption (so the build can pair it) and flags a rebuild.
	r, _ = c.PostForm(srv.URL+"/projects/"+p.ID+"/assets/asset-1/caption",
		url.Values{"caption": {"Chocolate cake"}, "csrf_token": {tok}})
	r.Body.Close()
	assets, _ := st.AssetsByProject(ctx, p.ID)
	if len(assets) != 1 || assets[0].Description != "Chocolate cake" {
		t.Fatalf("caption not saved: %+v", assets)
	}
	if got, _ := st.ProjectByID(ctx, p.ID); !got.ContentPending {
		t.Error("captioning after a build should flag ContentPending for a rebuild")
	}
}

// TestPickImage_HTMXClosesWithRedirect verifies the modal's "select" step: an
// htmx pick promotes the candidate to the slot's asset and returns HX-Redirect
// (so the dialog closes via a reload) rather than a body swap.
func TestPickImage_HTMXClosesWithRedirect(t *testing.T) {
	srv, st, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, nil)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: "proj-pick", UserID: u.ID, Name: "X", Status: project.StatusPreviewReady, IterationsUsed: 1,
		PendingImages: map[string]project.ImageCandidates{
			"logo": {Prompt: "a logo", Keys: []string{"projects/proj-pick/gen/logo/a.png"}},
		},
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)

	req, _ := http.NewRequest("POST", srv.URL+"/projects/proj-pick/content/pick",
		strings.NewReader(url.Values{"csrf_token": {tok}, "slot": {"logo"}, "index": {"0"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	noRedir := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedir.Do(req)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("HX-Redirect") != "/projects/proj-pick" {
		t.Fatalf("htmx pick should 200 + HX-Redirect, got %d / %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if assets, _ := st.AssetsByProject(ctx, "proj-pick"); len(assets) != 1 || !assets[0].Generated || assets[0].Slot != "logo" {
		t.Fatalf("pick should create the generated slot asset: %+v", assets)
	}
}

func TestGenerateImage_RunsInBackgroundAndPersistsResults(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseImages := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseImages()
	var calls atomic.Int32
	imageAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		encoded := base64.StdEncoding.EncodeToString([]byte("generated png"))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"b64_json": encoded},
			map[string]any{"b64_json": encoded},
			map[string]any{"b64_json": encoded},
		}})
	}))
	defer imageAPI.Close()

	images := imagegen.New(imageAPI.URL, "sk-test", "gpt-image-test")
	srv, st, _ := newTestServerAuthImageGen(t, config.Config{
		AdminEmail: "admin@example.com", ImageGenMaxPerProject: 20,
	}, nil, images)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := context.Background()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: "proj-background-image", UserID: u.ID, Name: "X", Status: project.StatusPreviewReady,
		Spec: project.PlanSpec{ContentNeeded: []project.ContentItem{
			{Slug: "hero-image", Names: map[string]string{"en": "Hero image"}, Kind: "file", Generatable: true},
			{Slug: "gallery-image", Names: map[string]string{"en": "Gallery image"}, Kind: "file", Generatable: true},
		}},
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)
	submit := func(slot string) (string, error) {
		req, _ := http.NewRequest("POST", srv.URL+"/projects/"+p.ID+"/content/generate",
			strings.NewReader(url.Values{"csrf_token": {tok}, "slot": {slot}, "prompt": {"A warm bakery image"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body), nil
	}

	type response struct {
		body string
		err  error
	}
	returned := make(chan response, 1)
	go func() {
		body, err := submit("hero-image")
		returned <- response{body: body, err: err}
	}()
	select {
	case result := <-returned:
		if result.err != nil {
			t.Fatalf("generate: %v", result.err)
		}
		if !strings.Contains(result.body, "close this window and start another image") {
			t.Fatalf("response did not show background state: %s", result.body)
		}
	case <-time.After(time.Second):
		t.Fatal("Forge waited for the image API instead of returning immediately")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background image API call did not start")
	}

	got, _ := st.ProjectByID(ctx, p.ID)
	if set := got.PendingImages["hero-image"]; set.Status != "running" || set.JobID == "" || got.ImageGenCount != 1 {
		t.Fatalf("running job was not persisted and charged once: %+v count=%d", set, got.ImageGenCount)
	}
	if _, err := submit("hero-image"); err != nil { // a double-submit must attach to the same job, not start or charge another
		t.Fatalf("double-submit: %v", err)
	}
	got, _ = st.ProjectByID(ctx, p.ID)
	if got.ImageGenCount != 1 || calls.Load() != 1 {
		t.Fatalf("double-submit started another generation: count=%d calls=%d", got.ImageGenCount, calls.Load())
	}
	if _, err := submit("gallery-image"); err != nil {
		t.Fatalf("second slot: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got, _ = st.ProjectByID(ctx, p.ID)
	if got.ImageGenCount != 2 || calls.Load() != 2 || got.PendingImages["gallery-image"].Status != "running" {
		t.Fatalf("different slots did not run concurrently: count=%d calls=%d jobs=%+v", got.ImageGenCount, calls.Load(), got.PendingImages)
	}

	releaseImages()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.ProjectByID(ctx, p.ID)
		hero := got.PendingImages["hero-image"]
		gallery := got.PendingImages["gallery-image"]
		if hero.Status == "ready" && len(hero.Keys) == 3 && gallery.Status == "ready" && len(gallery.Keys) == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	set := got.PendingImages["hero-image"]
	if set.Status != "ready" || len(set.Keys) != 3 {
		t.Fatalf("background result did not become reviewable: %+v", set)
	}
	if set := got.PendingImages["gallery-image"]; set.Status != "ready" || len(set.Keys) != 3 {
		t.Fatalf("second background result did not become reviewable: %+v", set)
	}
	resp, err := c.Get(srv.URL + "/projects/" + p.ID + "/content/generation?slot=hero-image")
	if err != nil {
		t.Fatalf("poll results: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Count(string(body), "Use this") != 3 {
		t.Fatalf("poll did not render three choices: %s", body)
	}
}
