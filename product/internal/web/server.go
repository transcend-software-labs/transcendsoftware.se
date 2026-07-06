// Package web is the HTTP layer: the public landing page, auth, and the
// logged-in dashboard where customers start and watch projects.
package web

import (
	"crypto/subtle"
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/notify"
	"github.com/transcend-software-labs/rasmus-ai/internal/oauth"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
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
	oauth    *oauth.Registry // social login (nil → none configured)
	notifier notify.Notifier // for magic-link emails
	tmpl     *template.Template
	log      *slog.Logger
}

// NewServer wires the HTTP server.
func NewServer(cfg config.Config, st store.Store, sessions *auth.Sessions, orch *orchestrator.Orchestrator, broker *stream.Broker, assets storage.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"statusLabel": statusLabel,
		"polling":     polling,
		"pollEvery":   pollEvery,
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, sessions: sessions, orch: orch, broker: broker,
		storage: assets, oauth: oauth.NewRegistry(), notifier: notify.Noop{}, tmpl: tmpl, log: log}, nil
}

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
	mux.HandleFunc("GET /auth/{provider}", s.handleOAuthStart)
	mux.HandleFunc("GET /auth/{provider}/callback", s.handleOAuthCallback)

	mux.HandleFunc("GET /dashboard", s.requireUser(s.handleDashboard))
	mux.HandleFunc("GET /projects/new", s.requireUser(s.handleNewProjectForm))
	mux.HandleFunc("POST /projects", s.requireUser(s.handleCreateProject))
	mux.HandleFunc("GET /projects/{id}", s.requireUser(s.handleProject))
	mux.HandleFunc("GET /projects/{id}/status", s.requireUser(s.handleProjectStatus))
	mux.HandleFunc("GET /projects/{id}/stream", s.requireUser(s.handleProjectStream))
	mux.HandleFunc("POST /projects/{id}/answer", s.requireUser(s.handleAnswer))
	mux.HandleFunc("POST /projects/{id}/assets", limitBody(maxUpload+(1<<20), s.requireUser(s.handleUploadAsset)))
	mux.HandleFunc("POST /projects/{id}/reiterate", s.requireUser(s.handleReiterate))
	mux.HandleFunc("POST /projects/{id}/accept", s.requireUser(s.handleAccept))

	// Operator/admin views (gated by ADMIN_EMAIL).
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("POST /admin/projects/{id}/approve", s.requireAdmin(s.handleAdminApprove))
	mux.HandleFunc("POST /admin/projects/{id}/reject", s.requireAdmin(s.handleAdminReject))
	mux.HandleFunc("POST /admin/projects/{id}/destroy-preview", s.requireAdmin(s.handleAdminDestroyPreview))
	mux.HandleFunc("POST /admin/projects/{id}/deliver", s.requireAdmin(s.handleAdminDeliver))
	mux.HandleFunc("POST /admin/projects/{id}/return", s.requireAdmin(s.handleAdminReturn))

	return logRequests(s.log, mux)
}

// View is the data passed to every page template.
type View struct {
	Title     string
	User      *user.User
	IsAdmin   bool
	CSRF      string
	Flash     string
	Data      any
	Providers []oauth.Provider // social-login buttons on auth pages
	MagicLink bool             // advertise passwordless email login
}

func (s *Server) view(r *http.Request, title string, data any) View {
	u := s.currentUser(r)
	return View{Title: title, User: u, IsAdmin: s.isAdmin(u), CSRF: s.csrfToken(r),
		Data: data, Providers: s.oauth.Enabled(), MagicLink: s.cfg.MagicLinkEnabled}
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

// ownedProject loads a project and verifies the user owns it.
func (s *Server) ownedProject(w http.ResponseWriter, r *http.Request, u *user.User) (*project.Project, bool) {
	p, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil || p.UserID != u.ID {
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
	case project.StatusNeedsInput, project.StatusPreviewReady, project.StatusDelivered,
		project.StatusRejected, project.StatusFailed, project.StatusExpired:
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
