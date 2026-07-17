package orchestrator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"strings"
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
)

// newReviewTestOrch is newTestOrch but keeps the storage handle so a test can
// plant a snapshot tarball, and wires the Fake as the review fallback (the
// same wiring cmd/server does in dev mode).
func newReviewTestOrch(st store.Store) (*Orchestrator, *storage.Memory) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	mem := storage.NewMemory()
	o := New(st, fake, fake, fake, b, machines, mem, stream.NewBroker(100), NoopVerifier{}, log)
	o.SetReviewer(fake)
	return o, mem
}

// makeSnapshot builds a workspace tarball the way the sandbox does
// (./-relative paths, gzip).
func makeSnapshot(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func seedPaidSnapshot(t *testing.T, st store.Store, mem *storage.Memory) string {
	t.Helper()
	ctx := context.Background()
	tgz := makeSnapshot(t, map[string]string{
		"main.go":    "package main\n\nfunc main() {}\n",
		"go.mod":     "module site\n",
		"index.html": "<html></html>\n",
	})
	if err := mem.Put(ctx, "projects/p1/snapshot.tgz", "application/gzip", bytes.NewReader(tgz), int64(len(tgz))); err != nil {
		t.Fatalf("put snapshot: %v", err)
	}
	now := time.Now().UTC()
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "test", Brief: "an apple farm site",
		Status: project.StatusPreviewReady, SnapshotKey: "projects/p1/snapshot.tgz",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return p.ID
}

func TestCodeReview_MarkPaidRunsOnce(t *testing.T) {
	st := store.NewMemory()
	o, mem := newReviewTestOrch(st)
	id := seedPaidSnapshot(t, st, mem)

	if err := o.MarkPaid(id, "manual"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	// The review runs in the background off MarkPaid — wait for it to land.
	deadline := time.Now().Add(8 * time.Second)
	var p *project.Project
	for time.Now().Before(deadline) {
		p, _ = st.ProjectByID(context.Background(), id)
		if !p.CodeReviewAt.IsZero() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.CodeReviewAt.IsZero() {
		t.Fatal("code review never ran after MarkPaid")
	}
	if !strings.HasPrefix(p.CodeReview, "SHIP") {
		t.Fatalf("expected the fake's SHIP verdict, got %q", p.CodeReview)
	}
	if !CodeReviewVerdictClean(p.CodeReview) {
		t.Error("SHIP verdict should read as clean")
	}

	// One-shot: a second (non-forced) run must not overwrite the stored review.
	p.CodeReview = "FIX\n\nsentinel"
	if err := o.save(context.Background(), p); err != nil {
		t.Fatalf("save sentinel: %v", err)
	}
	if err := o.runCodeReview(context.Background(), id, false); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if p.CodeReview != "FIX\n\nsentinel" {
		t.Fatalf("one-shot guard failed; review overwritten: %q", p.CodeReview)
	}
	if CodeReviewVerdictClean(p.CodeReview) {
		t.Error("FIX verdict should not read as clean")
	}

	// force=true (the operator's "run again") re-reviews.
	if err := o.runCodeReview(context.Background(), id, true); err != nil {
		t.Fatalf("forced re-run: %v", err)
	}
	p, _ = st.ProjectByID(context.Background(), id)
	if !strings.HasPrefix(p.CodeReview, "SHIP") {
		t.Fatalf("forced run should refresh the review, got %q", p.CodeReview)
	}
}

func TestCodeReview_NoSnapshotSkipsQuietly(t *testing.T) {
	st := store.NewMemory()
	o, _ := newReviewTestOrch(st)
	id := seedProject(t, st, "an apple farm site")

	if err := o.runCodeReview(context.Background(), id, false); err != nil {
		t.Fatalf("expected a quiet skip, got %v", err)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	if !p.CodeReviewAt.IsZero() || p.CodeReview != "" {
		t.Error("review must not be stamped when there is nothing to review")
	}
}

func TestSourceBundle_FiltersAndTruncates(t *testing.T) {
	big := strings.Repeat("x", maxReviewFileBytes+100)
	tgz := makeSnapshot(t, map[string]string{
		"main.go":              "package main\n",
		"static/logo.png":      "\x89PNG binary",
		"node_modules/x/x.js":  "junk",
		"go.sum":               "checksums",
		"big.md":               big,
		"templates/index.html": "<html></html>",
	})
	bundle, err := sourceBundle(tgz)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	for _, want := range []string{"=== FILE: main.go ===", "=== FILE: templates/index.html ===", "=== FILE: big.md (TRUNCATED) ==="} {
		if !strings.Contains(bundle, want) {
			t.Errorf("bundle missing %q", want)
		}
	}
	for _, not := range []string{"logo.png", "node_modules", "go.sum"} {
		if strings.Contains(bundle, not) {
			t.Errorf("bundle should not include %q", not)
		}
	}
}

func TestOperatorIterate_KeepsAcceptedStateAndCredits(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	id := seedProject(t, st, "A small site for an apple farm")
	startThroughIntake(t, orch, st, id)
	waitFor(t, st, id, project.StatusPreviewReady)

	if err := orch.AcceptPreview(id); err != nil {
		t.Fatalf("accept: %v", err)
	}
	before, _ := st.ProjectByID(context.Background(), id)

	orch.OperatorIterate(id, "tighten the hero copy")
	// building → preview_ready → restored to accepted, back in the queue.
	p := waitFor(t, st, id, project.StatusAccepted)

	if p.IterationsUsed != before.IterationsUsed {
		t.Errorf("operator fix consumed a customer credit: %d → %d", before.IterationsUsed, p.IterationsUsed)
	}
	its, err := st.IterationsByProject(context.Background(), id)
	if err != nil {
		t.Fatalf("iterations: %v", err)
	}
	found := false
	for _, it := range its {
		if it.Number == 0 && it.Prompt == "tighten the hero copy" {
			found = true
		}
	}
	if !found {
		t.Error("expected an internal (#0) iteration carrying the operator's instructions")
	}
}

func TestOperatorIterate_IgnoresWrongStateAndEmptyPrompt(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newReviewTestOrch(st)
	id := seedProject(t, st, "an apple farm site") // status: created

	orch.OperatorIterate(id, "do something") // wrong state → no-op
	orch.OperatorIterate(id, "   ")          // empty prompt → no-op
	p, _ := st.ProjectByID(context.Background(), id)
	if p.Status != project.StatusCreated {
		t.Fatalf("operator iterate must not touch a project outside accepted/preview_ready, got %q", p.Status)
	}
}
