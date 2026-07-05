package web_test

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
	"app/internal/web"
)

func newTestServer(t *testing.T) *httptest.Server {
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
	return httptest.NewServer(web.New(database, sessions, false, log).Handler())
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

	// Owner dashboard shows the message.
	resp, err = c.Get(srv.URL + "/app")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(body)
	if !strings.Contains(page, "owner@example.com") || !strings.Contains(page, "site owner") {
		t.Fatal("dashboard missing owner identity")
	}
	if !strings.Contains(page, "Anna") || !strings.Contains(page, "Vad kostar en tårta?") {
		t.Fatal("owner dashboard should list the contact message")
	}

	// Logout (CSRF-protected) then the dashboard requires login again.
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no csrf token on dashboard")
	}
	resp, err = c.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {m[1]}})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	resp, _ = c.Get(srv.URL + "/app")
	final := resp.Request.URL.Path
	resp.Body.Close()
	if final != "/login" {
		t.Fatalf("expected /app to redirect to /login after logout, got %s", final)
	}

	// Second signup is NOT an owner and sees no messages.
	resp, err = c.PostForm(srv.URL+"/signup", url.Values{
		"email": {"visitor@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatalf("signup 2: %v", err)
	}
	resp.Body.Close()
	resp, _ = c.Get(srv.URL + "/app")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Anna") {
		t.Fatal("non-owner must not see contact messages")
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
