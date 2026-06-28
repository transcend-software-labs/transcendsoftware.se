// Package builder runs one build pass: spawn an isolated sandbox, drive opencode
// to build/iterate the site, then deploy and return a preview URL.
//
// The same Sandbox builder works in dev mode (fake driver + fake machines → a
// simulated preview URL) and in real mode (real opencode + Fly Machines), where
// the deploy step is currently gated by fly.ErrDeployDisabled.
package builder

import (
	"context"

	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
)

// Request is one build pass.
type Request struct {
	ProjectID string
	Brief     string
	Plan      string
	Prompt    string // empty for the initial build; the change request on reiterations
	RepoURL   string // existing repo on reiterations
}

// Result is the outcome of a build pass.
type Result struct {
	RepoURL    string
	PreviewURL string
	Log        string
}

// Builder runs a build pass.
type Builder interface {
	Build(ctx context.Context, req Request) (Result, error)
}

// Sandbox builds inside an isolated, per-task sandbox.
type Sandbox struct {
	Driver       opencode.Driver
	Machines     fly.Machines
	SystemPrompt string // "Rasmus's decisions" operating spec, passed to the agent
}

// NewSandbox wires a sandboxed builder.
func NewSandbox(driver opencode.Driver, machines fly.Machines, systemPrompt string) *Sandbox {
	return &Sandbox{Driver: driver, Machines: machines, SystemPrompt: systemPrompt}
}

// Build spawns a sandbox, runs the agent, deploys, and tears the sandbox down.
func (b *Sandbox) Build(ctx context.Context, req Request) (Result, error) {
	sb, err := b.Machines.SpawnSandbox(ctx, req.ProjectID)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = b.Machines.DestroySandbox(context.WithoutCancel(ctx), sb) }()

	instruction := req.Plan
	if req.Prompt != "" {
		instruction = "Apply this change to the existing site, then redeploy:\n\n" + req.Prompt
	}

	res, err := b.Driver.Run(ctx, opencode.Spec{
		Workdir:      "/workspace",
		SystemPrompt: b.SystemPrompt,
		Instruction:  instruction,
	})
	if err != nil {
		return Result{Log: res.Log}, err
	}

	preview, err := b.Machines.Deploy(ctx, sb, req.ProjectID)
	if err != nil {
		// Surface the build log alongside the deploy error so the operator can see
		// how far it got (this is where fly.ErrDeployDisabled stops real runs).
		return Result{RepoURL: req.RepoURL, Log: res.Log}, err
	}

	return Result{RepoURL: req.RepoURL, PreviewURL: preview, Log: res.Log}, nil
}
