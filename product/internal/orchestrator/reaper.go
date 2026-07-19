package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// sandboxMaxAge is how old a sandbox machine may get before the sweep reaps
// it regardless of ownership. Builds are bounded by pipelineTimeout (110m),
// so 2h means "leaked". sandboxOrphanGrace is the much shorter leash for a
// machine no active build owns — e.g. a restart interrupted its teardown and
// left agents running in it, burning tokens (seen live on SEO Probe). The
// grace covers the window between machine creation and the iteration's
// MachineID being persisted.
const (
	sandboxMaxAge      = 150 * time.Minute
	sandboxOrphanGrace = 15 * time.Minute
	// preBuildStuckAfter bounds how long a project may sit in a transient
	// pre-build state before the sweep fails it. Those states are driven only by
	// a live goroutine and back no iteration row, so build recovery can't see
	// them — a deploy/crash mid-planning otherwise strands the customer on a
	// permanent spinner (and the quota lock blocks a fresh start). Well above the
	// LLM step timeouts; hasActive spares anything this process is still driving.
	preBuildStuckAfter = 10 * time.Minute
	// domainIntentGrace bounds how long a paid checkout's domain intent may sit
	// unprovisioned before the sweep re-drives it. provisionDomainIntent is
	// idempotent; the grace just lets the normally-instant provisioning goroutine
	// clear it first so we don't race it.
	domainIntentGrace = 10 * time.Minute
)

// Reap removes infrastructure that should no longer exist:
//
//   - preview apps of failed projects (a failed project keeps no site up),
//   - preview apps untouched longer than previewTTL — the project is marked
//     expired so the customer sees why the link died,
//   - sandbox machines no active build owns (orphan sweep, minutes) and any
//     older than a legitimate build could be (hard backstop).
//
// Idempotent and best-effort: one broken project must not stop the sweep.
// Called on startup and periodically (see StartReaper).
func (o *Orchestrator) Reap(ctx context.Context, previewTTL time.Duration) {
	// Zombie builds: a 'building' iteration whose heartbeat has been silent far
	// past any healthy quiet stretch, with no goroutine in this process driving
	// it — left behind by a crash/restart race. Without this sweep it stays
	// "building" forever, blocks a concurrency slot, and gets resurrected by
	// every restart. Re-attach if its sandbox is still alive, else reap — the
	// same idempotent pair startup recovery uses.
	const zombieAfter = 20 * time.Minute
	var keepMachines []string // machines owned by an active build — the sweep spares them
	if its, err := o.store.ActiveIterations(ctx); err == nil {
		for _, it := range its {
			if it.MachineID != "" {
				keepMachines = append(keepMachines, it.MachineID)
			}
			if time.Since(it.HeartbeatAt) < zombieAfter || o.hasActive(it.ProjectID) {
				continue
			}
			o.log.Warn("reap: zombie build", "project", it.ProjectID,
				"silent", time.Since(it.HeartbeatAt).Round(time.Minute))
			if o.reattachInterrupted(ctx, it) {
				continue
			}
			o.reapInterrupted(ctx, it)
		}
	}

	projects, err := o.store.Projects(ctx)
	if err != nil {
		o.log.Error("reap: list projects", "err", err)
		return
	}
	for _, p := range projects {
		switch {
		case p.Status == project.StatusFailed && p.IterationsUsed == 0 && hadBuild(p):
			// Failed before any successful build — the app (if the agent got as
			// far as deploying something) must not stay up. This repeats on every
			// sweep (failed is already terminal, so there is no state to flip);
			// DestroyApp tolerates 404s, making the repeat a cheap no-op.
			o.destroyPreviewApp(ctx, p, project.StatusFailed, "")
		case p.Status == project.StatusPreviewReady && !p.Paid && previewTTL > 0 &&
			time.Since(p.UpdatedAt) > previewTTL:
			// A paid subscriber's preview is never reaped — they're waiting on
			// delivery, not idle.
			o.log.Info("reap: expiring idle preview", "project", p.ID)
			o.destroyPreviewApp(ctx, p, project.StatusExpired,
				"Preview expired after "+formatDays(previewTTL)+" — start a new project to rebuild it.")
		case isStuckPreBuild(p) && !o.hasActive(p.ID) && time.Since(p.UpdatedAt) > preBuildStuckAfter:
			// A pre-build goroutine died (deploy/crash) with no iteration row for
			// build recovery to find. Fail it so the customer gets a Retry button
			// (RetryBuild redoes plan→gate→build) and the quota lock frees.
			o.log.Warn("reap: failing stranded pre-build project", "project", p.ID, "status", p.Status)
			p.Status = project.StatusFailed
			p.RejectReason = "Setup was interrupted before your build started — nothing was lost. Press Retry to run it again."
			if err := o.save(ctx, p); err != nil {
				o.log.Error("reap: fail stranded pre-build", "project", p.ID, "err", err)
			}
		case p.Paid && p.DomainIntent != "" && !p.HasDomain() && !o.hasActive(p.ID) &&
			time.Since(p.UpdatedAt) > domainIntentGrace:
			// A paid checkout's domain was never provisioned — the goroutine died
			// between charging and registering. Re-drive it (idempotent); the
			// customer paid and must get their domain or a refund.
			o.log.Warn("reap: re-driving stranded domain intent", "project", p.ID)
			go o.provisionDomainIntent(p.ID)
		}
	}

	if n, err := o.machines.SweepSandboxes(ctx, sandboxOrphanGrace, sandboxMaxAge, keepMachines); err != nil {
		o.log.Error("reap: sweep sandboxes", "err", err)
	} else if n > 0 {
		o.log.Warn("reap: destroyed leaked sandbox machines", "count", n)
	}

	// Not a reap, but the same cadence: upgrade previews stuck on their direct
	// fly.dev URL to the branded host once it answers (see preview.go).
	o.healBrandedPreviews(ctx)
}

// StartReaper runs Reap now and then every interval until ctx is done.
func (o *Orchestrator) StartReaper(ctx context.Context, interval, previewTTL time.Duration) {
	go func() {
		o.Reap(ctx, previewTTL)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				o.Reap(ctx, previewTTL)
			}
		}
	}()
}

// DestroyPreview is the operator action: tear down a project's preview app now
// and mark the project expired. Used from /admin.
func (o *Orchestrator) DestroyPreview(ctx context.Context, projectID string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	o.destroyPreviewApp(ctx, p, project.StatusExpired, "Preview taken down by the operator.")
	return nil
}

// PurgeProject fully removes a project: destroys its preview/customer Fly app
// (forge-<id> — never a core app, since the name is derived from the project
// id), frees any leftover sandbox machine, deletes every object under the
// project's storage prefix, and finally deletes the database row plus its
// iterations and asset metadata. Fly teardown is best-effort because leaked
// apps are recoverable; object deletion is mandatory so an apparently deleted
// project never leaves customer data behind.
func (o *Orchestrator) PurgeProject(ctx context.Context, projectID string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if err := o.machines.DestroyApp(ctx, builder.DeployAppName(p.ID)); err != nil {
		o.log.Error("purge: destroy app", "project", p.ID, "err", err)
	}
	if its, err := o.store.IterationsByProject(ctx, projectID); err == nil {
		for _, it := range its {
			if it.MachineID != "" {
				_ = o.machines.DestroySandbox(ctx, &fly.Sandbox{MachineID: it.MachineID})
			}
		}
	}
	if err := o.storage.DeletePrefix(ctx, "projects/"+projectID+"/"); err != nil {
		return fmt.Errorf("purge project objects: %w", err)
	}
	o.activity.Clear(projectID)
	return o.store.DeleteProject(ctx, projectID)
}

// PurgeAllProjects removes every project and its preview env — the operator's
// "clean the slate" action. Returns how many were purged. Individual failures
// are logged and skipped so one bad project can't block the rest.
func (o *Orchestrator) PurgeAllProjects(ctx context.Context) (int, error) {
	all, err := o.store.Projects(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range all {
		if err := o.PurgeProject(ctx, p.ID); err != nil {
			o.log.Error("purge all: project", "project", p.ID, "err", err)
			continue
		}
		n++
	}
	o.log.Info("purge all: complete", "purged", n, "of", len(all))
	return n, nil
}

// destroyPreviewApp deletes the project's Fly app and records the new status.
// The status write is skipped if the destroy failed, so the next Reap retries.
func (o *Orchestrator) destroyPreviewApp(ctx context.Context, p *project.Project, status project.Status, reason string) {
	if err := o.machines.DestroyApp(ctx, builder.DeployAppName(p.ID)); err != nil {
		o.log.Error("reap: destroy app", "project", p.ID, "err", err)
		return
	}
	if p.Status == status && reason == "" {
		return // e.g. failed → failed with nothing to add
	}
	p.Status = status
	if reason != "" {
		p.RejectReason = reason
	}
	if err := o.save(ctx, p); err != nil {
		o.log.Error("reap: save project", "project", p.ID, "err", err)
	}
}

// hadBuild reports whether a build was ever attempted (an app may exist).
func hadBuild(p *project.Project) bool {
	return p.PreviewURL != "" || p.Verdict == project.VerdictAllow
}

// isStuckPreBuild reports whether a project is in a transient state that only a
// live goroutine drives and no iteration row backs — so if that goroutine is
// gone, nothing recovers it. NeedsInput and AwaitingApproval are deliberately
// excluded: those are resting states waiting on the customer, not in-flight work.
func isStuckPreBuild(p *project.Project) bool {
	switch p.Status {
	case project.StatusCreated, project.StatusClarifying,
		project.StatusPlanning, project.StatusScreening:
		return true
	}
	return false
}

func formatDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}
