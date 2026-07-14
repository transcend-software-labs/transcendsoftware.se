package glesys

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	glesysgo "github.com/glesys/glesys-go/v8"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

// fakeDNS is an in-memory stand-in for the SDK's DNSDomainService (register /
// details / records) — NOT availability, which the client calls over REST. It
// records calls so tests can assert the request shape.
type fakeDNS struct {
	registered   []glesysgo.RegisterDNSDomainParams
	regState     string
	autoRenew    []glesysgo.SetAutoRenewParams
	autoRenewErr error
	detailsState string
	detailsErr   error
	records      []glesysgo.DNSDomainRecord
	added        []glesysgo.AddRecordParams
}

func (f *fakeDNS) Register(_ context.Context, p glesysgo.RegisterDNSDomainParams) (*glesysgo.DNSDomain, error) {
	f.registered = append(f.registered, p)
	st := f.regState
	if st == "" {
		st = "REGISTER"
	}
	return &glesysgo.DNSDomain{Name: p.Name, RegistrarInfo: glesysgo.RegistrarInfo{State: st}}, nil
}
func (f *fakeDNS) SetAutoRenew(_ context.Context, p glesysgo.SetAutoRenewParams) (*glesysgo.DNSDomain, error) {
	f.autoRenew = append(f.autoRenew, p)
	return &glesysgo.DNSDomain{Name: p.Name}, f.autoRenewErr
}
func (f *fakeDNS) Details(_ context.Context, name string) (*glesysgo.DNSDomain, error) {
	if f.detailsErr != nil {
		return nil, f.detailsErr
	}
	return &glesysgo.DNSDomain{Name: name, RegistrarInfo: glesysgo.RegistrarInfo{State: f.detailsState}}, nil
}
func (f *fakeDNS) ListRecords(_ context.Context, _ string) (*[]glesysgo.DNSDomainRecord, error) {
	r := f.records
	return &r, nil
}
func (f *fakeDNS) AddRecord(_ context.Context, p glesysgo.AddRecordParams) (*glesysgo.DNSDomainRecord, error) {
	f.added = append(f.added, p)
	return &glesysgo.DNSDomainRecord{DomainName: p.DomainName, Host: p.Host, Type: p.Type, Data: p.Data, TTL: p.TTL}, nil
}

func testRegistrant() Registrant {
	return Registrant{
		Firstname: "Rasmus", Lastname: "Kockum", Organization: "Transcend Software",
		NationalID: 5566778899, Address: "Storgatan 1", City: "Stockholm",
		ZipCode: "11122", Country: "SE", Email: "rasmus@transcendsoftware.se", PhoneNumber: "+46700000000",
	}
}

// TestSearchDomains_SendsSearchArg guards the SDK-bug workaround: GleSYS
// domain/available needs a named "search" argument (the SDK sends a bare
// string), so assert our request body carries it, and that the 1-year price is
// mapped from the response.
func TestSearchDomains_SendsSearchArg(t *testing.T) {
	var gotSearch string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Search string `json:"search"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotSearch = body.Search
		if _, _, ok := r.BasicAuth(); !ok {
			t.Error("availability request missing Basic auth")
		}
		_, _ = io.WriteString(w, `{"response":{"domain":[{"domainname":"mittbageri.se","available":true,`+
			`"prices":[{"amount":129,"currency":"SEK","years":1},{"amount":1290,"currency":"SEK","years":10}]}]}}`)
	}))
	defer srv.Close()

	c := newTest(&fakeDNS{}, srv.URL, testRegistrant())
	offers, err := c.SearchDomains(context.Background(), "MittBageri.se", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotPath != "/domain/available" {
		t.Errorf("path = %q, want /domain/available", gotPath)
	}
	if gotSearch != "mittbageri.se" {
		t.Errorf("search arg = %q, want mittbageri.se (the bare-string SDK bug)", gotSearch)
	}
	if len(offers) != 1 {
		t.Fatalf("want 1 offer, got %d", len(offers))
	}
	if o := offers[0]; o.Name != "mittbageri.se" || !o.Registrable || o.Price != 129 || o.Currency != "SEK" || o.Renewal != 129 {
		t.Fatalf("bad offer mapping: %+v", o)
	}
	if !offers[0].Buyable(300) {
		t.Errorf("129 SEK should be buyable under a 300 cap")
	}
}

func TestSearchDomains_SurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"response":{"status":{"code":401,"text":"Invalid credentials"}}}`)
	}))
	defer srv.Close()
	c := newTest(&fakeDNS{}, srv.URL, testRegistrant())
	if _, err := c.SearchDomains(context.Background(), "x.se", 5); err == nil {
		t.Fatal("a non-200 availability response should be an error, not empty results")
	}
}

func TestOffer_UnavailableAndNonSEK(t *testing.T) {
	// Taken → not registrable.
	if o := offer(glesysgo.DNSDomain{Name: "x.se", Available: false,
		Prices: []glesysgo.DNSDomainPrice{{Amount: 100, Currency: "SEK", Years: 1}}}); o.Registrable {
		t.Error("unavailable domain must not be registrable")
	}
	// Priced in EUR → we don't convert, so it's excluded.
	if o := offer(glesysgo.DNSDomain{Name: "x.io", Available: true,
		Prices: []glesysgo.DNSDomainPrice{{Amount: 30, Currency: "EUR", Years: 1}}}); o.Registrable {
		t.Error("non-SEK domain must not be registrable")
	}
}

func TestRegisterDomain_UsesRegistrantAndAutoRenew(t *testing.T) {
	f := &fakeDNS{regState: "REGISTER"}
	c := newTest(f, "", testRegistrant())

	state, err := c.RegisterDomain(context.Background(), "mittbageri.se")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if state != registrar.StatePending { // "REGISTER" → still registering
		t.Errorf("state = %q, want pending", state)
	}
	if len(f.registered) != 1 {
		t.Fatalf("want 1 register call, got %d", len(f.registered))
	}
	p := f.registered[0]
	if p.Name != "mittbageri.se" || p.NumYears != 1 {
		t.Errorf("register params name/years: %+v", p)
	}
	if p.Organization != "Transcend Software" || p.NationalID != 5566778899 || p.Country != "SE" || p.Email == "" {
		t.Errorf("registrant not applied: %+v", p)
	}
	if len(f.autoRenew) != 1 || f.autoRenew[0].SetAutoRenew != "yes" || f.autoRenew[0].Name != "mittbageri.se" {
		t.Errorf("auto-renew not set: %+v", f.autoRenew)
	}
}

func TestRegisterDomain_AutoRenewFailureNonFatal(t *testing.T) {
	f := &fakeDNS{regState: "OK", autoRenewErr: errors.New("not settled")}
	c := newTest(f, "", testRegistrant())
	state, err := c.RegisterDomain(context.Background(), "x.se")
	if err != nil {
		t.Fatalf("auto-renew failure should not fail registration: %v", err)
	}
	if state != registrar.StateSucceeded {
		t.Errorf("state = %q, want succeeded", state)
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

func TestRegistrationStatus(t *testing.T) {
	c := newTest(&fakeDNS{detailsState: "OK"}, "", testRegistrant())
	st, err := c.RegistrationStatus(context.Background(), "x.se")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st != registrar.StateSucceeded {
		t.Errorf("state = %q, want succeeded", st)
	}
}

func TestZoneID(t *testing.T) {
	// Domain in the account → its name is the zone key.
	c := newTest(&fakeDNS{detailsState: "OK"}, "", testRegistrant())
	if id, err := c.ZoneID(context.Background(), "x.se"); err != nil || id != "x.se" {
		t.Fatalf("zone ready: id=%q err=%v", id, err)
	}
	// Not in DNS yet → empty id, no error (caller retries).
	c2 := newTest(&fakeDNS{detailsErr: errors.New("not found")}, "", testRegistrant())
	if id, err := c2.ZoneID(context.Background(), "x.se"); err != nil || id != "" {
		t.Fatalf("zone not ready should be (\"\", nil): id=%q err=%v", id, err)
	}
}

func TestEnsureDNSRecord_RelativizesAndCreates(t *testing.T) {
	f := &fakeDNS{}
	c := newTest(f, "", testRegistrant())
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
	if len(f.added) != 2 {
		t.Fatalf("want 2 records added, got %d", len(f.added))
	}
	if f.added[0].Host != "@" || f.added[0].Type != "A" || f.added[0].Data != "1.2.3.4" || f.added[0].TTL != 300 {
		t.Errorf("apex record wrong: %+v", f.added[0])
	}
	if f.added[1].Host != "_acme-challenge" {
		t.Errorf("sub host = %q, want _acme-challenge", f.added[1].Host)
	}
}

func TestEnsureDNSRecord_Idempotent(t *testing.T) {
	f := &fakeDNS{records: []glesysgo.DNSDomainRecord{
		{Host: "@", Type: "A", Data: "1.2.3.4"},
	}}
	c := newTest(f, "", testRegistrant())
	if err := c.EnsureDNSRecord(context.Background(), "acme.se",
		registrar.Record{Type: "A", Name: "acme.se", Content: "1.2.3.4"}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(f.added) != 0 {
		t.Errorf("identical record must not be re-added, added: %+v", f.added)
	}
}
