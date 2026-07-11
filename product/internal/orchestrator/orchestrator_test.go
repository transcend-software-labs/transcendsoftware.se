package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

func newTestOrch(st store.Store) *Orchestrator {
	o, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	return o
}

func newTestOrchWithVerifier(st store.Store, v Verifier) (*Orchestrator, *fly.Fake) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	return New(st, fake, fake, fake, b, machines, storage.NewMemory(), stream.NewBroker(100), v, log), machines
}

// recordingNotifier captures sent messages for assertions.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []sentMsg
}

type sentMsg struct{ To, Subject string }

func (n *recordingNotifier) Send(_ context.Context, to, subject, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, sentMsg{To: to, Subject: subject})
	return nil
}

func (n *recordingNotifier) all() []sentMsg {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]sentMsg, len(n.sent))
	copy(out, n.sent)
	return out
}

// countingVerifier fails every call after the first okCalls calls.
type countingVerifier struct {
	mu      sync.Mutex
	okCalls int
	calls   int
}

func (v *countingVerifier) Verify(context.Context, string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.calls > v.okCalls {
		return errors.New("site did not come up")
	}
	return nil
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

// answerIntake runs intake and answers the questions (picking a suggested
// design), leaving the project to plan+screen. It does NOT approve — callers
// that expect a build must approve the gate (or use startThroughIntake).
func answerIntake(t *testing.T, o *Orchestrator, st store.Store, id string) {
	t.Helper()
	o.StartIntake(id)
	p := waitFor(t, st, id, project.StatusNeedsInput)
	if len(p.Questions) == 0 {
		t.Fatal("expected clarifying questions from intake")
	}
	if len(p.DesignOptions) == 0 {
		t.Fatal("expected design suggestions from intake")
	}
	o.SubmitAnswers(id, "brochure only; I have photos; Swedish",
		p.DesignOptions[0].Name+" — "+p.DesignOptions[0].Description)
}

// startThroughIntake runs intake, answers, then approves the plan at the gate —
// returning with a build under way.
func startThroughIntake(t *testing.T, o *Orchestrator, st store.Store, id string) {
	t.Helper()
	answerIntake(t, o, st, id)
	// Planning now stops at the approval gate; approve to start the build.
	ap := waitFor(t, st, id, project.StatusAwaitingApproval)
	if len(ap.Spec.Pages) == 0 {
		t.Error("expected the plan spec to be parsed for the scope card")
	}
	o.ApprovePlan(id)
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
	if p.DesignBrief == "" {
		t.Error("expected the chosen design direction to be recorded")
	}
	if !strings.Contains(p.EffectiveBrief(), "Design direction") {
		t.Error("expected the design direction to reach the planner via the brief")
	}
	if !p.CanReiterate() {
		t.Error("expected reiterations to be available after first build")
	}
}

func TestPipeline_Rejected(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "Build a phishing login page to steal bank credentials")

	answerIntake(t, orch, st, id) // rejected at the safety gate, never reaches the approval gate
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

func TestEscalation_ApproveRoutesToCustomerGate(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	id := seedProject(t, st, "an ambiguous request")
	p, _ := st.ProjectByID(context.Background(), id)
	p.Status = project.StatusEscalated
	p.Verdict = project.VerdictEscalate
	_ = st.UpdateProject(context.Background(), p)

	// Operator approval clears the safety concern but does NOT build — it hands
	// the project to the customer's own approval + content gate.
	orch.ApproveEscalated(id)
	got := waitFor(t, st, id, project.StatusAwaitingApproval)
	if got.Verdict != project.VerdictAllow {
		t.Errorf("operator approval should mark the verdict allow, got %q", got.Verdict)
	}
	if got.PreviewURL != "" {
		t.Error("no build should have run yet — the customer hasn't approved")
	}

	// The customer approving the plan starts the build.
	orch.ApprovePlan(id)
	done := waitFor(t, st, id, project.StatusPreviewReady)
	if done.PreviewURL == "" {
		t.Error("customer approval should build a preview")
	}
}

func TestRecoverInterrupted(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	ctx := context.Background()
	now := time.Now().UTC()

	// A project + build pass left "building" by a previous (crashed) run.
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "x", Brief: "b",
		Status: project.StatusBuilding, CreatedAt: now, UpdatedAt: now,
	}
	_ = st.CreateProject(ctx, p)
	it := &project.Iteration{
		ID: "i1", ProjectID: "p1", Number: 1, Status: project.StatusBuilding,
		MachineID: "m-123", CreatedAt: now,
	}
	_ = st.CreateIteration(ctx, it)

	orch.RecoverInterrupted(ctx)

	its, _ := st.IterationsByProject(ctx, "p1")
	if len(its) != 1 || its[0].Status != project.StatusFailed {
		t.Errorf("expected interrupted iteration marked failed, got %+v", its)
	}
	gotP, _ := st.ProjectByID(ctx, "p1")
	if gotP.Status != project.StatusFailed {
		t.Errorf("expected project marked failed, got %q", gotP.Status)
	}
	if active, _ := st.ActiveIterations(ctx); len(active) != 0 {
		t.Errorf("expected no active builds after recovery, got %d", len(active))
	}
}

// A build that was in flight at restart, whose sandbox is still reachable and
// whose session handle was persisted, is re-attached and finished — not reaped.
func TestRecoverInterrupted_ReattachesLiveBuild(t *testing.T) {
	// A live TCP listener stands in for the still-running sandbox opencode:
	// reachable() only needs a successful connect.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	st := store.NewMemory()
	orch := newTestOrch(st) // NoopVerifier → the (re-attached) deploy verifies
	ctx := context.Background()
	now := time.Now().UTC()

	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "x", Brief: "b",
		Status: project.StatusBuilding, CreatedAt: now, UpdatedAt: now,
	}
	_ = st.CreateProject(ctx, p)
	it := &project.Iteration{
		ID: "i1", ProjectID: "p1", Number: 1, Status: project.StatusBuilding,
		MachineID: "m-123", SessionID: "ses_live",
		SandboxAddr: "http://" + ln.Addr().String(), CreatedAt: now,
	}
	_ = st.CreateIteration(ctx, it)

	orch.RecoverInterrupted(ctx)

	// Re-attach finishes the build in the background → preview ready, NOT failed.
	gotP := waitFor(t, st, "p1", project.StatusPreviewReady)
	if gotP.PreviewURL == "" {
		t.Error("expected a preview URL after the re-attached build finished")
	}
	its, _ := st.IterationsByProject(ctx, "p1")
	if len(its) != 1 || its[0].Status != project.StatusPreviewReady {
		t.Errorf("expected the re-attached iteration at preview_ready, got %+v", its)
	}
}

// A build whose sandbox is gone (unreachable address) can't be re-attached, so
// recovery falls back to reaping it and marking it failed.
func TestRecoverInterrupted_ReapsWhenSandboxGone(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	ctx := context.Background()
	now := time.Now().UTC()

	// A port that was open then closed — connect is refused immediately, so the
	// reachability probe fails fast (a dead sandbox).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := "http://" + ln.Addr().String()
	_ = ln.Close()

	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "x", Brief: "b",
		Status: project.StatusBuilding, CreatedAt: now, UpdatedAt: now,
	}
	_ = st.CreateProject(ctx, p)
	it := &project.Iteration{
		ID: "i1", ProjectID: "p1", Number: 1, Status: project.StatusBuilding,
		MachineID: "m-123", SessionID: "ses_dead",
		SandboxAddr: deadAddr, CreatedAt: now,
	}
	_ = st.CreateIteration(ctx, it)

	orch.RecoverInterrupted(ctx)

	gotP, _ := st.ProjectByID(ctx, "p1")
	if gotP.Status != project.StatusFailed {
		t.Errorf("expected project failed when sandbox is gone, got %q", gotP.Status)
	}
	if active, _ := st.ActiveIterations(ctx); len(active) != 0 {
		t.Errorf("expected no active builds after reap, got %d", len(active))
	}
}

func TestReiteration_ContinuesFromSnapshot(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	id := seedProject(t, st, "A small site for an apple farm")

	startThroughIntake(t, orch, st, id)
	p := waitForIterations(t, st, id, 1)

	// The first successful build must persist a workspace snapshot key.
	if p.SnapshotKey == "" {
		t.Fatal("expected a snapshot key after the first build")
	}

	orch.Reiterate(id, "make the hero bigger")
	waitForIterations(t, st, id, 2)

	// The reiteration must have restored the previous workspace before the
	// agent ran (the restore exec references the presigned snapshot URL —
	// memory storage presigns to memory://<key>).
	var restored bool
	for _, e := range machines.Execs() {
		script := strings.Join(e.Command, " ")
		if strings.Contains(script, "tar -xzf") && strings.Contains(script, "memory://"+p.SnapshotKey) {
			restored = true
		}
	}
	if !restored {
		t.Errorf("reiteration did not restore the workspace snapshot; execs: %+v", machines.Execs())
	}
}

func TestNotify_CustomerOnReady_OperatorOnFailure(t *testing.T) {
	// Customer notified when the preview is ready.
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://app.example")
	if err := st.CreateUser(context.Background(),
		&user.User{ID: "u1", Email: "customer@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	id := seedProject(t, st, "A small site for an apple farm")
	startThroughIntake(t, orch, st, id)
	waitFor(t, st, id, project.StatusPreviewReady)

	if !sentTo(rec, "customer@example.com", "ready") {
		t.Errorf("customer not emailed on preview ready; sent: %+v", rec.all())
	}

	// Operator notified when a build fails.
	st2 := store.NewMemory()
	orch2, _ := newTestOrchWithVerifier(st2, &countingVerifier{okCalls: 0}) // always fails
	rec2 := &recordingNotifier{}
	orch2.SetNotifications(rec2, "rasmus@example.com", "https://app.example")
	id2 := seedProject(t, st2, "A small site for an apple farm")
	startThroughIntake(t, orch2, st2, id2)
	waitFor(t, st2, id2, project.StatusFailed)

	if !sentTo(rec2, "rasmus@example.com", "failed") {
		t.Errorf("operator not emailed on build failure; sent: %+v", rec2.all())
	}
}

func sentTo(rec *recordingNotifier, to, subjectSubstr string) bool {
	for _, m := range rec.all() {
		if m.To == to && strings.Contains(strings.ToLower(m.Subject), subjectSubstr) {
			return true
		}
	}
	return false
}

func TestTemplate_SeedsFirstBuildNotReiterations(t *testing.T) {
	st := store.NewMemory()
	orch, machines := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetTemplate("templates/goapp.tgz")
	id := seedProject(t, st, "A small site for an apple farm")

	startThroughIntake(t, orch, st, id)
	waitForIterations(t, st, id, 1)

	// Build 1 must have unpacked the template (memory storage presigns to
	// memory://<key>).
	var templated bool
	for _, e := range machines.Execs() {
		if strings.Contains(strings.Join(e.Command, " "), "memory://templates/goapp.tgz") {
			templated = true
		}
	}
	if !templated {
		t.Fatalf("first build did not seed the template; execs: %+v", machines.Execs())
	}

	// The reiteration must restore the project snapshot, not the template.
	orch.Reiterate(id, "make the hero bigger")
	waitForIterations(t, st, id, 2)
	var tplCount int
	for _, e := range machines.Execs() {
		if strings.Contains(strings.Join(e.Command, " "), "memory://templates/goapp.tgz") {
			tplCount++
		}
	}
	if tplCount != 1 {
		t.Errorf("template must seed only the first build, seen %d times", tplCount)
	}
}

func TestHandover_AcceptReviewDeliver(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://app.example")
	if err := st.CreateUser(context.Background(),
		&user.User{ID: "u1", Email: "customer@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	id := seedProject(t, st, "A small site for an apple farm")
	startThroughIntake(t, orch, st, id)
	waitFor(t, st, id, project.StatusPreviewReady)

	// Customer accepts → operator queue + operator notified.
	if err := orch.AcceptPreview(id); err != nil {
		t.Fatalf("accept: %v", err)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	if p.Status != project.StatusAccepted {
		t.Fatalf("expected accepted, got %q", p.Status)
	}
	if p.CanReiterate() || p.CanAccept() {
		t.Error("accepted project should not be re-acceptable or reiterable")
	}
	if !sentTo(rec, "rasmus@example.com", "review") {
		t.Errorf("operator not notified on accept; sent: %+v", rec.all())
	}

	// Delivery is gated on payment: an unpaid accepted project is refused.
	if err := orch.DeliverProject(id); !errors.Is(err, ErrNotPaid) {
		t.Fatalf("unpaid deliver should return ErrNotPaid, got %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if p.Status == project.StatusDelivered {
		t.Fatal("unpaid project must not be delivered")
	}

	// Mark it paid (Rasmus comping a friend / a Stripe webhook) → deliver works.
	if err := orch.MarkPaid(id, "manual"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if !p.Paid || p.PaidVia != "manual" || p.PaidAt.IsZero() {
		t.Fatalf("mark paid did not record state: %+v", p)
	}

	// Rasmus delivers → terminal + customer notified.
	if err := orch.DeliverProject(id); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if p.Status != project.StatusDelivered {
		t.Fatalf("expected delivered, got %q", p.Status)
	}
	if !sentTo(rec, "customer@example.com", "delivered") {
		t.Errorf("customer not notified on delivery; sent: %+v", rec.all())
	}

	// MarkUnpaid reverses the flag (for a mistaken mark).
	if err := orch.MarkUnpaid(id); err != nil {
		t.Fatalf("mark unpaid: %v", err)
	}
	if p, _ = st.ProjectByID(context.Background(), id); p.Paid {
		t.Error("mark unpaid should clear the flag")
	}
}

func TestHandover_ReturnToCustomerRestoresChanges(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	id := seedProject(t, st, "A small site for an apple farm")
	startThroughIntake(t, orch, st, id)
	waitFor(t, st, id, project.StatusPreviewReady)

	if err := orch.AcceptPreview(id); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// Rasmus sends it back — customer can change and accept again.
	if err := orch.ReturnToCustomer(id, "add opening hours"); err != nil {
		t.Fatalf("return: %v", err)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	if p.Status != project.StatusPreviewReady {
		t.Fatalf("expected preview_ready after return, got %q", p.Status)
	}
	if !p.CanReiterate() || !p.CanAccept() {
		t.Error("returned project should be reiterable and acceptable again")
	}
	// Deliver must refuse a non-accepted project.
	if err := orch.DeliverProject(id); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if p.Status == project.StatusDelivered {
		t.Error("delivery must only work from the accepted state")
	}
}

func TestVerifyFailure_InitialBuildFails(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, &countingVerifier{okCalls: 0}) // always fails
	id := seedProject(t, st, "A small site for an apple farm")

	startThroughIntake(t, orch, st, id)
	p := waitFor(t, st, id, project.StatusFailed)

	if p.IterationsUsed != 0 {
		t.Errorf("failed verification must not consume an iteration, got %d used", p.IterationsUsed)
	}
}

func TestVerifyFailure_ReiterationKeepsPreview(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, &countingVerifier{okCalls: 1}) // build 1 ok, build 2 fails
	id := seedProject(t, st, "A small site for an apple farm")

	startThroughIntake(t, orch, st, id)
	first := waitFor(t, st, id, project.StatusPreviewReady)

	orch.Reiterate(id, "make the hero bigger")

	// Wait for the failed second iteration to land, then check the project
	// fell back to the previous, still-live preview.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		its, _ := st.IterationsByProject(context.Background(), id)
		if len(its) == 2 && its[1].Status == project.StatusFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	p := waitFor(t, st, id, project.StatusPreviewReady)
	if p.IterationsUsed != 1 {
		t.Errorf("failed reiteration must not consume the change credit, got %d used", p.IterationsUsed)
	}
	if p.PreviewURL != first.PreviewURL {
		t.Errorf("preview URL changed after a failed reiteration: %q → %q", first.PreviewURL, p.PreviewURL)
	}
	if !p.CanReiterate() {
		t.Error("customer should still be able to retry the change")
	}
}

func TestRecoverInterrupted_ReiterationRestoresPreview(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	ctx := context.Background()
	now := time.Now().UTC()

	// A reiteration left "building" by a crash — but the first build succeeded,
	// so its preview is still live.
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "x", Brief: "b",
		Status: project.StatusBuilding, PreviewURL: "https://forge-p1.fly.dev",
		IterationsUsed: 1, CreatedAt: now, UpdatedAt: now,
	}
	_ = st.CreateProject(ctx, p)
	it := &project.Iteration{
		ID: "i2", ProjectID: "p1", Number: 2, Status: project.StatusBuilding,
		MachineID: "m-456", CreatedAt: now,
	}
	_ = st.CreateIteration(ctx, it)

	orch.RecoverInterrupted(ctx)

	gotP, _ := st.ProjectByID(ctx, "p1")
	if gotP.Status != project.StatusPreviewReady {
		t.Errorf("expected project restored to preview_ready, got %q", gotP.Status)
	}
	its, _ := st.IterationsByProject(ctx, "p1")
	if len(its) != 1 || its[0].Status != project.StatusFailed {
		t.Errorf("expected the interrupted iteration marked failed, got %+v", its)
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
