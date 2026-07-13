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

	CanSubscribe    bool   // show the subscribe panel (billing on, unpaid, preview/accepted)
	SubActive       bool   // paid via stripe — show "active" + manage-subscription
	SubProcessing   bool   // returned from Checkout success while the webhook is still in flight
	PriceStr        string // formatted plan price ("99 kr"), "" if unavailable
	IncludedChanges int    // monthly changes included in the plan, for the subscribe "what's included" list

	// Bundle-a-domain chooser on the subscribe panel (Phase B): pick a domain
	// before paying, provisioned after. DomainAddonStr (below) shows the buy fee.
	DomainOffer    bool // show the domain chooser on the subscribe panel
	DomainOfferBuy bool // the "buy a domain" option is available in the chooser
	ShowAccept     bool // show the explicit accept step (paid/comped customers, or billing off — subscribing accepts implicitly otherwise)

	// Forge Pro change model (see changes.go). Paid subscribers request site
	// changes here: a monthly allowance is included, extra changes bill a flat fee.
	ShowChange  bool   // the paid change panel is visible (CanRequestChange)
	ChangesLeft int    // included changes remaining this month
	OverageStr  string // formatted flat price of an extra change ("49 kr")

	// Custom domain panel (see handlers_domains.go). Visible only to paying
	// customers when the feature is wired.
	ShowDomain     bool                   // the domain panel is visible
	DomainBuyable  bool                   // buying a domain in-app is available
	DomainStatus   string                 // "" | registering | pending_dns | verifying | active | failed
	DomainName     string                 // the attached/purchased hostname
	DomainKind     string                 // "byod" | "purchased"
	DomainRecords  []project.DomainRecord // DNS records to show (pending_dns/verifying)
	DomainAddonStr string                 // the flat monthly add-on price ("29 kr"), for the buy copy
	DomainFlash    string                 // in-panel feedback after an action (rendered inside #domain-panel)
	DomainFlashErr bool                   // the flash reports a failure (render it red, not as neutral progress)
}

// rosterMember is one team person for the template, with a presigned photo URL.
type rosterMember struct {
	Name, Role, Bio string
	PhotoKey        string
	PhotoURL        string // presigned, "" when no photo
}

// slotAssetView is one uploaded file shown under its content slot.
type slotAssetView struct {
	ID        string
	Filename  string
	URL       string // presigned
	Generated bool
	Caption   string // customer's label for pairing (which recipe/item), editable
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
		slotAssets[a.Slot] = append(slotAssets[a.Slot], slotAssetView{
			ID: a.ID, Filename: a.Filename, URL: presign(a.Key), Generated: a.Generated, Caption: a.Description})
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

	pv := projectView{
		Project: p, Iterations: its, Assets: general,
		Shots: s.withScreenshots(ctx, p).Shots, Status: s.statusOf(r, p),
		FilledSlots: filled, Rosters: rosters, SlotAssets: slotAssets, Candidates: candidates,
		MissingReq: missing, ImageGen: s.imagegen != nil, GenSlots: genSlots, GenPrompts: genPrompts,
		GenExhausted: exhausted, GenNotice: genNotice,
	}
	sub := r.URL.Query().Get("sub")
	// The explicit accept step only exists where subscribing can't stand in for
	// it: paid/comped customers (nothing left to buy) and billing-off installs.
	// For an unpaid customer with billing on, "Prenumerera & få min sida" IS the
	// accept — paying flips the project into Rasmus's review queue.
	pv.ShowAccept = p.CanAccept() && (p.Paid || s.billing == nil)
	// Forge Pro change model: a paying subscriber requests site changes here (the
	// unpaid preview-refinement panel below is gated on CanReiterate, which is
	// false once paid). A monthly allowance is included; extra changes bill a flat
	// fee, disclosed in the panel copy.
	// The flat overage price is disclosed wherever the change model is explained
	// (the subscribe panel and the change panel), so populate it regardless of
	// paid state.
	pv.OverageStr = formatPrice(int64(s.orch.OverageOre()), "sek")
	if p.Paid {
		pv.ShowChange = p.CanRequestChange()
		pv.ChangesLeft = p.ChangesLeft(time.Now().UTC(), s.orch.ChangesPerMonth())
	}
	if s.billing != nil {
		pv.SubActive = p.Paid && p.PaidVia == "stripe" && p.StripeCustomerID != ""
		pv.SubProcessing = !p.Paid && sub == "success" // paid on Stripe, webhook still in flight
		pv.CanSubscribe = !p.Paid && subscribable(p) && !pv.SubProcessing
		if pv.CanSubscribe || pv.SubProcessing {
			if pr, err := s.billing.Price(ctx, s.cfg.StripePriceID); err == nil {
				pv.PriceStr = formatPrice(pr.UnitAmount, pr.Currency)
			}
			pv.IncludedChanges = s.orch.ChangesPerMonth() // for the "what's included" list
		}
		// Bundle-a-domain chooser (Phase B): offer it right on the subscribe panel
		// so the customer picks a domain before paying; it's provisioned after.
		if pv.CanSubscribe && s.orch.DomainsEnabled() && domainSelectable(p) {
			pv.DomainOffer = true
			pv.DomainOfferBuy = s.orch.DomainBuyEnabled()
			if pv.DomainOfferBuy {
				pv.DomainAddonStr = s.domainAddonStr(ctx)
			}
		}
	}
	// Domain panel: paying customers only, when the feature is wired. Everything
	// it renders comes from the cached project fields — no live API call here.
	if s.orch.DomainsEnabled() && p.Paid {
		pv.ShowDomain = true
		pv.DomainBuyable = s.orch.DomainBuyEnabled()
		pv.DomainStatus = string(p.DomainStatus)
		pv.DomainName = p.DomainName
		pv.DomainKind = p.DomainKind
		pv.DomainRecords = p.DomainRecords
		// The flat monthly add-on price (same for every domain) — shown on the buy
		// panel so the customer knows what buying costs, without exposing our
		// per-domain wholesale cost. Only fetched when the buy panel is rendered.
		if pv.DomainBuyable && p.DomainStatus == project.DomainNone {
			pv.DomainAddonStr = s.domainAddonStr(ctx)
		}
		// Feedback after an action shows inside the panel (which is what htmx
		// swaps back in), not as a top-of-page banner that the swap wouldn't touch.
		if code := r.URL.Query().Get("domain"); code != "" {
			pv.DomainFlash = i18n.T(lang, domainFlashKey(code))
			pv.DomainFlashErr = domainFlashIsError(code)
		}
	}
	v := s.view(r, p.Name, pv)
	switch sub {
	case "success":
		v.Flash = i18n.T(lang, "flash.sub_success")
	case "cancel":
		v.Flash = i18n.T(lang, "flash.sub_cancel")
	case "error":
		v.Flash = i18n.T(lang, "flash.sub_error")
	case "domainbad":
		v.Flash = i18n.T(lang, "flash.domain_unavailable")
	}
	s.render(w, http.StatusOK, "project", v)
}

// domainFlashKey maps a ?domain=<code> redirect param to an i18n flash key.
func domainFlashKey(code string) string {
	switch code {
	case "attached":
		return "flash.domain_attached"
	case "buying":
		return "flash.domain_buying"
	case "checking":
		return "flash.domain_checking"
	case "invalid":
		return "flash.domain_invalid"
	case "toopricey":
		return "flash.domain_toopricey"
	case "unavailable":
		return "flash.domain_unavailable"
	case "exists":
		return "flash.domain_exists"
	default:
		return "flash.domain_error"
	}
}

// domainFlashIsError reports whether a ?domain= code is a failure (shown red)
// rather than progress/confirmation (attached/buying/checking, shown neutral).
func domainFlashIsError(code string) bool {
	switch code {
	case "attached", "buying", "checking":
		return false
	default:
		return true
	}
}

// formatPrice renders a Stripe unit amount (minor units) for display, e.g.
// (9900,"sek") → "99 kr". Empty for a non-positive amount.
// domainAddonStr returns the formatted flat monthly domain add-on price, or ""
// when billing/the price id isn't configured or the fetch fails. Shared by the
// subscribe chooser and the post-pay domain panel. A fetch error is logged so a
// misconfigured STRIPE_DOMAIN_PRICE_ID (e.g. a product id) is visible.
func (s *Server) domainAddonStr(ctx context.Context) string {
	if s.billing == nil || s.cfg.StripeDomainPriceID == "" {
		return ""
	}
	pr, err := s.billing.Price(ctx, s.cfg.StripeDomainPriceID)
	if err != nil {
		s.log.Warn("domain add-on price fetch failed", "price_id", s.cfg.StripeDomainPriceID, "err", err)
		return ""
	}
	return formatPrice(pr.UnitAmount, pr.Currency)
}

func formatPrice(unitAmount int64, currency string) string {
	if unitAmount <= 0 {
		return ""
	}
	major, minor := unitAmount/100, unitAmount%100
	if currency == "sek" {
		if minor == 0 {
			return fmt.Sprintf("%d kr", major)
		}
		return fmt.Sprintf("%d,%02d kr", major, minor)
	}
	sym := strings.ToUpper(currency)
	if minor == 0 {
		return fmt.Sprintf("%s %d", sym, major)
	}
	return fmt.Sprintf("%s %d.%02d", sym, major, minor)
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

// handleCaptionAsset labels one uploaded photo ("which recipe / what it shows")
// so the build pairs it to the right place. Editing a label after a build offers
// a rebuild, like any content change.
func (s *Server) handleCaptionAsset(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	assetID := r.PathValue("assetID")
	assets, err := s.store.AssetsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	owned := false
	for _, a := range assets {
		if a.ID == assetID {
			owned = true
			break
		}
	}
	if !owned { // the asset must belong to this customer's project
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	caption := strings.TrimSpace(r.FormValue("caption"))
	if len(caption) > 300 {
		caption = caption[:300]
	}
	if err := s.store.SetAssetDescription(r.Context(), assetID, caption); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
	if !p.CanChange() || prompt == "" || len(prompt) > maxBriefLen {
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
	if !p.ContentPending || !p.CanChange() {
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
	// Saved in place: reply with just the "✓ Saved" badge so the other text
	// fields the customer is still typing into aren't wiped by a full reload.
	if isHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "✓ %s", template.HTMLEscapeString(i18n.T(s.lang(r), "prj.content.saved")))
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
