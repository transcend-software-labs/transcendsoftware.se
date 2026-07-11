// Package fly wraps the Fly Machines + Apps APIs: spawn an ephemeral microVM
// sandbox per build, run orchestrator-driven commands inside it (exec), create
// the per-customer app, and hand out the deploy token the agent uses to
// publish it.
//
// Each Fly Machine is a Firecracker microVM, so one Machine per task is the
// isolation boundary. The org *API* token stays on the trusted side and never
// enters the sandbox. The *deploy* token the sandbox receives is minted per
// build, scoped to that one throwaway customer app (see HTTP.AppDeployToken),
// so a compromised agent can only deploy its own app — with a configured
// org-scoped token as a fallback if minting isn't available.
package fly

import (
	"context"
	"strings"
	"sync"
	"time"
)

// SpawnSpec describes one sandbox to create.
type SpawnSpec struct {
	TaskID string
	Port   int               // opencode port (also injected as OPENCODE_PORT)
	Env    map[string]string // env injected into the machine (e.g. ANTHROPIC_API_KEY, REPO_URL)
}

// Sandbox is a running per-task microVM.
type Sandbox struct {
	MachineID string
	AppName   string
	Addr      string // opencode base URL on the private network, e.g. http://[fdaa::3]:4096
}

// ExecResult is the outcome of a command run inside a sandbox machine.
type ExecResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

// Machines manages ephemeral sandboxes and per-customer app provisioning.
type Machines interface {
	// SpawnSandbox creates an isolated microVM for one build task and returns it
	// once opencode is reachable at Sandbox.Addr.
	SpawnSandbox(ctx context.Context, spec SpawnSpec) (*Sandbox, error)
	// DestroySandbox tears the microVM down.
	DestroySandbox(ctx context.Context, s *Sandbox) error
	// Exec runs a command inside a sandbox machine (Fly Machines exec API) and
	// returns its output. Used for deterministic, orchestrator-driven steps —
	// restoring and saving workspace snapshots — that must not rely on the agent.
	Exec(ctx context.Context, machineID string, command []string, timeoutSec int) (ExecResult, error)
	// EnsureApp creates the per-customer Fly app if it doesn't exist. Done by the
	// orchestrator so app-creation privilege stays out of the sandbox.
	EnsureApp(ctx context.Context, appName string) error
	// SetAppSecrets sets runtime secrets on a per-customer app. Orchestrator
	// side (never the sandbox); used to inject the per-app backup credentials
	// the deployed site's litestream uses. Applied on the app's next deploy.
	SetAppSecrets(ctx context.Context, appName string, secrets map[string]string) error
	// DestroyApp deletes a per-customer app and everything in it (machines,
	// IPs). Destroying an already-absent app is not an error — the reaper and
	// the admin destroy action must be idempotent.
	DestroyApp(ctx context.Context, appName string) error
	// SweepSandboxes destroys sandbox machines older than olderThan, returning
	// how many were reaped. A build never legitimately outlives its pipeline
	// timeout, so anything older is a leak (e.g. manual testing leftovers).
	SweepSandboxes(ctx context.Context, olderThan time.Duration) (int, error)
	// AppDeployToken returns a deploy token for appName, injected into the
	// sandbox so the agent can run `fly deploy`. Scoped to appName alone, minted
	// per build (see HTTP.AppDeployToken).
	AppDeployToken(ctx context.Context, appName string) (string, error)
	// RepoDeployToken returns a longer-lived app-scoped deploy token for the
	// project's GitHub Action (deploy-on-push). "" if not available.
	RepoDeployToken(ctx context.Context, appName string) (string, error)
}

// DefaultPort is the opencode port used when a spec leaves Port unset.
const DefaultPort = 4096

// Fake is a dev-mode Machines that touches no real infra. It records Exec,
// DestroyApp and SetAppSecrets calls so tests can assert snapshot, reaper and
// backup-provisioning behavior.
type Fake struct {
	mu            sync.Mutex
	execs         []FakeExec
	destroyedApps []string
	appSecrets    map[string]map[string]string
}

// FakeExec is one recorded Exec call.
type FakeExec struct {
	MachineID string
	Command   []string
}

// NewFake returns a dev-mode Machines.
func NewFake() *Fake { return &Fake{} }

func (f *Fake) SpawnSandbox(_ context.Context, spec SpawnSpec) (*Sandbox, error) {
	// Addr empty → the driver factory uses the fake opencode driver (dev mode).
	return &Sandbox{MachineID: "dev-machine-" + spec.TaskID, AppName: "dev-app", Addr: ""}, nil
}

func (f *Fake) DestroySandbox(_ context.Context, _ *Sandbox) error { return nil }
func (f *Fake) EnsureApp(_ context.Context, _ string) error        { return nil }

func (f *Fake) SetAppSecrets(_ context.Context, appName string, secrets map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appSecrets == nil {
		f.appSecrets = map[string]map[string]string{}
	}
	f.appSecrets[appName] = secrets
	return nil
}

// AppSecrets returns the secrets recorded for an app (test helper).
func (f *Fake) AppSecrets(appName string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appSecrets[appName]
}

func (f *Fake) AppDeployToken(_ context.Context, _ string) (string, error)  { return "", nil }
func (f *Fake) RepoDeployToken(_ context.Context, _ string) (string, error) { return "", nil }

func (f *Fake) DestroyApp(_ context.Context, appName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyedApps = append(f.destroyedApps, appName)
	return nil
}

// DestroyedApps returns the app names destroyed so far.
func (f *Fake) DestroyedApps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.destroyedApps))
	copy(out, f.destroyedApps)
	return out
}

func (f *Fake) SweepSandboxes(_ context.Context, _ time.Duration) (int, error) { return 0, nil }

func (f *Fake) Exec(_ context.Context, machineID string, command []string, _ int) (ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, FakeExec{MachineID: machineID, Command: command})
	// The screenshot crawler expects a JSON manifest on stdout; return a
	// deterministic two-page one so dev/tests exercise the capture path.
	joined := strings.Join(command, " ")
	if strings.Contains(joined, "crawl.js") {
		return ExecResult{ExitCode: 0, Stdout: `[{"slot":0,"path":"/"},{"slot":1,"path":"/kontakt"}]`}, nil
	}
	// The design audit (rendered audit.js, or the source-scan fallback) expects
	// an impeccable JSON findings array on stdout; return a clean one.
	if strings.Contains(joined, "audit.js") || strings.Contains(joined, "impeccable detect") {
		return ExecResult{ExitCode: 0, Stdout: `[]`}, nil
	}
	return ExecResult{ExitCode: 0}, nil
}

// Execs returns the Exec calls recorded so far.
func (f *Fake) Execs() []FakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeExec, len(f.execs))
	copy(out, f.execs)
	return out
}
