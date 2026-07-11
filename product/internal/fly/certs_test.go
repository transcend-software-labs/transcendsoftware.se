package fly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestHTTP(machinesURL, graphqlURL string) *HTTP {
	return NewHTTP(Options{Token: "tok", Org: "org", MachinesURL: machinesURL, GraphQLURL: graphqlURL})
}

func TestAddCertificate_ApexRequirements(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{
			"hostname":"acme.se","configured":false,"status":"pending_validation",
			"dns_requirements":{
				"a":["66.0.0.1"],"aaaa":["2a09::99"],
				"acme_challenge":{"name":"_acme-challenge.acme.se","target":"acme.se.x.flydns.net"},
				"ownership":{"name":"_fly-ownership.acme.se","app_value":"app-123","org_value":"org-9"}
			}
		}`)
	}))
	defer srv.Close()

	req, err := newTestHTTP(srv.URL, "").AddCertificate(context.Background(), "app1", "acme.se")
	if err != nil {
		t.Fatalf("add cert: %v", err)
	}
	if gotPath != "/apps/app1/certificates/acme" || gotAuth != "Bearer tok" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["hostname"] != "acme.se" {
		t.Fatalf("body hostname = %v", gotBody["hostname"])
	}
	if !req.IsApex {
		t.Fatalf("expected apex")
	}
	// A + AAAA + acme-challenge CNAME + ownership TXT = 4 records.
	want := map[string]string{
		"A|acme.se":                     "66.0.0.1",
		"AAAA|acme.se":                  "2a09::99",
		"CNAME|_acme-challenge.acme.se": "acme.se.x.flydns.net",
		"TXT|_fly-ownership.acme.se":    "app-123",
	}
	if len(req.Records) != len(want) {
		t.Fatalf("records = %+v", req.Records)
	}
	for _, rec := range req.Records {
		if want[rec.Type+"|"+rec.Name] != rec.Value {
			t.Errorf("unexpected record %+v", rec)
		}
	}
}

func TestAddCertificate_SubdomainCNAME(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"hostname":"www.acme.se","status":"pending_validation",
			"dns_requirements":{"cname":"app1.fly.dev","acme_challenge":{"name":"_acme-challenge.www.acme.se","target":"t.flydns.net"}}
		}`)
	}))
	defer srv.Close()

	req, err := newTestHTTP(srv.URL, "").AddCertificate(context.Background(), "app1", "www.acme.se")
	if err != nil {
		t.Fatalf("add cert: %v", err)
	}
	if req.IsApex {
		t.Fatalf("www subdomain should not be apex")
	}
	var cname string
	for _, rec := range req.Records {
		if rec.Type == "CNAME" && rec.Name == "www.acme.se" {
			cname = rec.Value
		}
	}
	if cname != "app1.fly.dev" {
		t.Fatalf("expected CNAME to app, got records %+v", req.Records)
	}
}

func TestCheckCertificate(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"hostname":"acme.se","configured":true,"status":"active","validation":{"dns_configured":true,"alpn_configured":true}}`)
	}))
	defer srv.Close()

	st, err := newTestHTTP(srv.URL, "").CheckCertificate(context.Background(), "app1", "acme.se")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if gotPath != "/apps/app1/certificates/acme.se/check" {
		t.Fatalf("path = %q", gotPath)
	}
	if !st.Configured || !st.DNSConfigured || st.Status != "active" {
		t.Fatalf("status = %+v", st)
	}
}

func TestDeleteCertificate_404IsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %q", r.Method)
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if err := newTestHTTP(srv.URL, "").DeleteCertificate(context.Background(), "app1", "gone.se"); err != nil {
		t.Fatalf("delete of absent cert should be nil, got %v", err)
	}
}

func TestAllocateIPv6(t *testing.T) {
	var gotVars map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotVars = body.Variables
		_, _ = io.WriteString(w, `{"data":{"allocateIpAddress":{"ipAddress":{"address":"2a09:8280:1::a1","type":"v6"}}}}`)
	}))
	defer srv.Close()

	addr, err := newTestHTTP("", srv.URL).AllocateIPv6(context.Background(), "app1")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if addr != "2a09:8280:1::a1" {
		t.Fatalf("addr = %q", addr)
	}
	input, _ := gotVars["input"].(map[string]any)
	if input["appId"] != "app1" || input["type"] != "v6" {
		t.Fatalf("input = %v", input)
	}
}
