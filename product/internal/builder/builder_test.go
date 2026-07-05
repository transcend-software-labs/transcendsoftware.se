package builder

import (
	"context"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

func newTestBuilder(machines fly.Machines) *Sandbox {
	return NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, Config{})
}

// capturingDriver records the Spec it was run with.
type capturingDriver struct{ spec opencode.Spec }

func (d *capturingDriver) Run(_ context.Context, spec opencode.Spec, _ func(string)) (opencode.Result, error) {
	d.spec = spec
	return opencode.Result{Log: "done"}, nil
}

func TestBuild_SnapshotSaveAfterSuccess(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines)

	res, err := b.Build(context.Background(), Request{
		ProjectID:      "p1",
		Plan:           "build a site",
		SnapshotPutURL: "https://storage.example/put?sig=abc",
	}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !res.SnapshotSaved {
		t.Error("expected the snapshot to be saved")
	}

	execs := machines.Execs()
	if len(execs) != 1 {
		t.Fatalf("expected exactly one exec (the snapshot save), got %d", len(execs))
	}
	script := strings.Join(execs[0].Command, " ")
	if !strings.Contains(script, "tar") || !strings.Contains(script, "https://storage.example/put?sig=abc") {
		t.Errorf("snapshot save exec should tar and upload to the PUT URL, got: %s", script)
	}
}

func TestBuild_ReiterationRestoresBeforeAgent(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines)

	_, err := b.Build(context.Background(), Request{
		ProjectID:      "p1",
		Prompt:         "make the hero bigger",
		SnapshotGetURL: "https://storage.example/get?sig=xyz",
		SnapshotPutURL: "https://storage.example/put?sig=abc",
	}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	execs := machines.Execs()
	if len(execs) != 2 {
		t.Fatalf("expected restore + save execs, got %d", len(execs))
	}
	restore := strings.Join(execs[0].Command, " ")
	if !strings.Contains(restore, "https://storage.example/get?sig=xyz") ||
		!strings.Contains(restore, "tar -xzf") {
		t.Errorf("first exec should restore the snapshot, got: %s", restore)
	}
	save := strings.Join(execs[1].Command, " ")
	if !strings.Contains(save, "https://storage.example/put?sig=abc") {
		t.Errorf("second exec should save the snapshot, got: %s", save)
	}
}

func TestBuild_TemplateSeedsFirstBuild(t *testing.T) {
	machines := fly.NewFake()
	drv := &capturingDriver{}
	b := NewSandbox(machines, func(string) opencode.Driver { return drv }, Config{})

	_, err := b.Build(context.Background(), Request{
		ProjectID:      "p1",
		Plan:           "build a bakery site",
		TemplateGetURL: "https://storage.example/template?sig=tpl",
		SnapshotPutURL: "https://storage.example/put?sig=abc",
	}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	execs := machines.Execs()
	if len(execs) != 2 {
		t.Fatalf("expected template restore + snapshot save, got %d execs", len(execs))
	}
	if got := strings.Join(execs[0].Command, " "); !strings.Contains(got, "https://storage.example/template?sig=tpl") {
		t.Errorf("first exec should unpack the template, got: %s", got)
	}
	if !strings.Contains(drv.spec.Instruction, "AGENTS.md") ||
		!strings.Contains(drv.spec.Instruction, "build a bakery site") {
		t.Errorf("instruction should carry the template preamble + plan, got: %s", drv.spec.Instruction)
	}
}

func TestBuild_SnapshotWinsOverTemplate(t *testing.T) {
	machines := fly.NewFake()
	drv := &capturingDriver{}
	b := NewSandbox(machines, func(string) opencode.Driver { return drv }, Config{})

	// A reiteration has both a snapshot (previous build) and a configured
	// template — the snapshot must win, and no template preamble applies.
	_, err := b.Build(context.Background(), Request{
		ProjectID:      "p1",
		Prompt:         "make the hero bigger",
		SnapshotGetURL: "https://storage.example/get?sig=snap",
		TemplateGetURL: "https://storage.example/template?sig=tpl",
	}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	execs := machines.Execs()
	if len(execs) != 1 {
		t.Fatalf("expected only the snapshot restore, got %d execs", len(execs))
	}
	if got := strings.Join(execs[0].Command, " "); !strings.Contains(got, "sig=snap") {
		t.Errorf("restore should use the snapshot, got: %s", got)
	}
	if strings.Contains(drv.spec.Instruction, "AGENTS.md") {
		t.Error("reiteration instruction must not carry the template preamble")
	}
}

func TestBuild_NoSnapshotURLsNoExecs(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines)

	res, err := b.Build(context.Background(), Request{ProjectID: "p1", Plan: "build"}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.SnapshotSaved {
		t.Error("no PUT URL was given, nothing should have been saved")
	}
	if execs := machines.Execs(); len(execs) != 0 {
		t.Errorf("expected no execs, got %d", len(execs))
	}
}
