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
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"

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

// asset returns the versioned URL for a file in static/. The build-time image
// optimizer writes a WebP sibling for PNG/JPEG inputs; selecting it here makes
// existing {{asset "photo.png"}} calls automatically serve the smaller file.
func asset(name string) string {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	ext := strings.ToLower(path.Ext(clean))
	if ext == ".png" || ext == ".jpg" || ext == ".jpeg" {
		webp := strings.TrimSuffix(clean, path.Ext(clean)) + ".webp"
		if _, err := staticFS.ReadFile("static/" + webp); err == nil {
			clean = webp
		}
	}
	return "/static/" + clean + "?v=" + assetVersion
}

// assetSrcSet returns the responsive WebP variants created by
// scripts/optimize-images.js. Use it on content/hero <img> elements together
// with a meaningful sizes attribute. Empty means the asset has no variants.
func assetSrcSet(name string) string {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	base := strings.TrimSuffix(clean, path.Ext(clean))
	var out []string
	seen := map[int]bool{}
	for _, width := range []int{480, 768, 1200, 1600} {
		variant := fmt.Sprintf("%s-%d.webp", base, width)
		if _, err := staticFS.ReadFile("static/" + variant); err == nil {
			out = append(out, asset(variant)+fmt.Sprintf(" %dw", width))
			seen[width] = true
		}
	}
	var manifest map[string]struct {
		Width int `json:"width"`
	}
	if raw, err := staticFS.ReadFile("static/image-manifest.json"); err == nil && json.Unmarshal(raw, &manifest) == nil {
		if dim, ok := manifest[clean]; ok && dim.Width > 0 && !seen[dim.Width] {
			out = append(out, asset(clean)+fmt.Sprintf(" %dw", dim.Width))
		}
	}
	return strings.Join(out, ", ")
}

// cacheStatic serves embedded static files with caching enabled. embed.FS has
// no file modtimes, so without this every asset would be re-downloaded on every
// page view (no validators = effectively uncacheable).
func cacheStatic(next http.Handler) http.Handler {
	etag := `"` + assetVersion + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == assetVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		w.Header().Set("ETag", etag) // FileServer answers If-None-Match with 304 from this
		next.ServeHTTP(w, r)
	})
}

// Options configures the HTTP layer.
type Options struct {
	SecureCookie bool
	OwnerEmail   string                    // reserves the first (owner) account for this address
	SiteName     string                    // shown in notification copy
	Language     string                    // public HTML language, e.g. "sv"
	ThemeColor   string                    // browser chrome color, matching the public theme
	ColorScheme  string                    // "light", "dark", or "light dark"
	Notifiers    map[string]hooks.Notifier // by type ("email"); for hook "send test"
}

// Server is the HTTP layer.
type Server struct {
	db           *sql.DB
	sessions     *auth.Sessions
	secureCookie bool
	ownerEmail   string
	siteName     string
	language     string
	themeColor   string
	colorScheme  string
	notifiers    map[string]hooks.Notifier
	tmpl         *template.Template
	log          *slog.Logger
}

// New wires the HTTP layer.
func New(db *sql.DB, sessions *auth.Sessions, opts Options, log *slog.Logger) *Server {
	tmpl := template.Must(template.New("").
		Funcs(template.FuncMap{"asset": asset, "assetSrcSet": assetSrcSet}).
		ParseFS(templatesFS, "templates/*.html"))
	site := opts.SiteName
	if site == "" {
		site = "your site"
	}
	language := opts.Language
	if language == "" {
		language = "en"
	}
	themeColor := opts.ThemeColor
	if themeColor == "" {
		themeColor = "#ffffff"
	}
	colorScheme := opts.ColorScheme
	if colorScheme == "" {
		colorScheme = "light"
	}
	return &Server{db: db, sessions: sessions, secureCookie: opts.SecureCookie,
		ownerEmail: opts.OwnerEmail, siteName: site, language: language,
		themeColor: themeColor, colorScheme: colorScheme, notifiers: opts.Notifiers,
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

	// SEO: crawlable page list + crawl rules (see seo.go — add new public pages
	// to publicPages so they land in the sitemap).
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)

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
	return securityHeaders(mux)
}

// securityHeaders is intentionally centralized so generated projects inherit a
// safe browser baseline without each build having to remember it. The policy
// permits hosted fonts and customer imagery while keeping scripts, forms and
// framing same-origin. JSON-LD is the only inline script in the starter.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' data: https://fonts.gstatic.com; img-src 'self' data: https:; connect-src 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// View is the data passed to every page template.
type View struct {
	Title       string
	SiteName    string // shown in the Forge-branded admin header
	Language    string
	ThemeColor  string
	ColorScheme string

	// SEO — layout.html renders these into <head>; see seo.go and AGENTS.md.
	// SET Description ON EVERY PUBLIC PAGE (one honest sentence about THAT page):
	// it's the snippet Google and every social preview show. OGImage is optional
	// (an absolute or /static/… URL) and makes shared links show a picture.
	// Canonical + JSONLD are filled in for you.
	Description string
	OGImage     string
	Canonical   string
	JSONLD      template.JS

	User  *auth.User
	CSRF  string
	Flash string
	Data  any
}

func (s *Server) view(r *http.Request, title string, data any) View {
	return View{
		Title: title, SiteName: s.siteName, Language: s.language,
		ThemeColor: s.themeColor, ColorScheme: s.colorScheme,
		Canonical: absURL(r, r.URL.Path), JSONLD: s.siteJSONLD(r),
		User: s.currentUser(r), CSRF: s.csrfToken(r), Data: data,
	}
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
