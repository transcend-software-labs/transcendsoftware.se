// Package web holds the HTTP layer: routes, handlers, templates and static
// assets (both embedded, so the compiled binary is the entire app).
package web

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"app/internal/auth"
	"app/internal/hooks"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// assetVersion is a content hash of everything under static/, computed once at
// startup. It cache-busts asset URLs (see the asset template func) and doubles
// as the ETag for /static/ responses, so a redeploy with changed CSS/JS is
// picked up immediately while unchanged assets revalidate as cheap 304s.
var assetVersion = func() string {
	h := sha256.New()
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := staticFS.ReadFile(path)
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// asset returns the versioned URL for a file in static/.
func asset(name string) string { return "/static/" + name + "?v=" + assetVersion }

// cacheStatic serves embedded static files with caching enabled. embed.FS has
// no file modtimes, so without this every asset would be re-downloaded on every
// page view (no validators = effectively uncacheable).
func cacheStatic(next http.Handler) http.Handler {
	etag := `"` + assetVersion + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", etag) // FileServer answers If-None-Match with 304 from this
		next.ServeHTTP(w, r)
	})
}

// Options configures the HTTP layer.
type Options struct {
	SecureCookie bool
	OwnerEmail   string                    // reserves the first (owner) account for this address
	SiteName     string                    // shown in notification copy
	Notifiers    map[string]hooks.Notifier // by type ("email"); for hook "send test"
}

// Server is the HTTP layer.
type Server struct {
	db           *sql.DB
	sessions     *auth.Sessions
	secureCookie bool
	ownerEmail   string
	siteName     string
	notifiers    map[string]hooks.Notifier
	tmpl         *template.Template
	log          *slog.Logger
}

// New wires the HTTP layer.
func New(db *sql.DB, sessions *auth.Sessions, opts Options, log *slog.Logger) *Server {
	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{"asset": asset}).
		ParseFS(templatesFS, "templates/*.html"))
	site := opts.SiteName
	if site == "" {
		site = "your site"
	}
	return &Server{db: db, sessions: sessions, secureCookie: opts.SecureCookie,
		ownerEmail: opts.OwnerEmail, siteName: site, notifiers: opts.Notifiers,
		tmpl: tmpl, log: log}
}

// Handler returns the app's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", cacheStatic(http.FileServerFS(staticFS)))
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

	// Notification hooks: notify the owner when a row lands in a table.
	mux.HandleFunc("POST /admin/t/{table}/hooks", s.requireOwner(s.handleHookAdd))
	mux.HandleFunc("POST /admin/hooks/{id}/toggle", s.requireOwner(s.handleHookToggle))
	mux.HandleFunc("POST /admin/hooks/{id}/test", s.requireOwner(s.handleHookTest))
	mux.HandleFunc("POST /admin/hooks/{id}/delete", s.requireOwner(s.handleHookDelete))
	return mux
}

// View is the data passed to every page template.
type View struct {
	Title    string
	SiteName string // shown in the Forge-branded admin header
	User     *auth.User
	CSRF     string
	Flash    string
	Data     any
}

func (s *Server) view(r *http.Request, title string, data any) View {
	return View{Title: title, SiteName: s.siteName, User: s.currentUser(r), CSRF: s.csrfToken(r), Data: data}
}

// render executes the template into a buffer first so a template error becomes
// a loud 500 (caught by tests, smoke.js and the audit crawl) instead of a 200
// with silently truncated HTML.
func (s *Server) render(w http.ResponseWriter, status int, name string, v View) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, v); err != nil {
		s.log.Error("render", "template", name, "err", err)
		http.Error(w, "template error in "+name+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
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
