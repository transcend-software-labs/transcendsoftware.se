package builder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const landingWithForm = `<!doctype html><html><body>
<form method="post" action="/analyze">
  <input type="hidden" name="token" value="tok-123">
  <input type="url" name="site_url">
  <input type="email" name="email">
  <select name="depth"><option value="quick">Quick</option><option value="deep">Deep</option></select>
  <textarea name="notes"></textarea>
  <button type="submit">Analyze</button>
</form>
</body></html>`

// A submit that crashes (the SEO Probe failure) must produce an error finding.
func TestFormCheck_CrashingSubmitIsAFinding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, `template error in home: can't evaluate field CheckGroups`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(landingWithForm))
	}))
	defer srv.Close()

	f, note := auditPrimaryForm(context.Background(), srv.URL+"/")
	if f == nil {
		t.Fatalf("expected a finding for a crashing form; note: %s", note)
	}
	if f.Severity != "error" || f.Antipattern != "broken-primary-form" {
		t.Errorf("finding should be a broken-primary-form error, got %+v", f)
	}
	if !strings.Contains(note, "✗") {
		t.Errorf("log note should flag the failure, got %q", note)
	}
}

// A healthy submit passes — and so does a validation rejection (4xx), which
// means the handler RAN and judged our sample data, exactly what we verify.
func TestFormCheck_HealthyAndRejectedSubmitsPass(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		var got url.Values
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				_ = r.ParseForm()
				got = r.PostForm
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(landingWithForm))
		}))

		f, note := auditPrimaryForm(context.Background(), srv.URL+"/")
		if f != nil {
			t.Errorf("status %d should not be a finding, got %+v", status, f)
		}
		if !strings.Contains(note, "✓") {
			t.Errorf("status %d note should pass, got %q", status, note)
		}
		// The test data must be type-appropriate and hidden values preserved.
		if got.Get("token") != "tok-123" {
			t.Errorf("hidden field not preserved: %v", got)
		}
		if !strings.Contains(got.Get("email"), "@") {
			t.Errorf("email field should get an email, got %q", got.Get("email"))
		}
		if !strings.HasPrefix(got.Get("site_url"), "https://") {
			t.Errorf("url field should get a URL, got %q", got.Get("site_url"))
		}
		if got.Get("depth") != "quick" {
			t.Errorf("select should get its first option, got %q", got.Get("depth"))
		}
		srv.Close()
	}
}

// Auth forms are never exercised: a login-only page is skipped, not failed.
func TestFormCheck_SkipsAuthForms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("the check must not submit an auth form")
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body>
			<form method="post" action="/login">
			  <input type="email" name="email"><input type="password" name="password">
			</form></body></html>`))
	}))
	defer srv.Close()

	f, note := auditPrimaryForm(context.Background(), srv.URL+"/")
	if f != nil {
		t.Errorf("auth-only page should produce no finding, got %+v", f)
	}
	if !strings.Contains(note, "skipped") {
		t.Errorf("note should say the check was skipped, got %q", note)
	}
}

// GET forms (search boxes) are not the money path — skipped.
func TestFormCheck_SkipsGetForms(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<!doctype html><html><body>
			<form action="/search"><input type="text" name="q"></form></body></html>`))
	}))
	defer srv.Close()

	if f, note := auditPrimaryForm(context.Background(), srv.URL+"/"); f != nil || !strings.Contains(note, "skipped") {
		t.Errorf("GET-only page should be skipped, got finding=%v note=%q", f, note)
	}
}
