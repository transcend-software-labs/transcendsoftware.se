// Package fly wraps the Fly Machines API: spawn an ephemeral microVM sandbox
// per build, and (eventually) deploy the result.
//
// Each Fly Machine is itself a Firecracker microVM, so one Machine per task is
// the isolation boundary. Real credentials never enter the Machine — the
// orchestrator holds the token and performs the deploy.
//
// NOTE: the Deploy step is intentionally left unimplemented for now (the one
// piece switched off until you wire real Fly deploys). Sandbox lifecycle calls
// are scaffolded against the Machines API and must be confirmed against your
// org before use.
package fly

import (
	"context"
	"errors"
	"strings"
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

// Machines manages ephemeral sandboxes and deploys.
type Machines interface {
	// SpawnSandbox creates an isolated microVM for one build task and returns it
	// once opencode is reachable at Sandbox.Addr.
	SpawnSandbox(ctx context.Context, spec SpawnSpec) (*Sandbox, error)
	// DestroySandbox tears the microVM down.
	DestroySandbox(ctx context.Context, s *Sandbox) error
	// Deploy publishes the built site and returns its preview URL.
	Deploy(ctx context.Context, s *Sandbox, projectID string) (previewURL string, err error)
}

// DefaultPort is the opencode port used when a spec leaves Port unset.
const DefaultPort = 4096

// ErrDeployDisabled is returned by the real client's Deploy: the actual Fly
// deploy is the one step deliberately not yet enabled.
var ErrDeployDisabled = errors.New("fly: real deploy is not enabled yet")

// Fake is a dev-mode Machines that simulates the lifecycle and hands back a
// realistic-looking preview URL without touching Fly.
type Fake struct{}

// NewFake returns a dev-mode Machines.
func NewFake() *Fake { return &Fake{} }

func (Fake) SpawnSandbox(_ context.Context, spec SpawnSpec) (*Sandbox, error) {
	// Addr empty → the driver factory uses the fake opencode driver (dev mode).
	return &Sandbox{MachineID: "dev-machine-" + spec.TaskID, AppName: "dev-app", Addr: ""}, nil
}

func (Fake) DestroySandbox(_ context.Context, _ *Sandbox) error { return nil }

func (Fake) Deploy(ctx context.Context, _ *Sandbox, projectID string) (string, error) {
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	slug := strings.ToLower(projectID)
	if len(slug) > 8 {
		slug = slug[:8]
	}
	return "https://preview-" + slug + ".fly.dev", nil
}
