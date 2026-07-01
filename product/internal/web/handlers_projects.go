package web

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
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
	Assets     []*project.Asset
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
	assets, err := s.store.AssetsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "project", s.view(r, p.Name, projectView{Project: p, Iterations: its, Assets: assets}))
}

var allowedAssetTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

var filenameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeFilename(name string) string {
	name = filenameUnsafe.ReplaceAllString(filepath.Base(name), "_")
	if name == "" || name == "." || name == "_" {
		name = "file"
	}
	if len(name) > 100 {
		name = name[len(name)-100:]
	}
	return name
}

// handleUploadAsset stores a customer-uploaded file (photo, logo, content) in
// object storage and records its metadata.
func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	defer file.Close()

	ct := hdr.Header.Get("Content-Type")
	if !allowedAssetTypes[ct] || hdr.Size > maxUpload {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}

	filename := sanitizeFilename(hdr.Filename)
	key := fmt.Sprintf("projects/%s/assets/%s", p.ID, filename)
	if err := s.storage.Put(r.Context(), key, ct, file, hdr.Size); err != nil {
		s.log.Error("asset put", "err", err)
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}

	a := &project.Asset{
		ID: id.New(), ProjectID: p.ID, Key: key, Filename: filename,
		ContentType: ct, Size: hdr.Size, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateAsset(r.Context(), a); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleProjectStatus returns the live status fragment for HTMX polling. When the
// project leaves a polling state (build finished, rejected, needs input, …) it
// asks HTMX to reload the whole page, so the preview link, plan and reiterate
// form appear and the now-dead live-build log panel goes away. Polling only runs
// while a step is in progress, so this reload fires once, at the transition.
func (s *Server) handleProjectStatus(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if !polling(p) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "project_status", p); err != nil {
		s.log.Error("render status", "err", err)
	}
}

// handleProjectStream is a Server-Sent Events endpoint that relays live build
// progress to the browser (consumed by the HTMX SSE extension).
func (s *Server) handleProjectStream(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // don't let proxies buffer the stream

	history, ch, cancel := s.broker.Subscribe(p.ID)
	defer cancel()

	for _, e := range history {
		writeSSEEvent(w, e)
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			writeSSEEvent(w, e)
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes one event as an HTML fragment for an HTMX beforeend swap.
func writeSSEEvent(w io.Writer, e stream.Event) {
	html := `<div class="logline">` + template.HTMLEscapeString(e.Data) + `</div>`
	fmt.Fprintf(w, "event: %s\n", e.Type)
	for _, line := range strings.Split(html, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
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
