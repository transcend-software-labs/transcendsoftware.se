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
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	b := builder.NewSandbox(opencode.NewFake(), fly.NewFake(), "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := orchestrator.New(st, fake, fake, fake, b, log)
	sessions := auth.NewSessions(time.Hour)
	srv, err := web.NewServer(config.Config{AdminEmail: "admin@example.com"}, st, sessions, orch, log)
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

	// Answer them.
	resp, err = c.PostForm(srv.URL+"/projects/"+pid+"/answer", url.Values{
		"answers": {"brochure only; I have photos; Swedish"}, "csrf_token": {tok},
	})
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	resp.Body.Close()

	// Poll until preview ready.
	deadline := time.Now().Add(8 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		r, _ := c.Get(srv.URL + "/projects/" + pid + "/status")
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(b), "Preview ready") {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("project never reached preview ready")
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
