package web

// Internal tests (package web): the preview proxy needs its backend origin
// pointed at an httptest server via the unexported previewTarget seam.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
)

// newProxyServer builds a Forge server with PREVIEW_DOMAIN=preview.test whose
// preview backend is the given httptest origin, plus a memory store to seed.
func newProxyServer(t *testing.T, backend *httptest.Server) (*httptest.Server, store.Store) {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	cfg := config.Config{BaseURL: "https://forge.example", PreviewDomain: "preview.test"}
	srv, err := NewServer(cfg, st, auth.NewSessions(st, time.Hour), orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	bu, _ := url.Parse(backend.URL)
	srv.previewTarget = func(string) *url.URL { return bu }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

// echoBackend answers like a generated customer site would.
func echoBackend(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redir": // absolute redirect to the backend's own host — must not leak
			http.Redirect(w, r, "http://"+r.Host+"/target", http.StatusFound)
		default:
			w.Header().Set("X-Saw-Host", r.Host)
			w.Header().Set("X-Saw-Fwd-Host", r.Header.Get("X-Forwarded-Host"))
			_, _ = io.WriteString(w, "backend:"+r.Method+" "+r.URL.RequestURI())
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// seedProxyProject stores a project with a preview host.
func seedProxyProject(t *testing.T, st store.Store, id, host string, mutate func(*project.Project)) {
	t.Helper()
	p := &project.Project{
		ID: id, UserID: "u1", Name: "Bageriet", Status: project.StatusPreviewReady,
		PreviewHost: host, PreviewURL: "https://" + host + ".preview.test",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if mutate != nil {
		mutate(p)
	}
	if err := st.CreateProject(t.Context(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// getHost GETs path from the test server with an overridden Host header.
func getHost(t *testing.T, ts *httptest.Server, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // inspect redirects, don't follow
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("get %s%s: %v", host, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPreviewProxy_RoutesByHost(t *testing.T) {
	backend := echoBackend(t)
	ts, st := newProxyServer(t, backend)
	seedProxyProject(t, st, "p1aaaa", "bageriet-p1aaaa", nil)

	resp := getHost(t, ts, "bageriet-p1aaaa.preview.test", "/menu?week=2")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "backend:GET /menu?week=2" {
		t.Fatalf("proxy pass-through failed: %d %q", resp.StatusCode, body)
	}
	bu, _ := url.Parse(backend.URL)
	if got := resp.Header.Get("X-Saw-Host"); got != bu.Host {
		t.Errorf("backend should see its own Host (Fly routes by it), saw %q", got)
	}
	if got := resp.Header.Get("X-Saw-Fwd-Host"); got != "bageriet-p1aaaa.preview.test" {
		t.Errorf("X-Forwarded-Host should carry the branded host, saw %q", got)
	}
	// Unpaid preview → keep it out of search engines.
	if resp.Header.Get("X-Robots-Tag") != "noindex" {
		t.Error("unpaid preview should be noindex")
	}
}

func TestPreviewProxy_HostWithPortAndCase(t *testing.T) {
	ts, st := newProxyServer(t, echoBackend(t))
	seedProxyProject(t, st, "p2aaaa", "kiosk-p2aaaa", nil)

	resp := getHost(t, ts, "Kiosk-p2aaaa.Preview.Test:8443", "/")
	if resp.StatusCode != 200 {
		t.Fatalf("host with port/case should still route: %d", resp.StatusCode)
	}
}

func TestPreviewProxy_RewritesBackendRedirect(t *testing.T) {
	backend := echoBackend(t)
	ts, st := newProxyServer(t, backend)
	seedProxyProject(t, st, "p3aaaa", "shop-p3aaaa", nil)

	resp := getHost(t, ts, "shop-p3aaaa.preview.test", "/redir")
	loc := resp.Header.Get("Location")
	if loc != "https://shop-p3aaaa.preview.test/target" {
		t.Fatalf("Location must be rewritten to the branded host, got %q", loc)
	}
}

func TestPreviewProxy_PaidSiteIsIndexable(t *testing.T) {
	ts, st := newProxyServer(t, echoBackend(t))
	seedProxyProject(t, st, "p4aaaa", "firm-p4aaaa", func(p *project.Project) { p.Paid = true })

	resp := getHost(t, ts, "firm-p4aaaa.preview.test", "/")
	if resp.Header.Get("X-Robots-Tag") != "" {
		t.Error("a paid customer's site must not be forced noindex")
	}
}

func TestPreviewProxy_UnknownLabel404(t *testing.T) {
	ts, _ := newProxyServer(t, echoBackend(t))
	resp := getHost(t, ts, "nope.preview.test", "/")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "Nothing here") {
		t.Fatalf("unknown label: %d %q", resp.StatusCode, body)
	}
}

func TestPreviewProxy_ExpiredShowsLocalizedPage(t *testing.T) {
	ts, st := newProxyServer(t, echoBackend(t))
	seedProxyProject(t, st, "p5aaaa", "gammal-p5aaaa", func(p *project.Project) {
		p.Status = project.StatusExpired
		p.Locale = "sv"
	})

	resp := getHost(t, ts, "gammal-p5aaaa.preview.test", "/")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), "Förhandsvisningen har gått ut") {
		t.Fatalf("expired preview: %d %q", resp.StatusCode, body)
	}
}

func TestPreviewProxy_MainHostFallsThrough(t *testing.T) {
	ts, _ := newProxyServer(t, echoBackend(t))
	// The canonical host (and any non-preview host) must reach Forge itself.
	resp := getHost(t, ts, "forge.example", "/healthz")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("main host should serve Forge: %d %q", resp.StatusCode, body)
	}
	// The preview domain apex itself is not a preview either.
	resp = getHost(t, ts, "preview.test", "/healthz")
	if resp.StatusCode != 200 {
		t.Fatalf("preview-domain apex should fall through: %d", resp.StatusCode)
	}
}
