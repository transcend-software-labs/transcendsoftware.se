package web_test

import (
	"html"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPublicFunnelPreservesCampaignWithoutTrackingCookie(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	landingURL := srv.URL + "/?utm_source=linkedin&utm_medium=social&utm_campaign=launch"
	resp, err := http.Get(landingURL)
	if err != nil {
		t.Fatal(err)
	}
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("landing analytics set cookies: %+v", cookies)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := html.UnescapeString(string(body))
	trackedStart := "/start?utm_campaign=launch&utm_medium=social&utm_source=linkedin"
	if !strings.Contains(out, `href="`+trackedStart+`"`) {
		t.Fatalf("landing CTA did not preserve campaign: %s", out)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.Get(srv.URL + trackedStart)
	if err != nil {
		t.Fatal(err)
	}
	location := resp.Header.Get("Location")
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("start analytics set cookies: %+v", cookies)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || location != "/signup?utm_campaign=launch&utm_medium=social&utm_source=linkedin" {
		t.Fatalf("start redirect = %d %q", resp.StatusCode, location)
	}
	resp, err = http.Get(srv.URL + location)
	if err != nil {
		t.Fatal(err)
	}
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Fatalf("signup analytics set cookies: %+v", cookies)
	}
	resp.Body.Close()

	funnel, err := storeFor(srv.URL).MarketingFunnel(t.Context(), time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if funnel.LandingViews != 1 || funnel.Starts != 1 || funnel.SignupViews != 1 {
		t.Fatalf("public funnel = %+v", funnel)
	}
	if len(funnel.Sources) != 1 || funnel.Sources[0].Source != "linkedin" || funnel.Sources[0].Campaign != "launch" {
		t.Fatalf("campaign sources = %+v", funnel.Sources)
	}
}

func TestPublicFunnelSkipsKnownCrawlers(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("User-Agent", "Googlebot/2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	funnel, err := storeFor(srv.URL).MarketingFunnel(t.Context(), time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if funnel.LandingViews != 0 {
		t.Fatalf("crawler was counted: %+v", funnel)
	}
}
