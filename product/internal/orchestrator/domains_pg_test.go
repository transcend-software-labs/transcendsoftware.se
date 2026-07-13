package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/hostup"
	"github.com/transcend-software-labs/rasmus-ai/internal/pgtest"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// hostupMock is a canned Hostup REST API: every queried name is available &
// registrable at a fixed SEK price, and register orders come back pending. It
// records the register calls so a test can assert the provision actually fired.
type hostupMock struct {
	*httptest.Server
	mu         sync.Mutex
	registered []string
}

func newHostupMock() *hostupMock {
	m := &hostupMock{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/domains/availability":
			var body struct {
				Names []string `json:"names"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"data":[`)
			for i, n := range body.Names {
				if i > 0 {
					_, _ = io.WriteString(w, ",")
				}
				fmt.Fprintf(w, `{"name":%q,"available":true,"premium":false,`+
					`"actions":{"canRegister":{"allowed":true}},`+
					`"billing":{"amount":99,"currencyCode":"SEK"},"renewalAmount":169}`, n)
			}
			_, _ = io.WriteString(w, `]}`)
		case "/api/v2/orders":
			var body struct {
				Items []struct {
					DomainName string `json:"domainName"`
				} `json:"items"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			for _, it := range body.Items {
				m.registered = append(m.registered, it.DomainName)
			}
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"ord_1","status":"pending"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	return m
}

func (m *hostupMock) registeredNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.registered...)
}

// TestDomainAtCheckout_RealPostgres_MockedHostup covers the domain-bundled-at-
// checkout flow through the REAL Postgres store and the REAL Hostup client
// (pointed at a mock API) — the two layers the unit tests fake, and exactly
// where the shipped bug lived: SetDomainIntent validated the domain against the
// registrar, then failed to persist because UpdateProject's bindings were off,
// surfacing to the customer as "not registrable".
func TestDomainAtCheckout_RealPostgres_MockedHostup(t *testing.T) {
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, pgtest.Start(t))
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer pg.Close()

	mock := newHostupMock()
	defer mock.Close()

	orch, _ := newTestOrchWithVerifier(pg, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	// Real Hostup client, real store, cap 300 SEK.
	orch.SetDomains(hostup.New(mock.URL, "tok", "invoice"), &fakeBiller{}, "price_dom", 300)

	if err := pg.CreateUser(ctx, &user.User{ID: "u1", Email: "cust@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "Bageri", Status: project.StatusPreviewReady,
		Locale: "sv", PreviewURL: "https://forge-p1.fly.dev",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := pg.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// 1. Pre-checkout: the customer picks a domain to BUY. This re-checks the
	//    registrar (mock) AND writes the intent to Postgres — the step that broke.
	if err := orch.SetDomainIntent(ctx, "p1", "mittbageri.se", true); err != nil {
		t.Fatalf("SetDomainIntent(buy): %v", err)
	}
	got, err := pg.ProjectByID(ctx, "p1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.DomainIntent != "mittbageri.se" || !got.DomainIntentBuy {
		t.Fatalf("intent not persisted to Postgres: intent=%q buy=%v", got.DomainIntent, got.DomainIntentBuy)
	}

	// 2. Payment settles → the bundled domain is provisioned automatically:
	//    registered via Hostup, project moves to 'registering', intent cleared.
	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("SubscriptionStarted: %v", err)
	}
	got = waitForDomain(t, pg, "p1", func(pr *project.Project) bool {
		return pr.DomainStatus == project.DomainRegistering
	})
	if got.DomainKind != project.DomainKindPurchased || got.DomainName != "mittbageri.se" {
		t.Fatalf("after provision: kind=%q name=%q", got.DomainKind, got.DomainName)
	}
	if got.DomainIntent != "" {
		t.Errorf("intent should be cleared after provisioning, got %q", got.DomainIntent)
	}
	if reg := mock.registeredNames(); len(reg) != 1 || reg[0] != "mittbageri.se" {
		t.Fatalf("Hostup register calls = %v, want [mittbageri.se]", reg)
	}
}

// TestSetDomainIntent_TooPricey_RealPostgres confirms the price-cap guard rejects
// a buy over the cap without persisting an intent — end to end through the real
// client + store.
func TestSetDomainIntent_TooPricey_RealPostgres(t *testing.T) {
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, pgtest.Start(t))
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer pg.Close()

	mock := newHostupMock() // fixed price 99 SEK renewal 169
	defer mock.Close()

	orch, _ := newTestOrchWithVerifier(pg, NoopVerifier{})
	orch.SetDomains(hostup.New(mock.URL, "tok", "invoice"), &fakeBiller{}, "price_dom", 50) // cap below price

	if err := pg.CreateUser(ctx, &user.User{ID: "u1", Email: "c@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{ID: "p1", UserID: "u1", Name: "B", Status: project.StatusPreviewReady, PreviewURL: "https://x", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := pg.CreateProject(ctx, p); err != nil {
		t.Fatalf("project: %v", err)
	}

	if err := orch.SetDomainIntent(ctx, "p1", "mittbageri.se", true); err != ErrDomainTooPricey {
		t.Fatalf("over-cap buy should be ErrDomainTooPricey, got %v", err)
	}
	if got, _ := pg.ProjectByID(ctx, "p1"); got.DomainIntent != "" {
		t.Fatalf("rejected intent must not persist, got %q", got.DomainIntent)
	}
}
