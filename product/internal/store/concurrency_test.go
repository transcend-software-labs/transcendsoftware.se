package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

func TestMemoryProjectOptimisticLock(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	p := &project.Project{ID: "versioned", Name: "Original", ContentAnswers: map[string]string{"tagline": "Original"}, CreatedAt: time.Now().UTC()}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	a, _ := st.ProjectByID(ctx, p.ID)
	b, _ := st.ProjectByID(ctx, p.ID)
	a.Name = "First"
	a.ContentAnswers["tagline"] = "First"
	before, _ := st.ProjectByID(ctx, p.ID)
	if before.ContentAnswers["tagline"] != "Original" {
		t.Fatal("mutating a loaded map leaked into the stored project before UpdateProject")
	}
	if err := st.UpdateProject(ctx, a); err != nil {
		t.Fatal(err)
	}
	b.Name = "Stale"
	b.ContentAnswers["tagline"] = "Stale"
	if err := st.UpdateProject(ctx, b); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update = %v, want ErrConflict", err)
	}
	got, _ := st.ProjectByID(ctx, p.ID)
	if got.Name != "First" || got.ContentAnswers["tagline"] != "First" {
		t.Fatalf("stale update clobbered project: name=%q tagline=%q", got.Name, got.ContentAnswers["tagline"])
	}
}

func TestMemoryReserveIterationEnforcesWalletCaps(t *testing.T) {
	st := NewMemory()
	now := time.Now().UTC()
	first := &project.Iteration{ID: "i1", Status: project.StatusBuilding, CreatedAt: now}
	if err := st.ReserveIteration(context.Background(), first, 1, 10); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveIteration(context.Background(), &project.Iteration{ID: "i2", Status: project.StatusBuilding, CreatedAt: now}, 1, 10); !errors.Is(err, ErrBuildCapacity) {
		t.Fatalf("concurrent cap = %v", err)
	}
	first.Status = project.StatusPreviewReady
	if err := st.UpdateIteration(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := st.ReserveIteration(context.Background(), &project.Iteration{ID: "i3", Status: project.StatusBuilding, CreatedAt: now}, 2, 1); !errors.Is(err, ErrBuildDailyCap) {
		t.Fatalf("daily cap = %v", err)
	}
}
