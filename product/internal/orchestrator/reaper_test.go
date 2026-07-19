package orchestrator

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

func seedWithStatus(t *testing.T, st store.Store, id string, status project.Status, preview string, updated time.Time) {
	t.Helper()
	p := &project.Project{
		ID: id, UserID: "u1", Name: id, Brief: "b", Status: status,
		PreviewURL: preview, CreatedAt: updated, UpdatedAt: updated,
	}
	if preview != "" {
		p.IterationsUsed = 1
		p.Verdict = project.VerdictAllow
	}
	if err := st.CreateProject(context.Background(), p); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestReap_ExpiresIdlePreviews(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()
	now := time.Now().UTC()

	seedWithStatus(t, st, "fresh", project.StatusPreviewReady, "https://forge-fresh.fly.dev", now.Add(-time.Hour))
	seedWithStatus(t, st, "stale", project.StatusPreviewReady, "https://forge-stale.fly.dev", now.Add(-15*24*time.Hour))

	orch.Reap(ctx, 14*24*time.Hour)

	destroyed := machines.DestroyedApps()
	if !slices.Contains(destroyed, builder.DeployAppName("stale")) {
		t.Errorf("stale preview app not destroyed; destroyed: %v", destroyed)
	}
	if slices.Contains(destroyed, builder.DeployAppName("fresh")) {
		t.Errorf("fresh preview app must not be destroyed; destroyed: %v", destroyed)
	}

	stale, _ := st.ProjectByID(ctx, "stale")
	if stale.Status != project.StatusExpired || stale.RejectReason == "" {
		t.Errorf("stale project should be expired with a reason, got %q / %q", stale.Status, stale.RejectReason)
	}
	fresh, _ := st.ProjectByID(ctx, "fresh")
	if fresh.Status != project.StatusPreviewReady {
		t.Errorf("fresh project should stay preview_ready, got %q", fresh.Status)
	}
}

func TestReap_PaidPreviewSurvives(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()
	old := time.Now().UTC().Add(-15 * 24 * time.Hour)

	// A paid subscriber's idle preview must survive; an unpaid one of the same
	// age is the control that expires.
	seedWithStatus(t, st, "paid", project.StatusPreviewReady, "https://forge-paid.fly.dev", old)
	p, _ := st.ProjectByID(ctx, "paid")
	p.Paid = true
	if err := st.UpdateProject(ctx, p); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	seedWithStatus(t, st, "unpaid", project.StatusPreviewReady, "https://forge-unpaid.fly.dev", old)

	orch.Reap(ctx, 14*24*time.Hour)

	if slices.Contains(machines.DestroyedApps(), builder.DeployAppName("paid")) {
		t.Errorf("paid preview must survive the reaper; destroyed: %v", machines.DestroyedApps())
	}
	if got, _ := st.ProjectByID(ctx, "paid"); got.Status != project.StatusPreviewReady {
		t.Errorf("paid project should stay preview_ready, got %q", got.Status)
	}
	if got, _ := st.ProjectByID(ctx, "unpaid"); got.Status != project.StatusExpired {
		t.Errorf("unpaid control should expire, got %q", got.Status)
	}
}

func TestReap_DestroysFailedBuildApps(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()
	now := time.Now().UTC()

	// First build failed after the gate allowed it — an app may exist.
	p := &project.Project{
		ID: "failed1", UserID: "u1", Name: "x", Brief: "b",
		Status: project.StatusFailed, Verdict: project.VerdictAllow,
		CreatedAt: now, UpdatedAt: now,
	}
	_ = st.CreateProject(ctx, p)
	// Rejected pre-build — no app was ever created; must not be touched.
	seedWithStatus(t, st, "rejected1", project.StatusRejected, "", now)

	orch.Reap(ctx, 14*24*time.Hour)

	destroyed := machines.DestroyedApps()
	if !slices.Contains(destroyed, builder.DeployAppName("failed1")) {
		t.Errorf("failed project's app not destroyed; destroyed: %v", destroyed)
	}
	if slices.Contains(destroyed, builder.DeployAppName("rejected1")) {
		t.Errorf("rejected (never-built) project must not be touched; destroyed: %v", destroyed)
	}
	got, _ := st.ProjectByID(ctx, "failed1")
	if got.Status != project.StatusFailed {
		t.Errorf("failed project stays failed, got %q", got.Status)
	}
}

func TestDestroyPreview_OperatorAction(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()

	seedWithStatus(t, st, "live1", project.StatusPreviewReady, "https://forge-live1.fly.dev", time.Now().UTC())

	if err := orch.DestroyPreview(ctx, "live1"); err != nil {
		t.Fatalf("destroy preview: %v", err)
	}
	if !slices.Contains(machines.DestroyedApps(), builder.DeployAppName("live1")) {
		t.Error("preview app was not destroyed")
	}
	got, _ := st.ProjectByID(ctx, "live1")
	if got.Status != project.StatusExpired {
		t.Errorf("project should be expired after operator destroy, got %q", got.Status)
	}
}

func TestPurgeProject_RemovesAppAndRow(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()

	seedWithStatus(t, st, "live1", project.StatusPreviewReady, "https://forge-live1.fly.dev", time.Now().UTC())

	if err := orch.PurgeProject(ctx, "live1"); err != nil {
		t.Fatalf("purge project: %v", err)
	}
	if !slices.Contains(machines.DestroyedApps(), builder.DeployAppName("live1")) {
		t.Error("preview app was not destroyed")
	}
	if _, err := st.ProjectByID(ctx, "live1"); err == nil {
		t.Error("project row should be gone after purge")
	}
}

func TestPurgeAllProjects_CleansEverything(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()

	seedWithStatus(t, st, "a", project.StatusPreviewReady, "https://forge-a.fly.dev", time.Now().UTC())
	seedWithStatus(t, st, "b", project.StatusFailed, "", time.Now().UTC())
	seedWithStatus(t, st, "c", project.StatusDelivered, "https://forge-c.fly.dev", time.Now().UTC())

	n, err := orch.PurgeAllProjects(ctx)
	if err != nil {
		t.Fatalf("purge all: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 purged, got %d", n)
	}
	all, _ := st.Projects(ctx)
	if len(all) != 0 {
		t.Errorf("expected no projects left, got %d", len(all))
	}
	for _, id := range []string{"a", "c"} {
		if !slices.Contains(machines.DestroyedApps(), builder.DeployAppName(id)) {
			t.Errorf("app for %s not destroyed", id)
		}
	}
}

// The orphan sweep: a sandbox machine whose project has no active build dies
// after the short grace; a machine owned by a running build survives. This is
// the "restart interrupted the teardown, agents left burning tokens" case —
// before this sweep an orphan lived until the 150-minute age backstop.
func TestReap_SweepsOrphanedSandboxes(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()
	now := time.Now().UTC()

	// An active build owns m-active (fresh heartbeat, so the zombie pass
	// leaves it alone and it lands on the sweep's keep-list).
	seedWithStatus(t, st, "building", project.StatusBuilding, "", now)
	_ = st.CreateIteration(ctx, &project.Iteration{
		ID: "i-act", ProjectID: "building", Number: 1, Status: project.StatusBuilding,
		MachineID: "m-active", CreatedAt: now, HeartbeatAt: now,
	})

	machines.AddSandboxMachine("m-active", now.Add(-60*time.Minute)) // old but owned → keep
	machines.AddSandboxMachine("m-orphan", now.Add(-30*time.Minute)) // old, unowned → reap
	machines.AddSandboxMachine("m-fresh", now.Add(-5*time.Minute))   // young, unowned → grace
	machines.AddSandboxMachine("m-ancient", now.Add(-3*time.Hour))   // unowned + past backstop → reap

	orch.Reap(ctx, 0)

	alive := machines.SandboxMachines()
	if !slices.Contains(alive, "m-active") {
		t.Errorf("active build's machine was reaped; alive: %v", alive)
	}
	if !slices.Contains(alive, "m-fresh") {
		t.Errorf("machine inside the grace window was reaped; alive: %v", alive)
	}
	for _, gone := range []string{"m-orphan", "m-ancient"} {
		if slices.Contains(alive, gone) {
			t.Errorf("%s should have been reaped; alive: %v", gone, alive)
		}
	}
}

// A deploy or crash mid-planning kills the driving goroutine, leaving no
// iteration row for build recovery to find. The reaper must fail such a stranded
// project so the customer gets a Retry button (and the quota lock frees) instead
// of a permanent spinner — but only once it's genuinely stale, and never a
// resting state that's legitimately waiting on the customer.
func TestReap_FailsStrandedPreBuild(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	ctx := context.Background()
	now := time.Now().UTC()

	seedWithStatus(t, st, "stranded", project.StatusPlanning, "", now.Add(-30*time.Minute))
	seedWithStatus(t, st, "recent", project.StatusPlanning, "", now.Add(-time.Minute))
	seedWithStatus(t, st, "waiting", project.StatusNeedsInput, "", now.Add(-30*time.Minute))

	orch.Reap(ctx, 0)

	if p, _ := st.ProjectByID(ctx, "stranded"); p.Status != project.StatusFailed {
		t.Errorf("stale pre-build project should be failed, got %q", p.Status)
	} else if !p.CanRetry() {
		t.Error("failed pre-build project must be retryable")
	}
	if p, _ := st.ProjectByID(ctx, "recent"); p.Status != project.StatusPlanning {
		t.Errorf("a recently-active pre-build project must be left alone, got %q", p.Status)
	}
	if p, _ := st.ProjectByID(ctx, "waiting"); p.Status != project.StatusNeedsInput {
		t.Errorf("needs_input rests on the customer and must not be failed, got %q", p.Status)
	}
}
