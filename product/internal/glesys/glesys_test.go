package glesys

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

func testRegistrant() Registrant {
	return Registrant{
		Firstname: "Rasmus", Lastname: "Kockum", Organization: "Transcend Software",
		NationalID: 5566778899, Address: "Storgatan 1", City: "Stockholm",
		ZipCode: "11122", Country: "SE", Email: "rasmus@transcendsoftware.se", PhoneNumber: "+46700000000",
	}
}

// newMockClient wires a client to an httptest server running h.
func newMockClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return newTest(srv.URL, testRegistrant())
}

// TestAvailable_DecodesStringTypedResponse is the regression for the live bug:
// GleSYS returns available/amount/years as JSON STRINGS, and the request needs
// a named "search" arg. Assert both.
func TestAvailable_DecodesStringTypedResponse(t *testing.T) {
	var gotPath, gotSearch string
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var b struct {
			Search string `json:"search"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		gotSearch = b.Search
		if _, _, ok := r.BasicAuth(); !ok {
			t.Error("availability request missing Basic auth")
		}
		_, _ = io.WriteString(w, `{"response":{"domain":[{"domainname":"mittbageri.se","available":"yes",`+
			`"prices":[{"amount":"129.00","currency":"SEK","years":"1"},{"amount":"1290","currency":"SEK","years":"10"}]}]}}`)
	})

	offers, err := c.SearchDomains(context.Background(), "MittBageri.se", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotPath != "/domain/available" {
		t.Errorf("path = %q, want /domain/available", gotPath)
	}
	if gotSearch != "mittbageri.se" {
		t.Errorf("search arg = %q, want mittbageri.se", gotSearch)
	}
	if len(offers) != 1 {
		t.Fatalf("want 1 offer, got %d", len(offers))
	}
	if o := offers[0]; o.Name != "mittbageri.se" || !o.Registrable || o.Price != 129 || o.Currency != "SEK" || o.Renewal != 129 {
		t.Fatalf("bad offer mapping (string-typed): %+v", o)
	}
	if !offers[0].Buyable(300) {
		t.Errorf("129 SEK should be buyable under a 300 cap")
	}
}

// TestAvailable_DecodesNativeTypedResponse: the flex scalars also accept real
// JSON bools/numbers (the SDK's fixture shape), so we're robust either way.
func TestAvailable_DecodesNativeTypedResponse(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"response":{"domain":[{"domainname":"x.se","available":true,`+
			`"prices":[{"amount":99,"currency":"SEK","years":1}]}]}}`)
	})
	offers, err := c.CheckDomains(context.Background(), []string{"x.se"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(offers) != 1 || !offers[0].Registrable || offers[0].Price != 99 {
		t.Fatalf("native-typed mapping failed: %+v", offers)
	}
}

func TestAvailable_UnavailableAndNonSEK(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"response":{"domain":[`+
			`{"domainname":"x.io","available":"yes","prices":[{"amount":"30","currency":"EUR","years":"1"}]},`+
			`{"domainname":"y.se","available":"no","prices":[{"amount":"99","currency":"SEK","years":"1"}]}]}}`)
	})
	offers, err := c.CheckDomains(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, o := range offers {
		if o.Registrable {
			t.Errorf("%s should not be registrable (EUR / taken)", o.Name)
		}
	}
}

func TestAvailable_SurfacesError(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"response":{"status":{"code":401,"text":"Invalid credentials"}}}`)
	})
	if _, err := c.SearchDomains(context.Background(), "x.se", 5); err == nil {
		t.Fatal("a non-200 availability response should be an error, not empty results")
	}
}

func TestRegisterDomain_UsesRegistrantAndAutoRenew(t *testing.T) {
	var regParams map[string]any
	autoRenew := false
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/domain/register":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &regParams)
			_, _ = io.WriteString(w, `{"response":{"domain":{"domainname":"mittbageri.se",`+
				`"registrarinfo":{"state":"REGISTER","autorenew":"yes"}}}}`)
		case "/domain/setautorenew":
			autoRenew = true
			_, _ = io.WriteString(w, `{"response":{"domain":{"domainname":"mittbageri.se"}}}`)
		default:
			http.NotFound(w, r)
		}
	})

	state, err := c.RegisterDomain(context.Background(), "mittbageri.se")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state != registrar.StatePending { // "REGISTER" → still registering
		t.Errorf("state = %q, want pending", state)
	}
	if regParams["domainname"] != "mittbageri.se" || regParams["numyears"] != float64(1) {
		t.Errorf("register params domainname/numyears: %+v", regParams)
	}
	if regParams["organization"] != "Transcend Software" || regParams["nationalid"] != float64(5566778899) ||
		regParams["country"] != "SE" || regParams["email"] == "" {
		t.Errorf("registrant not applied: %+v", regParams)
	}
	if !autoRenew {
		t.Error("auto-renew not set after registration")
	}
}

// TestRegisterDomain_RegistrarInfoAsString: GleSYS may return registrarinfo as
// the string "None" instead of an object — must not fail the decode.
func TestRegisterDomain_RegistrarInfoAsString(t *testing.T) {
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/domain/register" {
			_, _ = io.WriteString(w, `{"response":{"domain":{"domainname":"x.se","registrarinfo":"None"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"response":{}}`)
	})
	state, err := c.RegisterDomain(context.Background(), "x.se")
	if err != nil {
		t.Fatalf("register with string registrarinfo: %v", err)
	}
	if state != registrar.StatePending { // empty state → pending
		t.Errorf("state = %q, want pending", state)
	}
}

func TestRegistrationStatus(t *testing.T) {
	// A registered domain reports state "OK" → succeeded.
	ok := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"response":{"domain":{"registrarinfo":{"state":"OK"}}}}`)
	})
	if st, err := ok.RegistrationStatus(context.Background(), "x.se"); err != nil || st != registrar.StateSucceeded {
		t.Fatalf("OK → succeeded: st=%q err=%v", st, err)
	}
	// A 404 (domain not in the account yet) → pending, not an error.
	nf := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"response":{"status":{"text":"Not Found"}}}`)
	})
	if st, err := nf.RegistrationStatus(context.Background(), "x.se"); err != nil || st != registrar.StatePending {
		t.Fatalf("404 → pending: st=%q err=%v", st, err)
	}
}

func TestZoneID(t *testing.T) {
	// In the account → its name is the zone key.
	in := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"response":{"domain":{"registrarinfo":{"state":"OK"}}}}`)
	})
	if id, err := in.ZoneID(context.Background(), "x.se"); err != nil || id != "x.se" {
		t.Fatalf("zone ready: id=%q err=%v", id, err)
	}
	// Not in DNS yet (404) → empty id, no error (caller retries).
	nf := newMockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"response":{"status":{"text":"Not Found"}}}`)
	})
	if id, err := nf.ZoneID(context.Background(), "x.se"); err != nil || id != "" {
		t.Fatalf("zone not ready should be (\"\", nil): id=%q err=%v", id, err)
	}
}

func TestEnsureDNSRecord_RelativizesAndCreates(t *testing.T) {
	var added []map[string]any
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/domain/listrecords":
			_, _ = io.WriteString(w, `{"response":{"records":[]}}`)
		case "/domain/addrecord":
			var p map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &p)
			added = append(added, p)
			_, _ = io.WriteString(w, `{"response":{"record":{"recordid":"1"}}}`)
		default:
			http.NotFound(w, r)
		}
	})
	// Apex A record: FQDN "acme.se" relativizes to "@".
	if err := c.EnsureDNSRecord(context.Background(), "acme.se",
		registrar.Record{Type: "A", Name: "acme.se", Content: "1.2.3.4"}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Sub record: "_acme-challenge.acme.se" → "_acme-challenge".
	if err := c.EnsureDNSRecord(context.Background(), "acme.se",
		registrar.Record{Type: "TXT", Name: "_acme-challenge.acme.se", Content: "token"}); err != nil {
		t.Fatalf("ensure txt: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("want 2 records added, got %d", len(added))
	}
	if added[0]["host"] != "@" || added[0]["type"] != "A" || added[0]["data"] != "1.2.3.4" {
		t.Errorf("apex record wrong: %+v", added[0])
	}
	if added[1]["host"] != "_acme-challenge" {
		t.Errorf("sub host = %v, want _acme-challenge", added[1]["host"])
	}
}

// TestEnsureDNSRecord_Idempotent also proves string-typed recordid/ttl in the
// list response don't break decoding (we don't decode them).
func TestEnsureDNSRecord_Idempotent(t *testing.T) {
	addCalled := false
	c := newMockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/domain/listrecords":
			_, _ = io.WriteString(w, `{"response":{"records":[{"recordid":"7","host":"@","type":"A","data":"1.2.3.4","ttl":"300"}]}}`)
		case "/domain/addrecord":
			addCalled = true
			_, _ = io.WriteString(w, `{"response":{}}`)
		default:
			http.NotFound(w, r)
		}
	})
	if err := c.EnsureDNSRecord(context.Background(), "acme.se",
		registrar.Record{Type: "A", Name: "acme.se", Content: "1.2.3.4"}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if addCalled {
		t.Error("identical record must not be re-added")
	}
}

func TestMapState(t *testing.T) {
	cases := map[string]string{
		"OK":         registrar.StateSucceeded,
		"active":     registrar.StateSucceeded,
		"REGISTERED": registrar.StateSucceeded,
		"delegated":  registrar.StateSucceeded, // unknown-but-live → provision, cert is the real gate
		"":           registrar.StatePending,
		"REGISTER":   registrar.StatePending,
		"PENDING":    registrar.StatePending,
		"processing": registrar.StatePending,
		"transfer":   registrar.StatePending,
		"FAILED":     registrar.StateFailed,
		"error":      registrar.StateFailed,
		"quarantine": registrar.StateFailed,
	}
	for raw, want := range cases {
		if got := mapState("x.se", raw); got != want {
			t.Errorf("mapState(%q) = %q, want %q", raw, got, want)
		}
	}
}
