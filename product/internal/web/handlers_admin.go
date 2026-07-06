package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// adminView is the data for the operator dashboard.
type adminView struct {
	Escalated []*project.Project
	Accepted  []*project.Project // accepted by the customer, awaiting delivery review
	Active    []activeBuild
	Previews  []*project.Project // live preview apps (cost money; can be destroyed)
	Stats     buildStats
}

// buildStats summarizes build activity over the last 24h (visibility until
// email-on-failure exists).
type buildStats struct {
	Total       int
	Succeeded   int
	Failed      int
	Building    int
	AvgBuildStr string // human "4m12s" over completed builds, or "—"
}

func computeStats(its []*project.Iteration) buildStats {
	var s buildStats
	var totalDur time.Duration
	var completed int
	for _, it := range its {
		s.Total++
		switch it.Status {
		case project.StatusPreviewReady:
			s.Succeeded++
			if d := it.HeartbeatAt.Sub(it.CreatedAt); d > 0 {
				totalDur += d
				completed++
			}
		case project.StatusFailed:
			s.Failed++
		case project.StatusBuilding:
			s.Building++
		}
	}
	if completed > 0 {
		s.AvgBuildStr = (totalDur / time.Duration(completed)).Round(time.Second).String()
	} else {
		s.AvgBuildStr = "—"
	}
	return s
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
	var previews, accepted []*project.Project
	for _, p := range all {
		switch p.Status {
		case project.StatusPreviewReady:
			previews = append(previews, p)
		case project.StatusAccepted:
			accepted = append(accepted, p)
		}
	}

	// Last-24h build stats (money + reliability at a glance).
	recent, err := s.store.IterationsSince(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.render(w, http.StatusOK, "admin", s.view(r, "Operator review", adminView{
		Escalated: escalated, Accepted: accepted, Active: active, Previews: previews,
		Stats: computeStats(recent),
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
