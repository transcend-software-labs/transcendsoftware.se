package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// adminView is the data for the operator dashboard.
type adminView struct {
	Escalated []*project.Project
	Accepted  []reviewItem  // accepted by the customer, awaiting delivery review
	Waiting   []waitingItem // customer's turn (answering questions / approving the plan)
	Active    []activeBuild
	Previews  []reviewItem // live preview apps (cost money; can be destroyed)
	Stats     buildStats
	Recent    []recentBuild // recent builds with cost/timing
}

// waitingItem is a project sitting on the customer (needs input / plan approval).
type waitingItem struct {
	ID         string
	Name       string
	Status     project.Status
	OwnerEmail string
	Since      time.Time // last update, so the operator sees how long it's been idle
}

// recentBuild is one build's cost + timing line for /admin.
type recentBuild struct {
	Project  string
	When     time.Time
	Duration string // "4m12s"
	Tokens   int
	CostStr  string // rough "$0.007"
	Model    string // impl model that ran the build (model experiments)
	Status   project.Status
}

// reviewItem is a project plus short-lived presigned URLs for its per-page
// preview screenshots, for visual review in /admin.
type reviewItem struct {
	*project.Project
	Shots []reviewShot
}

// reviewShot is one page screenshot. URL points at the app's shot handler
// (not a presigned link) so it never expires — the handler presigns fresh on
// every request. Fixes "Request has expired" when a page sits open past the
// presign TTL and the customer/operator clicks a thumbnail.
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

// withScreenshots attaches per-page shot URLs that route through handleShot,
// which presigns on demand — so open pages and bookmarks never break.
func (s *Server) withScreenshots(_ context.Context, p *project.Project) reviewItem {
	item := reviewItem{Project: p}
	for i, sc := range p.Screenshots {
		item.Shots = append(item.Shots, reviewShot{Path: sc.Path, URL: fmt.Sprintf("/projects/%s/shots/%d", p.ID, i)})
	}
	return item
}

// handleShot redirects to a freshly presigned URL for one of a project's page
// screenshots. Viewable by the project's owner or an operator. Because the
// redirect is minted per request, the image link never expires.
func (s *Server) handleShot(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if p.UserID != u.ID && !s.isAdmin(u) {
		http.NotFound(w, r) // don't reveal the project exists
		return
	}
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil || i < 0 || i >= len(p.Screenshots) {
		http.NotFound(w, r)
		return
	}
	url, err := s.storage.PresignGet(r.Context(), p.Screenshots[i].Key, 10*time.Minute)
	if err != nil {
		http.Error(w, "unavailable", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
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
	var waiting []waitingItem
	for _, p := range all {
		names[p.ID] = p.Name
		switch p.Status {
		case project.StatusPreviewReady:
			previews = append(previews, s.withScreenshots(ctx, p))
		case project.StatusAccepted:
			accepted = append(accepted, s.withScreenshots(ctx, p))
		case project.StatusNeedsInput, project.StatusAwaitingApproval:
			// The customer's turn — the operator can't act, but seeing these
			// lets Rasmus nudge a stalled project instead of it going quiet.
			owner := ""
			if u, err := s.store.UserByID(ctx, p.UserID); err == nil {
				owner = u.Email
			}
			waiting = append(waiting, waitingItem{
				ID: p.ID, Name: p.Name, Status: p.Status,
				Since: p.UpdatedAt, OwnerEmail: owner,
			})
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
			Tokens: it.Tokens, CostStr: estCost(it.Duration(), rate), Model: it.ImplModel, Status: it.Status,
		})
	}

	v := s.view(r, "Operator review", adminView{
		Escalated: escalated, Accepted: accepted, Waiting: waiting, Active: active, Previews: previews,
		Stats: computeStats(recent, rate), Recent: builds,
	})
	v.Lang = "en" // operator pages are English regardless of the customer-facing selector
	if r.URL.Query().Get("err") == "unpaid" {
		v.Flash = "That project isn’t marked paid yet — mark it paid to enable delivery."
	}
	s.render(w, http.StatusOK, "admin", v)
}

// adminProjectView backs the operator's technical view of one project.
type adminProjectView struct {
	Item       reviewItem
	Iterations []adminBuildRow
	OwnerEmail string

	// Per-build model selection (experiment): the enabled profiles for the
	// dropdowns and the project's current choice (defaults when unset).
	Profiles       []config.ModelProfile
	PlannerProfile string
	ImplProfile    string
}

// adminBuildRow is one iteration plus its rough LLM cost (from the token split
// × the project's implementation-profile prices).
type adminBuildRow struct {
	*project.Iteration
	CostStr string
}

// handleAdminProject is the operator's technical view of one project: raw plan,
// live build stream, full iteration logs, machine ids, audit and critique —
// everything the customer page deliberately no longer shows.
func (s *Server) handleAdminProject(w http.ResponseWriter, r *http.Request, _ *user.User) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	its, err := s.store.IterationsByProject(r.Context(), p.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	owner := ""
	if u, err := s.store.UserByID(r.Context(), p.UserID); err == nil {
		owner = u.Email
	}
	// Price each iteration by the project's implementation profile (experiments
	// run one combo per project, so this is accurate).
	implKey := p.ImplProfile
	if implKey == "" {
		implKey = s.cfg.DefaultImplProfile
	}
	implModel, _ := s.cfg.ResolveModel(implKey)
	rows := make([]adminBuildRow, 0, len(its))
	for _, it := range its {
		cost := ""
		if it.Tokens > 0 {
			cost = formatPrice(int64(implModel.CostOre(it.TokensInput, it.Tokens)), "sek")
		}
		rows = append(rows, adminBuildRow{Iteration: it, CostStr: cost})
	}
	plannerKey := p.PlannerProfile
	if plannerKey == "" {
		plannerKey = s.cfg.DefaultPlannerProfile
	}
	v := s.view(r, p.Name+" — operator", adminProjectView{
		Item: s.withScreenshots(r.Context(), p), Iterations: rows, OwnerEmail: owner,
		Profiles: s.cfg.ModelProfiles(), PlannerProfile: plannerKey, ImplProfile: implKey})
	v.Lang = "en" // operator pages are English regardless of the customer-facing selector
	s.render(w, http.StatusOK, "admin_project", v)
}

// handleAdminBuildModels sets the project's planner + implementation profiles
// and re-runs the pipeline with them (operator experiment).
func (s *Server) handleAdminBuildModels(w http.ResponseWriter, r *http.Request, _ *user.User) {
	id := r.PathValue("id")
	planner := r.FormValue("planner_profile")
	impl := r.FormValue("impl_profile")
	// Only accept keys that are actually enabled.
	valid := map[string]bool{}
	for _, pr := range s.cfg.ModelProfiles() {
		valid[pr.Key] = true
	}
	if !valid[planner] {
		planner = ""
	}
	if !valid[impl] {
		impl = ""
	}
	s.orch.RebuildWithModels(id, planner, impl)
	http.Redirect(w, r, "/admin/projects/"+id, http.StatusSeeOther)
}

// handleAdminDestroyPreview tears down a project's preview app immediately.
func (s *Server) handleAdminDestroyPreview(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.DestroyPreview(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminDeleteProject fully removes one project and its preview env.
// (requireAdmin already enforces admin auth + CSRF on this POST.)
func (s *Server) handleAdminDeleteProject(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.PurgeProject(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminPurgeAll removes every project and its preview env — the operator's
// clean-the-slate action. Confirmed in the UI; requireAdmin guards auth + CSRF.
func (s *Server) handleAdminPurgeAll(w http.ResponseWriter, r *http.Request, _ *user.User) {
	n, err := s.orch.PurgeAllProjects(r.Context())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Info("admin purge all", "purged", n)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminDeliver completes the handover: Rasmus has reviewed + guaranteed
// an accepted project. Refused until the project is paid.
func (s *Server) handleAdminDeliver(w http.ResponseWriter, r *http.Request, _ *user.User) {
	err := s.orch.DeliverProject(r.PathValue("id"))
	if errors.Is(err, orchestrator.ErrNotPaid) {
		http.Redirect(w, r, "/admin?err=unpaid", http.StatusSeeOther)
		return
	}
	if err != nil {
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

// handleAdminMarkPaid records a manual payment (comps for Rasmus + friends; the
// same choke-point a Stripe webhook will hit). handleAdminMarkUnpaid reverses a
// mistaken mark. Both return to wherever the operator clicked from.
func (s *Server) handleAdminMarkPaid(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.MarkPaid(r.PathValue("id"), "manual"); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, adminBackTo(r), http.StatusSeeOther)
}

func (s *Server) handleAdminMarkUnpaid(w http.ResponseWriter, r *http.Request, _ *user.User) {
	if err := s.orch.MarkUnpaid(r.PathValue("id")); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, adminBackTo(r), http.StatusSeeOther)
}

// adminBackTo returns the operator to the page they acted from (the project
// detail page or the dashboard), defaulting to /admin.
func adminBackTo(r *http.Request) string {
	if ref := r.Referer(); strings.Contains(ref, "/admin/projects/") {
		return "/admin/projects/" + r.PathValue("id")
	}
	return "/admin"
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
