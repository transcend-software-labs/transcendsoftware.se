package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// adminView is the data for the operator dashboard.
type adminView struct {
	Escalated []*project.Project
	Accepted  []reviewItem // accepted by the customer, awaiting delivery review
	Active    []activeBuild
	Previews  []reviewItem // live preview apps (cost money; can be destroyed)
	Stats     buildStats
	Recent    []recentBuild // recent builds with cost/timing
}

// recentBuild is one build's cost + timing line for /admin.
type recentBuild struct {
	Project  string
	When     time.Time
	Duration string // "4m12s"
	Tokens   int
	CostStr  string // rough "$0.007"
	Status   project.Status
}

// reviewItem is a project plus short-lived presigned URLs for its per-page
// preview screenshots, for visual review in /admin.
type reviewItem struct {
	*project.Project
	Shots []reviewShot
}

// reviewShot is one page screenshot with a presigned URL.
type reviewShot struct {
	Path string
	URL  string
}

// Thumb returns the first shot's URL (for compact listings), or "".
func (r reviewItem) Thumb() string {
	if len(r.Shots) > 0 {
		return r.Shots[0].URL
	}
	return ""
}

// withScreenshots presigns short-lived GET URLs for each of p's page shots.
func (s *Server) withScreenshots(ctx context.Context, p *project.Project) reviewItem {
	item := reviewItem{Project: p}
	for _, sc := range p.Screenshots {
		if u, err := s.storage.PresignGet(ctx, sc.Key, 10*time.Minute); err == nil {
			item.Shots = append(item.Shots, reviewShot{Path: sc.Path, URL: u})
		}
	}
	return item
}

// buildStats summarizes build activity over the last 24h.
type buildStats struct {
	Total       int
	Succeeded   int
	Failed      int
	Building    int
	AvgBuildStr string // human "4m12s" over completed builds, or "—"
	TotalTokens int
	CostStr     string // rough total machine cost, "$0.14"
}

func computeStats(its []*project.Iteration, costPerHour float64) buildStats {
	var s buildStats
	var totalDur, completedDur time.Duration
	var completed int
	for _, it := range its {
		s.Total++
		s.TotalTokens += it.Tokens
		totalDur += it.Duration()
		switch it.Status {
		case project.StatusPreviewReady:
			s.Succeeded++
			if d := it.Duration(); d > 0 {
				completedDur += d
				completed++
			}
		case project.StatusFailed:
			s.Failed++
		case project.StatusBuilding:
			s.Building++
		}
	}
	if completed > 0 {
		s.AvgBuildStr = (completedDur / time.Duration(completed)).Round(time.Second).String()
	} else {
		s.AvgBuildStr = "—"
	}
	s.CostStr = estCost(totalDur, costPerHour)
	return s
}

// estCost renders a rough machine cost for a duration at $/hour.
func estCost(d time.Duration, costPerHour float64) string {
	return fmt.Sprintf("$%.3f", d.Hours()*costPerHour)
}

// activeBuild is one in-flight build, for the "what are we working on" view.
type activeBuild struct {
	ProjectID   string
	ProjectName string
	MachineID   string
	Started     time.Time
}

// handleAdmin shows escalated projects and the builds currently in flight.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request, _ *user.User) {
	ctx := r.Context()
	escalated, err := s.store.EscalatedProjects(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	its, err := s.store.ActiveIterations(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	active := make([]activeBuild, 0, len(its))
	for _, it := range its {
		name := it.ProjectID
		if p, err := s.store.ProjectByID(ctx, it.ProjectID); err == nil {
			name = p.Name
		}
		active = append(active, activeBuild{
			ProjectID: it.ProjectID, ProjectName: name,
			MachineID: it.MachineID, Started: it.CreatedAt,
		})
	}

	// Live preview apps — each one is running infrastructure.
	all, err := s.store.Projects(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	names := make(map[string]string, len(all))
	var previews, accepted []reviewItem
	for _, p := range all {
		names[p.ID] = p.Name
		switch p.Status {
		case project.StatusPreviewReady:
			previews = append(previews, s.withScreenshots(ctx, p))
		case project.StatusAccepted:
			accepted = append(accepted, s.withScreenshots(ctx, p))
		}
	}

	// Last-24h build stats + a per-build cost/timing table.
	recent, err := s.store.IterationsSince(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rate := s.cfg.SandboxCostPerHour
	builds := make([]recentBuild, 0, len(recent))
	for i, it := range recent {
		if i == 20 {
			break // newest 20 is plenty
		}
		name := names[it.ProjectID]
		if name == "" {
			name = it.ProjectID
		}
		builds = append(builds, recentBuild{
			Project: name, When: it.CreatedAt, Duration: it.Duration().Round(time.Second).String(),
			Tokens: it.Tokens, CostStr: estCost(it.Duration(), rate), Status: it.Status,
		})
	}

	s.render(w, http.StatusOK, "admin", s.view(r, "Operator review", adminView{
		Escalated: escalated, Accepted: accepted, Active: active, Previews: previews,
		Stats: computeStats(recent, rate), Recent: builds,
	}))
}

// handleAdminDestroyPreview tears down a project's preview app immediately.
func (s *Server) handleAdminDestroyPreview(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.DestroyPreview(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminMirror re-pushes a project's latest workspace snapshot to GitHub.
// Backfills projects built before mirroring worked and retries after a token
// fix, with no rebuild. On failure it shows the exact error (e.g. the GitHub API
// response) so the cause — a token scope, repo access — is diagnosable at once.
func (s *Server) handleAdminMirror(w http.ResponseWriter, r *http.Request, _ *user.User) {
	id := r.PathValue("id")
	if err := s.orch.RemirrorProject(r.Context(), id); err != nil {
		s.log.Error("admin: re-mirror failed", "project", id, "err", err)
		http.Error(w, "Re-mirror failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/projects/"+id, http.StatusSeeOther)
}

// handleAdminDeliver completes the handover: Rasmus has reviewed + guaranteed
// an accepted project.
func (s *Server) handleAdminDeliver(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.DeliverProject(r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminReturn sends an accepted project back to the customer for changes.
func (s *Server) handleAdminReturn(w http.ResponseWriter, r *http.Request, _ *user.User) {
	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.orch.ReturnToCustomer(r.PathValue("id"), note); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminApprove clears an escalated project to build.
func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request, _ *user.User) {
	s.orch.ApproveEscalated(r.PathValue("id"))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminReject declines an escalated project.
func (s *Server) handleAdminReject(w http.ResponseWriter, r *http.Request, _ *user.User) {
	reason := strings.TrimSpace(r.FormValue("reason"))
	if err := s.orch.RejectEscalated(r.PathValue("id"), reason); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
