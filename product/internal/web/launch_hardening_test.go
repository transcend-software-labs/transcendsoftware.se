package web_test

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

func TestForgePagesShipStrictSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, header := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if resp.Header.Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
	nonce := regexp.MustCompile(`nonce="([^"]+)"`).FindSubmatch(body)
	if nonce == nil || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "'nonce-"+string(nonce[1])+"'") {
		t.Fatal("inline scripts do not carry the response CSP nonce")
	}
	if strings.Contains(string(body), "fonts.googleapis.com") {
		t.Fatal("Forge pages must not contact Google Fonts at runtime")
	}
	if !strings.Contains(string(body), `name="htmx-config" content='{`+`"allowEval":false`) {
		t.Fatal("htmx eval must stay disabled under the strict CSP")
	}
	if strings.Contains(string(body), "hx-on") {
		t.Fatal("inline htmx event handlers are incompatible with the strict CSP")
	}
}

func TestLegalPagesAndWithdrawalFunction(t *testing.T) {
	n := &recNotifier{}
	srv, _, _ := newTestServerAuth(t, config.Config{AdminEmail: "admin@example.com"}, n)
	defer srv.Close()
	for _, path := range []string{"/terms", "/privacy", "/withdraw"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Accept-Language", "en")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "Senast uppdaterad") {
			t.Fatalf("%s did not render in English", path)
		}
	}
	form := url.Values{"email": {"buyer@example.com"}, "project_id": {"project-1"}, "confirm": {"yes"}}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/withdraw", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Reference:") {
		t.Fatalf("withdrawal response = %d: %s", resp.StatusCode, body)
	}
	if len(n.bodies) != 2 { // durable customer confirmation + operator notification
		t.Fatalf("withdrawal should send two notifications, got %d", len(n.bodies))
	}
	forged, _ := http.NewRequest(http.MethodPost, srv.URL+"/withdraw", strings.NewReader(form.Encode()))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forged.Header.Set("Origin", "https://attacker.example")
	forgedResp, err := http.DefaultClient.Do(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgedResp.Body.Close()
	if forgedResp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site withdrawal status = %d, want 403", forgedResp.StatusCode)
	}
}

func TestCustomerCanEraseUnpaidProject(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	st := storeFor(srv.URL)
	u, _ := st.UserByEmail(t.Context(), "neighbour@example.com")
	p := &project.Project{ID: "erase-p", UserID: u.ID, Name: "Erase", Status: project.StatusFailed,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.CreateProject(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	tok := csrfToken(t, c, srv.URL)
	resp, err := c.PostForm(srv.URL+"/projects/erase-p/delete", url.Values{
		"csrf_token": {tok}, "confirm": {"delete"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := st.ProjectByID(t.Context(), "erase-p"); err == nil {
		t.Fatal("project metadata remained after customer erasure")
	}
}
