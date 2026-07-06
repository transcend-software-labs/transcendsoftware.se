package web_test

import (
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
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
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
	return httptest.NewServer(srv.Handler())
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
