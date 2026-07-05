// Package orchestrator drives a project through its lifecycle: intake
// (clarifying questions), planning, the safety gate, and one or more sandboxed
// build passes. Steps run asynchronously so the HTTP layer returns immediately
// and the dashboard polls for status.
//
//	created → clarifying → needs_input → planning → screening
//	                                                   ├─ allow    → building → preview_ready
//	                                                   ├─ escalate → escalated → (operator) → building
//	                                                   └─ reject   → rejected
package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
)

// pipelineTimeout bounds a single plan→gate→build pass.
const pipelineTimeout = 45 * time.Minute

// Orchestrator coordinates intake, planning, screening and building.
type Orchestrator struct {
	store    store.Store
	intake   llm.Intake
	planner  llm.Planner
	gate     llm.SafetyGate
	builder  builder.Builder
	machines fly.Machines  // for reaping orphaned sandboxes on recovery
	storage  storage.Store // for presigning asset URLs into builds
	broker   *stream.Broker
	verifier Verifier // smoke-checks the deployed preview before preview_ready
	log      *slog.Logger
}

// New returns an orchestrator.
func New(s store.Store, in llm.Intake, p llm.Planner, g llm.SafetyGate, b builder.Builder, m fly.Machines, as storage.Store, br *stream.Broker, v Verifier, log *slog.Logger) *Orchestrator {
	return &Orchestrator{store: s, intake: in, planner: p, gate: g, builder: b, machines: m, storage: as, broker: br, verifier: v, log: log}
}

// assetManifest builds filename → presigned GET URL for a project's uploaded
// assets, so the sandbox can fetch them without holding storage credentials.
func (o *Orchestrator) assetManifest(ctx context.Context, projectID string) map[string]string {
	assets, err := o.store.AssetsByProject(ctx, projectID)
	if err != nil || len(assets) == 0 {
		return nil
	}
	m := make(map[string]string, len(assets))
	for _, a := range assets {
		url, err := o.storage.PresignGet(ctx, a.Key, time.Hour)
		if err != nil {
			o.log.Error("presign asset", "key", a.Key, "err", err)
			continue
		}
		m[a.Filename] = url
	}
	return m
}

// RecoverInterrupted handles builds left in the building state by a previous run
// (e.g. a crash or deploy): it reaps each orphaned sandbox machine and marks the
// build failed. Call once on startup.
func (o *Orchestrator) RecoverInterrupted(ctx context.Context) {
	its, err := o.store.ActiveIterations(ctx)
	if err != nil {
		o.log.Error("recover: list active builds", "err", err)
		return
	}
	for _, it := range its {
		o.log.Warn("recovering interrupted build", "project", it.ProjectID, "machine", it.MachineID)
		if it.MachineID != "" {
			if err := o.machines.DestroySandbox(ctx, &fly.Sandbox{MachineID: it.MachineID}); err != nil {
				o.log.Error("recover: reap machine", "machine", it.MachineID, "err", err)
			}
		}
		it.Status = project.StatusFailed
		_ = o.store.UpdateIteration(ctx, it)
		if p, err := o.store.ProjectByID(ctx, it.ProjectID); err == nil && p.Status == project.StatusBuilding {
			// Same rule as markFailed: an interrupted reiteration falls back to
			// the still-live previous preview instead of bricking the project.
			if p.IterationsUsed >= 1 && p.PreviewURL != "" {
				p.Status = project.StatusPreviewReady
			} else {
				p.Status = project.StatusFailed
				p.RejectReason = "Build interrupted by a restart."
			}
			_ = o.save(ctx, p)
		}
	}
}

func (o *Orchestrator) async(projectID string, fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), pipelineTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			o.log.Error("pipeline step failed", "project", projectID, "err", err)
			o.markFailed(ctx, projectID, err)
		}
	}()
}

// StartIntake generates clarifying questions for a freshly created project. If
// the intake returns no questions, it proceeds straight to planning.
func (o *Orchestrator) StartIntake(projectID string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		if err := o.setStatus(ctx, p, project.StatusClarifying); err != nil {
			return err
		}
		qs, err := o.intake.Questions(ctx, p.Brief)
		if err != nil {
			return err
		}
		if len(qs) == 0 {
			return o.runPlanGateBuild(ctx, projectID)
		}
		p.Questions = qs
		p.Status = project.StatusNeedsInput
		return o.save(ctx, p)
	})
}

// SubmitAnswers records the customer's answers and proceeds to plan→gate→build.
func (o *Orchestrator) SubmitAnswers(projectID, answers string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		p.Answers = answers
		if err := o.save(ctx, p); err != nil {
			return err
		}
		return o.runPlanGateBuild(ctx, projectID)
	})
}

// Reiterate runs another build pass against a customer change request.
func (o *Orchestrator) Reiterate(projectID, prompt string) {
	o.async(projectID, func(ctx context.Context) error {
		return o.runBuild(ctx, projectID, prompt)
	})
}

// ApproveEscalated lets an operator clear an escalated project to build.
func (o *Orchestrator) ApproveEscalated(projectID string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		if p.Status != project.StatusEscalated {
			return nil
		}
		p.Verdict = project.VerdictAllow
		if err := o.save(ctx, p); err != nil {
			return err
		}
		return o.runBuild(ctx, projectID, "")
	})
}

// RejectEscalated lets an operator decline an escalated project.
func (o *Orchestrator) RejectEscalated(projectID, reason string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.Status != project.StatusEscalated {
		return nil
	}
	p.Status = project.StatusRejected
	p.Verdict = project.VerdictReject
	if reason == "" {
		reason = "Declined after review."
	}
	p.RejectReason = reason
	return o.save(ctx, p)
}

func (o *Orchestrator) runPlanGateBuild(ctx context.Context, projectID string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}

	// 1) Plan.
	if err := o.setStatus(ctx, p, project.StatusPlanning); err != nil {
		return err
	}
	planRes, err := o.planner.Plan(ctx, p.EffectiveBrief())
	if err != nil {
		return err
	}
	p.Plan = planRes.Plan
	if planRes.Name != "" {
		p.Name = planRes.Name
	}
	p.Status = project.StatusScreening
	if err := o.save(ctx, p); err != nil {
		return err
	}

	// 2) Safety gate (tool-less).
	gateRes, err := o.gate.Screen(ctx, p.EffectiveBrief(), p.Plan)
	if err != nil {
		return err
	}
	p.Verdict = gateRes.Verdict
	switch gateRes.Verdict {
	case project.VerdictReject:
		p.Status = project.StatusRejected
		p.RejectReason = gateRes.Reason
		return o.save(ctx, p)
	case project.VerdictEscalate:
		p.Status = project.StatusEscalated
		p.RejectReason = gateRes.Reason
		return o.save(ctx, p)
	case project.VerdictAllow:
		if err := o.save(ctx, p); err != nil {
			return err
		}
	}

	// 3) Build the first iteration.
	return o.runBuild(ctx, projectID, "")
}

func (o *Orchestrator) runBuild(ctx context.Context, projectID, prompt string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if prompt != "" && !p.CanReiterate() {
		return errors.New("no reiterations remaining")
	}

	number := p.IterationsUsed + 1
	it := &project.Iteration{
		ID:        id.New(),
		ProjectID: p.ID,
		Number:    number,
		Prompt:    prompt,
		Status:    project.StatusBuilding,
		CreatedAt: time.Now().UTC(),
	}
	if err := o.store.CreateIteration(ctx, it); err != nil {
		return err
	}
	if err := o.setStatus(ctx, p, project.StatusBuilding); err != nil {
		return err
	}

	// Live progress: reset history, accumulate, publish, and periodically persist
	// the log + heartbeat so an in-flight build survives a restart.
	o.broker.Reset(p.ID)
	var logBuf strings.Builder
	lines := 0
	persist := func() {
		it.Log = logBuf.String()
		it.HeartbeatAt = time.Now().UTC()
		_ = o.store.UpdateIteration(ctx, it)
	}
	onLog := func(line string) {
		logBuf.WriteString(line)
		logBuf.WriteByte('\n')
		o.broker.Publish(p.ID, stream.Event{Type: "log", Data: line})
		lines++
		if lines%8 == 0 {
			persist() // throttle DB writes
		}
	}

	res, err := o.builder.Build(ctx, builder.Request{
		ProjectID:     p.ID,
		Brief:         p.EffectiveBrief(),
		Plan:          p.Plan,
		Prompt:        prompt,
		RepoURL:       p.RepoURL,
		AssetManifest: o.assetManifest(ctx, p.ID),
	}, builder.Hooks{
		OnLog: onLog,
		OnSandbox: func(machineID, _ string) {
			// Persist the machine id immediately so a restart can reap it.
			it.MachineID = machineID
			it.HeartbeatAt = time.Now().UTC()
			_ = o.store.UpdateIteration(ctx, it)
		},
	})

	// Never assert "preview ready" — verify it. The agent ran `fly deploy`
	// inside the sandbox; a politely-failed deploy would otherwise hand the
	// customer a dead link.
	if err == nil && res.PreviewURL != "" {
		onLog("Verifying the deployed site…")
		if verr := o.verifier.Verify(ctx, res.PreviewURL); verr != nil {
			err = verr
		} else {
			onLog("Verified live ✓")
		}
	}
	if err != nil {
		it.Status = project.StatusFailed
		it.Log = logBuf.String()
		_ = o.store.UpdateIteration(ctx, it)
		return err
	}

	if res.PreviewURL != "" {
		onLog("Preview ready: " + res.PreviewURL)
	}
	it.Status = project.StatusPreviewReady
	it.PreviewURL = res.PreviewURL
	it.Log = logBuf.String()
	if err := o.store.UpdateIteration(ctx, it); err != nil {
		return err
	}

	p.IterationsUsed = number
	p.PreviewURL = res.PreviewURL
	if res.RepoURL != "" {
		p.RepoURL = res.RepoURL
	}
	p.Status = project.StatusPreviewReady
	return o.save(ctx, p)
}

func (o *Orchestrator) setStatus(ctx context.Context, p *project.Project, s project.Status) error {
	p.Status = s
	return o.save(ctx, p)
}

func (o *Orchestrator) save(ctx context.Context, p *project.Project) error {
	p.UpdatedAt = time.Now().UTC()
	return o.store.UpdateProject(ctx, p)
}

func (o *Orchestrator) markFailed(ctx context.Context, projectID string, cause error) {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return
	}
	// A failed reiteration must not brick the project: the previous preview is
	// still live and the change credit was not consumed (IterationsUsed only
	// advances on success). Return to preview_ready; the failed attempt stays
	// visible in the iteration history.
	if p.IterationsUsed >= 1 && p.PreviewURL != "" {
		p.Status = project.StatusPreviewReady
		_ = o.save(ctx, p)
		return
	}
	p.Status = project.StatusFailed
	if p.RejectReason == "" {
		p.RejectReason = cause.Error()
	}
	_ = o.save(ctx, p)
}
