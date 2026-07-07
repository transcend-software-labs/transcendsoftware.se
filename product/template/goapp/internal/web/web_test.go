package web_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"app/internal/auth"
	"app/internal/db"
	"app/internal/hooks"
	"app/internal/web"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerOwner(t, "")
}

// newTestServerOwner starts a server with OWNER_EMAIL semantics.
func newTestServerOwner(t *testing.T, ownerEmail string) *httptest.Server {
	t.Helper()
	srv, _ := newTestServerHooks(t, ownerEmail, nil)
	return srv
}

// newTestServerHooks starts a server, optionally with a recording email
// notifier so hook-delivery tests can assert what was sent.
func newTestServerHooks(t *testing.T, ownerEmail string, email hooks.Notifier) (*httptest.Server, *sql.DB) {
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
	notifiers := map[string]hooks.Notifier{}
	if email != nil {
		notifiers["email"] = email
	}
	srv := web.New(database, sessions, web.Options{
		OwnerEmail: ownerEmail, SiteName: "Test Site", Notifiers: notifiers,
	}, log)
	return httptest.NewServer(srv.Handler()), database
}

// get fetches a page and returns its body.
func get(t *testing.T, c *http.Client, url string) (string, *http.Response) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b), resp
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func TestFullFlow_SignupDashboardContactLogout(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	// Health.
	resp, err := c.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v (%d)", err, resp.StatusCode)
	}
	resp.Body.Close()

	// Anonymous visitor sends a contact message.
	resp, err = c.PostForm(srv.URL+"/contact", url.Values{
		"name": {"Anna"}, "email": {"anna@example.com"}, "message": {"Hej! Vad kostar en tårta?"},
	})
	if err != nil {
		t.Fatalf("contact: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "message has been sent") {
		t.Fatal("contact form did not confirm")
	}

	// First signup → becomes the site owner.
	resp, err = c.PostForm(srv.URL+"/signup", url.Values{
		"email": {"owner@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// The owner's /app lands in the site admin, where the messages table —
	// like every table — is rendered by introspection.
	page, resp := get(t, c, srv.URL+"/app")
	if resp.Request.URL.Path != "/admin" {
		t.Fatalf("owner /app should land on /admin, got %s", resp.Request.URL.Path)
	}
	if !strings.Contains(page, "messages") || !strings.Contains(page, "users") {
		t.Fatal("admin should list the messages and users tables")
	}
	grid, _ := get(t, c, srv.URL+"/admin/t/messages")
	if !strings.Contains(grid, "Anna") || !strings.Contains(grid, "Vad kostar en tårta?") {
		t.Fatal("messages grid should show the contact message")
	}

	// Logout (CSRF-protected) then the admin requires login again.
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on admin")
	}
	resp, err = c.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {m[1]}})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	_, resp = get(t, c, srv.URL+"/app")
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("expected /app to redirect to /login after logout, got %s", resp.Request.URL.Path)
	}

	// Second signup is NOT an owner: plain account page, no admin access.
	resp, err = c.PostForm(srv.URL+"/signup", url.Values{
		"email": {"visitor@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatalf("signup 2: %v", err)
	}
	resp.Body.Close()
	body2, _ := get(t, c, srv.URL+"/app")
	if strings.Contains(body2, "Anna") {
		t.Fatal("non-owner must not see contact messages")
	}
	_, resp = get(t, c, srv.URL+"/admin")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-owner /admin should 404, got %d", resp.StatusCode)
	}
}

// ownerClient signs up the first (owner) account and returns a logged-in client.
func ownerClient(t *testing.T, base, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(base+"/signup", url.Values{
		"email": {email}, "password": {"password123"},
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	resp.Body.Close()
	return c
}

func TestAdmin_IntrospectionHidesInternalsAndMasksSecrets(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := ownerClient(t, srv.URL, "owner@example.com")

	page, _ := get(t, c, srv.URL+"/admin")
	for _, want := range []string{"messages", "users"} {
		if !strings.Contains(page, "/admin/t/"+want) {
			t.Errorf("admin index missing table %q", want)
		}
	}
	for _, hidden := range []string{"sessions", "schema_migrations"} {
		if strings.Contains(page, "/admin/t/"+hidden) {
			t.Errorf("admin index must hide internal table %q", hidden)
		}
	}

	// users grid: the bcrypt hash must be masked.
	grid, _ := get(t, c, srv.URL+"/admin/t/users")
	if !strings.Contains(grid, "owner@example.com") {
		t.Error("users grid should show the account email")
	}
	if strings.Contains(grid, "$2a$") || strings.Contains(grid, "$2b$") {
		t.Error("users grid leaked a bcrypt hash")
	}
	if !strings.Contains(grid, "•••••") {
		t.Error("masked column should render as dots")
	}
	if !strings.Contains(grid, "read-only") {
		t.Error("users grid should state it is read-only")
	}

	// Hidden/unknown tables 404 even with a valid session.
	for _, path := range []string{"/admin/t/sessions", "/admin/t/nope", "/admin/t/users%3B--"} {
		_, resp := get(t, c, srv.URL+path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s should 404, got %d", path, resp.StatusCode)
		}
	}
}

func TestAdmin_RowDetailDeleteAndReadOnly(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := ownerClient(t, srv.URL, "owner@example.com")
	// Seed a contact message.
	resp, _ := c.PostForm(srv.URL+"/contact", url.Values{
		"name": {"Berit"}, "email": {"berit@example.com"}, "message": {"Beställning till lördag"},
	})
	resp.Body.Close()

	// Row detail shows the full row.
	detail, _ := get(t, c, srv.URL+"/admin/t/messages/r/1")
	if !strings.Contains(detail, "Berit") || !strings.Contains(detail, "Beställning till lördag") {
		t.Fatal("row detail missing the message fields")
	}

	// Delete needs CSRF.
	resp, _ = c.PostForm(srv.URL+"/admin/t/messages/r/1/delete", url.Values{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tokenless delete should 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	page, _ := get(t, c, srv.URL+"/admin")
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token")
	}
	resp, _ = c.PostForm(srv.URL+"/admin/t/messages/r/1/delete", url.Values{"csrf_token": {m[1]}})
	resp.Body.Close()
	grid, _ := get(t, c, srv.URL+"/admin/t/messages")
	if strings.Contains(grid, "Berit") {
		t.Fatal("deleted row still shown")
	}

	// users is read-only: delete must be rejected and the account must survive.
	resp, _ = c.PostForm(srv.URL+"/admin/t/users/r/1/delete", url.Values{"csrf_token": {m[1]}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete on read-only table should 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	grid, _ = get(t, c, srv.URL+"/admin/t/users")
	if !strings.Contains(grid, "owner@example.com") {
		t.Fatal("read-only delete removed the account")
	}
}

func TestAdmin_CSVExportSkipsMaskedColumns(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := ownerClient(t, srv.URL, "owner@example.com")
	resp, _ := c.PostForm(srv.URL+"/contact", url.Values{
		"name": {"Cesar"}, "email": {"c@example.com"}, "message": {"Hej, \"offert\" tack"},
	})
	resp.Body.Close()

	csvBody, resp := get(t, c, srv.URL+"/admin/t/messages/csv")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("csv content type = %q", ct)
	}
	if !strings.Contains(csvBody, "Cesar") || !strings.Contains(csvBody, `"Hej, ""offert"" tack"`) {
		t.Fatalf("csv missing/misquoted row: %q", csvBody)
	}

	usersCSV, _ := get(t, c, srv.URL+"/admin/t/users/csv")
	if strings.Contains(usersCSV, "password_hash") || strings.Contains(usersCSV, "$2") {
		t.Fatal("users CSV must not contain the password hash column")
	}
	if !strings.Contains(usersCSV, "owner@example.com") {
		t.Fatal("users CSV should contain the email")
	}
}

func TestSignup_OwnerEmailReservesFirstAccount(t *testing.T) {
	srv := newTestServerOwner(t, "Owner@Example.com")
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	// A stranger cannot claim the empty site.
	resp, err := c.PostForm(srv.URL+"/signup", url.Values{
		"email": {"squatter@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "reserved for the site owner") {
		t.Fatalf("stranger's first signup should be rejected, got %d", resp.StatusCode)
	}

	// The owner (case-insensitive) can — and becomes admin.
	c2 := ownerClient(t, srv.URL, "owner@example.com")
	if _, resp := get(t, c2, srv.URL+"/admin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("owner should reach /admin, got %d", resp.StatusCode)
	}

	// After the owner exists, anyone may sign up as a regular user.
	jar3, _ := cookiejar.New(nil)
	c3 := &http.Client{Jar: jar3}
	resp, err = c3.PostForm(srv.URL+"/signup", url.Values{
		"email": {"squatter@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, r := get(t, c3, srv.URL+"/admin"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("later signup must not be owner, /admin gave %d", r.StatusCode)
	}
}

// recEmail records what would be sent, so hook tests can assert delivery.
type recEmail struct {
	mu    sync.Mutex
	sends []recSend
}
type recSend struct {
	target, table, body, replyTo string
}

func (r *recEmail) Notify(_ context.Context, target string, e hooks.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, f := range e.Fields {
		b.WriteString(f.Name + "=" + f.Value + ";")
	}
	r.sends = append(r.sends, recSend{target: target, table: e.Table, body: b.String(), replyTo: e.ReplyTo})
	return nil
}
func (r *recEmail) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sends) }
func (r *recEmail) last() recSend {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends[len(r.sends)-1]
}

func TestHooks_EmailOnInsertViaTriggerOutbox(t *testing.T) {
	rec := &recEmail{}
	srv, database := newTestServerHooks(t, "owner@example.com", rec)
	defer srv.Close()
	c := ownerClient(t, srv.URL, "owner@example.com")

	// The dispatcher isn't started by the test server, so run drains manually.
	disp := hooks.NewDispatcher(database, "Test Site", map[string]hooks.Notifier{"email": rec},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Enable an email hook on the messages table.
	page, _ := get(t, c, srv.URL+"/admin/t/messages")
	tok := csrfRe.FindStringSubmatch(page)[1]
	resp, _ := c.PostForm(srv.URL+"/admin/t/messages/hooks", url.Values{"csrf_token": {tok}, "target": {"owner@example.com"}})
	resp.Body.Close()

	// A submission BEFORE the hook shouldn't have been captured (trigger is new),
	// so submit AFTER enabling.
	resp, _ = c.PostForm(srv.URL+"/contact", url.Values{
		"name": {"Dagny"}, "email": {"dagny@example.com"}, "message": {"Order till fredag"},
	})
	resp.Body.Close()

	if err := disp.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 email, got %d", rec.count())
	}
	s := rec.last()
	if s.target != "owner@example.com" || s.table != "messages" {
		t.Fatalf("wrong delivery: %+v", s)
	}
	if !strings.Contains(s.body, "Dagny") || !strings.Contains(s.body, "Order till fredag") {
		t.Fatalf("email body missing fields: %s", s.body)
	}
	if s.replyTo != "dagny@example.com" {
		t.Errorf("reply-to should be the submitter email, got %q", s.replyTo)
	}

	// Turn the hook off → no trigger → new rows aren't captured.
	page, _ = get(t, c, srv.URL+"/admin/t/messages")
	hookID := regexp.MustCompile(`/admin/hooks/([a-f0-9]+)/toggle`).FindStringSubmatch(page)[1]
	tok = csrfRe.FindStringSubmatch(page)[1]
	resp, _ = c.PostForm(srv.URL+"/admin/hooks/"+hookID+"/toggle", url.Values{"csrf_token": {tok}})
	resp.Body.Close()
	resp, _ = c.PostForm(srv.URL+"/contact", url.Values{"name": {"Erik"}, "email": {"e@example.com"}, "message": {"x"}})
	resp.Body.Close()
	if err := disp.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("disabled hook should not deliver; got %d emails", rec.count())
	}
}

func TestHooks_MaskedColumnsNotSent(t *testing.T) {
	rec := &recEmail{}
	srv, database := newTestServerHooks(t, "owner@example.com", rec)
	defer srv.Close()
	c := ownerClient(t, srv.URL, "owner@example.com")
	disp := hooks.NewDispatcher(database, "Test Site", map[string]hooks.Notifier{"email": rec},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Hook the users table, then create a second account → a new users row.
	page, _ := get(t, c, srv.URL+"/admin/t/users")
	tok := csrfRe.FindStringSubmatch(page)[1]
	resp, _ := c.PostForm(srv.URL+"/admin/t/users/hooks", url.Values{"csrf_token": {tok}, "target": {"owner@example.com"}})
	resp.Body.Close()

	jar2, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar2}
	resp, _ = c2.PostForm(srv.URL+"/signup", url.Values{"email": {"member@example.com"}, "password": {"password123"}})
	resp.Body.Close()

	if err := disp.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 email for the new user, got %d", rec.count())
	}
	body := rec.last().body
	if strings.Contains(body, "$2a$") || strings.Contains(body, "$2b$") {
		t.Fatalf("password hash leaked into notification: %s", body)
	}
	if !strings.Contains(body, "password_hash=•••••") {
		t.Errorf("masked column should be dotted, got: %s", body)
	}
	if !strings.Contains(body, "member@example.com") {
		t.Errorf("email should include the new account address")
	}
}

func TestBoost_HtmxVendoredAndWired(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	// htmx is embedded (no CDN) and served.
	js, resp := get(t, http.DefaultClient, srv.URL+"/static/htmx.min.js")
	if resp.StatusCode != http.StatusOK || !strings.Contains(js, "htmx") {
		t.Fatalf("htmx.min.js not served (%d)", resp.StatusCode)
	}
	// Pages opt into boosting and load it locally.
	page, _ := get(t, http.DefaultClient, srv.URL+"/")
	if !strings.Contains(page, `hx-boost="true"`) {
		t.Error("body should enable hx-boost")
	}
	if !strings.Contains(page, "/static/htmx.min.js") {
		t.Error("page should load the vendored htmx, not a CDN")
	}
	if strings.Contains(page, "unpkg.com") || strings.Contains(page, "cdn.") {
		t.Error("no third-party script hosts — sites must stay self-contained")
	}
}

func TestLogout_RequiresCSRF(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp, err := c.PostForm(srv.URL+"/signup", url.Values{
		"email": {"a@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	resp.Body.Close()

	resp, err = c.PostForm(srv.URL+"/logout", url.Values{}) // no token
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tokenless logout should be rejected, got %d", resp.StatusCode)
	}
}
