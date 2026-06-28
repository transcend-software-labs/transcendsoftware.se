package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, u *user.User) {
	projects, err := s.store.ProjectsByUser(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "dashboard", s.view(r, "Dashboard", projects))
}

func (s *Server) handleNewProjectForm(w http.ResponseWriter, r *http.Request, _ *user.User) {
	s.render(w, http.StatusOK, "new_project", s.view(r, "Start a project", nil))
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, u *user.User) {
	brief := strings.TrimSpace(r.FormValue("brief"))
	name := strings.TrimSpace(r.FormValue("name"))
	if len(brief) < 10 {
		s.render(w, http.StatusBadRequest, "new_project", View{Title: "Start a project", User: u,
			Flash: "Tell me a bit more about the site you want (at least a sentence)."})
		return
	}
	if name == "" {
		name = "New project"
	}

	now := time.Now().UTC()
	p := &project.Project{
		ID:        id.New(),
		UserID:    u.ID,
		Name:      name,
		Brief:     brief,
		Status:    project.StatusCreated,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateProject(r.Context(), p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Signal the orchestrator: intake → plan → gate → build (async).
	s.orch.StartIntake(p.ID)

	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleAnswer records the customer's answers to the clarifying questions and
// kicks off planning.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	answers := strings.TrimSpace(r.FormValue("answers"))
	if p.Status != project.StatusNeedsInput || answers == "" {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	s.orch.SubmitAnswers(p.ID, answers)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

type projectView struct {
	Project    *project.Project
	Iterations []*project.Iteration
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	its, err := s.store.IterationsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "project", s.view(r, p.Name, projectView{Project: p, Iterations: its}))
}

// handleProjectStatus returns the live status fragment for HTMX polling.
func (s *Server) handleProjectStatus(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "project_status", p); err != nil {
		s.log.Error("render status", "err", err)
	}
}

func (s *Server) handleReiterate(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if !p.CanReiterate() || prompt == "" {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	s.orch.Reiterate(p.ID, prompt)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}
