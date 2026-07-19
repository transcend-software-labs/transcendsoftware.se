package web_test

// SEO invariants for the Forge site itself: the public <head> carries
// canonical/Open Graph/structured data pinned to BaseURL (never the serving
// host), the crawl pair works, and the landing JSON-LD carries the real Stripe
// price as an Offer. Tags, not copy — the words are i18n's business.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
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

// newSEOServer builds a public-facing test server on BaseURL
// https://forge.example, with a fake Stripe answering the price lookup so the
// landing JSON-LD gets its Offer.
func newSEOServer(t *testing.T) *httptest.Server {
	t.Helper()
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/prices/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"unit_amount": 29900, "currency": "sek",
				"recurring": map[string]any{"interval": "month"},
			})
			return
		}
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
	cfg := config.Config{AdminEmail: "admin@example.com", BaseURL: "https://forge.example",
		StripePriceID: "price_base"}
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetBilling(billing.New(stripe.URL, "sk_test_x"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSEO_ForgeHead(t *testing.T) {
	ts := newSEOServer(t)
	body := getBody(t, http.DefaultClient, ts.URL+"/")
	for _, want := range []string{
		`<meta name="description"`,
		// Canonical + og:url pin to BaseURL even though the test serves on
		// 127.0.0.1 — exactly the fly.dev-vs-real-domain case.
		`<link rel="canonical" href="https://forge.example/">`,
		`<meta property="og:url" content="https://forge.example/">`,
		`<meta property="og:image" content="https://forge.example/static/og.png">`,
		`<link rel="alternate" hreflang="sv" href="https://forge.example/?lang=sv">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`application/ld+json`,
		`"@type":"Service"`,                   // the landing gets the richer graph…
		`"price":"299","priceCurrency":"SEK"`, // …with the live Stripe price as an Offer
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing <head> missing %q", want)
		}
	}

	// Other public pages carry the site-wide Organization node.
	terms := getBody(t, http.DefaultClient, ts.URL+"/terms")
	if !strings.Contains(terms, `"@type":"Organization"`) {
		t.Error("terms page missing the Organization JSON-LD")
	}
	if !strings.Contains(terms, `<link rel="canonical" href="https://forge.example/terms">`) {
		t.Error("terms page missing its canonical")
	}
}

func TestSEO_LocalizedPublicPageHasStableCanonical(t *testing.T) {
	ts := newSEOServer(t)
	resp, err := http.Get(ts.URL + "/?lang=sv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := string(body)
	for _, want := range []string{
		`<html lang="sv">`,
		`<link rel="canonical" href="https://forge.example/?lang=sv">`,
		`<meta property="og:url" content="https://forge.example/?lang=sv">`,
		`<link rel="alternate" hreflang="en" href="https://forge.example/">`,
		`<link rel="alternate" hreflang="x-default" href="https://forge.example/">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("localized landing missing %q", want)
		}
	}
}

func TestSEO_ForgeCrawlPair(t *testing.T) {
	ts := newSEOServer(t)

	sm := getBody(t, http.DefaultClient, ts.URL+"/sitemap.xml")
	for _, want := range []string{"<urlset", "<loc>https://forge.example/</loc>", "<loc>https://forge.example/terms</loc>",
		"<loc>https://forge.example/?lang=sv</loc>", `hreflang="ru" href="https://forge.example/privacy?lang=ru"`} {
		if !strings.Contains(sm, want) {
			t.Errorf("sitemap.xml missing %q, got:\n%s", want, sm)
		}
	}

	rb := getBody(t, http.DefaultClient, ts.URL+"/robots.txt")
	for _, want := range []string{"User-agent: *", "Disallow: /dashboard", "Disallow: /admin",
		"Disallow: /start", "Sitemap: https://forge.example/sitemap.xml"} {
		if !strings.Contains(rb, want) {
			t.Errorf("robots.txt missing %q, got:\n%s", want, rb)
		}
	}
}

// The static social card must actually exist — a dangling og:image URL turns
// every share preview blank.
func TestSEO_OGImageServed(t *testing.T) {
	ts := newSEOServer(t)
	resp, err := http.Get(ts.URL + "/static/og.png")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(b) == 0 {
		t.Fatalf("og.png = %d (%d bytes), want a real image", resp.StatusCode, len(b))
	}
}
