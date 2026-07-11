package web

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, u *user.User) {
	projects, err := s.store.ProjectsByUser(r.Context(), u.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v := s.view(r, s.t(r, "nav.dashboard"), projects)
	switch {
	case r.URL.Query().Get("verified") != "":
		v.Flash = s.t(r, "flash.verified")
	case r.URL.Query().Get("verify_sent") != "":
		v.Flash = s.t(r, "flash.verify_sent")
	}
	s.render(w, http.StatusOK, "dashboard", v)
}

func (s *Server) handleNewProjectForm(w http.ResponseWriter, r *http.Request, _ *user.User) {
	s.render(w, http.StatusOK, "new_project", s.view(r, s.t(r, "new.h1"), nil))
}

// maxBriefLen caps customer-provided text fed into the pipeline (each build
// spends real money; unbounded input is both a cost and a prompt-size risk).
const maxBriefLen = 4000

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, u *user.User) {
	// Email must be confirmed before a project can be created — building spends
	// real money, so we don't let unverified addresses trigger it.
	if !u.Verified {
		v := s.view(r, s.t(r, "new.h1"), nil)
		v.Flash = s.t(r, "flash.verify_required")
		s.render(w, http.StatusForbidden, "new_project", v)
		return
	}
	brief := strings.TrimSpace(r.FormValue("brief"))
	name := strings.TrimSpace(r.FormValue("name"))
	if len(brief) < 10 {
		v := s.view(r, s.t(r, "new.h1"), nil)
		v.Flash = s.t(r, "flash.brief_short")
		s.render(w, http.StatusBadRequest, "new_project", v)
		return
	}
	if len(brief) > maxBriefLen {
		v := s.view(r, s.t(r, "new.h1"), nil)
		v.Flash = s.t(r, "flash.brief_long")
		s.render(w, http.StatusBadRequest, "new_project", v)
		return
	}
	if flash := s.quotaBlock(r, u); flash != "" {
		v := s.view(r, s.t(r, "new.h1"), nil)
		v.Flash = flash
		s.render(w, http.StatusTooManyRequests, "new_project", v)
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
		Locale:    s.lang(r), // the language their emails go out in
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
	design := resolveDesign(p, r.FormValue("design_choice"), r.FormValue("design_custom"))
	// Require answers when questions were asked; design-only submissions are
	// fine when intake had no questions.
	if p.Status != project.StatusNeedsInput || len(answers) > maxBriefLen ||
		(len(p.Questions) > 0 && answers == "") || (answers == "" && design == "") {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	s.orch.SubmitAnswers(p.ID, answers, design)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// resolveDesign turns the design form fields into the design brief: a picked
// suggestion (name + description, so the builder gets the full direction) or
// the customer's own words. Only offered options are accepted as picks.
func resolveDesign(p *project.Project, choice, custom string) string {
	choice = strings.TrimSpace(choice)
	custom = strings.TrimSpace(custom)
	if len(custom) > 500 {
		custom = custom[:500]
	}
	// Own words win whenever provided — customers often type without
	// re-selecting the radio.
	if custom != "" && (choice == "__custom" || choice == "") {
		return custom
	}
	for _, o := range p.DesignOptions {
		if o.Name == choice {
			return o.Name + " — " + o.Description
		}
	}
	if choice == "__custom" {
		return custom
	}
	return ""
}

// quotaBlock reports why a new build must not start right now ("" = go ahead):
// per-user daily cap, one in-flight pipeline per user, and a global concurrent
// build cap. Builds cost real money — these are the wallet's seatbelt.
func (s *Server) quotaBlock(r *http.Request, u *user.User) string {
	ctx := r.Context()

	projects, err := s.store.ProjectsByUser(ctx, u.ID)
	if err != nil {
		return s.t(r, "flash.error")
	}
	recent := 0
	for _, p := range projects {
		if time.Since(p.CreatedAt) < 24*time.Hour {
			recent++
		}
		switch p.Status {
		case project.StatusClarifying, project.StatusPlanning,
			project.StatusScreening, project.StatusBuilding:
			return s.t(r, "flash.one_at_a_time")
		}
	}
	// A cap of 0 means "not configured" (e.g. tests) — no limit.
	if s.cfg.MaxProjectsPerDay > 0 && recent >= s.cfg.MaxProjectsPerDay {
		return fmt.Sprintf(s.t(r, "flash.daily_limit"), s.cfg.MaxProjectsPerDay)
	}
	if s.cfg.MaxConcurrentBuilds > 0 {
		if active, err := s.store.ActiveIterations(ctx); err == nil && len(active) >= s.cfg.MaxConcurrentBuilds {
			return s.t(r, "flash.capacity")
		}
	}
	return ""
}

type projectView struct {
	Project      *project.Project
	Iterations   []*project.Iteration
	Assets       []*project.Asset            // general (un-slotted) uploads
	Shots        []reviewShot                // presigned page screenshots of the current build
	Status       statusView                  // the live status box (also re-rendered by the poll)
	FilledSlots  map[string]bool             // content-slot slug → provided (file/text/roster)
	Rosters      map[string][]rosterMember   // roster slug → its people (presigned photos)
	SlotAssets   map[string][]slotAssetView  // file/files slug → its uploaded assets (presigned)
	Candidates   map[string][]candidateImage // slot → pending AI-image candidates awaiting pick
	MissingReq   []string                    // localized names of required, unprovided content (for the approve gate)
	ImageGen     bool                        // "Generate with AI" is available
	GenSlots     map[string]bool             // slug → has a chosen AI-generated image (offer "improve")
	GenPrompts   map[string]string           // slug → the auto-seeded prompt (shown, editable)
	GenExhausted bool                        // the project has hit its AI-generation cap
	GenNotice    string                      // localized notice for a failed/blocked generation attempt
}

// rosterMember is one team person for the template, with a presigned photo URL.
type rosterMember struct {
	Name, Role, Bio string
	PhotoKey        string
	PhotoURL        string // presigned, "" when no photo
}

// slotAssetView is one uploaded file shown under its content slot.
type slotAssetView struct {
	Filename  string
	URL       string // presigned
	Generated bool
}

// candidateImage is one pending AI-generated image awaiting the customer's pick.
type candidateImage struct {
	Index int
	URL   string // presigned
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, u *user.User) {
	ctx := r.Context()
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	its, err := s.store.IterationsByProject(ctx, p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	assets, err := s.store.AssetsByProject(ctx, p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	presign := func(key string) string {
		if key == "" {
			return ""
		}
		if u, err := s.storage.PresignGet(ctx, key, 30*time.Minute); err == nil {
			return u
		}
		return ""
	}

	// Split assets: slot-tagged ones sit under their content item; the rest are
	// "other files". Slotted slots count as filled.
	filled := map[string]bool{}
	slotAssets := map[string][]slotAssetView{}
	genSlots := map[string]bool{}
	var general []*project.Asset
	for _, a := range assets {
		if a.Slot == "" {
			general = append(general, a)
			continue
		}
		filled[a.Slot] = true
		if a.Generated {
			genSlots[a.Slot] = true
		}
		slotAssets[a.Slot] = append(slotAssets[a.Slot], slotAssetView{Filename: a.Filename, URL: presign(a.Key), Generated: a.Generated})
	}
	// Text answers and roster people also fill their slots.
	for slug, v := range p.ContentAnswers {
		if v != "" {
			filled[slug] = true
		}
	}
	// For each roster slot, render its existing people plus a few blank rows to
	// add more. The row's index in this slice is its form index (name_<i>, …).
	rosters := map[string][]rosterMember{}
	for _, c := range p.Spec.ContentNeeded {
		if !c.IsRoster() {
			continue
		}
		entries := p.ContentRosters[c.Slug]
		if len(entries) > 0 {
			filled[c.Slug] = true
		}
		rows := make([]rosterMember, 0, len(entries)+3)
		for _, e := range entries {
			rows = append(rows, rosterMember{Name: e.Name, Role: e.Role, Bio: e.Bio, PhotoKey: e.PhotoKey, PhotoURL: presign(e.PhotoKey)})
		}
		for i := 0; i < 3 && len(rows) < maxRosterEntries; i++ {
			rows = append(rows, rosterMember{}) // blank rows for adding people
		}
		rosters[c.Slug] = rows
	}
	// Pending AI-image candidates awaiting a pick.
	candidates := map[string][]candidateImage{}
	for slug, set := range p.PendingImages {
		for i, key := range set.Keys {
			candidates[slug] = append(candidates[slug], candidateImage{Index: i, URL: presign(key)})
		}
	}

	// Required content the customer hasn't provided — surfaced at the approve
	// gate so they consciously choose "provide now" or "build with placeholders".
	lang := s.lang(r)
	var missing []string
	genPrompts := map[string]string{}
	for _, c := range p.Spec.ContentNeeded {
		if c.Required && !filled[c.Slug] {
			missing = append(missing, c.Name(lang))
		}
		if s.imagegen != nil && c.CanGenerate() {
			genPrompts[c.Slug] = defaultImagePrompt(p, c, lang) // shown pre-filled, editable
		}
	}

	exhausted := s.imageGenExhausted(p)
	genNotice := ""
	switch {
	case r.URL.Query().Get("genlimit") != "" || (exhausted && r.URL.Query().Get("genfail") != ""):
		genNotice = i18n.T(lang, "prj.gen.limit_note")
	case r.URL.Query().Get("genfail") != "":
		genNotice = i18n.T(lang, "prj.gen.failed")
	}

	s.render(w, http.StatusOK, "project", s.view(r, p.Name, projectView{
		Project: p, Iterations: its, Assets: general,
		Shots: s.withScreenshots(ctx, p).Shots, Status: s.statusOf(r, p),
		FilledSlots: filled, Rosters: rosters, SlotAssets: slotAssets, Candidates: candidates,
		MissingReq: missing, ImageGen: s.imagegen != nil, GenSlots: genSlots, GenPrompts: genPrompts,
		GenExhausted: exhausted, GenNotice: genNotice,
	}))
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

// handleUploadAsset stores customer-uploaded file(s) — one, or several at once
// for a gallery slot — in object storage and records their metadata.
func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(maxUpload + (1 << 20)); err != nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	// The customer's one-liner (general uploader) and the optional content-slot
	// this fills. For a named slot, its label doubles as the description.
	desc := strings.TrimSpace(r.FormValue("description"))
	if len(desc) > 300 {
		desc = desc[:300]
	}
	slot := slotID(r.FormValue("slot"))
	if slot != "" && desc == "" {
		desc = slotLabel(p, slot)
	}

	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	for _, hdr := range files {
		if _, err := s.storeUpload(r.Context(), p.ID, hdr, slot, desc, false); err != nil {
			s.log.Error("asset upload", "err", err)
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
	}
	if markContentPending(p); p.ContentPending {
		if err := s.store.UpdateProject(r.Context(), p); err != nil {
			s.log.Error("flag content pending", "project", p.ID, "err", err)
		}
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// storeUpload validates and stores one uploaded file as a project asset,
// returning its object-storage key (or "" if skipped as disallowed/oversized).
func (s *Server) storeUpload(ctx context.Context, projectID string, hdr *multipart.FileHeader, slot, desc string, generated bool) (string, error) {
	ct := hdr.Header.Get("Content-Type")
	if !allowedAssetTypes[ct] || hdr.Size > maxUpload {
		return "", nil // silently skip a disallowed/oversized file among a batch
	}
	file, err := hdr.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	filename := sanitizeFilename(hdr.Filename)
	key := fmt.Sprintf("projects/%s/assets/%s", projectID, filename)
	if err := s.storage.Put(ctx, key, ct, file, hdr.Size); err != nil {
		return "", err
	}
	return key, s.store.CreateAsset(ctx, &project.Asset{
		ID: id.New(), ProjectID: projectID, Key: key, Filename: filename,
		ContentType: ct, Description: desc, Slot: slot, Generated: generated, Size: hdr.Size, CreatedAt: time.Now().UTC(),
	})
}

// markContentPending flags (in memory — the caller saves) that the customer
// changed content after a build already ran, so the project page can offer a
// rebuild that applies it. No-op before the first build: that content flows into
// the first build normally.
func markContentPending(p *project.Project) {
	if p.IterationsUsed > 0 {
		p.ContentPending = true
	}
}

// maxRosterEntries caps how many people a roster slot renders/accepts.
const maxRosterEntries = 12

// handleRoster saves the structured people for a roster-kind content slot — a
// team, where each person's name, role, bio AND photo are kept together in one
// entry so the build never has to guess which face goes with which bio. Rows
// with no name are dropped; a row's existing photo is preserved unless replaced.
func (s *Server) handleRoster(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(maxUpload + (1 << 20)); err != nil {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	slot := slotID(r.FormValue("slot"))
	if !isRosterSlot(p, slot) {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	clip := func(s string, n int) string {
		s = strings.TrimSpace(s)
		if len(s) > n {
			s = s[:n]
		}
		return s
	}
	var entries []project.RosterEntry
	for i := 0; i < maxRosterEntries; i++ {
		name := clip(r.FormValue(fmt.Sprintf("name_%d", i)), 120)
		if name == "" {
			continue // a person is defined by having a name
		}
		e := project.RosterEntry{
			Name:     name,
			Role:     clip(r.FormValue(fmt.Sprintf("role_%d", i)), 120),
			Bio:      clip(r.FormValue(fmt.Sprintf("bio_%d", i)), 600),
			PhotoKey: r.FormValue(fmt.Sprintf("photokey_%d", i)), // preserve existing
		}
		// A newly uploaded photo replaces it — stored as an asset (so the build
		// gets the file) tagged to this slot and described with the person.
		if fhs := r.MultipartForm.File[fmt.Sprintf("photo_%d", i)]; len(fhs) > 0 {
			if key, err := s.storeUpload(r.Context(), p.ID, fhs[0], slot, name, false); err == nil && key != "" {
				e.PhotoKey = key
			}
		}
		entries = append(entries, e)
	}
	if p.ContentRosters == nil {
		p.ContentRosters = map[string][]project.RosterEntry{}
	}
	if len(entries) == 0 {
		delete(p.ContentRosters, slot)
	} else {
		p.ContentRosters[slot] = entries
	}
	markContentPending(p)
	if err := s.store.UpdateProject(r.Context(), p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// isRosterSlot reports whether slot is a roster-kind content item in the plan.
func isRosterSlot(p *project.Project, slot string) bool {
	for _, c := range p.Spec.ContentNeeded {
		if c.Slug == slot {
			return c.IsRoster()
		}
	}
	return false
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
	if err := s.tmpl.ExecuteTemplate(w, "project_status", s.statusOf(r, p)); err != nil {
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
	if !p.CanReiterate() || prompt == "" || len(prompt) > maxBriefLen {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	// A reiteration spawns a build too — respect the global capacity cap.
	if s.cfg.MaxConcurrentBuilds > 0 {
		if active, err := s.store.ActiveIterations(r.Context()); err == nil && len(active) >= s.cfg.MaxConcurrentBuilds {
			http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
			return
		}
	}
	s.orch.Reiterate(p.ID, prompt)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// applyContentPrompt directs a reiteration to fold in content the customer
// provided after the last build. The assets/text/roster reach the agent through
// AssetNotes; this just tells it to actually use them.
const applyContentPrompt = "The customer has provided new content since the last build — uploaded logo/photos, AI-generated images, text, and/or team info (see the attached assets and notes). Incorporate it: place each asset where it belongs (match by filename and description), replace any placeholder or stand-in imagery and copy with the real content, and fill in the text they supplied. Keep the existing design, layout and structure — this is a content update, not a redesign. Then verify and redeploy."

// handleApplyContent rebuilds the site to fold in content the customer added
// after the last build. It counts as a change (consumes one reiteration).
func (s *Server) handleApplyContent(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if !p.ContentPending || !p.CanReiterate() {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	// Like any reiteration this spawns a build — respect the global capacity cap.
	if s.cfg.MaxConcurrentBuilds > 0 {
		if active, err := s.store.ActiveIterations(r.Context()); err == nil && len(active) >= s.cfg.MaxConcurrentBuilds {
			http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
			return
		}
	}
	s.orch.Reiterate(p.ID, applyContentPrompt)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleRetry re-runs a failed build (e.g. an agent error, or a build
// interrupted by a crash or deploy). It consumes no change credit.
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if !p.CanRetry() {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	// A retry spawns a build too — respect the global capacity cap.
	if s.cfg.MaxConcurrentBuilds > 0 {
		if active, err := s.store.ActiveIterations(r.Context()); err == nil && len(active) >= s.cfg.MaxConcurrentBuilds {
			http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
			return
		}
	}
	s.orch.RetryBuild(p.ID)
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleApprovePlan is the customer approving the scope card, which starts the
// build. Only valid at the awaiting_approval gate.
func (s *Server) handleApprovePlan(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if p.CanApprovePlan() {
		s.orch.ApprovePlan(p.ID)
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// slotSanitize keeps a slot id to a safe slug.
var slotSanitize = regexp.MustCompile(`[^a-z0-9_-]+`)

// slotID normalizes a submitted content-slot id.
func slotID(s string) string {
	s = slotSanitize.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// slotLabel returns the plan's English label for a content slot (a stable
// description for the build agent), or the slug.
func slotLabel(p *project.Project, slot string) string {
	for _, c := range p.Spec.ContentNeeded {
		if c.Slug == slot {
			return c.Name("en")
		}
	}
	return slot
}

// textSlot returns the content item for a text-kind slot, or false — so the
// handler only accepts answers for slots the plan actually asked to be typed.
func textSlot(p *project.Project, slot string) (project.ContentItem, bool) {
	for _, c := range p.Spec.ContentNeeded {
		if c.Slug == slot && c.IsText() {
			return c, true
		}
	}
	return project.ContentItem{}, false
}

// handleContentAnswer saves the customer's typed value for a text-kind content
// slot (a contact email, opening hours, copy) — the counterpart to uploading a
// file for a file-kind slot.
func (s *Server) handleContentAnswer(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	slot := slotID(r.FormValue("slot"))
	if _, ok := textSlot(p, slot); !ok {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	value := strings.TrimSpace(r.FormValue("value"))
	if len(value) > 2000 {
		value = value[:2000]
	}
	if p.ContentAnswers == nil {
		p.ContentAnswers = map[string]string{}
	}
	if value == "" {
		delete(p.ContentAnswers, slot) // clearing the field un-fills the slot
	} else {
		p.ContentAnswers[slot] = value
	}
	markContentPending(p)
	if err := s.store.UpdateProject(r.Context(), p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

// handleAccept records the customer accepting the preview, sending it to
// Rasmus's final-review queue.
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if p.CanAccept() {
		if err := s.orch.AcceptPreview(p.ID); err != nil {
			s.log.Error("accept preview", "err", err)
		}
	}
	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}
