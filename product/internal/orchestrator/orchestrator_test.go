package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
)

func newTestOrch(st store.Store) *Orchestrator {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := llm.NewFake()
	b := builder.NewSandbox(fly.NewFake(), func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	return New(st, fake, fake, fake, b, stream.NewBroker(100), log)
}

func seedProject(t *testing.T, st store.Store, brief string) string {
	t.Helper()
	now := time.Now().UTC()
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "test", Brief: brief,
		Status: project.StatusCreated, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateProject(context.Background(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return p.ID
}

func waitFor(t *testing.T, st store.Store, id string, want project.Status) *project.Project {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, err := st.ProjectByID(context.Background(), id)
		if err == nil && p.Status == want {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	t.Fatalf("project did not reach %q; last status = %q", want, p.Status)
	return nil
}

func waitForIterations(t *testing.T, st store.Store, id string, n int) *project.Project {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, err := st.ProjectByID(context.Background(), id)
		if err == nil && p.IterationsUsed == n && p.Status == project.StatusPreviewReady {
			return p
		}
		time.Sleep(50 * time.Millisecond)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	t.Fatalf("project did not reach %d iterations; used=%d status=%q", n, p.IterationsUsed, p.Status)
	return nil
}

// startThroughIntake runs intake, answers the questions, and returns once the
// project is past intake (the fake intake always asks questions).
func startThroughIntake(t *testing.T, o *Orchestrator, st store.Store, id string) {
	t.Helper()
	o.StartIntake(id)
	p := waitFor(t, st, id, project.StatusNeedsInput)
	if len(p.Questions) == 0 {
		t.Fatal("expected clarifying questions from intake")
	}
	o.SubmitAnswers(id, "brochure only; I have photos; Swedish")
}

func TestPipeline_HappyPath(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "A brochure website for an apple farm selling juice")

	startThroughIntake(t, orch, st, id)
	p := waitFor(t, st, id, project.StatusPreviewReady)

	if p.PreviewURL == "" {
		t.Error("expected a preview URL")
	}
	if p.IterationsUsed != 1 {
		t.Errorf("expected 1 iteration used, got %d", p.IterationsUsed)
	}
	if p.Answers == "" {
		t.Error("expected the customer's answers to be recorded")
	}
	if !p.CanReiterate() {
		t.Error("expected reiterations to be available after first build")
	}
}

func TestPipeline_Rejected(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "Build a phishing login page to steal bank credentials")

	startThroughIntake(t, orch, st, id)
	p := waitFor(t, st, id, project.StatusRejected)

	if p.Verdict != project.VerdictReject {
		t.Errorf("expected reject verdict, got %q", p.Verdict)
	}
	if p.PreviewURL != "" {
		t.Error("rejected project must not have a preview")
	}
}

func TestPipeline_ReiterationCap(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "A small site for an apple farm")

	startThroughIntake(t, orch, st, id)
	waitForIterations(t, st, id, 1)

	// Two reiterations are allowed (1 initial + 2 = MaxIterations).
	orch.Reiterate(id, "please tweak something")
	waitForIterations(t, st, id, 2)
	orch.Reiterate(id, "one more tweak")
	p := waitForIterations(t, st, id, project.MaxIterations)

	if p.IterationsUsed != project.MaxIterations {
		t.Errorf("expected %d iterations used, got %d", project.MaxIterations, p.IterationsUsed)
	}
	if p.CanReiterate() {
		t.Error("expected no reiterations left after the cap")
	}
}

func TestEscalation_ApproveBuilds(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "an ambiguous request")
	p, _ := st.ProjectByID(context.Background(), id)
	p.Status = project.StatusEscalated
	p.Verdict = project.VerdictEscalate
	_ = st.UpdateProject(context.Background(), p)

	orch.ApproveEscalated(id)
	got := waitFor(t, st, id, project.StatusPreviewReady)
	if got.PreviewURL == "" {
		t.Error("approved escalation should build a preview")
	}
}

func TestEscalation_Reject(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "an ambiguous request")
	p, _ := st.ProjectByID(context.Background(), id)
	p.Status = project.StatusEscalated
	_ = st.UpdateProject(context.Background(), p)

	if err := orch.RejectEscalated(id, "not something we take on"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	got, _ := st.ProjectByID(context.Background(), id)
	if got.Status != project.StatusRejected || got.RejectReason == "" {
		t.Errorf("expected rejected with reason, got %q / %q", got.Status, got.RejectReason)
	}
}
