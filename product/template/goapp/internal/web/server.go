// Package web holds the HTTP layer: routes, handlers, templates and static
// assets (both embedded, so the compiled binary is the entire app).
package web

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"html/template"
	"log/slog"
	"net/http"

	"app/internal/auth"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the HTTP layer.
type Server struct {
	db           *sql.DB
	sessions     *auth.Sessions
	secureCookie bool
	ownerEmail   string // while no accounts exist, only this email may register (empty → anyone)
	tmpl         *template.Template
	log          *slog.Logger
}

// New wires the HTTP layer. ownerEmail (optional, from OWNER_EMAIL) reserves
// the first — owner — account for the site's real owner.
func New(db *sql.DB, sessions *auth.Sessions, secureCookie bool, ownerEmail string, log *slog.Logger) *Server {
	tmpl := template.Must(template.ParseFS(templatesFS, "templates/*.html"))
	return &Server{db: db, sessions: sessions, secureCookie: secureCookie,
		ownerEmail: ownerEmail, tmpl: tmpl, log: log}
}

// Handler returns the app's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("POST /contact", s.handleContact)

	mux.HandleFunc("GET /signup", s.handleSignupForm)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /app", s.requireUser(s.handleDashboard))

	// The site admin: every table in the database, rendered by introspection.
	mux.HandleFunc("GET /admin", s.requireOwner(s.handleAdmin))
	mux.HandleFunc("GET /admin/t/{table}", s.requireOwner(s.handleAdminTable))
	mux.HandleFunc("GET /admin/t/{table}/csv", s.requireOwner(s.handleAdminCSV))
	mux.HandleFunc("GET /admin/t/{table}/r/{rowid}", s.requireOwner(s.handleAdminRow))
	mux.HandleFunc("POST /admin/t/{table}/r/{rowid}/delete", s.requireOwner(s.handleAdminRowDelete))
	return mux
}

// View is the data passed to every page template.
type View struct {
	Title string
	User  *auth.User
	CSRF  string
	Flash string
	Data  any
}

func (s *Server) view(r *http.Request, title string, data any) View {
	return View{Title: title, User: s.currentUser(r), CSRF: s.csrfToken(r), Data: data}
}

func (s *Server) render(w http.ResponseWriter, status int, name string, v View) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

// currentUser returns the logged-in user, or nil.
func (s *Server) currentUser(r *http.Request) *auth.User {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return nil
	}
	uid, ok := s.sessions.UserID(r.Context(), c.Value)
	if !ok {
		return nil
	}
	u, err := auth.UserByID(r.Context(), s.db, uid)
	if err != nil {
		return nil
	}
	return u
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

// checkCSRF validates the form token against the session's CSRF token. Use it
// on every authenticated POST.
func (s *Server) checkCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	got := r.FormValue("csrf_token")
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

type authedHandler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// requireUser gates a route behind login.
func (s *Server) requireUser(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r, u)
	}
}
