package web_test

// These tests cover the INVARIANTS every generated site must uphold no matter
// what is built on top of the starter: the auth/owner model, session + CSRF
// security, and the admin never leaking secrets. They deliberately do NOT assert
// scaffold specifics (contact-form copy, which business tables /admin lists,
// exact grids) so that EXTENDING the app — new pages, tables, fields — does not
// break them. If you change behavior one of these asserts, that is a real
// regression to fix, not a test to trim. Functional coverage of the actual site
// you build is the browser smoke test (scripts/smoke.js), not this file.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"app/internal/auth"
	"app/internal/db"
	"app/internal/hooks"
	"app/internal/web"
)

func newTestServer(t *testing.T, ownerEmail string) *httptest.Server {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sessions := auth.NewSessions(database, time.Hour)
	srv := web.New(database, sessions, web.Options{
		OwnerEmail: ownerEmail, SiteName: "Test Site",
		Notifiers: map[string]hooks.Notifier{},
	}, log)
	return httptest.NewServer(srv.Handler())
}

func get(t *testing.T, c *http.Client, u string) (string, *http.Response) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get %s: %v", u, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b), resp
}

// signup creates an account and returns a logged-in client (email+password is
// the auth contract the starter ships and the agent must keep).
func signup(t *testing.T, base, email string) (*http.Client, *http.Response) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/signup", url.Values{"email": {email}, "password": {"password123"}})
	if err != nil {
		t.Fatalf("signup %s: %v", email, err)
	}
	resp.Body.Close()
	return c, resp
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// The platform health check must always answer 200.
func TestHealthzOK(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	if _, resp := get(t, &http.Client{}, srv.URL+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}
}

// The owner model: the first account is the owner/admin; later accounts are
// members who cannot reach /admin; logged-out visitors are sent to /login.
func TestAuthModel_OwnerMemberGating(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()

	// Anonymous /admin → login.
	jar, _ := cookiejar.New(nil)
	if _, resp := get(t, &http.Client{Jar: jar}, srv.URL+"/admin"); resp.Request.URL.Path != "/login" {
		t.Fatalf("anon /admin should redirect to /login, got %s", resp.Request.URL.Path)
	}

	// First signup → owner; /app lands in /admin.
	owner, _ := signup(t, srv.URL, "owner@example.com")
	page, resp := get(t, owner, srv.URL+"/app")
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("owner /app should land on /admin, got %s", resp.Request.URL.Path)
	}

	// Logout (CSRF-protected) → /admin needs login again.
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on the admin page")
	}
	r, err := owner.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {m[1]}})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	r.Body.Close()
	if _, r := get(t, owner, srv.URL+"/app"); r.Request.URL.Path != "/login" {
		t.Fatalf("after logout /app should redirect to /login, got %s", r.Request.URL.Path)
	}

	// Second signup → member; /admin is hidden (404).
	member, _ := signup(t, srv.URL, "member@example.com")
	if _, r := get(t, member, srv.URL+"/admin"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("member /admin should 404, got %d", r.StatusCode)
	}
}

// OWNER_EMAIL reserves the first account for the customer the site was built for.
func TestOwnerEmail_ReservesFirstAccount(t *testing.T) {
	srv := newTestServer(t, "Owner@Example.com")
	defer srv.Close()

	// A stranger cannot claim the empty site.
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(srv.URL+"/signup", url.Values{"email": {"stranger@example.com"}, "password": {"password123"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stranger's first signup should be forbidden, got %d", resp.StatusCode)
	}

	// The owner (case-insensitive) can, and reaches /admin.
	owner, _ := signup(t, srv.URL, "owner@example.com")
	if _, r := get(t, owner, srv.URL+"/admin"); r.StatusCode != http.StatusOK {
		t.Fatalf("owner should reach /admin, got %d", r.StatusCode)
	}
}

// CSRF: a state-changing POST without a valid token is rejected.
func TestCSRF_TokenlessPostRejected(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	owner, _ := signup(t, srv.URL, "owner@example.com")
	resp, err := owner.PostForm(srv.URL+"/logout", url.Values{}) // no csrf_token
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tokenless logout should be 400, got %d", resp.StatusCode)
	}
}

// Every starter page must render without template errors. render() turns a
// template failure into a 500, so a plain 200 check catches missing fields,
// bad pipelines and broken layout includes at test time.
func TestPagesRenderWithoutTemplateErrors(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	owner, _ := signup(t, srv.URL, "owner@example.com")
	member, _ := signup(t, srv.URL, "member@example.com")
	anon := &http.Client{}

	for _, tc := range []struct {
		name, path string
		c          *http.Client
	}{
		{"landing", "/", anon},
		{"landing authed", "/", owner},
		{"login", "/login", anon},
		{"signup", "/signup", anon},
		{"dashboard", "/app", member},
		{"admin index", "/admin", owner},
		{"admin table", "/admin/t/users", owner},
		{"admin row", "/admin/t/users/r/1", owner},
	} {
		body, resp := get(t, tc.c, srv.URL+tc.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s (GET %s) = %d, want 200 — body: %.300s", tc.name, tc.path, resp.StatusCode, body)
		}
	}
}

// Static assets must be cacheable (Cache-Control + ETag with 304 revalidation)
// and pages must link them with a cache-busting version. embed.FS files have no
// modtime, so losing these headers would silently make every asset re-download
// on every page view.
func TestStaticAssetsAreCacheable(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	c := &http.Client{}

	page, _ := get(t, c, srv.URL+"/")
	re := regexp.MustCompile(`href="(/static/tokens\.css\?v=[^"]+)"`)
	m := re.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("landing page does not link versioned tokens.css")
	}
	resp, err := c.Get(srv.URL + m[1])
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tokens.css = %d, want 200", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q, want a max-age", cc)
	} else if !strings.Contains(cc, "max-age=31536000") || !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want one-year immutable caching for versioned assets", cc)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("static response has no ETag")
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+m[1], nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", resp2.StatusCode)
	}

	// Pages must reference assets through the versioned helper.
	if !strings.Contains(page, "/static/app.css?v=") {
		t.Error("landing page does not link app.css with a ?v= cache-buster")
	}
}

func TestPublicLayoutCarriesQualityBaseline(t *testing.T) {
	srv := newTestServer(t, "owner@example.com")
	defer srv.Close()
	page, resp := get(t, &http.Client{}, srv.URL+"/")
	for _, header := range []string{"Content-Security-Policy", "Permissions-Policy", "Referrer-Policy", "Strict-Transport-Security", "X-Content-Type-Options", "X-Frame-Options"} {
		if resp.Header.Get(header) == "" {
			t.Errorf("missing security header %s", header)
		}
	}
	for _, want := range []string{
		`<html lang="en"`, `name="theme-color"`, `name="color-scheme"`,
		`rel="icon"`, `class="skip-link" href="#main-content"`,
		`id="main-content"`, `href="/login">Owner login</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("public layout missing %q", want)
		}
	}
	headerEnd := strings.Index(page, "</header>")
	if headerEnd > 0 && strings.Contains(page[:headerEnd], `href="/login"`) {
		t.Error("owner login must stay out of the public primary navigation")
	}
}

func TestCanonicalUsesBrandedForwardedHost(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "example.forge.test")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `rel="canonical" href="https://example.forge.test/"`) {
		t.Errorf("canonical did not honor the public forwarded host: %.500s", body)
	}
}

// The admin must never expose password hashes and must hide internal tables —
// a security invariant that has to survive whatever tables the site adds.
func TestAdmin_MasksSecretsAndHidesInternals(t *testing.T) {
	srv := newTestServer(t, "")
	defer srv.Close()
	owner, _ := signup(t, srv.URL, "owner@example.com")

	index, _ := get(t, owner, srv.URL+"/admin")
	for _, hidden := range []string{"/admin/t/sessions", "/admin/t/schema_migrations"} {
		if strings.Contains(index, hidden) {
			t.Errorf("admin must not list internal table %s", hidden)
		}
	}

	grid, _ := get(t, owner, srv.URL+"/admin/t/users")
	if !strings.Contains(grid, "owner@example.com") {
		t.Error("users grid should show the account email")
	}
	if strings.Contains(grid, "$2a$") || strings.Contains(grid, "$2b$") {
		t.Error("users grid leaked a bcrypt hash")
	}

	csv, _ := get(t, owner, srv.URL+"/admin/t/users/csv")
	if strings.Contains(csv, "$2a$") || strings.Contains(csv, "$2b$") {
		t.Error("users CSV leaked a bcrypt hash")
	}
}

// TestSEO_BaselineSurvives asserts the SEO floor every generated site must keep:
// the head carries description/canonical/Open Graph/structured data, and the
// crawl pair (sitemap.xml + robots.txt) works. It checks the TAGS, never the
// copy — rewriting the words is expected; dropping the tags is a regression.
func TestSEO_BaselineSurvives(t *testing.T) {
	srv := newTestServer(t, "")
	c := srv.Client()

	body, _ := get(t, c, srv.URL+"/")
	for _, want := range []string{
		`<meta name="description"`, // the search/social snippet
		`rel="canonical"`,
		`property="og:title"`,
		`property="og:site_name"`,
		`name="twitter:card"`,
		`application/ld+json`,    // structured data
		`"@type":"Organization"`, // …of the right shape
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing <head> missing %q", want)
		}
	}

	sm, _ := get(t, c, srv.URL+"/sitemap.xml")
	if !strings.Contains(sm, "<urlset") || !strings.Contains(sm, "<loc>"+srv.URL+"/</loc>") {
		t.Errorf("sitemap.xml should list absolute public URLs, got:\n%s", sm)
	}

	rb, _ := get(t, c, srv.URL+"/robots.txt")
	for _, want := range []string{"User-agent: *", "Disallow: /admin", "Sitemap: " + srv.URL + "/sitemap.xml"} {
		if !strings.Contains(rb, want) {
			t.Errorf("robots.txt missing %q, got:\n%s", want, rb)
		}
	}
}
