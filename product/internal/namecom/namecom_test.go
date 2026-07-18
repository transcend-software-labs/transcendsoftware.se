package namecom

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

func newMockClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "forge-test", "tok", 10) // 10 SEK/USD for round numbers
}

func TestCheckDomains_MapsOffersAndConvertsCurrency(t *testing.T) {
	var gotBody map[string]any
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/core/v1/domains:checkAvailability" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if u, p, _ := r.BasicAuth(); u != "forge-test" || p != "tok" {
			t.Errorf("basic auth = %q/%q", u, p)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"results":[
			{"domainName":"acme.se","sld":"acme","tld":"se","purchasable":true,"purchaseType":"registration","purchasePrice":12.99,"renewalPrice":14.99},
			{"domainName":"taken.se","sld":"taken","tld":"se","purchasable":false},
			{"domainName":"fancy.com","sld":"fancy","tld":"com","purchasable":true,"premium":true,"purchaseType":"registration","purchasePrice":500},
			{"domainName":"drop.com","sld":"drop","tld":"com","purchasable":true,"purchaseType":"aftermarket","purchasePrice":99}
		]}`)
	})
	offers, err := c.CheckDomains(context.Background(), []string{"acme.se", "taken.se", "fancy.com", "drop.com"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if gotBody["purchaseType"] != "registration" {
		t.Errorf("check should restrict to registration, body: %v", gotBody)
	}
	if len(offers) != 4 {
		t.Fatalf("want 4 offers, got %d", len(offers))
	}
	acme := offers[0]
	if !acme.Registrable || acme.Price != 129.9 || acme.Renewal != 149.9 || acme.Currency != "SEK" {
		t.Errorf("acme.se offer wrong (USD→SEK conversion): %+v", acme)
	}
	if offers[1].Registrable {
		t.Error("a non-purchasable domain must not be registrable")
	}
	if offers[2].Registrable {
		t.Error("a premium domain must not be registrable (volatile pricing)")
	}
	if offers[3].Registrable {
		t.Error("an aftermarket acquisition must not be registrable")
	}
}

func TestSearchDomains_FiltersAndLimits(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/core/v1/domains:search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"results":[
			{"domainName":"a.se","sld":"a","tld":"se","purchasable":true,"purchaseType":"registration","purchasePrice":10},
			{"domainName":"b.se","sld":"b","tld":"se","purchasable":false},
			{"domainName":"c.se","sld":"c","tld":"se","purchasable":true,"purchaseType":"registration","purchasePrice":12},
			{"domainName":"d.se","sld":"d","tld":"se","purchasable":true,"purchaseType":"registration","purchasePrice":13}
		]}`)
	})
	offers, err := c.SearchDomains(context.Background(), "a", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(offers) != 2 || offers[0].Name != "a.se" || offers[1].Name != "c.se" {
		t.Fatalf("search should return the first 2 registrable results, got %+v", offers)
	}
}

func TestRegisterDomain_CreatesWithAutorenew(t *testing.T) {
	var created map[string]any
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/core/v1/domains/acme.se":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found","details":"domain does not exist"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/core/v1/domains":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &created)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("create must send Content-Type application/json")
			}
			_, _ = io.WriteString(w, `{"order":1234,"totalPaid":12.99,"domain":{"domainName":"acme.se","expireDate":"2027-07-18T12:00:00Z","autorenewEnabled":true}}`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	})
	state, err := c.RegisterDomain(context.Background(), "acme.se")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state != registrar.StateSucceeded {
		t.Errorf("state = %q, want succeeded (synchronous create)", state)
	}
	dom, _ := created["domain"].(map[string]any)
	if dom["domainName"] != "acme.se" || dom["autorenewEnabled"] != true {
		t.Errorf("create payload domain wrong: %v", created)
	}
	if created["purchaseType"] != "registration" || created["years"] != float64(1) {
		t.Errorf("create payload terms wrong: %v", created)
	}
}

// TestRegisterDomain_AlreadyOursSkipsOrder: the GleSYS lesson — a retry must
// never re-buy a domain the account already holds.
func TestRegisterDomain_AlreadyOursSkipsOrder(t *testing.T) {
	ordered := false
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/core/v1/domains/acme.se":
			_, _ = io.WriteString(w, `{"domainName":"acme.se","expireDate":"2027-01-01T00:00:00Z","autorenewEnabled":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/core/v1/domains":
			ordered = true
			_, _ = io.WriteString(w, `{"order":1,"totalPaid":1,"domain":{"domainName":"acme.se"}}`)
		}
	})
	state, err := c.RegisterDomain(context.Background(), "acme.se")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state != registrar.StateSucceeded || ordered {
		t.Fatalf("already-owned domain must not be re-ordered (state=%q ordered=%v)", state, ordered)
	}
}

// TestRegisterDomain_PaymentErrorSurfaces: a 402 (reseller account can't pay)
// must reach the caller verbatim — that's an operator problem, not a retry.
func TestRegisterDomain_PaymentErrorSurfaces(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"message":"Payment failed","details":"Insufficient Funds"}`)
	})
	_, err := c.RegisterDomain(context.Background(), "acme.se")
	if err == nil || !strings.Contains(err.Error(), "Insufficient Funds") {
		t.Fatalf("want the payment error surfaced, got %v", err)
	}
}

func TestRegistrationStatusAndZoneAndExpiry(t *testing.T) {
	inAccount := false
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !inAccount {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_, _ = io.WriteString(w, `{"domainName":"acme.se","expireDate":"2027-07-18T12:00:00Z","autorenewEnabled":true}`)
	})
	ctx := context.Background()

	if st, err := c.RegistrationStatus(ctx, "acme.se"); err != nil || st != registrar.StatePending {
		t.Errorf("not ours → pending, got %q %v", st, err)
	}
	if z, err := c.ZoneID(ctx, "acme.se"); err != nil || z != "" {
		t.Errorf("not ours → empty zone, got %q %v", z, err)
	}
	if ts, err := c.DomainExpiry(ctx, "acme.se"); err != nil || !ts.IsZero() {
		t.Errorf("not ours → zero expiry, got %v %v", ts, err)
	}

	inAccount = true
	if st, err := c.RegistrationStatus(ctx, "acme.se"); err != nil || st != registrar.StateSucceeded {
		t.Errorf("ours → succeeded, got %q %v", st, err)
	}
	if z, err := c.ZoneID(ctx, "acme.se"); err != nil || z != "acme.se" {
		t.Errorf("ours → zone = name, got %q %v", z, err)
	}
	ts, err := c.DomainExpiry(ctx, "acme.se")
	if err != nil || ts.IsZero() || ts.Year() != 2027 {
		t.Errorf("expiry parse failed: %v %v", ts, err)
	}
}

func TestEnsureDNSRecord_RelativizesAndIsIdempotent(t *testing.T) {
	var added []map[string]any
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"records":[{"id":1,"host":"","type":"A","answer":"1.2.3.4","ttl":300}],"to":1,"from":1,"totalCount":1}`)
		case r.Method == http.MethodPost:
			var b map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &b)
			added = append(added, b)
			_, _ = io.WriteString(w, `{"id":2}`)
		}
	})
	ctx := context.Background()

	// Identical apex record (FQDN form) → no write.
	if err := c.EnsureDNSRecord(ctx, "acme.se", registrar.Record{Type: "A", Name: "acme.se", Content: "1.2.3.4"}); err != nil {
		t.Fatalf("ensure existing: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("identical record must not be re-added: %v", added)
	}

	// A new subdomain record is relativized and created with the minimum TTL.
	if err := c.EnsureDNSRecord(ctx, "acme.se", registrar.Record{Type: "CNAME", Name: "_acme-challenge.acme.se", Content: "x.flydns.net"}); err != nil {
		t.Fatalf("ensure new: %v", err)
	}
	if len(added) != 1 || added[0]["host"] != "_acme-challenge" || added[0]["ttl"] != float64(300) {
		t.Fatalf("added record wrong: %v", added)
	}
}

func TestListRecords_FollowsPagination(t *testing.T) {
	calls := 0
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("page") == "" {
			_, _ = io.WriteString(w, `{"records":[{"host":"a","type":"A","answer":"1.1.1.1"}],"nextPage":2,"to":1,"from":1,"totalCount":2}`)
			return
		}
		_, _ = io.WriteString(w, `{"records":[{"host":"b","type":"A","answer":"2.2.2.2"}],"to":2,"from":2,"totalCount":2}`)
	})
	recs, err := c.listRecords(context.Background(), "acme.se")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 2 || calls != 2 {
		t.Fatalf("pagination not followed: %d records over %d calls", len(recs), calls)
	}
}

func TestSetAutoRenew_PicksVerb(t *testing.T) {
	var paths []string
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `{}`)
	})
	ctx := context.Background()
	if err := c.SetAutoRenew(ctx, "acme.se", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAutoRenew(ctx, "acme.se", false); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/core/v1/domains/acme.se:enableAutorenew" || paths[1] != "/core/v1/domains/acme.se:disableAutorenew" {
		t.Fatalf("autorenew verbs wrong: %v", paths)
	}
}

func TestRelativize(t *testing.T) {
	cases := map[string]string{
		"acme.se":                 "",
		"@":                       "",
		"":                        "",
		"_acme-challenge.acme.se": "_acme-challenge",
		"www.acme.se":             "www",
		"already-relative":        "already-relative",
		"Acme.SE.":                "",
	}
	for in, want := range cases {
		if got := relativize(in, "acme.se"); got != want {
			t.Errorf("relativize(%q) = %q, want %q", in, got, want)
		}
	}
}
