package builder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// erroringDriver simulates an agent run that fails (e.g. a build timeout).
type erroringDriver struct{}

func (erroringDriver) Run(_ context.Context, _ opencode.Spec, _ func(string)) (opencode.Result, error) {
	return opencode.Result{Log: "partial work"}, errors.New("context deadline exceeded")
}

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

func TestBuild_CapturesScreenshot(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines)

	res, err := b.Build(context.Background(), Request{
		ProjectID:         "p1",
		Plan:              "build a site",
		ScreenshotPutURLs: []string{"https://storage.example/0?sig=a", "https://storage.example/1?sig=b"},
	}, Hooks{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.Screenshots) != 2 || res.Screenshots[0].Path != "/" || res.Screenshots[1].Path != "/kontakt" {
		t.Errorf("expected the crawler's two-page manifest, got %+v", res.Screenshots)
	}
	var found bool
	for _, e := range machines.Execs() {
		cmd := strings.Join(e.Command, " ")
		if strings.Contains(cmd, "node /tmp/crawl.js") && strings.Contains(cmd, "storage.example/0?sig=a") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a crawler exec with the put URLs, got: %+v", machines.Execs())
	}
}

func TestBuild_InjectsBackupSecretsWhenConfigured(t *testing.T) {
	machines := fly.NewFake()
	b := NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, Config{
		BackupBucket:    "transcend-forge-backups",
		BackupEndpoint:  "fly.storage.tigris.dev",
		BackupRegion:    "auto",
		BackupAccessKey: "ak",
		BackupSecretKey: "sk",
	})
	if _, err := b.Build(context.Background(), Request{ProjectID: "p9", Plan: "build"}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	app := DeployAppName("p9")
	secrets := machines.AppSecrets(app)
	if secrets == nil {
		t.Fatal("no backup secrets injected for the app")
	}
	if secrets["LITESTREAM_BUCKET"] != "transcend-forge-backups" {
		t.Errorf("LITESTREAM_BUCKET = %q", secrets["LITESTREAM_BUCKET"])
	}
	if secrets["LITESTREAM_PATH"] != app {
		t.Errorf("LITESTREAM_PATH = %q, want per-app prefix %q", secrets["LITESTREAM_PATH"], app)
	}
	if secrets["LITESTREAM_SECRET_ACCESS_KEY"] != "sk" {
		t.Error("secret access key not injected")
	}
}

func TestBuild_NoBackupSecretsWhenUnconfigured(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines) // Config{} → no backup bucket
	if _, err := b.Build(context.Background(), Request{ProjectID: "p10", Plan: "build"}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if s := machines.AppSecrets(DeployAppName("p10")); s != nil {
		t.Errorf("expected no backup secrets when unconfigured, got %v", s)
	}
}

func TestBuild_InjectsOwnerEmail(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines) // no backup config — owner email alone must inject
	if _, err := b.Build(context.Background(), Request{
		ProjectID: "p13", Plan: "build", OwnerEmail: "kund@example.se",
	}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	s := machines.AppSecrets(DeployAppName("p13"))
	if s == nil || s["OWNER_EMAIL"] != "kund@example.se" {
		t.Fatalf("OWNER_EMAIL not injected, got %v", s)
	}
	if _, ok := s["LITESTREAM_BUCKET"]; ok {
		t.Error("no backup config → no litestream secrets expected")
	}
}

func TestBuild_InjectsSiteEmailWithDisplayName(t *testing.T) {
	machines := fly.NewFake()
	b := NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, Config{
		SitesEmailKey: "re_key", SitesEmailFrom: "notify@forge.transcendsoftware.se",
	})
	if _, err := b.Build(context.Background(), Request{
		ProjectID: "p14", Plan: "build", SiteName: `Kvarnens "Bageri"`, OwnerEmail: "k@example.se",
	}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	s := machines.AppSecrets(DeployAppName("p14"))
	if s["EMAIL_API_KEY"] != "re_key" {
		t.Errorf("EMAIL_API_KEY not injected: %v", s)
	}
	if s["SITE_NAME"] != `Kvarnens "Bageri"` {
		t.Errorf("SITE_NAME wrong: %q", s["SITE_NAME"])
	}
	// The From display name must be sanitized (quotes stripped) so it can't
	// break the header.
	if got := s["EMAIL_FROM"]; got != `"Kvarnens Bageri" <notify@forge.transcendsoftware.se>` {
		t.Errorf("EMAIL_FROM = %q", got)
	}
}

func TestBuild_ImpeccableGateOnlyWhenEnabled(t *testing.T) {
	// Off by default: no design-gate instruction.
	off := &capturingDriver{}
	b := NewSandbox(fly.NewFake(), func(string) opencode.Driver { return off }, Config{})
	if _, err := b.Build(context.Background(), Request{ProjectID: "pa", Plan: "build"}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if strings.Contains(off.spec.Instruction, "impeccable detect") {
		t.Error("impeccable gate must not appear when disabled")
	}

	// On: the instruction carries the detector gate.
	on := &capturingDriver{}
	b2 := NewSandbox(fly.NewFake(), func(string) opencode.Driver { return on }, Config{Impeccable: true})
	if _, err := b2.Build(context.Background(), Request{ProjectID: "pb", Plan: "build"}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(on.spec.Instruction, "impeccable detect --json") {
		t.Errorf("impeccable gate missing when enabled: %q", on.spec.Instruction)
	}
}

func TestBuild_NoSiteEmailWithoutKey(t *testing.T) {
	machines := fly.NewFake()
	b := newTestBuilder(machines) // no SitesEmailKey
	if _, err := b.Build(context.Background(), Request{
		ProjectID: "p15", Plan: "build", SiteName: "Site", OwnerEmail: "k@example.se",
	}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if s := machines.AppSecrets(DeployAppName("p15")); s["EMAIL_API_KEY"] != "" {
		t.Errorf("no key configured → no EMAIL_API_KEY, got %q", s["EMAIL_API_KEY"])
	}
}

func TestBuild_SavesSnapshotOnFailureToResume(t *testing.T) {
	machines := fly.NewFake()
	b := NewSandbox(machines, func(string) opencode.Driver { return erroringDriver{} }, Config{})
	res, err := b.Build(context.Background(), Request{
		ProjectID: "p11", Plan: "build", SnapshotPutURL: "https://put.example/snap",
	}, Hooks{})
	if err == nil {
		t.Fatal("expected the build to fail")
	}
	if !res.SnapshotSaved {
		t.Error("a failed build must still save the workspace so Retry can resume")
	}
	if len(machines.Execs()) == 0 {
		t.Error("expected a snapshot-save exec on failure")
	}
}

func TestBuild_FailureWithoutPutURLSavesNothing(t *testing.T) {
	machines := fly.NewFake()
	b := NewSandbox(machines, func(string) opencode.Driver { return erroringDriver{} }, Config{})
	res, err := b.Build(context.Background(), Request{ProjectID: "p11b", Plan: "build"}, Hooks{})
	if err == nil {
		t.Fatal("expected the build to fail")
	}
	if res.SnapshotSaved {
		t.Error("no PUT URL → nothing to save")
	}
}

func TestBuild_ResumedBuildGetsResumeInstruction(t *testing.T) {
	machines := fly.NewFake()
	drv := &capturingDriver{}
	b := NewSandbox(machines, func(string) opencode.Driver { return drv }, Config{})
	// A snapshot to restore + no change prompt = resuming an interrupted build.
	if _, err := b.Build(context.Background(), Request{
		ProjectID: "p12", Plan: "build the site", SnapshotGetURL: "https://get.example/snap",
	}, Hooks{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(drv.spec.Instruction, "INTERRUPTED") {
		t.Errorf("resumed build should get the resume preamble, got: %q", drv.spec.Instruction)
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
