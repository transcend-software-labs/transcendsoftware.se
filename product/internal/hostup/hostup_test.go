package hostup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

func TestCheckDomains(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"data":[
			{"name":"acme.se","available":true,"premium":false,
			 "actions":{"canRegister":{"allowed":true}},
			 "billing":{"amount":99,"currencyCode":"SEK"},"renewalAmount":169},
			{"name":"acme.nu","available":false,"reason":"taken",
			 "actions":{"canRegister":{"allowed":false}}}
		]}`)
	}))
	defer srv.Close()

	offers, err := New(srv.URL, "tok", "invoice").CheckDomains(context.Background(), []string{"acme.se", "acme.nu"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if gotPath != "/api/v2/domains/availability" || gotAuth != "Bearer tok" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if names, _ := gotBody["names"].([]any); len(names) != 2 || names[0] != "acme.se" {
		t.Fatalf("request names = %v", gotBody["names"])
	}
	if o := offers[0]; !o.Registrable || o.Price != 99 || o.Currency != "SEK" {
		t.Fatalf("offer[0] = %+v", o)
	}
	if o := offers[1]; o.Registrable {
		t.Fatalf("offer[1] should not be registrable: %+v", o)
	}
}

func TestCheckDomains_AsyncPoll(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"operation":{"status":"queued","jobId":"dcheck_1","pollUrl":"/api/v2/domains/availability/dcheck_1"}}`)
		case strings.HasSuffix(r.URL.Path, "dcheck_1"):
			polls++
			if polls < 2 {
				_, _ = io.WriteString(w, `{"operation":{"status":"processing","pollUrl":"/api/v2/domains/availability/dcheck_1"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"name":"acme.se","available":true,"actions":{"canRegister":{"allowed":true}},"billing":{"amount":99,"currencyCode":"SEK"}}]}`)
		}
	}))
	defer srv.Close()

	offers, err := New(srv.URL, "tok", "").CheckDomains(context.Background(), []string{"acme.se"})
	if err != nil {
		t.Fatalf("async check: %v", err)
	}
	if len(offers) != 1 || !offers[0].Registrable {
		t.Fatalf("offers = %+v", offers)
	}
	if polls < 2 {
		t.Fatalf("expected poll retries, got %d", polls)
	}
}

func TestSearchDomains_FansOutTLDs(t *testing.T) {
	var gotNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Names []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotNames = body.Names
		_, _ = io.WriteString(w, `{"data":[{"name":"mittbageri.se","available":true,"actions":{"canRegister":{"allowed":true}},"billing":{"amount":99,"currencyCode":"SEK"}}]}`)
	}))
	defer srv.Close()

	offers, err := New(srv.URL, "tok", "").SearchDomains(context.Background(), "Mitt Bageri", 12)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(offers) != 1 || offers[0].Name != "mittbageri.se" {
		t.Fatalf("offers = %+v", offers)
	}
	// Bare phrase fans out over the TLD list, .se first, slugified (space + case).
	if len(gotNames) != len(searchTLDs) || gotNames[0] != "mittbageri.se" {
		t.Fatalf("checked names = %v", gotNames)
	}
}

func TestSearchDomains_ExactDomainAsTyped(t *testing.T) {
	var gotNames []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Names []string `json:"names"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotNames = body.Names
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "tok", "").SearchDomains(context.Background(), "Acme.SE", 12); err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(gotNames) != 1 || gotNames[0] != "acme.se" {
		t.Fatalf("checked names = %v", gotNames)
	}
}

func TestRegisterDomain_OrderBodyAndStates(t *testing.T) {
	var gotBody map[string]any
	status := "pending"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/orders" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"ord_1","status":"`+status+`"}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", "invoice")

	state, err := c.RegisterDomain(context.Background(), "acme.se")
	if err != nil || state != registrar.StatePending {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if gotBody["paymentMethod"] != "invoice" {
		t.Fatalf("paymentMethod = %v", gotBody["paymentMethod"])
	}
	items, _ := gotBody["items"].([]any)
	item, _ := items[0].(map[string]any)
	if item["type"] != "domain" || item["action"] != "register" || item["domainName"] != "acme.se" {
		t.Fatalf("order item = %v", item)
	}
	// .se carries the registry-terms acceptance.
	if terms, _ := item["acceptedTerms"].([]any); len(terms) != 1 || terms[0] != "se_registration_terms" {
		t.Fatalf("acceptedTerms = %v", item["acceptedTerms"])
	}

	status = "completed"
	if state, err := c.RegisterDomain(context.Background(), "acme.com"); err != nil || state != registrar.StateSucceeded {
		t.Fatalf("completed → %q err=%v", state, err)
	}
	status = "failed"
	if state, err := c.RegisterDomain(context.Background(), "acme.com"); err == nil || state != registrar.StateFailed {
		t.Fatalf("failed → %q err=%v", state, err)
	}
}

func TestRegisterDomain_NoTermsForGTLD(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"ord_1","status":"pending"}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "tok", "").RegisterDomain(context.Background(), "acme.com"); err != nil {
		t.Fatalf("register: %v", err)
	}
	items, _ := gotBody["items"].([]any)
	item, _ := items[0].(map[string]any)
	if _, has := item["acceptedTerms"]; has {
		t.Fatalf("gTLD order should not carry .se terms: %v", item)
	}
}

func TestRegistrationStatus(t *testing.T) {
	item := `{"name":"acme.se","available":false,"existingDomainId":"dom_1","existingDomainServiceStatus":"active"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[`+item+`]}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", "")

	if state, err := c.RegistrationStatus(context.Background(), "acme.se"); err != nil || state != registrar.StateSucceeded {
		t.Fatalf("ours+active → %q err=%v", state, err)
	}
	item = `{"name":"acme.se","available":false,"existingDomainId":"dom_1","existingDomainServiceStatus":"pending"}`
	if state, _ := c.RegistrationStatus(context.Background(), "acme.se"); state != registrar.StateInProgress {
		t.Fatalf("ours+pending → %q", state)
	}
	item = `{"name":"acme.se","available":true}`
	if state, _ := c.RegistrationStatus(context.Background(), "acme.se"); state != registrar.StatePending {
		t.Fatalf("still available → %q", state)
	}
}

func TestZoneID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/dns-zones" || r.URL.Query().Get("name") != "acme.se" {
			t.Errorf("path=%q name=%q", r.URL.Path, r.URL.Query().Get("name"))
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"zone_1","name":"acme.se","status":"active"}],"hasMore":false}`)
	}))
	defer srv.Close()

	id, err := New(srv.URL, "tok", "").ZoneID(context.Background(), "acme.se")
	if err != nil || id != "zone_1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

func TestZoneID_NoZoneYet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"hasMore":false}`)
	}))
	defer srv.Close()

	id, err := New(srv.URL, "tok", "").ZoneID(context.Background(), "acme.se")
	if err != nil || id != "" {
		t.Fatalf("expected empty id no error, got id=%q err=%v", id, err)
	}
}

func TestEnsureDNSRecord_RelativizesAndCreates(t *testing.T) {
	var created recordWire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/dns-zones/zone_1" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"zone_1","name":"gutka.org"}`)
		case strings.HasSuffix(r.URL.Path, "/records") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"data":[{"id":"drr_0","type":"NS","name":"@","value":"ns1.hostup.se","ttl":3600}]}`)
		case strings.HasSuffix(r.URL.Path, "/records") && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"drr_1"}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "tok", "")

	// Apex FQDN → "@".
	err := c.EnsureDNSRecord(context.Background(), "zone_1",
		registrar.Record{Type: "A", Name: "gutka.org", Content: "66.0.0.1"})
	if err != nil {
		t.Fatalf("ensure apex: %v", err)
	}
	if created.Name != "@" || created.Type != "A" || created.Value != "66.0.0.1" || created.TTL == 0 {
		t.Fatalf("created = %+v", created)
	}

	// Sub-name FQDN → relative label (zone name now cached — no extra zone GET).
	err = c.EnsureDNSRecord(context.Background(), "zone_1",
		registrar.Record{Type: "CNAME", Name: "_acme-challenge.gutka.org", Content: "gutka.org.x.flydns.net"})
	if err != nil {
		t.Fatalf("ensure sub: %v", err)
	}
	if created.Name != "_acme-challenge" {
		t.Fatalf("relativized name = %q", created.Name)
	}
}

func TestEnsureDNSRecord_IdempotentAgainstFQDNListing(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/dns-zones/zone_1":
			_, _ = io.WriteString(w, `{"id":"zone_1","name":"gutka.org"}`)
		case strings.HasSuffix(r.URL.Path, "/records") && r.Method == http.MethodGet:
			// Hostup may return normalized FQDN names in listings.
			_, _ = io.WriteString(w, `{"data":[{"id":"drr_1","type":"A","name":"gutka.org","value":"66.0.0.1","ttl":300}]}`)
		case r.Method == http.MethodPost:
			posted = true
		}
	}))
	defer srv.Close()

	err := New(srv.URL, "tok", "").EnsureDNSRecord(context.Background(), "zone_1",
		registrar.Record{Type: "A", Name: "gutka.org", Content: "66.0.0.1"})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if posted {
		t.Fatal("should not re-create an existing record")
	}
}

func TestProblemDetailSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"about:blank","title":"Forbidden","status":403,"detail":"missing scope write:orders","code":"forbidden_scope"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "tok", "").RegisterDomain(context.Background(), "acme.com")
	if err == nil || !strings.Contains(err.Error(), "missing scope write:orders") {
		t.Fatalf("expected surfaced problem detail, got %v", err)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Mitt Bageri":  "mittbageri",
		"Kött & Fläsk": "kottflask",
		"åäö":          "aao",
		"!!!":          "",
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
