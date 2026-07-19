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

// newBundleServer wires both the Stripe paywall (fake checkout + price) and the
// domain feature (Cloudflare pointed at an unreachable URL — BYOD intent needs
// no registrar call, and these tests don't provision). Enough to exercise the
// "bundle a domain into the subscribe flow" chooser and intent capture.
func newBundleServer(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			_, _ = w.Write([]byte(`{"unit_amount":2900,"currency":"sek","recurring":{"interval":"month"}}`))
		case r.URL.Path == "/v1/checkout/sessions":
			_, _ = w.Write([]byte(`{"id":"cs_1","url":"https://checkout.stripe.com/pay/cs_1"}`))
		default:
			http.NotFound(w, r)
		}
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
	orch.SetDomains(namecom.New("http://127.0.0.1:1", "forge-test", "tok", 10, 0), bill, 100)
	cfg := config.Config{
		AdminEmail: "admin@example.com", BaseURL: "https://forge.example",
		StripeSecretKey: "sk_test_x", StripePriceID: "price_base",
		StripeWebhookSecret: webhookSecret,
	}
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

func seedSubProject(t *testing.T, st store.Store, id string) {
	t.Helper()
	ctx := t.Context()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: id, UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady,
		PreviewURL: "https://forge-" + id + ".fly.dev", Paid: false,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// The subscribe panel offers an optional domain chooser (BYOD + buy) before the
// customer pays; the per-domain price is shown in the search results, not as a
// fixed fee on the panel.
func TestSubscribeChooser_ShownForUnpaid(t *testing.T) {
	srv, st := newBundleServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedSubProject(t, st, "sub1")

	body := getBody(t, c, srv.URL+"/projects/sub1")
	for _, want := range []string{
		`name="domain_mode"`, `value="byod"`, `value="buy"`,
		"Add a domain",           // sub.domain.legend
		`name="byod_host"`,       // the BYOD input
		`domain/search?select=1`, // the pre-pay buy search
	} {
		if !strings.Contains(body, want) {
			t.Errorf("subscribe chooser missing %q", want)
		}
	}
}

// Choosing BYOD before paying records the intent and still proceeds to Stripe
// Checkout — the domain is attached automatically once payment settles.
func TestSubscribe_BundlesBYODIntent(t *testing.T) {
	srv, st := newBundleServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedSubProject(t, st, "sub2")
	tok := csrfToken(t, c, srv.URL)

	noRedir := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedir.PostForm(srv.URL+"/projects/sub2/subscribe", url.Values{
		"csrf_token": {tok}, "domain_mode": {"byod"}, "byod_host": {"myown.se"},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://checkout.stripe.com/pay/cs_1" {
		t.Fatalf("subscribe should 303 to checkout, got %d / %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	p, _ := st.ProjectByID(t.Context(), "sub2")
	if p.DomainIntent != "myown.se" || p.DomainIntentBuy {
		t.Fatalf("BYOD intent not captured: intent=%q buy=%v", p.DomainIntent, p.DomainIntentBuy)
	}
}

// Subscribing without choosing a domain leaves no intent (nothing gets
// provisioned after payment).
func TestSubscribe_NoDomainLeavesNoIntent(t *testing.T) {
	srv, st := newBundleServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedSubProject(t, st, "sub3")
	tok := csrfToken(t, c, srv.URL)

	noRedir := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedir.PostForm(srv.URL+"/projects/sub3/subscribe", url.Values{
		"csrf_token": {tok}, "domain_mode": {"none"},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	resp.Body.Close()
	if p, _ := st.ProjectByID(t.Context(), "sub3"); p.DomainIntent != "" {
		t.Fatalf("no-domain subscribe should leave no intent, got %q", p.DomainIntent)
	}
}
