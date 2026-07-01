// Package fly wraps the Fly Machines + Apps APIs: spawn an ephemeral microVM
// sandbox per build, create the per-customer app, and mint an app-scoped deploy
// token the agent uses to publish that one app.
//
// Each Fly Machine is a Firecracker microVM, so one Machine per task is the
// isolation boundary. The org token never enters the sandbox — only a deploy
// token scoped to a single throwaway customer app (blast radius: that one app).
package fly

import "context"

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

// Machines manages ephemeral sandboxes and per-customer app provisioning.
type Machines interface {
	// SpawnSandbox creates an isolated microVM for one build task and returns it
	// once opencode is reachable at Sandbox.Addr.
	SpawnSandbox(ctx context.Context, spec SpawnSpec) (*Sandbox, error)
	// DestroySandbox tears the microVM down.
	DestroySandbox(ctx context.Context, s *Sandbox) error
	// EnsureApp creates the per-customer Fly app if it doesn't exist. Done by the
	// orchestrator so app-creation privilege stays out of the sandbox.
	EnsureApp(ctx context.Context, appName string) error
	// AppDeployToken mints a deploy token scoped to appName only. Injected into
	// the sandbox so the agent can deploy that one app and nothing else.
	AppDeployToken(ctx context.Context, appName string) (string, error)
}

// DefaultPort is the opencode port used when a spec leaves Port unset.
const DefaultPort = 4096

// Fake is a dev-mode Machines that touches no real infra.
type Fake struct{}

// NewFake returns a dev-mode Machines.
func NewFake() *Fake { return &Fake{} }

func (Fake) SpawnSandbox(_ context.Context, spec SpawnSpec) (*Sandbox, error) {
	// Addr empty → the driver factory uses the fake opencode driver (dev mode).
	return &Sandbox{MachineID: "dev-machine-" + spec.TaskID, AppName: "dev-app", Addr: ""}, nil
}

func (Fake) DestroySandbox(_ context.Context, _ *Sandbox) error         { return nil }
func (Fake) EnsureApp(_ context.Context, _ string) error                { return nil }
func (Fake) AppDeployToken(_ context.Context, _ string) (string, error) { return "", nil }
