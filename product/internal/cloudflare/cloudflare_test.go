package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// wrap renders a Cloudflare v4 success envelope around a result JSON literal.
func wrap(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestSearchDomains(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("authorization")
		_, _ = io.WriteString(w, wrap(`{"domains":[
			{"name":"acme.dev","registrable":true,"tier":"standard","pricing":{"currency":"USD","registration_cost":"10.44","renewal_cost":"10.44"}},
			{"name":"acme.com","registrable":false,"reason":"domain_unavailable"}
		]}`))
	}))
	defer srv.Close()

	offers, err := New(srv.URL, "tok", "acct1").SearchDomains(context.Background(), "acme corp", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotPath != "/accounts/acct1/registrar/domain-search?limit=3&q=acme+corp" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(offers) != 2 {
		t.Fatalf("offers = %d", len(offers))
	}
	if o := offers[0]; !o.Registrable || o.Price != 10.44 || o.Currency != "USD" || o.Name != "acme.dev" {
		t.Fatalf("offer[0] = %+v", o)
	}
	if o := offers[1]; o.Registrable || o.Price != 0 {
		t.Fatalf("offer[1] should be unregistrable/zero-price: %+v", o)
	}
}

func TestCheckDomains(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/accounts/acct1/registrar/domain-check" {
			t.Errorf("method=%q path=%q", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, wrap(`{"domains":[
			{"name":"acme.dev","registrable":true,"tier":"premium","pricing":{"currency":"USD","registration_cost":"999.00","renewal_cost":"999.00"}}
		]}`))
	}))
	defer srv.Close()

	offers, err := New(srv.URL, "tok", "acct1").CheckDomains(context.Background(), []string{"acme.dev"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if names, _ := gotBody["domains"].([]any); len(names) != 1 || names[0] != "acme.dev" {
		t.Fatalf("request body domains = %v", gotBody["domains"])
	}
	if o := offers[0]; !o.Premium || o.Price != 999 {
		t.Fatalf("premium offer = %+v", o)
	}
}

func TestRegisterDomain(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/acct1/registrar/registrations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, wrap(`{"domain_name":"acme.dev","state":"pending","completed":false}`))
	}))
	defer srv.Close()

	state, err := New(srv.URL, "tok", "acct1").RegisterDomain(context.Background(), "acme.dev")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state != StatePending {
		t.Fatalf("state = %q", state)
	}
	if gotBody["domain_name"] != "acme.dev" || gotBody["auto_renew"] != true || gotBody["privacy_mode"] != "redaction" {
		t.Fatalf("register body = %v", gotBody)
	}
}

func TestRegisterDomain_FailedStateSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, wrap(`{"domain_name":"acme.dev","state":"failed","error":{"code":"payment_required","message":"no payment method"}}`))
	}))
	defer srv.Close()

	state, err := New(srv.URL, "tok", "acct1").RegisterDomain(context.Background(), "acme.dev")
	if state != StateFailed || err == nil {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestRegistrationStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/acct1/registrar/registrations/acme.dev/registration-status" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, wrap(`{"domain_name":"acme.dev","state":"succeeded","completed":true}`))
	}))
	defer srv.Close()

	state, err := New(srv.URL, "tok", "acct1").RegistrationStatus(context.Background(), "acme.dev")
	if err != nil || state != StateSucceeded {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestZoneID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "acme.dev" {
			t.Errorf("name = %q", r.URL.Query().Get("name"))
		}
		_, _ = io.WriteString(w, wrap(`[{"id":"zone123","name":"acme.dev","status":"active"}]`))
	}))
	defer srv.Close()

	id, err := New(srv.URL, "tok", "acct1").ZoneID(context.Background(), "acme.dev")
	if err != nil || id != "zone123" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestZoneID_NoZoneYet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, wrap(`[]`))
	}))
	defer srv.Close()

	id, err := New(srv.URL, "tok", "acct1").ZoneID(context.Background(), "acme.dev")
	if err != nil || id != "" {
		t.Fatalf("expected empty id no error, got id=%q err=%v", id, err)
	}
}

func TestEnsureDNSRecord_CreatesWhenMissing_Unproxied(t *testing.T) {
	var created DNSRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, wrap(`[{"id":"r1","type":"TXT","name":"other","content":"x","proxied":false}]`))
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&created)
			_, _ = io.WriteString(w, wrap(`{"id":"r2"}`))
		}
	}))
	defer srv.Close()

	err := New(srv.URL, "tok", "acct1").EnsureDNSRecord(context.Background(), "zone123",
		DNSRecord{Type: "A", Name: "acme.dev", Content: "1.2.3.4"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if created.Type != "A" || created.Content != "1.2.3.4" {
		t.Fatalf("created = %+v", created)
	}
	if created.Proxied {
		t.Fatalf("record must be created proxied:false")
	}
}

func TestEnsureDNSRecord_Idempotent(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_, _ = io.WriteString(w, wrap(`[{"id":"r1","type":"A","name":"acme.dev","content":"1.2.3.4","proxied":false}]`))
	}))
	defer srv.Close()

	err := New(srv.URL, "tok", "acct1").EnsureDNSRecord(context.Background(), "zone123",
		DNSRecord{Type: "A", Name: "acme.dev", Content: "1.2.3.4"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if posted {
		t.Fatalf("should not create a record that already exists")
	}
}

func TestErrorEnvelopeSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1003,"message":"Invalid or missing zone id"}],"result":null}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok", "acct1").ZoneID(context.Background(), "acme.dev")
	if err == nil || !contains(err.Error(), "Invalid or missing zone id") {
		t.Fatalf("expected surfaced CF error, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
