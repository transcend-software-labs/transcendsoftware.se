// Package builder runs one build pass: spawn an isolated sandbox, drive opencode
// inside it to build/iterate the site, then deploy and return a preview URL.
//
// The opencode driver is built per task from the spawned sandbox's address, so
// the same Sandbox builder works in dev mode (fake machines → empty address →
// fake driver) and in real mode (a Fly Machine → private address → HTTP driver).
// The deploy step is currently gated by fly.ErrDeployDisabled.
package builder

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// deployAppName is the per-customer Fly app for a project (globally unique).
func deployAppName(projectID string) string {
	id := strings.ToLower(projectID)
	if len(id) > 12 {
		id = id[:12]
	}
	return "forge-" + id
}

// Request is one build pass.
type Request struct {
	ProjectID     string
	Brief         string
	Plan          string
	Prompt        string            // empty for the initial build; the change request on reiterations
	RepoURL       string            // existing repo on reiterations
	AssetManifest map[string]string // filename → short-lived presigned GET URL
}

// Result is the outcome of a build pass.
type Result struct {
	RepoURL    string
	PreviewURL string
	Log        string
}

// Hooks observe a build pass.
type Hooks struct {
	OnLog     func(string)                 // progress lines, live
	OnSandbox func(machineID, addr string) // called once the sandbox is spawned
}

// Builder runs a build pass.
type Builder interface {
	Build(ctx context.Context, req Request, hooks Hooks) (Result, error)
}

// Config holds the sandbox builder's settings.
type Config struct {
	SystemPrompt string // "Rasmus's decisions" operating spec, passed to the agent
	OpencodePort int    // port opencode listens on inside the sandbox
	// AnthropicKey is injected so opencode can call Claude (if used).
	AnthropicKey string
	// LLM* configure an OpenAI-compatible model for opencode (e.g. Moonshot/Kimi).
	// The entrypoint writes an opencode provider config from these. The key is
	// the one credential that must be inside the sandbox; the Fly deploy token
	// stays out (the orchestrator deploys).
	LLMBaseURL string
	LLMKey     string
	LLMModel   string
}

// DriverFactory builds an opencode driver for a sandbox at the given address.
// An empty address (dev/fake mode) should yield a fake driver.
type DriverFactory func(addr string) opencode.Driver

// Sandbox builds inside an isolated, per-task sandbox.
type Sandbox struct {
	machines  fly.Machines
	newDriver DriverFactory
	cfg       Config
}

// NewSandbox wires a sandboxed builder.
func NewSandbox(machines fly.Machines, newDriver DriverFactory, cfg Config) *Sandbox {
	if cfg.OpencodePort == 0 {
		cfg.OpencodePort = fly.DefaultPort
	}
	return &Sandbox{machines: machines, newDriver: newDriver, cfg: cfg}
}

// Build spawns a sandbox, runs the agent, deploys, and tears the sandbox down.
func (b *Sandbox) Build(ctx context.Context, req Request, hooks Hooks) (Result, error) {
	env := map[string]string{}
	if b.cfg.AnthropicKey != "" {
		env["ANTHROPIC_API_KEY"] = b.cfg.AnthropicKey
	}
	if b.cfg.LLMKey != "" {
		env["LLM_API_KEY"] = b.cfg.LLMKey
		env["LLM_BASE_URL"] = b.cfg.LLMBaseURL
		env["LLM_MODEL"] = b.cfg.LLMModel
	}
	if req.RepoURL != "" {
		env["REPO_URL"] = req.RepoURL
	}
	if len(req.AssetManifest) > 0 {
		// Presigned GET URLs — the entrypoint downloads these into the workspace.
		// No storage credentials enter the sandbox.
		if b, err := json.Marshal(req.AssetManifest); err == nil {
			env["ASSETS_MANIFEST"] = string(b)
		}
	}

	// Provision the per-customer app (orchestrator side) and inject the app +
	// an app-scoped deploy token so the agent can `fly deploy` that one app.
	appName := deployAppName(req.ProjectID)
	if err := b.machines.EnsureApp(ctx, appName); err != nil {
		return Result{}, err
	}
	token, err := b.machines.AppDeployToken(ctx, appName)
	if err != nil {
		return Result{}, err
	}
	env["FLY_APP"] = appName
	if token != "" {
		env["FLY_DEPLOY_TOKEN"] = token
	}

	emit(hooks.OnLog, "Spawning isolated sandbox…")
	sb, err := b.machines.SpawnSandbox(ctx, fly.SpawnSpec{
		TaskID: req.ProjectID,
		Port:   b.cfg.OpencodePort,
		Env:    env,
	})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = b.machines.DestroySandbox(context.WithoutCancel(ctx), sb) }()
	if hooks.OnSandbox != nil {
		hooks.OnSandbox(sb.MachineID, sb.Addr)
	}
	emit(hooks.OnLog, "Sandbox ready, starting the agent…")

	instruction := req.Plan
	if req.Prompt != "" {
		instruction = "Apply this change to the existing site, then redeploy:\n\n" + req.Prompt
	}

	driver := b.newDriver(sb.Addr)
	res, err := driver.Run(ctx, opencode.Spec{
		Workdir:      "/workspace",
		SystemPrompt: b.cfg.SystemPrompt,
		Instruction:  instruction,
	}, hooks.OnLog)
	if err != nil {
		return Result{Log: res.Log}, err
	}

	// The agent ran `fly deploy` inside the sandbox (per the operating spec); the
	// app URL is deterministic.
	preview := "https://" + appName + ".fly.dev"
	return Result{RepoURL: req.RepoURL, PreviewURL: preview, Log: res.Log}, nil
}

func emit(onLog func(string), line string) {
	if onLog != nil {
		onLog(line)
	}
}
