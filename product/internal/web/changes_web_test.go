package web_test

import (
	"io"
	"log/slog"
	"net/http/httptest"
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
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

// newChangeServer builds a test server with the Forge Pro change policy wired
// (perMonth included changes, overageOre flat overage) but no Stripe — enough to
// render the change panels.
func newChangeServer(t *testing.T, perMonth, overageOre int) (*httptest.Server, store.Store) {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	orch.SetChangePolicy(nil, perMonth, overageOre)
	cfg := config.Config{AdminEmail: "admin@example.com", BaseURL: "https://forge.example"}
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	testStores.Store(ts.URL, testAuthState{store: st, sessions: sessions})
	return ts, st
}

func seedChangeProject(t *testing.T, st store.Store, id string, mutate func(*project.Project)) {
	t.Helper()
	ctx := t.Context()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{
		ID: id, UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady,
		PreviewURL: "https://forge-" + id + ".fly.dev",
		CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	mutate(p)
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

// A delivered, paying subscriber sees the monthly change panel (with the
// included allowance) and the change form — the unpaid "N changes left"
// refinement copy is gone.
func TestChangePanel_PaidDeliveredShowsMonthlyAllowance(t *testing.T) {
	srv, st := newChangeServer(t, 3, 4900)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedChangeProject(t, st, "chg-live", func(p *project.Project) {
		p.Status = project.StatusDelivered
		p.Paid = true
		p.DeliveredAt = time.Now().UTC()
	})

	body := getBody(t, c, srv.URL+"/projects/chg-live")
	if !strings.Contains(body, "/projects/chg-live/reiterate") {
		t.Fatal("delivered paid project should offer the change form")
	}
	if !strings.Contains(body, "3 changes included this month") {
		t.Errorf("expected the monthly-allowance copy; body:\n%s", body)
	}
	if strings.Contains(body, "changes left") {
		t.Error("paid project must not show the unpaid preview-refinement copy")
	}
}

// Once the monthly allowance is spent, the panel discloses the flat overage
// price and that it lands on the next invoice — but the change form stays open.
func TestChangePanel_OverageDisclosedWhenAllowanceSpent(t *testing.T) {
	srv, st := newChangeServer(t, 3, 4900)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedChangeProject(t, st, "chg-over", func(p *project.Project) {
		p.Status = project.StatusDelivered
		p.Paid = true
		p.DeliveredAt = time.Now().UTC()
		p.ChangePeriodStart = time.Now().UTC() // current window, so it doesn't roll
		p.ChangesThisPeriod = 3                // allowance exhausted
	})

	body := getBody(t, c, srv.URL+"/projects/chg-over")
	if !strings.Contains(body, "49 kr") || !strings.Contains(body, "next invoice") {
		t.Errorf("expected the overage price + next-invoice note; body:\n%s", body)
	}
	if !strings.Contains(body, "/projects/chg-over/reiterate") {
		t.Error("overage must not block the change form")
	}
}

// An unpaid preview still gets the free "try before you buy" refinement panel,
// not the monthly change model.
func TestChangePanel_UnpaidShowsFreeRefinements(t *testing.T) {
	srv, st := newChangeServer(t, 3, 4900)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	seedChangeProject(t, st, "chg-free", func(p *project.Project) {}) // preview_ready, unpaid

	body := getBody(t, c, srv.URL+"/projects/chg-free")
	if !strings.Contains(body, "changes left") {
		t.Errorf("unpaid preview should show the free-refinement copy; body:\n%s", body)
	}
	if strings.Contains(body, "included this month") {
		t.Error("unpaid project must not show the paid monthly-allowance copy")
	}
}
