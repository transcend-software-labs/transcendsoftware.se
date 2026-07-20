package web_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// The screenshot route redirects to a freshly presigned URL (so links never
// expire) and is viewable only by the project's owner or an operator.
func TestShotRedirect_FreshAndAuthorized(t *testing.T) {
	stripe := fakeStripe(t, nil)
	defer stripe.Close()
	srv, st := newBillingServer(t, stripe.URL)
	defer srv.Close()
	ctx := t.Context()

	owner := signedInClient(t, srv.URL)
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{ID: "shot1", UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady,
		Screenshots: []project.Screenshot{{Path: "/", Key: "projects/shot1/screenshots/0.png"}}}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Don't follow the redirect — assert the 302 + fresh Location.
	noFollow := func(c *http.Client) {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	noFollow(owner)
	resp, err := owner.Get(srv.URL + "/projects/shot1/shots/0")
	if err != nil {
		t.Fatalf("owner get shot: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("owner shot status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "projects/shot1/screenshots/0.png") {
		t.Fatalf("redirect Location = %q", loc)
	}

	// Out-of-range index → 404.
	if resp, _ := owner.Get(srv.URL + "/projects/shot1/shots/9"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("out-of-range shot = %d, want 404", resp.StatusCode)
	}

	// A different signed-in user cannot see someone else's shots (404, not 403,
	// so the project's existence isn't revealed).
	stranger := signedInAs(t, srv.URL, "stranger@example.com", true)
	stranger.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if resp, _ := stranger.Get(srv.URL + "/projects/shot1/shots/0"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stranger shot status = %d, want 404", resp.StatusCode)
	}
}
