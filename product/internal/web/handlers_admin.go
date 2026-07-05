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
	Active    []activeBuild
	Previews  []*project.Project // live preview apps (cost money; can be destroyed)
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
	var previews []*project.Project
	for _, p := range all {
		if p.Status == project.StatusPreviewReady {
			previews = append(previews, p)
		}
	}

	s.render(w, http.StatusOK, "admin", s.view(r, "Operator review", adminView{
		Escalated: escalated, Active: active, Previews: previews,
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
