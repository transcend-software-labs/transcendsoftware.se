// Package web is the HTTP layer: the public landing page, auth, and the
// logged-in dashboard where customers start and watch projects.
package web

import (
	"crypto/subtle"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/imagegen"
	"github.com/transcend-software-labs/rasmus-ai/internal/notify"
	"github.com/transcend-software-labs/rasmus-ai/internal/oauth"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server holds the HTTP dependencies.
type Server struct {
	cfg      config.Config
	store    store.Store
	sessions *auth.Sessions
	orch     *orchestrator.Orchestrator
	broker   *stream.Broker
	storage  storage.Store
	oauth    *oauth.Registry  // social login (nil → none configured)
	notifier notify.Notifier  // for magic-link emails
	imagegen *imagegen.Client // "Generate with AI" for image slots (nil = disabled)
	billing  *billing.Client  // Stripe subscription paywall (nil = disabled)
	tmpl     *template.Template
	log      *slog.Logger
}

// NewServer wires the HTTP server.
func NewServer(cfg config.Config, st store.Store, sessions *auth.Sessions, orch *orchestrator.Orchestrator, broker *stream.Broker, assets storage.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, sessions: sessions, orch: orch, broker: broker,
		storage: assets, oauth: oauth.NewRegistry(), notifier: notify.Noop{}, tmpl: tmpl, log: log}, nil
}

// SetImageGen enables "Generate with AI" for image content slots.
func (s *Server) SetImageGen(c *imagegen.Client) { s.imagegen = c }

// SetBilling enables the Stripe subscription paywall.
func (s *Server) SetBilling(c *billing.Client) { s.billing = c }

// SetAuth wires social login and the notifier used for magic-link emails.
func (s *Server) SetAuth(reg *oauth.Registry, notifier notify.Notifier) {
	if reg != nil {
		s.oauth = reg
	}
	if notifier != nil {
		s.notifier = notifier
	}
}

// maxUpload caps a single asset upload.
const maxUpload = 10 << 20 // 10 MB

// limitBody caps the request body size before downstream parsing.
func limitBody(n int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next(w, r)
	}
}

// Handler returns the configured router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /signup", s.handleSignupForm)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Passwordless + social login.
	mux.HandleFunc("POST /auth/magic", s.handleMagicRequest)
	mux.HandleFunc("GET /auth/magic", s.handleMagicConsume)
	mux.HandleFunc("GET /verify", s.handleVerify)
	mux.HandleFunc("POST /verify/resend", s.requireUser(s.handleResendVerification))
	mux.HandleFunc("GET /auth/{provider}", s.handleOAuthStart)
	mux.HandleFunc("GET /auth/{provider}/callback", s.handleOAuthCallback)

	mux.HandleFunc("GET /dashboard", s.requireUser(s.handleDashboard))
	mux.HandleFunc("GET /projects/new", s.requireUser(s.handleNewProjectForm))
	mux.HandleFunc("POST /projects", s.requireUser(s.handleCreateProject))
	mux.HandleFunc("GET /projects/{id}", s.requireUser(s.handleProject))
	mux.HandleFunc("GET /projects/{id}/status", s.requireUser(s.handleProjectStatus))
	mux.HandleFunc("GET /projects/{id}/stream", s.requireUser(s.handleProjectStream))
	mux.HandleFunc("POST /projects/{id}/answer", s.requireUser(s.handleAnswer))
	mux.HandleFunc("POST /projects/{id}/approve-plan", s.requireUser(s.handleApprovePlan))
	mux.HandleFunc("POST /projects/{id}/assets", limitBody(maxUpload+(1<<20), s.requireUser(s.handleUploadAsset)))
	mux.HandleFunc("POST /projects/{id}/assets/{assetID}/caption", s.requireUser(s.handleCaptionAsset))
	mux.HandleFunc("POST /projects/{id}/content", s.requireUser(s.handleContentAnswer))
	mux.HandleFunc("POST /projects/{id}/roster", s.requireUser(s.handleRoster))
	mux.HandleFunc("POST /projects/{id}/content/generate", s.requireUser(s.handleGenerateImage))
	mux.HandleFunc("POST /projects/{id}/content/pick", s.requireUser(s.handlePickImage))
	mux.HandleFunc("POST /projects/{id}/content/improve", s.requireUser(s.handleImproveImage))
	mux.HandleFunc("POST /projects/{id}/reiterate", s.requireUser(s.handleReiterate))
	mux.HandleFunc("POST /projects/{id}/apply-content", s.requireUser(s.handleApplyContent))
	mux.HandleFunc("POST /projects/{id}/retry", s.requireUser(s.handleRetry))
	mux.HandleFunc("POST /projects/{id}/accept", s.requireUser(s.handleAccept))
	mux.HandleFunc("POST /projects/{id}/subscribe", s.requireUser(s.handleSubscribe))
	mux.HandleFunc("POST /projects/{id}/billing", s.requireUser(s.handleBillingPortal))

	// Stripe webhook — no session/CSRF (Stripe can't have either; the signature
	// is the authentication). Bare handler like the magic-link route.
	mux.HandleFunc("POST /webhooks/stripe", limitBody(1<<20, s.handleStripeWebhook))
	mux.HandleFunc("GET /terms", s.handleTerms)

	// Operator/admin views (gated by ADMIN_EMAIL).
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("GET /admin/projects/{id}", s.requireAdmin(s.handleAdminProject))
	mux.HandleFunc("POST /admin/projects/{id}/approve", s.requireAdmin(s.handleAdminApprove))
	mux.HandleFunc("POST /admin/projects/{id}/reject", s.requireAdmin(s.handleAdminReject))
	mux.HandleFunc("POST /admin/projects/{id}/destroy-preview", s.requireAdmin(s.handleAdminDestroyPreview))
	mux.HandleFunc("POST /admin/projects/{id}/delete", s.requireAdmin(s.handleAdminDeleteProject))
	mux.HandleFunc("POST /admin/purge-all", s.requireAdmin(s.handleAdminPurgeAll))
	mux.HandleFunc("POST /admin/projects/{id}/deliver", s.requireAdmin(s.handleAdminDeliver))
	mux.HandleFunc("POST /admin/projects/{id}/mark-paid", s.requireAdmin(s.handleAdminMarkPaid))
	mux.HandleFunc("POST /admin/projects/{id}/mark-unpaid", s.requireAdmin(s.handleAdminMarkUnpaid))
	mux.HandleFunc("POST /admin/projects/{id}/return", s.requireAdmin(s.handleAdminReturn))

	return logRequests(s.log, langSelector(mux))
}

// langSelector turns ?lang=xx on any GET into a persistent cookie choice and
// redirects back to the same URL without the parameter, so the footer switcher
// works on every page with zero per-handler code.
func langSelector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l := r.URL.Query().Get("lang"); l != "" && r.Method == http.MethodGet {
			if i18n.Supported(l) {
				http.SetCookie(w, &http.Cookie{Name: langCookie, Value: l, Path: "/",
					MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode})
			}
			q := r.URL.Query()
			q.Del("lang")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// templateFuncs is the single FuncMap for the template set (NewServer and the
// template-render tests parse with the same one).
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"statusLabel":   statusLabel, // admin pages: always English
		"trStatus":      trStatus,    // customer pages: localized status
		"tr":            i18n.T,      // for fragments rendered without a View
		"timelineSteps": func() []string { return project.TimelineSteps },
		"polling":       polling,
		"pollEvery":     pollEvery,
		"dur":           func(d time.Duration) string { return d.Round(time.Second).String() },
		// dict builds a map so a fragment can be rendered both inline (from a page
		// template) and standalone (from an htmx handler) with the same shape.
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					m[k] = kv[i+1]
				}
			}
			return m
		},
	}
}

// View is the data passed to every page template.
type View struct {
	Title      string
	User       *user.User
	IsAdmin    bool
	CSRF       string
	Flash      string
	Lang       string // resolved UI language ("en", "sv", "ru")
	Data       any
	Providers  []oauth.Provider // social-login buttons on auth pages
	MagicLink  bool             // advertise passwordless email login
	Unverified bool             // logged in but email not yet confirmed → show the verify banner
}

// T translates a catalog key into the view's language (templates: {{.T "nav.login"}}).
func (v View) T(key string) string { return i18n.T(v.Lang, key) }

// Languages lists the selectable UI languages for the footer switcher.
func (v View) Languages() []i18n.Lang { return i18n.Langs }

// statusView carries a project plus the UI language into the "project_status"
// fragment, which is also rendered standalone by the htmx status poll and so
// can't rely on a surrounding View.
type statusView struct {
	*project.Project
	Lang     string
	Activity string // language-neutral activity code of a running build ("" = none)
	Billing  bool   // Stripe paywall on — the accepted-state note nudges payment when unpaid
}

// statusOf assembles the live status box for a project: resolved language, the
// running build's activity code, and the page checklist. Shared by the full
// project page and the htmx status poll so both render identically.
func (s *Server) statusOf(r *http.Request, p *project.Project) statusView {
	lang := s.lang(r)
	return statusView{Project: p, Lang: lang, Activity: s.orch.Activity(p.ID), Billing: s.billing != nil}
}

// trStatus is statusLabel's localized sibling; unknown statuses fall back to
// the English label rather than leaking a raw catalog key.
func trStatus(lang string, s project.Status) string {
	key := "status." + string(s)
	if v := i18n.T(lang, key); v != key {
		return v
	}
	return statusLabel(s)
}

const langCookie = "forge_lang"

// lang resolves the UI language: explicit choice (cookie) wins, then the
// browser's Accept-Language, then English.
func (s *Server) lang(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && i18n.Supported(c.Value) {
		return c.Value
	}
	return i18n.FromAcceptLanguage(r.Header.Get("Accept-Language"))
}

// t translates a catalog key into the request's language — for strings built
// in handlers (flashes, titles) rather than templates.
func (s *Server) t(r *http.Request, key string) string { return i18n.T(s.lang(r), key) }

func (s *Server) view(r *http.Request, title string, data any) View {
	u := s.currentUser(r)
	return View{Title: title, User: u, IsAdmin: s.isAdmin(u), CSRF: s.csrfToken(r), Lang: s.lang(r),
		Data: data, Providers: s.oauth.Enabled(), MagicLink: s.cfg.MagicLinkEnabled,
		Unverified: u != nil && !u.Verified}
}

// authView builds a View for the login/signup pages with a flash message,
// keeping the social-login providers and magic-link availability so error
// re-renders still show every login method.
func (s *Server) authView(r *http.Request, title, flash string) View {
	v := s.view(r, title, nil)
	v.Flash = flash
	return v
}

// csrfToken returns the CSRF token bound to the request's session, or "".
func (s *Server) csrfToken(r *http.Request) string {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return ""
	}
	tok, ok := s.sessions.CSRF(r.Context(), c.Value)
	if !ok {
		return ""
	}
	return tok
}

// checkCSRF validates the form token against the session's CSRF token.
func (s *Server) checkCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	got := r.FormValue("csrf_token")
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func (s *Server) render(w http.ResponseWriter, status int, name string, v View) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

// currentUser returns the logged-in user, or nil.
func (s *Server) currentUser(r *http.Request) *user.User {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return nil
	}
	uid, ok := s.sessions.UserID(r.Context(), c.Value)
	if !ok {
		return nil
	}
	u, err := s.store.UserByID(r.Context(), uid)
	if err != nil {
		return nil
	}
	return u
}

type authedHandler func(w http.ResponseWriter, r *http.Request, u *user.User)

func (s *Server) requireUser(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !s.checkCSRF(r) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r, u)
	}
}

// isAdmin reports whether u is the configured operator.
func (s *Server) isAdmin(u *user.User) bool {
	return u != nil && s.cfg.AdminEmail != "" && u.Email == s.cfg.AdminEmail
}

func (s *Server) requireAdmin(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !s.isAdmin(u) {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && !s.checkCSRF(r) {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r, u)
	}
}

// ownedProject loads a project and verifies the user may open it: its owner, or
// the operator (admin), who reviews every project and links to them from the
// admin dashboard. 404 (not 403) so a non-owner can't probe which IDs exist.
func (s *Server) ownedProject(w http.ResponseWriter, r *http.Request, u *user.User) (*project.Project, bool) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil || (p.UserID != u.ID && !s.isAdmin(u)) {
		http.NotFound(w, r)
		return nil, false
	}
	return p, true
}

func statusLabel(s project.Status) string {
	switch s {
	case project.StatusCreated:
		return "Queued"
	case project.StatusClarifying:
		return "Reading your brief…"
	case project.StatusNeedsInput:
		return "A few quick questions"
	case project.StatusPlanning:
		return "Planning your site…"
	case project.StatusScreening:
		return "Reviewing the request…"
	case project.StatusAwaitingApproval:
		return "Waiting for your approval"
	case project.StatusEscalated:
		return "Held for review by Rasmus"
	case project.StatusBuilding:
		return "Building your site…"
	case project.StatusPreviewReady:
		return "Preview ready"
	case project.StatusAccepted:
		return "Accepted — final review by Rasmus"
	case project.StatusDelivered:
		return "Delivered & guaranteed"
	case project.StatusRejected:
		return "Declined"
	case project.StatusFailed:
		return "Failed"
	case project.StatusExpired:
		return "Preview expired"
	default:
		return string(s)
	}
}

// polling reports whether the dashboard should keep polling this project.
// It stops on resting states: waiting on the customer, the operator, or done.
// polling reports whether the project page should keep refreshing its status.
// Escalated projects poll too (slowly) so the page moves on its own once the
// operator approves or declines — see pollEvery.
func polling(p *project.Project) bool {
	switch p.Status {
	case project.StatusNeedsInput, project.StatusAwaitingApproval, project.StatusPreviewReady,
		project.StatusDelivered, project.StatusRejected, project.StatusFailed, project.StatusExpired:
		return false
	default:
		return true
	}
}

// pollEvery is the HTMX polling cadence: fast while a step is actively
// running, slow while waiting on the operator (which can take hours).
func pollEvery(p *project.Project) string {
	// Waiting on a human (Rasmus) — poll slowly; those states can take a while.
	if p.Status == project.StatusEscalated || p.Status == project.StatusAccepted {
		return "15s"
	}
	return "2s"
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
