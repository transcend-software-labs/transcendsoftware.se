package web_test

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
	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/cloudflare"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

// newDomainServer builds a test server with the custom-domain feature wired to a
// Cloudflare client (pointed at cfURL, unused for BYOD flows) and the Stripe
// paywall. Buying needs a price id, so the search UI is available too.
func newDomainServer(t *testing.T, cfURL string) (*httptest.Server, store.Store) {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	orch.SetDomains(cloudflare.New(cfURL, "tok", "acct"), nil, "price_dom", 100)
	cfg := config.Config{AdminEmail: "admin@example.com", BaseURL: "https://forge.example"}
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// A real (unused for BYOD) Stripe client so nothing nil-panics.
	srv.SetBilling(billing.New(cfURL, "sk_test_x"))
	ts := httptest.NewServer(srv.Handler())
	testStores.Store(ts.URL, st)
	return ts, st
}

func seedPaidProject(t *testing.T, st store.Store, id string, paid bool) {
	t.Helper()
	ctx := t.Context()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: id, UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady,
		PreviewURL: "https://forge-" + id + ".fly.dev", Paid: paid,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func TestDomainPanel_HiddenUntilPaid(t *testing.T) {
	srv, st := newDomainServer(t, "http://127.0.0.1:1")
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedPaidProject(t, st, "dom-unpaid", false)

	body := getBody(t, c, srv.URL+"/projects/dom-unpaid")
	if strings.Contains(body, "/domain/attach") {
		t.Error("domain panel must be hidden for an unpaid project")
	}
}

func TestDomainAttach_ShowsRecords(t *testing.T) {
	srv, st := newDomainServer(t, "http://127.0.0.1:1")
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedPaidProject(t, st, "dom1", true)
	tok := csrfToken(t, c, srv.URL)

	// Paid project shows the attach form (and, since buying is enabled, search).
	if body := getBody(t, c, srv.URL+"/projects/dom1"); !strings.Contains(body, "/projects/dom1/domain/attach") ||
		!strings.Contains(body, "/projects/dom1/domain/search") {
		t.Fatalf("domain panel not shown for a paid project")
	}

	// Attach the customer's own domain.
	resp, err := c.PostForm(srv.URL+"/projects/dom1/domain/attach", url.Values{
		"csrf_token": {tok}, "host": {"acme.se"},
	})
	if err != nil {
		t.Fatalf("attach post: %v", err)
	}
	resp.Body.Close()

	p, _ := st.ProjectByID(t.Context(), "dom1")
	if p.DomainStatus != project.DomainPendingDNS || p.DomainName != "acme.se" {
		t.Fatalf("after attach: status=%s name=%s", p.DomainStatus, p.DomainName)
	}
	if len(p.DomainRecords) == 0 {
		t.Fatal("attach should store DNS records")
	}

	// The page now shows the records table (the ACME-challenge record) + Verify.
	body := getBody(t, c, srv.URL+"/projects/dom1")
	if !strings.Contains(body, "_acme-challenge.acme.se") || !strings.Contains(body, "/projects/dom1/domain/verify") {
		t.Fatalf("pending_dns page missing records or verify button")
	}
}

func TestDomainAttach_RejectsFlyHost(t *testing.T) {
	srv, st := newDomainServer(t, "http://127.0.0.1:1")
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedPaidProject(t, st, "dom2", true)
	tok := csrfToken(t, c, srv.URL)

	resp, err := c.PostForm(srv.URL+"/projects/dom2/domain/attach", url.Values{
		"csrf_token": {tok}, "host": {"forge-dom2.fly.dev"},
	})
	if err != nil {
		t.Fatalf("attach post: %v", err)
	}
	resp.Body.Close()

	if p, _ := st.ProjectByID(t.Context(), "dom2"); p.HasDomain() {
		t.Fatal("a *.fly.dev host must be rejected")
	}
}

// getBody GETs a URL and returns the response body as a string.
func getBody(t *testing.T, c *http.Client, urlStr string) string {
	t.Helper()
	resp, err := c.Get(urlStr)
	if err != nil {
		t.Fatalf("get %s: %v", urlStr, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
