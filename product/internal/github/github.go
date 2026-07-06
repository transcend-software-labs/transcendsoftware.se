// Package github mirrors each customer project into a private repo under the
// Transcend org and wires a deploy-on-push GitHub Action. It lets Rasmus review
// the actual source (with a diff per reiteration) and gives the delivered
// project continuous deployment.
//
// Everything runs on the trusted orchestrator side — the GitHub token never
// enters a build sandbox. Like the other integrations it has an interface with
// a Fake (dev) and a real HTTP implementation.
package github

import "context"

// PushSpec is one mirror push.
type PushSpec struct {
	Repo     string            // repository name under the org, e.g. "forge-abc123"
	Message  string            // commit message
	Files    map[string][]byte // path → content (the whole site + the workflow)
	FlyToken string            // app-scoped deploy token → repo secret FLY_API_TOKEN
}

// Mirror creates/updates a project's repo and returns its web URL.
type Mirror interface {
	Push(ctx context.Context, spec PushSpec) (repoURL string, err error)
}

// Fake is a dev-mode Mirror that records pushes and touches no network.
type Fake struct{ Pushes []PushSpec }

// NewFake returns a dev-mode mirror.
func NewFake() *Fake { return &Fake{} }

func (f *Fake) Push(_ context.Context, spec PushSpec) (string, error) {
	f.Pushes = append(f.Pushes, spec)
	return "https://github.com/dev-org/" + spec.Repo, nil
}
