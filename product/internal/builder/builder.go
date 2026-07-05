// Package builder runs one build pass: spawn an isolated sandbox, restore the
// previous workspace snapshot (on reiterations), drive opencode inside it to
// build the site, let the agent deploy it, save a new snapshot, and tear the
// sandbox down.
//
// The opencode driver is built per task from the spawned sandbox's address, so
// the same Sandbox builder works in dev mode (fake machines → empty address →
// fake driver) and in real mode (a Fly Machine → private address → HTTP driver).
//
// Credentials in the sandbox: the LLM API key (opencode needs it) and FLY_APP +
// FLY_DEPLOY_TOKEN so the agent can run `fly deploy`. The deploy token is
// currently org-scoped (interim — see fly_http.go for the per-app hardening
// TODO and its blocker). Storage is never credentialed: assets and snapshots
// move through short-lived presigned URLs only.
package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// DeployAppName is the per-customer Fly app for a project (globally unique).
// Exported so the orchestrator's reaper and the admin destroy action can name
// the app to remove.
func DeployAppName(projectID string) string {
	id := strings.ToLower(projectID)
	if len(id) > 12 {
		id = id[:12]
	}
	return "forge-" + id
}

// Request is one build pass.
type Request struct {
	ProjectID string
	Brief     string
	Plan      string
	Prompt    string // empty for the initial build; the change request on reiterations

	// SnapshotGetURL, when set, is a presigned GET URL of the workspace snapshot
	// from the previous successful build; it is restored into /workspace before
	// the agent runs, so reiterations edit the existing site instead of
	// rebuilding from scratch.
	SnapshotGetURL string
	// TemplateGetURL, when set (and there is no snapshot), is a presigned GET
	// URL of the starter-app tarball unpacked into /workspace before the first
	// build — the agent extends a working app instead of scaffolding.
	TemplateGetURL string
	// SnapshotPutURL, when set, is a presigned PUT URL the workspace is uploaded
	// to after a successful build. Both URLs keep storage credentials out of the
	// sandbox (same model as asset downloads).
	SnapshotPutURL string

	AssetManifest map[string]string // filename → short-lived presigned GET URL
}

// Result is the outcome of a build pass.
type Result struct {
	PreviewURL    string
	Log           string
	SnapshotSaved bool // the workspace snapshot was uploaded to SnapshotPutURL
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
	// The entrypoint writes an opencode provider config from these. The LLM key
	// and the deploy token (FLY_APP/FLY_DEPLOY_TOKEN, injected in Build) are the
	// only credentials inside the sandbox.
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
	if len(req.AssetManifest) > 0 {
		// Presigned GET URLs — the entrypoint downloads these into the workspace.
		// No storage credentials enter the sandbox.
		if b, err := json.Marshal(req.AssetManifest); err == nil {
			env["ASSETS_MANIFEST"] = string(b)
		}
	}

	// Provision the per-customer app (orchestrator side) and inject the app name
	// + a deploy token so the agent can `fly deploy` it. The token is org-scoped
	// for now (per-app minting is blocked — see fly.HTTP.AppDeployToken).
	appName := DeployAppName(req.ProjectID)
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

	// Seed the workspace before the agent runs — orchestrator-driven (Machines
	// exec), never left to the agent. Reiterations restore the previous build's
	// snapshot; first builds unpack the starter template when one is configured.
	switch {
	case req.SnapshotGetURL != "":
		emit(hooks.OnLog, "Restoring your site from the previous build…")
		if err := b.restoreSnapshot(ctx, sb.MachineID, req.SnapshotGetURL); err != nil {
			return Result{}, err
		}
	case req.TemplateGetURL != "":
		emit(hooks.OnLog, "Preparing the Forge starter app…")
		if err := b.restoreSnapshot(ctx, sb.MachineID, req.TemplateGetURL); err != nil {
			return Result{}, err
		}
	}
	emit(hooks.OnLog, "Sandbox ready, starting the agent…")

	instruction := req.Plan
	if req.Prompt != "" {
		instruction = "Apply this change to the existing site, then redeploy:\n\n" + req.Prompt
	} else if req.TemplateGetURL != "" {
		instruction = templatePreamble + "\n\n" + req.Plan
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

	// Save the workspace so the next iteration can continue from it. A failed
	// snapshot degrades the next change, not this build — the site is deployed.
	snapshotSaved := false
	if req.SnapshotPutURL != "" {
		emit(hooks.OnLog, "Saving workspace snapshot…")
		if err := b.saveSnapshot(ctx, sb.MachineID, req.SnapshotPutURL); err != nil {
			emit(hooks.OnLog, "Warning: could not save the workspace snapshot.")
		} else {
			snapshotSaved = true
		}
	}

	// The agent ran `fly deploy` inside the sandbox (per the operating spec); the
	// app URL is deterministic.
	preview := "https://" + appName + ".fly.dev"
	return Result{PreviewURL: preview, Log: res.Log, SnapshotSaved: snapshotSaved}, nil
}

// templatePreamble tells the agent the workspace is a working starter app, not
// an empty directory. Prepended to the plan on first builds from the template.
const templatePreamble = `The workspace /workspace already contains our production-ready Go starter app
(one binary serving frontend + backend, SQLite, working auth, contact form).
Read AGENTS.md first, then EXTEND this app to implement the plan below.
Do not scaffold a new project. Keep /healthz, auth and CSRF intact.`

// restoreSnapshot unpacks the previous build's workspace into /workspace.
func (b *Sandbox) restoreSnapshot(ctx context.Context, machineID, getURL string) error {
	script := `curl -fsSL '` + getURL + `' | tar -xzf - -C /workspace`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 60)
	if err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restore snapshot: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

// saveSnapshot uploads the workspace (minus reinstallable dependencies) to the
// presigned PUT URL.
func (b *Sandbox) saveSnapshot(ctx context.Context, machineID, putURL string) error {
	script := `cd /workspace && tar --exclude=node_modules --exclude=.cache -czf /tmp/snapshot.tgz . && ` +
		`curl -fsS -T /tmp/snapshot.tgz -o /dev/null '` + putURL + `'`
	res, err := b.machines.Exec(ctx, machineID, []string{"/bin/sh", "-c", script}, 120)
	if err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("save snapshot: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return nil
}

func emit(onLog func(string), line string) {
	if onLog != nil {
		onLog(line)
	}
}
