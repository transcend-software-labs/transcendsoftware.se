package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

// generatableSlot returns the content item for a slot if it's an image slot
// the AI may generate, else false.
func generatableSlot(p *project.Project, slot string) (project.ContentItem, bool) {
	for _, c := range p.Spec.ContentNeeded {
		if c.Slug == slot && c.CanGenerate() {
			return c, true
		}
	}
	return project.ContentItem{}, false
}

const imageJobTimeout = 4 * time.Minute

// genCandidates stores generated PNGs and completes the matching background
// job. JobID keeps a late completion from replacing a newer request.
func (s *Server) genCandidates(ctx context.Context, projectID, slot, jobID, prompt string, pngs [][]byte) (*project.Project, error) {
	keys := make([]string, 0, len(pngs))
	for _, png := range pngs {
		key := fmt.Sprintf("projects/%s/gen/%s/%s.png", projectID, slot, id.New())
		if err := s.storage.Put(ctx, key, "image/png", bytes.NewReader(png), int64(len(png))); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return s.mutateProject(ctx, projectID, func(fresh *project.Project) {
		if fresh.PendingImages == nil {
			fresh.PendingImages = map[string]project.ImageCandidates{}
		}
		if current, ok := fresh.PendingImages[slot]; ok && current.JobID == jobID {
			fresh.PendingImages[slot] = project.ImageCandidates{Prompt: prompt, Keys: keys, Status: "ready", JobID: jobID}
		}
	})
}

// reserveImageJob atomically claims a slot and one unit of the project's paid
// generation allowance. It lets requests for different slots start freely,
// while a double-click for the same slot remains a no-op.
func (s *Server) reserveImageJob(ctx context.Context, projectID, slot, prompt string) (*project.Project, string, string, error) {
	jobID := id.New()
	outcome := ""
	p, err := s.mutateProject(ctx, projectID, func(fresh *project.Project) {
		outcome = ""
		if current, ok := fresh.PendingImages[slot]; ok && current.Status == "running" {
			jobID = current.JobID
			outcome = "running"
			return
		}
		if s.imageGenExhausted(fresh) {
			outcome = "limit"
			return
		}
		if fresh.PendingImages == nil {
			fresh.PendingImages = map[string]project.ImageCandidates{}
		}
		fresh.PendingImages[slot] = project.ImageCandidates{
			Prompt: prompt, Status: "running", JobID: jobID, StartedAt: time.Now().UTC(),
		}
		fresh.ImageGenCount++
		outcome = "started"
	})
	return p, jobID, outcome, err
}

func (s *Server) failImageJob(projectID, slot, jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.mutateProject(ctx, projectID, func(fresh *project.Project) {
		if current, ok := fresh.PendingImages[slot]; ok && current.JobID == jobID {
			current.Status = "failed"
			current.StartedAt = time.Time{}
			fresh.PendingImages[slot] = current
		}
	}); err != nil {
		s.log.Error("imagegen mark failed", "project", projectID, "slot", slot, "err", err)
	}
}

func (s *Server) runImageJob(projectID, slot, jobID, prompt string, generate func(context.Context) ([][]byte, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), imageJobTimeout)
	defer cancel()
	pngs, err := generate(ctx)
	if err == nil {
		_, err = s.genCandidates(ctx, projectID, slot, jobID, prompt, pngs)
	}
	if err != nil {
		s.log.Error("background imagegen", "project", projectID, "slot", slot, "err", err)
		s.failImageJob(projectID, slot, jobID)
	}
}

// imageGenExhausted reports whether the project has hit its generation cap.
// Each generate or improve is a real paid API call, so we bound the spend a
// single project can incur.
func (s *Server) imageGenExhausted(p *project.Project) bool {
	cap := s.cfg.ImageGenMaxPerProject
	return cap > 0 && p.ImageGenCount >= cap
}

// slotCandidates presigns a slot's pending AI images for display in the modal.
func (s *Server) slotCandidates(ctx context.Context, p *project.Project, slot string) []candidateImage {
	set, ok := p.PendingImages[slot]
	if !ok {
		return nil
	}
	out := make([]candidateImage, 0, len(set.Keys))
	for i, k := range set.Keys {
		if u, err := s.storage.PresignGet(ctx, k, 30*time.Minute); err == nil {
			out = append(out, candidateImage{Index: i, URL: u})
		}
	}
	return out
}

// renderGen renders one of the AI-modal fragments (gen_prompt / gen_improve /
// gen_candidates) for an htmx swap. data is the fragment's ".Data"; Lang/CSRF
// are supplied so the fragment can translate and post safely.
func (s *Server) renderGen(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	full := map[string]any{"Lang": s.lang(r), "CSRF": s.csrfToken(r), "Data": data}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, full); err != nil {
		s.log.Error("render gen fragment", "err", err)
	}
}

// isHTMX reports whether the request came from htmx (an in-page swap) rather
// than a full navigation, so handlers can return a fragment instead of a redirect.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// defaultImagePrompt seeds a generation prompt in the customer's language from
// the plan's design direction and the slot's purpose, so the customer sees a
// ready prompt they can edit. gpt-image handles non-English prompts fine.
func defaultImagePrompt(p *project.Project, c project.ContentItem, lang string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(i18n.T(lang, "gen.prompt.lead"), c.Name(lang)))
	if brief := strings.TrimSpace(p.Brief); brief != "" {
		if len(brief) > 300 {
			brief = brief[:300]
		}
		b.WriteString(" " + i18n.T(lang, "gen.prompt.business") + brief)
	}
	if p.DesignBrief != "" {
		b.WriteString(" " + i18n.T(lang, "gen.prompt.direction") + p.DesignBrief + ".")
	}
	b.WriteString(" " + i18n.T(lang, "gen.prompt.style"))
	b.WriteString(" " + imagePromptGuard(p, c, lang))
	return b.String()
}

func imagePromptGuard(p *project.Project, c project.ContentItem, lang string) string {
	label := strings.ToLower(c.Slug + " " + c.Name("en") + " " + c.Name("sv") + " " + c.Name("ru"))
	if strings.Contains(label, "logo") || strings.Contains(label, "logotyp") || strings.Contains(label, "логотип") {
		return fmt.Sprintf(i18n.T(lang, "gen.prompt.logo_guard"), p.Name)
	}
	return i18n.T(lang, "gen.prompt.photo_guard")
}

// handleGenerateImage queues 3 candidate images and returns immediately.
func (s *Server) handleGenerateImage(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	lang := s.lang(r)
	slot := slotID(r.FormValue("slot"))
	c, ok := generatableSlot(p, slot)
	if !ok || s.imagegen == nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		prompt = defaultImagePrompt(p, c, lang)
	} else {
		if len(prompt) > 1000 {
			prompt = prompt[:1000]
		}
		prompt += " " + imagePromptGuard(p, c, lang)
	}
	p, jobID, outcome, err := s.reserveImageJob(r.Context(), p.ID, slot, prompt)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if outcome == "limit" {
		if isHTMX(r) {
			s.renderGen(w, r, "gen_prompt", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": prompt, "Err": i18n.T(lang, "prj.gen.limit_note")})
			return
		}
		http.Redirect(w, r, "/projects/"+p.ID+"?genlimit=1", http.StatusSeeOther)
		return
	}
	if outcome == "started" {
		go s.runImageJob(p.ID, slot, jobID, prompt, func(ctx context.Context) ([][]byte, error) {
			return s.imagegen.Generate(ctx, prompt, 3)
		})
	}
	if isHTMX(r) {
		s.renderGen(w, r, "gen_running", map[string]any{"PID": p.ID, "Slug": slot})
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleImageGenerationStatus is the small polling endpoint used by a modal.
// Reloading the project reconstructs the same state from the persisted job.
func (s *Server) handleImageGenerationStatus(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	lang := s.lang(r)
	slot := slotID(r.URL.Query().Get("slot"))
	c, ok := generatableSlot(p, slot)
	if !ok || s.imagegen == nil {
		http.NotFound(w, r)
		return
	}
	set, exists := p.PendingImages[slot]
	if exists && set.Status == "running" && !set.StartedAt.IsZero() && time.Since(set.StartedAt) > imageJobTimeout+time.Minute {
		s.failImageJob(p.ID, slot, set.JobID)
		set.Status = "failed"
	}
	switch {
	case exists && set.Status == "running":
		s.renderGen(w, r, "gen_running", map[string]any{"PID": p.ID, "Slug": slot})
	case exists && len(set.Keys) > 0:
		s.renderGen(w, r, "gen_candidates", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": set.Prompt, "Candidates": s.slotCandidates(r.Context(), p, slot)})
	case exists && set.Status == "failed":
		s.renderGen(w, r, "gen_prompt", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": set.Prompt, "Err": i18n.T(lang, "prj.gen.failed")})
	default:
		s.renderGen(w, r, "gen_prompt", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": defaultImagePrompt(p, c, lang)})
	}
}

// handlePickImage promotes a chosen candidate to the slot's asset (so the build
// uses it) and clears the pending set.
func (s *Server) handlePickImage(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	slot := slotID(r.FormValue("slot"))
	set, ok := p.PendingImages[slot]
	if !ok {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	idx, _ := strconv.Atoi(r.FormValue("index"))
	if idx < 0 || idx >= len(set.Keys) {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	key := set.Keys[idx]
	a := &project.Asset{
		ID: id.New(), ProjectID: p.ID, Key: key, Filename: path.Base(key),
		ContentType: "image/png", Description: slotLabel(p, slot) + " (AI-generated)",
		Slot: slot, Generated: true, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateAsset(r.Context(), a); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p, err := s.mutateProject(r.Context(), p.ID, func(fresh *project.Project) {
		// Do not clear a newer candidate set created in another tab while this
		// pick was in flight. The chosen asset is still valid and marks content
		// pending; only the set containing that key is consumed.
		if current, ok := fresh.PendingImages[slot]; ok {
			for _, candidateKey := range current.Keys {
				if candidateKey == key {
					delete(fresh.PendingImages, slot)
					break
				}
			}
		}
		markContentPending(fresh)
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// From the modal (htmx): tell the client to reload so the chosen image shows
	// in the slot and the dialog goes away. Full-nav fallback redirects.
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/projects/"+p.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleImproveImage queues an edit of the chosen image in the background.
func (s *Server) handleImproveImage(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	lang := s.lang(r)
	slot := slotID(r.FormValue("slot"))
	c, ok := generatableSlot(p, slot)
	if !ok || s.imagegen == nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	improveErr := func(key string) {
		if isHTMX(r) {
			s.renderGen(w, r, "gen_improve", map[string]any{"PID": p.ID, "Slug": slot, "Err": i18n.T(lang, key)})
			return
		}
		http.Redirect(w, r, "/projects/"+p.ID+"?genfail=1", http.StatusSeeOther)
	}
	instruction := strings.TrimSpace(r.FormValue("instruction"))
	if instruction == "" {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	if len(instruction) > 1000 {
		instruction = instruction[:1000]
	}
	instruction += " " + fmt.Sprintf(i18n.T(lang, "gen.prompt.improve_guard"), c.Name(lang)) + " " + imagePromptGuard(p, c, lang)
	// The image to improve: the slot's most recent generated asset.
	assets, err := s.store.AssetsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var srcKey string
	for _, a := range assets {
		if a.Slot == slot && a.Generated {
			srcKey = a.Key // last wins (assets are created-at ascending)
		}
	}
	if srcKey == "" {
		improveErr("prj.gen.failed")
		return
	}
	src, err := s.storage.Get(r.Context(), srcKey)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p, jobID, outcome, err := s.reserveImageJob(r.Context(), p.ID, slot, instruction)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if outcome == "limit" {
		improveErr("prj.gen.limit_note")
		return
	}
	if outcome == "started" {
		go s.runImageJob(p.ID, slot, jobID, instruction, func(ctx context.Context) ([][]byte, error) {
			return s.imagegen.Edit(ctx, src, instruction, 3)
		})
	}
	if isHTMX(r) {
		s.renderGen(w, r, "gen_running", map[string]any{"PID": p.ID, "Slug": slot})
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}
