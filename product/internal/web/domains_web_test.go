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
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/namecom"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

// newDomainServer builds a test server with the custom-domain feature wired to a
// name.com client (pointed at cfURL, unused for BYOD flows) and a Stripe
// biller. Buying is enabled by the non-nil biller, so the search UI is available.
func newDomainServer(t *testing.T, cfURL string) (*httptest.Server, store.Store) {
	t.Helper()
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(stripe.Close)
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	bill := billing.New(stripe.URL, "sk_test_x")
	orch.SetDomains(namecom.New(cfURL, "forge-test", "tok", namecom.FixedRate(10), 0), bill, 100)
	cfg := config.Config{AdminEmail: "admin@example.com", BaseURL: "https://forge.example"}
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetBilling(bill)
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

func TestDomainSearch_KeywordSuggests(t *testing.T) {
	// name.com's search takes bare keywords and suggests across endings — the
	// old require-a-TLD restriction (a GleSYS slowness workaround) is gone.
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/core/v1/domains:search" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"results":[
			{"domainName":"mybakery.se","sld":"mybakery","tld":"se","purchasable":true,"purchaseType":"registration","purchasePrice":5.9,"renewalPrice":9.9}
		]}`)
	}))
	defer reg.Close()

	srv, st := newDomainServer(t, reg.URL)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedPaidProject(t, st, "domtld", true)

	body := getBody(t, c, srv.URL+"/projects/domtld/domain/search?q=mybakery")
	if !strings.Contains(body, "mybakery.se") {
		t.Fatalf("keyword search should render suggestions, got: %s", body)
	}
	// The intro discount is disclosed: first-year price AND the renewal price
	// (5.9/9.9 USD × 10 SEK, markup 0 in this server → "59 kr … 99 kr").
	if !strings.Contains(body, "59 kr first year, then 99 kr/yr") {
		t.Fatalf("results should disclose the renewal price, got: %s", body)
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
