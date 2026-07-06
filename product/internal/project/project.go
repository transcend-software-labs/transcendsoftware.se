// Package project holds the core domain types and the project state machine.
//
// A project moves through a fixed lifecycle driven by the orchestrator:
//
//	created → planning → screening → (rejected | building) → preview_ready
//	                                                       ↘ failed
//
// From preview_ready the customer may request up to MaxIterations-1 further
// builds (reiterations); each reiteration loops building → preview_ready.
package project

import (
	"errors"
	"time"
)

// Status is the lifecycle state of a project.
type Status string

const (
	StatusCreated      Status = "created"       // just created, nothing run yet
	StatusClarifying   Status = "clarifying"    // intake agent is generating clarifying questions
	StatusNeedsInput   Status = "needs_input"   // waiting for the customer to answer questions
	StatusPlanning     Status = "planning"      // LLM is turning the brief into a plan
	StatusScreening    Status = "screening"     // safety gate is evaluating the request
	StatusEscalated    Status = "escalated"     // safety gate escalated; waiting on operator review
	StatusRejected     Status = "rejected"      // safety gate rejected the request (terminal)
	StatusBuilding     Status = "building"      // a sandboxed build is running
	StatusPreviewReady Status = "preview_ready" // a build finished, preview link attached
	StatusFailed       Status = "failed"        // a build or pipeline step errored (terminal)
	StatusExpired      Status = "expired"       // preview reaped after the retention window (terminal)
)

// MaxIterations is the total number of build passes a project gets:
// one initial build plus two customer reiterations.
const MaxIterations = 3

// Verdict is the outcome of the safety gate.
type Verdict string

const (
	VerdictAllow    Verdict = "allow"    // proceed to build
	VerdictReject   Verdict = "reject"   // refuse outright
	VerdictEscalate Verdict = "escalate" // ambiguous — route to a human (Rasmus)
)

// DesignOption is one suggested visual direction from the intake step. The
// customer picks one — or states their own preference — before building.
type DesignOption struct {
	Name        string `json:"name"`        // short label, e.g. "Varmt & rustikt"
	Description string `json:"description"` // one sentence: mood, colors, typography
}

// Project is a single customer request to have a website built.
type Project struct {
	ID             string
	UserID         string
	Name           string
	Brief          string // the customer's description of what they want
	Status         Status
	Questions      []string       // clarifying questions from the intake step
	DesignOptions  []DesignOption // suggested design directions from intake
	DesignBrief    string         // the customer's chosen/stated design direction
	Answers        string         // the customer's answers to those questions
	Plan           string         // generated build plan (markdown)
	Verdict        Verdict        // safety-gate outcome
	RejectReason   string         // populated when Status == rejected
	PreviewURL     string         // latest deployed preview link
	RepoURL        string         // reserved for GitHub mirroring (Phase 4); not populated yet
	SnapshotKey    string         // object-storage key of the workspace snapshot from the last successful build
	IterationsUsed int            // number of build passes consumed (1..MaxIterations)
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// EffectiveBrief is the brief enriched with the customer's clarifying answers
// and chosen design direction, as fed to the planner and safety gate.
func (p *Project) EffectiveBrief() string {
	b := p.Brief
	if p.Answers != "" {
		b += "\n\nClarifications from the customer:\n" + p.Answers
	}
	if p.DesignBrief != "" {
		b += "\n\nDesign direction chosen by the customer:\n" + p.DesignBrief
	}
	return b
}

// CanReiterate reports whether the customer may request another build pass.
func (p *Project) CanReiterate() bool {
	return p.Status == StatusPreviewReady && p.IterationsUsed < MaxIterations
}

// IterationsLeft is the number of reiterations still available to the customer.
func (p *Project) IterationsLeft() int {
	left := MaxIterations - p.IterationsUsed
	if left < 0 {
		return 0
	}
	return left
}

// Iteration records a single build pass within a project.
type Iteration struct {
	ID          string
	ProjectID   string
	Number      int    // 1 for the initial build, 2..MaxIterations for reiterations
	Prompt      string // empty for the initial build; the customer's change request otherwise
	PreviewURL  string
	Status      Status
	Log         string    // human-readable trace of what the build did
	MachineID   string    // Fly Machine running this build (for recovery/reaping)
	HeartbeatAt time.Time // last time the build reported progress
	CreatedAt   time.Time
}

// Asset is a customer-uploaded file (photo, logo, content) for a project. The
// bytes live in object storage; this is the metadata row.
type Asset struct {
	ID          string
	ProjectID   string
	Key         string // object-storage key, e.g. projects/<id>/assets/<file>
	Filename    string
	ContentType string
	Size        int64
	CreatedAt   time.Time
}

// ErrNotFound is returned by stores when an entity does not exist.
var ErrNotFound = errors.New("not found")
