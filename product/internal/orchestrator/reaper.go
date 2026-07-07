package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// sandboxMaxAge is how old a sandbox machine may get before the sweep reaps
// it. Builds are bounded by pipelineTimeout (70m), so 2h means "leaked".
const sandboxMaxAge = 2 * time.Hour

// Reap removes infrastructure that should no longer exist:
//
//   - preview apps of failed projects (a failed project keeps no site up),
//   - preview apps untouched longer than previewTTL — the project is marked
//     expired so the customer sees why the link died,
//   - sandbox machines older than any legitimate build (leak sweep).
//
// Idempotent and best-effort: one broken project must not stop the sweep.
// Called on startup and periodically (see StartReaper).
func (o *Orchestrator) Reap(ctx context.Context, previewTTL time.Duration) {
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
		case p.Status == project.StatusPreviewReady && previewTTL > 0 &&
			time.Since(p.UpdatedAt) > previewTTL:
			o.log.Info("reap: expiring idle preview", "project", p.ID)
			o.destroyPreviewApp(ctx, p, project.StatusExpired,
				"Preview expired after "+formatDays(previewTTL)+" — start a new project to rebuild it.")
		}
	}

	if n, err := o.machines.SweepSandboxes(ctx, sandboxMaxAge); err != nil {
		o.log.Error("reap: sweep sandboxes", "err", err)
	} else if n > 0 {
		o.log.Warn("reap: destroyed leaked sandbox machines", "count", n)
	}
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

func formatDays(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}
