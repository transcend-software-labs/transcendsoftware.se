// Package glesys drives custom domains through the GleSYS API
// (github.com/glesys/glesys-go), the registrar we use for .se and the other
// TLDs a customer might want. Unlike the bespoke Cloudflare/Hostup HTTP
// clients, this wraps GleSYS's official SDK: its DNSDomainService covers the
// registration, registrar-state and DNS-record calls we need.
//
// One exception: availability. The SDK's DNSDomainService.Available sends the
// domain as a bare JSON string body, but GleSYS's domain/available expects a
// named "search" argument, so the SDK call always fails ("unavailable" in the
// UI). We therefore make the availability request ourselves with the correct
// body, reusing the same Basic auth + base URL, and use the SDK for everything
// else (register/details/records, which send correctly-named args and work).
//
// It implements the same orchestrator.DomainRegistrar surface as
// internal/cloudflare and internal/hostup, so the provider is chosen at wiring
// time (cmd/server/main.go).
//
// GleSYS notes:
//   - Auth is HTTP Basic: username = the project key ("CL12345"), password =
//     the API key.
//   - Registration is tied to a named registrant (contact details are
//     required); we register every domain to Forge's own company contact, set
//     once at wiring time (config.Registrant). The customer pays the
//     registration cost on their next invoice.
//   - DNS records are keyed by domain name (not a zone id), so ZoneID returns
//     the domain name itself and EnsureDNSRecord relativizes FQDNs to the
//     GleSYS "host" form ("@" for the apex).
package glesys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	glesysgo "github.com/glesys/glesys-go/v8"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

const defaultBaseURL = "https://api.glesys.com"

// Registrant is Forge's own contact, used as the registrant on every
// registration (GleSYS requires full contact details). NationalID is the
// company's organisationsnummer for .se; for a company these are public
// business data, so they live in config, not a secret.
type Registrant struct {
	Firstname    string
	Lastname     string
	Organization string
	NationalID   int
	Address      string
	City         string
	ZipCode      string
	Country      string // ISO code, e.g. "SE"
	Email        string
	PhoneNumber  string
}

// dnsService is the slice of glesys-go's DNSDomainService we use — narrowed to
// an interface so tests can substitute a fake without a live SDK. Note it does
// NOT include Available: that call is broken in the SDK (see the package doc),
// so we make it ourselves against the REST endpoint.
type dnsService interface {
	Register(ctx context.Context, params glesysgo.RegisterDNSDomainParams) (*glesysgo.DNSDomain, error)
	SetAutoRenew(ctx context.Context, params glesysgo.SetAutoRenewParams) (*glesysgo.DNSDomain, error)
	Details(ctx context.Context, domainname string) (*glesysgo.DNSDomain, error)
	ListRecords(ctx context.Context, domainname string) (*[]glesysgo.DNSDomainRecord, error)
	AddRecord(ctx context.Context, params glesysgo.AddRecordParams) (*glesysgo.DNSDomainRecord, error)
}

// Client talks to GleSYS: the SDK for register/details/records, and a direct
// REST call for availability (where the SDK is broken).
type Client struct {
	dns        dnsService
	http       *http.Client
	baseURL    string
	project    string // Basic-auth username (project key, "CL12345")
	apiKey     string // Basic-auth password
	registrant Registrant
}

// New returns a client. project is the GleSYS project key (Basic-auth
// username, e.g. "CL12345"); apiKey is the project's API key. reg is the
// registrant every domain is registered to.
func New(project, apiKey string, reg Registrant) *Client {
	sdk := glesysgo.NewClient(project, apiKey, "transcend-forge/1.0")
	return &Client{
		dns:        sdk.DNSDomains,
		http:       &http.Client{Timeout: 25 * time.Second},
		baseURL:    defaultBaseURL,
		project:    project,
		apiKey:     apiKey,
		registrant: reg,
	}
}

// newTest is the test seam: inject a fake dnsService for the SDK-backed calls
// and point the availability call at a test server (baseURL "" keeps the real
// host for tests that don't touch availability).
func newTest(dns dnsService, baseURL string, reg Registrant) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		dns:        dns,
		http:       &http.Client{Timeout: 10 * time.Second},
		baseURL:    baseURL,
		project:    "CLtest",
		apiKey:     "test-key",
		registrant: reg,
	}
}

// offer maps a GleSYS DNSDomain to the neutral Offer, taking the one-year
// registration price. Non-SEK offers are marked unregistrable — the whole
// billing chain (Stripe subscription, the invoice item) is SEK, and we don't
// convert.
func offer(d glesysgo.DNSDomain) registrar.Offer {
	o := registrar.Offer{Name: d.Name, Registrable: d.Available}
	for _, pr := range d.Prices {
		if pr.Years == 1 {
			o.Price = pr.Amount
			o.Currency = pr.Currency
			// GleSYS reports the registration price; renewals are billed at cost
			// yearly, so use the same figure for the renewal cap check.
			o.Renewal = pr.Amount
			break
		}
	}
	if !strings.EqualFold(o.Currency, "SEK") {
		o.Registrable = false
	}
	return o
}

// available calls GleSYS domain/available with the correctly-named "search"
// argument (the SDK omits it, sending a bare string). A full domain incl. TLD
// returns just that domain; a bare keyword fans out across every TLD (slow —
// which is why the UI requires a full domain).
func (c *Client) available(ctx context.Context, search string) ([]glesysgo.DNSDomain, error) {
	body, err := json.Marshal(struct {
		Search string `json:"search"`
	}{search})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/domain/available", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.project, c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("glesys: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Response struct {
				Status struct {
					Text string `json:"text"`
				} `json:"status"`
			} `json:"response"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, fmt.Errorf("glesys: available %q: status %d: %s", search, resp.StatusCode, strings.TrimSpace(e.Response.Status.Text))
	}
	var out struct {
		Response struct {
			Domain []glesysgo.DNSDomain `json:"domain"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("glesys: available %q: decode: %w", search, err)
	}
	return out.Response.Domain, nil
}

// SearchDomains checks availability + the one-year price for a query. The web
// layer requires a full domain (name + TLD), so this is a single fast lookup.
func (c *Client) SearchDomains(ctx context.Context, query string, limit int) ([]registrar.Offer, error) {
	return c.CheckDomains(ctx, []string{strings.ToLower(strings.TrimSpace(query))})
}

// CheckDomains returns availability + one-year price (SEK) for specific names.
func (c *Client) CheckDomains(ctx context.Context, names []string) ([]registrar.Offer, error) {
	var offers []registrar.Offer
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		domains, err := c.available(ctx, name)
		if err != nil {
			return nil, err
		}
		for _, d := range domains {
			offers = append(offers, offer(d))
		}
	}
	return offers, nil
}

// RegisterDomain registers a domain to Forge's contact for one year and turns
// on auto-renew so a customer site never lapses (re-billing renewals is a
// follow-up). It returns the neutral workflow state from the registrar.
func (c *Client) RegisterDomain(ctx context.Context, name string) (string, error) {
	d, err := c.dns.Register(ctx, glesysgo.RegisterDNSDomainParams{
		Name:         name,
		NumYears:     1,
		Firstname:    c.registrant.Firstname,
		Lastname:     c.registrant.Lastname,
		Organization: c.registrant.Organization,
		NationalID:   c.registrant.NationalID,
		Address:      c.registrant.Address,
		City:         c.registrant.City,
		ZipCode:      c.registrant.ZipCode,
		Country:      c.registrant.Country,
		Email:        c.registrant.Email,
		PhoneNumber:  c.registrant.PhoneNumber,
	})
	if err != nil {
		return "", fmt.Errorf("glesys: register %q: %w", name, err)
	}
	// Best-effort: keep the domain from lapsing. A fresh .se may still be
	// settling, so a failure here is logged, not fatal — GleSYS also defaults new
	// registrations to auto-renew.
	if _, err := c.dns.SetAutoRenew(ctx, glesysgo.SetAutoRenewParams{Name: name, SetAutoRenew: "yes"}); err != nil {
		slog.Warn("glesys set auto-renew", "domain", name, "err", err)
	}
	return mapState(name, d.RegistrarInfo.State), nil
}

// RegistrationStatus reports how far a registration has come, from the
// registrar state on the domain's details.
func (c *Client) RegistrationStatus(ctx context.Context, name string) (string, error) {
	d, err := c.dns.Details(ctx, name)
	if err != nil {
		return "", fmt.Errorf("glesys: details %q: %w", name, err)
	}
	return mapState(name, d.RegistrarInfo.State), nil
}

// mapState maps GleSYS's registrar state onto the neutral workflow states. The
// vocabulary isn't exhaustively documented (seen: "OK" for a live domain,
// "REGISTER" while registering), so it's logged, and — like the Hostup client
// — anything that isn't obviously still-being-set-up or failed is treated as
// live: the Fly cert step (itself retried by the poller) is the real liveness
// gate, so provisioning early is safe and avoids a live domain sitting until
// the 72h stuck-timeout under an unrecognized state word.
func mapState(name, raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	slog.Info("glesys registration status", "domain", name, "state", raw)
	switch {
	case s == "":
		return registrar.StatePending
	case containsAny(s, "fail", "error", "reject", "cancel", "expired", "quarantine", "blocked", "deleted"):
		return registrar.StateFailed
	// "REGISTER" is GleSYS's in-progress state; guard the exact/gerund forms so a
	// completed "REGISTERED" isn't misread as still-registering by the substring.
	case s == "register" || strings.Contains(s, "registering") ||
		containsAny(s, "pending", "process", "progress", "queue", "wait", "new", "request", "transfer", "await"):
		return registrar.StatePending
	default:
		return registrar.StateSucceeded
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ZoneID returns the key used for DNS-record operations. GleSYS keys records by
// domain name, so once the domain exists in the account (Details succeeds) the
// name itself is the "zone id". An empty return (no error) means the domain
// isn't in DNS yet — the caller retries (registration creates it).
func (c *Client) ZoneID(ctx context.Context, name string) (string, error) {
	if _, err := c.dns.Details(ctx, name); err != nil {
		// Not in the account yet — treat as "no zone", retry next pass rather than
		// surfacing a hard error that would fail the whole reconcile.
		slog.Info("glesys zone not ready", "domain", name, "err", err)
		return "", nil
	}
	return name, nil
}

// EnsureDNSRecord creates rec in the domain if an identical one (type+host+data)
// isn't already present, so it is safe to re-run. Record names are relativized
// to the GleSYS "host" form ("acme.se" → "@", "_acme-challenge.acme.se" →
// "_acme-challenge"). zoneID is the domain name (see ZoneID).
func (c *Client) EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error {
	name := zoneID
	host := relativize(rec.Name, name)

	existing, err := c.dns.ListRecords(ctx, name)
	if err != nil {
		return fmt.Errorf("glesys: list records %q: %w", name, err)
	}
	if existing != nil {
		for _, e := range *existing {
			if strings.EqualFold(e.Type, rec.Type) &&
				strings.EqualFold(relativize(e.Host, name), host) &&
				trimQuotes(e.Data) == trimQuotes(rec.Content) {
				return nil // already present — idempotent
			}
		}
	}

	ttl := rec.TTL
	if ttl == 0 {
		ttl = 300 // low default so validation records propagate fast
	}
	if _, err := c.dns.AddRecord(ctx, glesysgo.AddRecordParams{
		DomainName: name, Host: host, Type: rec.Type, Data: rec.Content, TTL: ttl,
	}); err != nil {
		return fmt.Errorf("glesys: add record %s %s: %w", rec.Type, host, err)
	}
	return nil
}

// relativize converts a FQDN to a GleSYS-relative host: the apex becomes "@",
// hosts under the domain lose the domain suffix, and anything already relative
// passes through.
func relativize(name, domain string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	switch {
	case n == "" || n == "@" || n == d:
		return "@"
	case strings.HasSuffix(n, "."+d):
		return strings.TrimSuffix(n, "."+d)
	default:
		return n
	}
}

// trimQuotes strips surrounding quotes for value comparison — registries apply
// DNS-safe quoting to TXT values.
func trimQuotes(s string) string { return strings.Trim(s, `"`) }
