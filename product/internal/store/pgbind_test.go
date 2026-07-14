package store

import (
	"context"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/pgtest"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// TestPostgresProjectBindings exercises the real INSERT/UPDATE/SELECT against a
// live Postgres so a placeholder/arg mismatch — which the in-memory store can't
// catch, and which shipped a broken UpdateProject in the domain-at-checkout work
// — fails loudly here. Uses testcontainers (or PG_TEST_URL); skips if Docker is
// unavailable.
func TestPostgresProjectBindings(t *testing.T) {
	ctx := context.Background()
	st, err := NewPostgres(ctx, pgtest.Start(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	u := &user.User{ID: "bind-u1", Email: "bind@example.com", Verified: true, CreatedAt: now}
	_ = st.CreateUser(ctx, u) // ignore "already exists" on reruns

	p := &project.Project{
		ID: "bindtest1", UserID: u.ID, Name: "Bind", Status: project.StatusPreviewReady,
		PreviewURL: "https://x", CreatedAt: now, UpdatedAt: now,
	}
	_ = st.DeleteProject(ctx, p.ID)
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject (INSERT binding): %v", err)
	}
	// Exactly what SetDomainIntent + BuyDomain + the change meter + the branded
	// preview host do around checkout/build.
	p.DomainIntent, p.DomainIntentBuy = "pelleuttning.se", true
	p.DomainCostOre = 12900
	p.PreviewHost = "bind-preview-a1b2c3"
	p.ChangesThisPeriod, p.ChangePeriodStart, p.DeliveredAt = 2, now, now
	if err := st.UpdateProject(ctx, p); err != nil {
		t.Fatalf("UpdateProject (the shipped $44/$45 binding bug): %v", err)
	}
	got, err := st.ProjectByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("ProjectByID (SELECT/scan binding): %v", err)
	}
	if got.DomainIntent != "pelleuttning.se" || !got.DomainIntentBuy || got.ChangesThisPeriod != 2 || got.DomainCostOre != 12900 || got.PreviewHost != "bind-preview-a1b2c3" {
		t.Fatalf("round-trip lost data: intent=%q buy=%v changes=%d cost=%d host=%q",
			got.DomainIntent, got.DomainIntentBuy, got.ChangesThisPeriod, got.DomainCostOre, got.PreviewHost)
	}
	// The reverse proxy's host lookup resolves through the real index.
	byHost, err := st.ProjectByPreviewHost(ctx, "bind-preview-a1b2c3")
	if err != nil || byHost.ID != p.ID {
		t.Fatalf("ProjectByPreviewHost: got %v err=%v", byHost, err)
	}
	if _, err := st.ProjectByPreviewHost(ctx, ""); err == nil {
		t.Fatal("empty preview host must not resolve to a project")
	}
}
