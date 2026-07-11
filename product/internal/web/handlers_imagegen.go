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
	return s.store.UpdateProject(ctx, p)
}

// defaultImagePrompt seeds a generation prompt from the plan's design direction
// and the slot's purpose, so the customer usually needn't write one.
func defaultImagePrompt(p *project.Project, c project.ContentItem) string {
	var b strings.Builder
	b.WriteString(c.Name("en") + " for this website.")
	if brief := strings.TrimSpace(p.Brief); brief != "" {
		if len(brief) > 300 {
			brief = brief[:300]
		}
		b.WriteString(" Business: " + brief)
	}
	if p.DesignBrief != "" {
		b.WriteString(" Design direction: " + p.DesignBrief + ".")
	}
	b.WriteString(" Clean and professional, ready to place directly on the site, no watermark or text unless it is a logo wordmark.")
	return b.String()
}

// handleGenerateImage generates 3 candidate images for a generatable slot.
func (s *Server) handleGenerateImage(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	slot := slotID(r.FormValue("slot"))
	c, ok := generatableSlot(p, slot)
	if !ok || s.imagegen == nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		prompt = defaultImagePrompt(p, c)
	} else if len(prompt) > 1000 {
		prompt = prompt[:1000]
	}
	pngs, err := s.imagegen.Generate(r.Context(), prompt, 3)
	if err != nil {
		s.log.Error("imagegen generate", "err", err)
		http.Redirect(w, r, "/projects/"+p.ID+"?genfail=1", http.StatusSeeOther)
		return
	}
	if err := s.genCandidates(r.Context(), p, slot, prompt, pngs); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	if err := s.store.UpdateProject(r.Context(), p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	slot := slotID(r.FormValue("slot"))
	if _, ok := generatableSlot(p, slot); !ok || s.imagegen == nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
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
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
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
		http.Redirect(w, r, "/projects/"+p.ID+"?genfail=1", http.StatusSeeOther)
		return
	}
	if err := s.genCandidates(r.Context(), p, slot, instruction, pngs); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}
