package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/namecom"
	"github.com/transcend-software-labs/rasmus-ai/internal/pgtest"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// namecomMock is a canned name.com Core API: every queried name is available &
// registrable at a fixed USD price (9.9/16.9 → 99/169 SEK at the test rate of
// 10), creation succeeds synchronously, and created domains resolve on GET. It
// records register calls so a test can assert the provision actually fired.
type namecomMock struct {
	*httptest.Server
	mu         sync.Mutex
	registered []string
}

func newNamecomMock() *namecomMock {
	m := &namecomMock{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/core/v1/domains:checkAvailability":
			var body struct {
				DomainNames []string `json:"domainNames"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = io.WriteString(w, `{"results":[`)
			for i, n := range body.DomainNames {
				if i > 0 {
					_, _ = io.WriteString(w, ",")
				}
				fmt.Fprintf(w, `{"domainName":%q,"sld":"x","tld":"se","purchasable":true,`+
					`"purchaseType":"registration","purchasePrice":9.9,"renewalPrice":16.9}`, n)
			}
			_, _ = io.WriteString(w, `]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/core/v1/domains":
			var body struct {
				Domain struct {
					DomainName string `json:"domainName"`
				} `json:"domain"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.registered = append(m.registered, body.Domain.DomainName)
			m.mu.Unlock()
			fmt.Fprintf(w, `{"order":1,"totalPaid":9.9,"domain":{"domainName":%q,`+
				`"expireDate":"2027-07-18T00:00:00Z","autorenewEnabled":true}}`, body.Domain.DomainName)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/core/v1/domains/") && !strings.Contains(r.URL.Path, "/records"):
			name := strings.TrimPrefix(r.URL.Path, "/core/v1/domains/")
			m.mu.Lock()
			owned := false
			for _, n := range m.registered {
				if n == name {
					owned = true
				}
			}
			m.mu.Unlock()
			if !owned {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			fmt.Fprintf(w, `{"domainName":%q,"expireDate":"2027-07-18T00:00:00Z","autorenewEnabled":true}`, name)
		case strings.Contains(r.URL.Path, "/records"):
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"records":[],"to":0,"from":0,"totalCount":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	return m
}

func (m *namecomMock) registeredNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.registered...)
}

// seedPGProject idempotently creates a preview_ready project + owner in a real
// store. IDs must be unique per test: CI reuses ONE Postgres across the
// package's PG tests, so fixed ids would collide. Tolerates a leftover user and
// replaces any leftover project.
func seedPGProject(t *testing.T, st store.Store, uid, pid string) {
	t.Helper()
	ctx := context.Background()
	_ = st.CreateUser(ctx, &user.User{ID: uid, Email: uid + "@example.com", CreatedAt: time.Now().UTC()})
	_ = st.DeleteProject(ctx, pid)
	p := &project.Project{
		ID: pid, UserID: uid, Name: "Bageri", Status: project.StatusPreviewReady,
		Locale: "sv", PreviewURL: "https://forge-" + pid + ".fly.dev",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// TestDomainAtCheckout_RealPostgres_MockedNameCom covers the domain-bundled-at-
// checkout flow through the REAL Postgres store and the REAL name.com client
// (pointed at a mock API) — the two layers the unit tests fake, and exactly
// where the shipped bug lived: SetDomainIntent validated the domain against the
// registrar, then failed to persist because UpdateProject's bindings were off,
// surfacing to the customer as "not registrable".
func TestDomainAtCheckout_RealPostgres_MockedNameCom(t *testing.T) {
	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, pgtest.Start(t))
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer pg.Close()

	mock := newNamecomMock()
	defer mock.Close()

	orch, _ := newTestOrchWithVerifier(pg, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	// Real name.com client, real store, cap 300 SEK.
	orch.SetDomains(namecom.New(mock.URL, "forge-test", "tok", 10), &fakeBiller{}, 300)

	seedPGProject(t, pg, "u-checkout", "p-checkout")

	// 1. Pre-checkout: the customer picks a domain to BUY. This re-checks the
	//    registrar (mock) AND writes the intent to Postgres — the step that broke.
	if err := orch.SetDomainIntent(ctx, "p-checkout", "mittbageri.se", true); err != nil {
		t.Fatalf("SetDomainIntent(buy): %v", err)
	}
	got, err := pg.ProjectByID(ctx, "p-checkout")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.DomainIntent != "mittbageri.se" || !got.DomainIntentBuy {
		t.Fatalf("intent not persisted to Postgres: intent=%q buy=%v", got.DomainIntent, got.DomainIntentBuy)
	}

	// 2. Payment settles → the bundled domain is provisioned automatically:
	//    registered via name.com and the intent cleared. name.com registers
	//    SYNCHRONOUSLY, so the async reconcile may already have advanced the
	//    status past 'registering' (→ verifying/active) by the time we poll —
	//    any post-registration state proves the provision fired.
	if err := orch.SubscriptionStarted("p-checkout", "cus_1", "sub_1"); err != nil {
		t.Fatalf("SubscriptionStarted: %v", err)
	}
	got = waitForDomain(t, pg, "p-checkout", func(pr *project.Project) bool {
		switch pr.DomainStatus {
		case project.DomainRegistering, project.DomainVerifying, project.DomainActive:
			return true
		}
		return false
	})
	if got.DomainKind != project.DomainKindPurchased || got.DomainName != "mittbageri.se" {
		t.Fatalf("after provision: kind=%q name=%q", got.DomainKind, got.DomainName)
	}
	if got.DomainIntent != "" {
		t.Errorf("intent should be cleared after provisioning, got %q", got.DomainIntent)
	}
	if reg := mock.registeredNames(); len(reg) != 1 || reg[0] != "mittbageri.se" {
		t.Fatalf("name.com register calls = %v, want [mittbageri.se]", reg)
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

	mock := newNamecomMock() // fixed price 9.9 USD → 99 SEK, renewal 169
	defer mock.Close()

	orch, _ := newTestOrchWithVerifier(pg, NoopVerifier{})
	orch.SetDomains(namecom.New(mock.URL, "forge-test", "tok", 10), &fakeBiller{}, 50) // cap below price

	seedPGProject(t, pg, "u-pricey", "p-pricey")

	if err := orch.SetDomainIntent(ctx, "p-pricey", "mittbageri.se", true); err != ErrDomainTooPricey {
		t.Fatalf("over-cap buy should be ErrDomainTooPricey, got %v", err)
	}
	if got, _ := pg.ProjectByID(ctx, "p-pricey"); got.DomainIntent != "" {
		t.Fatalf("rejected intent must not persist, got %q", got.DomainIntent)
	}
}
