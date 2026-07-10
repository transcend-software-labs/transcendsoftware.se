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
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/notify"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
)

// pipelineTimeout bounds a single plan→gate→build pass. Must stay above the
// opencode buildDeadline (90m) with headroom for the surrounding steps: plan,
// gate, snapshot save, deploy verification (with its retry window) and the
// screenshot crawl — and below sandboxMaxAge so the reaper doesn't kill a
// legitimately long build.
const pipelineTimeout = 110 * time.Minute

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

	notifier      notify.Notifier // email; defaults to Noop until configured
	operatorEmail string          // Rasmus — escalation/failure notices
	baseURL       string          // for links in emails
	templateKey   string          // object-storage key of the starter-app tarball ("" = greenfield)
	implModel     string          // active build/intake/gate model, stamped on iterations for experiment analysis
	plannerModel  string          // active planning model, stamped on iterations
	activeMu      sync.Mutex      // guards active
	active        map[string]bool // projects with an in-flight pipeline goroutine in THIS process

	critic     llm.Critic // vision design review of preview screenshots (nil = off)
	autoPolish bool       // let a POLISH critique trigger one internal refinement pass
}

// SetTemplate points first builds at a starter-app tarball in object storage.
// Empty means greenfield (the pre-template behavior).
func (o *Orchestrator) SetTemplate(key string) { o.templateKey = key }

// SetModels records the active model wiring so every iteration is stamped with
// the models that produced it — the data model experiments are analyzed on.
func (o *Orchestrator) SetModels(impl, planner string) { o.implModel, o.plannerModel = impl, planner }

// SetCritic wires the vision design critic that reviews preview screenshots.
// autoPolish lets a POLISH verdict trigger one internal refinement build that
// consumes none of the customer's change credits.
func (o *Orchestrator) SetCritic(c llm.Critic, autoPolish bool) {
	o.critic, o.autoPolish = c, autoPolish
}

// New returns an orchestrator.
func New(s store.Store, in llm.Intake, p llm.Planner, g llm.SafetyGate, b builder.Builder, m fly.Machines, as storage.Store, br *stream.Broker, v Verifier, log *slog.Logger) *Orchestrator {
	return &Orchestrator{store: s, intake: in, planner: p, gate: g, builder: b, machines: m, storage: as, broker: br, verifier: v, log: log, notifier: notify.Noop{}, active: map[string]bool{}}
}

// SetNotifications wires transactional email. operatorEmail receives escalation
// and failure notices; customers are emailed when a preview is ready. baseURL
// is used for links. A nil notifier leaves the default Noop in place.
func (o *Orchestrator) SetNotifications(n notify.Notifier, operatorEmail, baseURL string) {
	if n != nil {
		o.notifier = n
	}
	o.operatorEmail = operatorEmail
	o.baseURL = baseURL
}

// notifyOperator emails Rasmus, best-effort (a failed send must never affect
// the pipeline; the state is already persisted by the time we get here).
func (o *Orchestrator) notifyOperator(ctx context.Context, subject, body string) {
	if o.operatorEmail == "" {
		return
	}
	if err := o.notifier.Send(ctx, o.operatorEmail, subject, body); err != nil {
		o.log.Error("notify operator", "err", err)
	}
}

// notifyCustomer emails the project's owner, best-effort.
func (o *Orchestrator) notifyCustomer(ctx context.Context, userID, subject, body string) {
	u, err := o.store.UserByID(ctx, userID)
	if err != nil {
		o.log.Error("notify customer: lookup user", "err", err)
		return
	}
	if err := o.notifier.Send(ctx, u.Email, subject, body); err != nil {
		o.log.Error("notify customer", "err", err)
	}
}

func (o *Orchestrator) projectLink(id string) string {
	if o.baseURL == "" {
		return ""
	}
	return o.baseURL + "/projects/" + id
}

// baseURLOr returns baseURL+path, or just path if no base URL is configured.
func (o *Orchestrator) baseURLOr(path string) string {
	return o.baseURL + path
}

// assetContext renders the customer's uploaded files for the plan/gate/build
// prompts: the filename plus their own words on what each file is. "" when
// nothing is uploaded.
func (o *Orchestrator) assetContext(ctx context.Context, projectID string) string {
	assets, err := o.store.AssetsByProject(ctx, projectID)
	if err != nil || len(assets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Files uploaded by the customer (available in /workspace/assets/ during the build), with their description of what each one is:")
	for _, a := range assets {
		b.WriteString("\n- " + a.Filename)
		if a.Description != "" {
			b.WriteString(" — " + a.Description)
		}
	}
	b.WriteString("\nUse them where the customer's description says they belong.")
	return b.String()
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
// RecoverInterrupted runs at startup. A build runs server-side in its sandbox
// (opencode async session), so it keeps going while the orchestrator is down —
// for each build that was in flight, try to re-attach to the still-running
// sandbox and finish it. Only if that isn't possible (no handle, past deadline,
// or the sandbox is gone) do we reap it and mark it failed.
func (o *Orchestrator) RecoverInterrupted(ctx context.Context) {
	its, err := o.store.ActiveIterations(ctx)
	if err != nil {
		o.log.Error("recover: list active builds", "err", err)
		return
	}
	for _, it := range its {
		if o.reattachInterrupted(ctx, it) {
			continue
		}
		o.reapInterrupted(ctx, it)
	}
}

// reattachInterrupted re-connects to a build that was running when the
// orchestrator restarted and finishes it in the background. Returns false when
// the build can't be resumed — a missing handle, a build already past its
// deadline, or an unreachable (reaped/dead) sandbox — so the caller reaps it.
func (o *Orchestrator) reattachInterrupted(ctx context.Context, it *project.Iteration) bool {
	if it.MachineID == "" || it.SessionID == "" || it.SandboxAddr == "" {
		return false
	}
	remaining := opencode.BuildDeadline - time.Since(it.CreatedAt)
	if remaining <= 0 {
		return false
	}
	if !reachable(it.SandboxAddr) {
		return false
	}
	o.log.Info("re-attaching to interrupted build",
		"project", it.ProjectID, "machine", it.MachineID, "session", it.SessionID,
		"remaining", remaining.Round(time.Second))
	o.async(it.ProjectID, func(actx context.Context) error {
		return o.resumeBuild(actx, it, remaining)
	})
	return true
}

// reapInterrupted is the fallback when a build can't be re-attached: destroy the
// (dead or unreachable) sandbox and record the interruption, falling a
// reiteration back to its still-live previous preview rather than bricking it.
func (o *Orchestrator) reapInterrupted(ctx context.Context, it *project.Iteration) {
	o.log.Warn("reaping interrupted build", "project", it.ProjectID, "machine", it.MachineID)
	if it.MachineID != "" {
		if err := o.machines.DestroySandbox(ctx, &fly.Sandbox{MachineID: it.MachineID}); err != nil {
			o.log.Error("recover: reap machine", "machine", it.MachineID, "err", err)
		}
	}
	it.Status = project.StatusFailed
	_ = o.store.UpdateIteration(ctx, it)
	if p, err := o.store.ProjectByID(ctx, it.ProjectID); err == nil && p.Status == project.StatusBuilding {
		if p.IterationsUsed >= 1 && p.PreviewURL != "" {
			p.Status = project.StatusPreviewReady
		} else {
			p.Status = project.StatusFailed
			p.RejectReason = "The build was interrupted by a server restart. Nothing was lost — press Retry to run it again."
		}
		_ = o.save(ctx, p)
	}
}

// resumeBuild finishes a re-attached build: reconnect the customer's live log,
// re-open the sandbox's opencode event stream, and finalise exactly as a fresh
// build would (verify → preview ready, or snapshot + fail).
func (o *Orchestrator) resumeBuild(ctx context.Context, it *project.Iteration, remaining time.Duration) error {
	p, err := o.store.ProjectByID(ctx, it.ProjectID)
	if err != nil {
		return err
	}
	// Cap the resumed pass at the original build's remaining deadline.
	rctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()

	o.broker.Reset(p.ID)
	var logBuf strings.Builder
	logBuf.WriteString(it.Log)
	lines := 0
	onLog := func(line string) {
		line = strings.ToValidUTF8(line, "")
		logBuf.WriteString(line)
		logBuf.WriteByte('\n')
		o.broker.Publish(p.ID, stream.Event{Type: "log", Data: line})
		lines++
		if lines%8 == 0 {
			it.Log = logBuf.String()
			it.HeartbeatAt = time.Now().UTC()
			_ = o.store.UpdateIteration(rctx, it)
		}
	}

	snapshotKey := "projects/" + p.ID + "/snapshot.tgz"
	snapshotPut := ""
	if u, err := o.storage.PresignPut(ctx, snapshotKey, time.Hour); err == nil {
		snapshotPut = u
	} else {
		o.log.Error("presign snapshot put", "project", p.ID, "err", err)
	}
	screenshotPuts, screenshotKeys := o.presignScreenshots(ctx, p.ID)

	res, err := o.builder.Attach(rctx, builder.AttachRequest{
		ProjectID:         p.ID,
		MachineID:         it.MachineID,
		Addr:              it.SandboxAddr,
		SessionID:         it.SessionID,
		SnapshotPutURL:    snapshotPut,
		ScreenshotPutURLs: screenshotPuts,
	}, builder.Hooks{OnLog: onLog})

	return o.finishBuild(ctx, p, it, res, err, snapshotKey, screenshotKeys,
		func() string { return logBuf.String() }, onLog)
}

// reachable reports whether the sandbox's opencode address still answers — a
// quick TCP connect, so a reaped/dead machine is detected in seconds instead of
// tying up a re-attach goroutine until the build deadline.
func reachable(addr string) bool {
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (o *Orchestrator) async(projectID string, fn func(context.Context) error) {
	o.activeMu.Lock()
	o.active[projectID] = true
	o.activeMu.Unlock()
	go func() {
		defer func() {
			o.activeMu.Lock()
			delete(o.active, projectID)
			o.activeMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), pipelineTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			o.log.Error("pipeline step failed", "project", projectID, "err", err)
			o.markFailed(ctx, projectID, err)
		}
	}()
}

// hasActive reports whether this process is currently driving a pipeline step
// for the project — the reaper must not touch builds that are merely quiet.
func (o *Orchestrator) hasActive(projectID string) bool {
	o.activeMu.Lock()
	defer o.activeMu.Unlock()
	return o.active[projectID]
}

// StartIntake generates clarifying questions + design suggestions for a
// freshly created project. If the intake returns neither, it proceeds straight
// to planning.
func (o *Orchestrator) StartIntake(projectID string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		if err := o.setStatus(ctx, p, project.StatusClarifying); err != nil {
			return err
		}
		res, err := o.intake.Questions(ctx, p.Brief)
		if err != nil {
			return err
		}
		if len(res.Questions) == 0 && len(res.DesignOptions) == 0 {
			return o.runPlanGateBuild(ctx, projectID)
		}
		p.Questions = res.Questions
		p.DesignOptions = res.DesignOptions
		p.Status = project.StatusNeedsInput
		return o.save(ctx, p)
	})
}

// SubmitAnswers records the customer's answers and chosen design direction,
// then proceeds to plan→gate→build.
func (o *Orchestrator) SubmitAnswers(projectID, answers, design string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		p.Answers = answers
		p.DesignBrief = design
		if err := o.save(ctx, p); err != nil {
			return err
		}
		return o.runPlanGateBuild(ctx, projectID)
	})
}

// Reiterate runs another build pass against a customer change request.
func (o *Orchestrator) Reiterate(projectID, prompt string) {
	o.async(projectID, func(ctx context.Context) error {
		return o.runBuild(ctx, projectID, prompt, false)
	})
}

// RetryBuild re-runs a failed initial build. It's the recovery path for any
// failure — an agent error, a crash, or a build interrupted by a deploy. It
// consumes no change credit (IterationsUsed only advances on success) and, if
// planning never completed, redoes plan + safety gate first.
func (o *Orchestrator) RetryBuild(projectID string) {
	o.async(projectID, func(ctx context.Context) error {
		p, err := o.store.ProjectByID(ctx, projectID)
		if err != nil {
			return err
		}
		if !p.CanRetry() {
			return nil // already recovered or not in a retryable state
		}
		// Defensive sweep: a crash race can leave a previous attempt 'building'
		// with a live sandbox. Starting a new build then double-builds the same
		// app and collides on the deterministic machine name (seen live as a
		// 409 unique-name violation). Fail leftovers and free the name first.
		if its, err := o.store.IterationsByProject(ctx, p.ID); err == nil {
			for _, old := range its {
				if old.Status != project.StatusBuilding {
					continue
				}
				o.log.Warn("retry: reaping leftover building iteration", "project", p.ID, "machine", old.MachineID)
				if old.MachineID != "" {
					_ = o.machines.DestroySandbox(ctx, &fly.Sandbox{MachineID: old.MachineID})
				}
				old.Status = project.StatusFailed
				_ = o.store.UpdateIteration(ctx, old)
			}
		}
		p.RejectReason = ""
		if err := o.save(ctx, p); err != nil {
			return err
		}
		// If planning never produced a plan, redo the whole plan→gate→build;
		// otherwise just re-run the build from the existing plan.
		if p.Plan == "" {
			return o.runPlanGateBuild(ctx, projectID)
		}
		return o.runBuild(ctx, projectID, "", false)
	})
}

// AcceptPreview records the customer accepting the current preview and moves
// the project into Rasmus's final-review queue. Rasmus's personal guarantee is
// now enforced by the state machine: nothing reaches "delivered" without his
// approval.
func (o *Orchestrator) AcceptPreview(projectID string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !p.CanAccept() {
		return nil
	}
	p.Status = project.StatusAccepted
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: a project is ready for your review",
		"\""+p.Name+"\" was accepted by the customer and is waiting for your final review + guarantee.\n\n"+
			"Preview: "+p.PreviewURL+"\nReview it: "+o.baseURLOr("/admin"))
	return nil
}

// DeliverProject is the operator action completing the handover: Rasmus has
// reviewed and guaranteed the site. Terminal.
func (o *Orchestrator) DeliverProject(projectID string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.Status != project.StatusAccepted {
		return nil
	}
	p.Status = project.StatusDelivered
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyCustomer(ctx, p.UserID, "Your website is delivered",
		"Good news — \""+p.Name+"\" has been reviewed and is now delivered:\n\n"+p.PreviewURL+"\n\n"+
			"Rasmus has personally checked it. Reply to this email if you need anything.")
	return nil
}

// ReturnToCustomer sends an accepted project back for more changes (Rasmus
// wasn't satisfied, or the customer asked). It returns to preview_ready with
// its remaining reiterations intact.
func (o *Orchestrator) ReturnToCustomer(projectID, note string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.Status != project.StatusAccepted {
		return nil
	}
	p.Status = project.StatusPreviewReady
	if err := o.save(ctx, p); err != nil {
		return err
	}
	body := "\"" + p.Name + "\" was sent back for another look."
	if note != "" {
		body += "\n\nNote: " + note
	}
	body += "\n\nOpen your project: " + o.projectLink(p.ID)
	o.notifyCustomer(ctx, p.UserID, "An update on your website", body)
	return nil
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
		return o.runBuild(ctx, projectID, "", false)
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
	brief := p.EffectiveBrief()
	if a := o.assetContext(ctx, p.ID); a != "" {
		brief += "\n\n" + a
	}
	planRes, err := o.planner.Plan(ctx, brief)
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
	gateRes, err := o.gate.Screen(ctx, brief, p.Plan)
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
		if err := o.save(ctx, p); err != nil {
			return err
		}
		o.notifyOperator(ctx, "Forge: a project needs your review",
			"A project was flagged for review: "+p.Name+"\n\n"+
				"Brief:\n"+p.Brief+"\n\nReason: "+gateRes.Reason+"\n\n"+
				"Review it: "+o.baseURLOr("/admin"))
		return nil
	case project.VerdictAllow:
		if err := o.save(ctx, p); err != nil {
			return err
		}
	}

	// 3) Build the first iteration.
	return o.runBuild(ctx, projectID, "", false)
}

// runBuild executes one sandboxed build pass. internal marks an
// orchestrator-initiated refinement (the design critic's polish pass): it is
// numbered 0, bypasses the customer reiteration quota and never consumes it.
func (o *Orchestrator) runBuild(ctx context.Context, projectID, prompt string, internal bool) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if prompt != "" && !internal && !p.CanReiterate() {
		return errors.New("no reiterations remaining")
	}

	number := p.IterationsUsed + 1
	if internal {
		number = 0
	}
	it := &project.Iteration{
		ID:           id.New(),
		ProjectID:    p.ID,
		Number:       number,
		Prompt:       prompt,
		Status:       project.StatusBuilding,
		ImplModel:    o.implModel,
		PlannerModel: o.plannerModel,
		CreatedAt:    time.Now().UTC(),
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
		// The agent can emit non-UTF-8 bytes into the log (e.g. it `read`s a
		// binary file like a downloaded image). Postgres rejects invalid UTF-8,
		// which would fail the whole build at the persistence step even though
		// the site built and deployed fine. Scrub to valid UTF-8 at the source.
		line = strings.ToValidUTF8(line, "")
		logBuf.WriteString(line)
		logBuf.WriteByte('\n')
		o.broker.Publish(p.ID, stream.Event{Type: "log", Data: line})
		lines++
		if lines%8 == 0 {
			persist() // throttle DB writes
		}
	}

	// Workspace snapshots make reiterations continue from the previous build
	// instead of starting over. Presigned URLs only — the sandbox never holds
	// storage credentials.
	snapshotKey := "projects/" + p.ID + "/snapshot.tgz"
	var snapshotGet, snapshotPut string
	if p.SnapshotKey != "" {
		if u, err := o.storage.PresignGet(ctx, p.SnapshotKey, time.Hour); err == nil {
			snapshotGet = u
		} else {
			o.log.Error("presign snapshot get", "project", p.ID, "err", err)
		}
	}
	if u, err := o.storage.PresignPut(ctx, snapshotKey, time.Hour); err == nil {
		snapshotPut = u
	} else {
		o.log.Error("presign snapshot put", "project", p.ID, "err", err)
	}

	// Pre-mint presigned PUT URLs for up to maxScreenshots pages; the crawler
	// fills as many as the site has. Slot i ↔ key screenshots/<i>.png.
	screenshotPuts, screenshotKeys := o.presignScreenshots(ctx, p.ID)

	// First build with a configured starter template: seed the workspace with
	// it so the agent extends a working app instead of scaffolding.
	var templateGet string
	if p.SnapshotKey == "" && o.templateKey != "" {
		if u, err := o.storage.PresignGet(ctx, o.templateKey, time.Hour); err == nil {
			templateGet = u
		} else {
			o.log.Error("presign template get", "project", p.ID, "err", err)
		}
	}

	// The customer's email rides along as the site's OWNER_EMAIL, reserving
	// the generated site's first (owner) account for them.
	ownerEmail := ""
	if u, err := o.store.UserByID(ctx, p.UserID); err == nil {
		ownerEmail = u.Email
	}

	res, err := o.builder.Build(ctx, builder.Request{
		ProjectID:         p.ID,
		Brief:             p.EffectiveBrief(),
		Plan:              p.Plan,
		Prompt:            prompt,
		SnapshotGetURL:    snapshotGet,
		SnapshotPutURL:    snapshotPut,
		ScreenshotPutURLs: screenshotPuts,
		TemplateGetURL:    templateGet,
		AssetManifest:     o.assetManifest(ctx, p.ID),
		AssetNotes:        o.assetContext(ctx, p.ID),
		OwnerEmail:        ownerEmail,
		SiteName:          p.Name,
	}, builder.Hooks{
		OnLog: onLog,
		OnSandbox: func(machineID, addr string) {
			// Persist the handle immediately so a restart can re-attach (or reap).
			it.MachineID = machineID
			it.SandboxAddr = addr
			it.HeartbeatAt = time.Now().UTC()
			_ = o.store.UpdateIteration(ctx, it)
		},
		OnSession: func(sessionID string) {
			// The session id is what a restart re-attaches to — persist it before
			// the agent's work starts.
			it.SessionID = sessionID
			_ = o.store.UpdateIteration(ctx, it)
		},
	})

	return o.finishBuild(ctx, p, it, res, err, snapshotKey, screenshotKeys,
		func() string { return logBuf.String() }, onLog)
}

// finishBuild finalises a build pass — shared by a fresh runBuild and a
// re-attached build after a restart. It verifies the deploy, then either
// records the failure (persisting a resume snapshot) or marks the preview ready
// and notifies the customer. logSnapshot returns the full accumulated log; the
// caller owns the buffer, so both paths report the complete trace.
func (o *Orchestrator) finishBuild(ctx context.Context, p *project.Project, it *project.Iteration,
	res builder.Result, err error, snapshotKey string, screenshotKeys []string,
	logSnapshot func() string, onLog func(string)) error {
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
		it.Log = logSnapshot()
		_ = o.store.UpdateIteration(ctx, it)
		// Preserve partial progress so a Retry resumes from it instead of
		// rebuilding from scratch. Detached context — the pipeline ctx may be
		// past its deadline (a timeout is the usual reason a build fails here).
		if res.SnapshotSaved {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			p.SnapshotKey = snapshotKey
			if serr := o.save(pctx, p); serr != nil {
				o.log.Error("persist resume snapshot key", "project", p.ID, "err", serr)
			} else {
				o.log.Info("saved resume snapshot", "project", p.ID, "key", snapshotKey)
			}
			cancel()
		}
		return err
	}

	if res.PreviewURL != "" {
		onLog("Preview ready: " + res.PreviewURL)
	}
	it.Status = project.StatusPreviewReady
	it.PreviewURL = res.PreviewURL
	it.Log = logSnapshot()
	it.Tokens = res.Tokens
	it.HeartbeatAt = time.Now().UTC() // final timestamp → accurate build duration
	if err := o.store.UpdateIteration(ctx, it); err != nil {
		return err
	}

	if it.Number > 0 { // internal polish passes (number 0) consume no credit
		p.IterationsUsed = it.Number
	}
	p.PreviewURL = res.PreviewURL
	if res.SnapshotSaved {
		p.SnapshotKey = snapshotKey
	}
	if len(res.Screenshots) > 0 {
		shots := make([]project.Screenshot, 0, len(res.Screenshots))
		for _, s := range res.Screenshots {
			if s.Slot >= 0 && s.Slot < len(screenshotKeys) {
				shots = append(shots, project.Screenshot{Path: s.Path, Key: screenshotKeys[s.Slot]})
			}
		}
		p.Screenshots = shots
	}
	// Findings is non-nil exactly when the design audit ran (empty = clean); a nil
	// result means the audit failed, so leave any prior findings untouched.
	if res.Findings != nil {
		fs := make([]project.Finding, len(res.Findings))
		for i, f := range res.Findings {
			fs[i] = project.Finding{
				Antipattern: f.Antipattern, Name: f.Name, Description: f.Description,
				Severity: f.Severity, File: f.File, Line: f.Line, Snippet: f.Snippet,
			}
		}
		p.Findings = fs
	}
	p.Status = project.StatusPreviewReady
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyCustomer(ctx, p.UserID, "Your website preview is ready",
		"Your preview for \""+p.Name+"\" is ready to view:\n\n"+res.PreviewURL+"\n\n"+
			"Open your project to review it or request a change: "+o.projectLink(p.ID))

	// Visual design review — a vision model looks at the deployed pages the way
	// a customer would. Internal polish passes (number 0) are not re-reviewed,
	// which also caps the loop at exactly one refinement per build.
	if o.critic != nil && it.Number > 0 && len(p.Screenshots) > 0 {
		o.async(p.ID, func(cctx context.Context) error {
			o.critiquePreview(cctx, p.ID)
			return nil
		})
	}
	return nil
}

// critiquePreview downloads the preview's screenshots, asks the critic for a
// verdict, stores it for /admin, and (when enabled) turns a POLISH verdict into
// one internal refinement build. Best-effort throughout: any failure just means
// no critique.
func (o *Orchestrator) critiquePreview(ctx context.Context, projectID string) {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil || p.Status != project.StatusPreviewReady {
		return
	}
	var pngs [][]byte
	for _, sc := range p.Screenshots {
		if b, err := o.storage.Get(ctx, sc.Key); err == nil {
			pngs = append(pngs, b)
		}
	}
	if len(pngs) == 0 {
		return
	}
	brief := "The design direction this site was built to:\n\n" + designSection(p.Plan) +
		"\n\nReview the attached screenshots of the deployed pages."
	verdict, err := o.critic.CritiqueDesign(ctx, brief, pngs)
	if err != nil {
		o.log.Warn("design critic failed (skipped)", "project", p.ID, "err", err)
		return
	}
	p.Critique = strings.TrimSpace(verdict)
	if err := o.save(ctx, p); err != nil {
		return
	}
	polish := strings.HasPrefix(strings.ToUpper(p.Critique), "POLISH")
	o.log.Info("design critique", "project", p.ID, "polish", polish)
	if polish && o.autoPolish {
		o.broker.Publish(p.ID, stream.Event{Type: "log",
			Data: "Design review found polish items — running one refinement pass…"})
		if err := o.runBuild(ctx, p.ID, "DESIGN POLISH — the deployed site passed all functional checks. A design "+
			"director reviewed screenshots of the LIVE pages and found these visual issues. "+
			"Fix exactly these (CSS/template work), verify, and redeploy. Change nothing else.\n\n"+p.Critique, true); err != nil {
			o.log.Warn("polish pass failed", "project", p.ID, "err", err)
		}
	}
}

// designSection extracts the plan's "## Design" section for the critic; falls
// back to a truncated plan when the marker is missing.
func designSection(plan string) string {
	if i := strings.Index(plan, "## Design"); i >= 0 {
		rest := plan[i:]
		if j := strings.Index(rest[3:], "\n## "); j >= 0 {
			return rest[:j+3]
		}
		return rest
	}
	if len(plan) > 4000 {
		return plan[:4000]
	}
	return plan
}

// presignScreenshots pre-mints presigned PUT URLs for up to maxScreenshots
// pages (slot i ↔ key screenshots/<i>.png); the crawler fills as many as the
// site has. Shared by a fresh build and a re-attach.
func (o *Orchestrator) presignScreenshots(ctx context.Context, projectID string) (puts, keys []string) {
	const maxScreenshots = 8
	for i := 0; i < maxScreenshots; i++ {
		key := fmt.Sprintf("projects/%s/screenshots/%d.png", projectID, i)
		u, err := o.storage.PresignPut(ctx, key, time.Hour)
		if err != nil {
			o.log.Error("presign screenshot put", "project", projectID, "err", err)
			break
		}
		puts = append(puts, u)
		keys = append(keys, key)
	}
	return puts, keys
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
	o.notifyOperator(ctx, "Forge: a build failed",
		"Build failed for \""+p.Name+"\": "+p.RejectReason+"\n\n"+o.projectLink(p.ID))
}
