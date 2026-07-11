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

// genCandidates stores n generated PNGs under a slot's gen prefix and records
// them as the slot's pending candidates awaiting the customer's pick.
func (s *Server) genCandidates(ctx context.Context, p *project.Project, slot, prompt string, pngs [][]byte) error {
	keys := make([]string, 0, len(pngs))
	for _, png := range pngs {
		key := fmt.Sprintf("projects/%s/gen/%s/%s.png", p.ID, slot, id.New())
		if err := s.storage.Put(ctx, key, "image/png", bytes.NewReader(png), int64(len(png))); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	if p.PendingImages == nil {
		p.PendingImages = map[string]project.ImageCandidates{}
	}
	p.PendingImages[slot] = project.ImageCandidates{Prompt: prompt, Keys: keys}
	p.ImageGenCount++ // each generate/improve is one paid API call; count it toward the cap
	return s.store.UpdateProject(ctx, p)
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
	return b.String()
}

// handleGenerateImage generates 3 candidate images for a generatable slot.
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
	} else if len(prompt) > 1000 {
		prompt = prompt[:1000]
	}
	if s.imageGenExhausted(p) {
		if isHTMX(r) {
			s.renderGen(w, r, "gen_prompt", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": prompt, "Err": i18n.T(lang, "prj.gen.limit_note")})
			return
		}
		http.Redirect(w, r, "/projects/"+p.ID+"?genlimit=1", http.StatusSeeOther)
		return
	}
	pngs, err := s.imagegen.Generate(r.Context(), prompt, 3)
	if err != nil {
		s.log.Error("imagegen generate", "err", err)
		if isHTMX(r) {
			s.renderGen(w, r, "gen_prompt", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": prompt, "Err": i18n.T(lang, "prj.gen.failed")})
			return
		}
		http.Redirect(w, r, "/projects/"+p.ID+"?genfail=1", http.StatusSeeOther)
		return
	}
	if err := s.genCandidates(r.Context(), p, slot, prompt, pngs); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		s.renderGen(w, r, "gen_candidates", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": prompt, "Candidates": s.slotCandidates(r.Context(), p, slot)})
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
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
	delete(p.PendingImages, slot)
	markContentPending(p) // a picked image changes what the build should use
	if err := s.store.UpdateProject(r.Context(), p); err != nil {
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

// handleImproveImage edits the slot's currently chosen image with the
// customer's instruction, producing a fresh set of candidates to pick from.
func (s *Server) handleImproveImage(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	lang := s.lang(r)
	slot := slotID(r.FormValue("slot"))
	if _, ok := generatableSlot(p, slot); !ok || s.imagegen == nil {
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
	if s.imageGenExhausted(p) {
		if isHTMX(r) {
			s.renderGen(w, r, "gen_improve", map[string]any{"PID": p.ID, "Slug": slot, "Err": i18n.T(lang, "prj.gen.limit_note")})
			return
		}
		http.Redirect(w, r, "/projects/"+p.ID+"?genlimit=1", http.StatusSeeOther)
		return
	}
	instruction := strings.TrimSpace(r.FormValue("instruction"))
	if instruction == "" {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	if len(instruction) > 1000 {
		instruction = instruction[:1000]
	}
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
	pngs, err := s.imagegen.Edit(r.Context(), src, instruction, 3)
	if err != nil {
		s.log.Error("imagegen edit", "err", err)
		improveErr("prj.gen.failed")
		return
	}
	if err := s.genCandidates(r.Context(), p, slot, instruction, pngs); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if isHTMX(r) {
		s.renderGen(w, r, "gen_candidates", map[string]any{"PID": p.ID, "Slug": slot, "Prompt": instruction, "Candidates": s.slotCandidates(r.Context(), p, slot)})
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}
