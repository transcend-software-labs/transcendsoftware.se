// Package namecom drives custom domains through the name.com Core API v1:
// availability + price, registration, DNS records, expiry and auto-renew. It
// replaces the earlier GleSYS/Hostup/Cloudflare registrars (scrapped 2026-07-18
// after GleSYS blocked registration account-side) and implements the same
// orchestrator.DomainRegistrar surface, so the provider swap is wiring-only.
//
// House style: a bespoke net/http client (see internal/billing, the late
// internal/glesys) decoding only the fields we use.
//
// name.com notes:
//   - Auth is HTTP Basic: username + API token. The sandbox environment
//     (https://api.dev.name.com) uses the account username with a "-test"
//     suffix and its own token — registrations there are simulated, which
//     finally allows a true end-to-end test without spending money.
//   - Registration is SYNCHRONOUS: CreateDomain returns the created domain.
//     No add-then-register dance, no pending polling in the common case.
//   - DNS records are keyed by the domain name ("zone id" = the name, like
//     GleSYS); record hosts are zone-relative ("" or "@" for the apex).
//   - Prices are USD. They are converted to SEK here, at the configured rate,
//     so everything downstream (the buy cap, DomainCostOre in öre, the Stripe
//     invoice line in SEK) keeps its existing semantics untouched.
//   - Contacts are optional on registration — the account's default contacts
//     apply, so no per-registrant config plumbing is needed.
package namecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

// Base URLs. Production serves real registrations billed to the reseller
// account; the dev host simulates them (separate credentials).
const (
	ProdBaseURL = "https://api.name.com"
	DevBaseURL  = "https://api.dev.name.com"
)

// Client talks to the name.com Core API (JSON in/out, Basic auth).
type Client struct {
	http      *http.Client
	baseURL   string
	username  string
	token     string
	sekPerUSD float64 // USD→SEK conversion for offers/prices (name.com bills USD)
}

// New returns a client. baseURL is ProdBaseURL or DevBaseURL (or an httptest
// server in tests); sekPerUSD converts name.com's USD prices into the SEK the
// rest of the pipeline works in.
func New(baseURL, username, token string, sekPerUSD float64) *Client {
	if sekPerUSD <= 0 {
		sekPerUSD = 10.5
	}
	return &Client{
		http:      &http.Client{Timeout: 30 * time.Second},
		baseURL:   strings.TrimRight(baseURL, "/"),
		username:  username,
		token:     token,
		sekPerUSD: sekPerUSD,
	}
}

// apiError is a non-2xx name.com response, keeping the HTTP status so callers
// can special-case 404 ("domain not in the account").
type apiError struct {
	op      string
	status  int
	message string
	details string
}

func (e *apiError) Error() string {
	if e.details != "" {
		return fmt.Sprintf("namecom %s: status %d: %s: %s", e.op, e.status, e.message, e.details)
	}
	return fmt.Sprintf("namecom %s: status %d: %s", e.op, e.status, e.message)
}

// isNotFound reports whether err is a name.com 404 (not in the account).
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

// do performs one API call: method + path (relative, may contain the ":verb"
// suffixes name.com uses), optional JSON body, decode into out.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json") // the API 415s without it
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("namecom %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
			Details string `json:"details"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{op: method + " " + path, status: resp.StatusCode,
			message: strings.TrimSpace(e.Message), details: strings.TrimSpace(e.Details)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("namecom %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// --- Discovery: search + availability ---------------------------------------

// searchResult is the shared shape of Search and CheckAvailability results.
type searchResult struct {
	DomainName    string  `json:"domainName"`
	Purchasable   bool    `json:"purchasable"`
	Premium       bool    `json:"premium"`
	PurchasePrice float64 `json:"purchasePrice"`
	RenewalPrice  float64 `json:"renewalPrice"`
	PurchaseType  string  `json:"purchaseType"`
}

// offer maps a name.com result to the provider-neutral Offer. Only plain
// registrations count as registrable: premium and aftermarket/expiring/
// backorder acquisitions have volatile pricing and non-instant fulfillment —
// per name.com's own reseller recommendation we don't sell those.
func (c *Client) offer(r searchResult) registrar.Offer {
	registration := r.PurchaseType == "" || strings.EqualFold(r.PurchaseType, "registration")
	o := registrar.Offer{
		Name:        strings.ToLower(r.DomainName),
		Registrable: r.Purchasable && registration && !r.Premium,
	}
	if o.Registrable {
		o.Price = r.PurchasePrice * c.sekPerUSD
		o.Renewal = r.RenewalPrice * c.sekPerUSD
		if o.Renewal == 0 {
			o.Renewal = o.Price
		}
		o.Currency = "SEK"
	}
	return o
}

// SearchDomains suggests registrable domains for a query (the UI search box).
func (c *Client) SearchDomains(ctx context.Context, query string, limit int) ([]registrar.Offer, error) {
	var out struct {
		Results []searchResult `json:"results"`
	}
	req := map[string]any{"keyword": query, "purchaseType": "registration", "timeout": 6000}
	if err := c.do(ctx, http.MethodPost, "/core/v1/domains:search", req, &out); err != nil {
		return nil, fmt.Errorf("namecom: search %q: %w", query, err)
	}
	offers := make([]registrar.Offer, 0, len(out.Results))
	for _, r := range out.Results {
		if o := c.offer(r); o.Registrable {
			offers = append(offers, o)
			if limit > 0 && len(offers) >= limit {
				break
			}
		}
	}
	return offers, nil
}

// CheckDomains checks specific names (the authoritative pre-buy re-check).
// Unlike Search, non-purchasable names come back too (Registrable=false).
func (c *Client) CheckDomains(ctx context.Context, names []string) ([]registrar.Offer, error) {
	var out struct {
		Results []searchResult `json:"results"`
	}
	req := map[string]any{"domainNames": names, "purchaseType": "registration"}
	if err := c.do(ctx, http.MethodPost, "/core/v1/domains:checkAvailability", req, &out); err != nil {
		return nil, fmt.Errorf("namecom: check %v: %w", names, err)
	}
	offers := make([]registrar.Offer, 0, len(out.Results))
	for _, r := range out.Results {
		offers = append(offers, c.offer(r))
	}
	return offers, nil
}

// --- Registration ------------------------------------------------------------

// domainInfo is the slice of the Domain object we read.
type domainInfo struct {
	DomainName       string `json:"domainName"`
	ExpireDate       string `json:"expireDate"`
	AutorenewEnabled bool   `json:"autorenewEnabled"`
}

// getDomain fetches one account domain; a 404 apiError means not ours.
func (c *Client) getDomain(ctx context.Context, name string) (domainInfo, error) {
	var out domainInfo
	err := c.do(ctx, http.MethodGet, "/core/v1/domains/"+name, nil, &out)
	return out, err
}

// RegisterDomain registers name for one year with auto-renew on, billed to the
// reseller account's payment method. Synchronous: success means the domain is
// ours. Idempotent — a domain already in the account is never re-ordered (the
// GleSYS lesson: retries must not double-buy).
func (c *Client) RegisterDomain(ctx context.Context, name string) (string, error) {
	if _, err := c.getDomain(ctx, name); err == nil {
		return registrar.StateSucceeded, nil // already ours
	} else if !isNotFound(err) {
		return "", fmt.Errorf("namecom: pre-register details %q: %w", name, err)
	}
	req := map[string]any{
		"domain": map[string]any{
			"domainName":       name,
			"autorenewEnabled": true, // a customer site must never lapse
		},
		"purchaseType": "registration",
		"years":        1,
	}
	var out struct {
		Domain domainInfo `json:"domain"`
		Order  int        `json:"order"`
	}
	if err := c.do(ctx, http.MethodPost, "/core/v1/domains", req, &out); err != nil {
		return "", fmt.Errorf("namecom: register %q: %w", name, err)
	}
	return registrar.StateSucceeded, nil
}

// RegistrationStatus reports how far a registration has come. name.com creates
// synchronously, so in-account = live; 404 = not (yet) ours → pending, which
// keeps the reconcile poller's retry semantics.
func (c *Client) RegistrationStatus(ctx context.Context, name string) (string, error) {
	if _, err := c.getDomain(ctx, name); err != nil {
		if isNotFound(err) {
			return registrar.StatePending, nil
		}
		return "", fmt.Errorf("namecom: status %q: %w", name, err)
	}
	return registrar.StateSucceeded, nil
}

// ZoneID returns the key for DNS-record operations. name.com keys records by
// domain name, so once the domain is in the account the name itself is the
// "zone id". Empty (no error) = not ours yet — the caller retries.
func (c *Client) ZoneID(ctx context.Context, name string) (string, error) {
	if _, err := c.getDomain(ctx, name); err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("namecom: zone %q: %w", name, err)
	}
	return name, nil
}

// DomainExpiry returns the registry expiry date, for detecting (auto-)renewals.
// Zero time (no error) when unknown or the domain isn't ours.
func (c *Client) DomainExpiry(ctx context.Context, name string) (time.Time, error) {
	d, err := c.getDomain(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("namecom: expiry %q: %w", name, err)
	}
	if d.ExpireDate == "" {
		return time.Time{}, nil
	}
	t, perr := time.Parse(time.RFC3339, strings.TrimSpace(d.ExpireDate))
	if perr != nil {
		return time.Time{}, nil // unparsable → unknown, never an error
	}
	return t, nil
}

// SetAutoRenew toggles name.com auto-renew. Off is used when a customer
// detaches a purchased domain, so we stop paying to renew it.
func (c *Client) SetAutoRenew(ctx context.Context, name string, on bool) error {
	verb := ":disableAutorenew"
	if on {
		verb = ":enableAutorenew"
	}
	if err := c.do(ctx, http.MethodPost, "/core/v1/domains/"+name+verb, map[string]any{}, nil); err != nil {
		return fmt.Errorf("namecom: autorenew %q on=%v: %w", name, on, err)
	}
	return nil
}

// --- DNS records -------------------------------------------------------------

// record is the slice of the Record object we compare.
type record struct {
	Host   string `json:"host"`
	Type   string `json:"type"`
	Answer string `json:"answer"`
}

// listRecords returns every DNS record of the zone, following pagination.
func (c *Client) listRecords(ctx context.Context, name string) ([]record, error) {
	var all []record
	page := 0
	for {
		path := "/core/v1/domains/" + name + "/records?perPage=1000"
		if page > 0 {
			path += fmt.Sprintf("&page=%d", page)
		}
		var out struct {
			Records  []record `json:"records"`
			NextPage int      `json:"nextPage"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Records...)
		if out.NextPage == 0 {
			return all, nil
		}
		page = out.NextPage
	}
}

// EnsureDNSRecord creates rec in the zone if an identical one (type+host+answer)
// isn't already present, so it is safe to re-run. Record names are relativized
// to name.com's host form ("acme.se" → "", "_acme-challenge.acme.se" →
// "_acme-challenge"). zoneID is the domain name (see ZoneID).
func (c *Client) EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error {
	name := zoneID
	host := relativize(rec.Name, name)

	existing, err := c.listRecords(ctx, name)
	if err != nil {
		return fmt.Errorf("namecom: list records %q: %w", name, err)
	}
	for _, e := range existing {
		if strings.EqualFold(e.Type, rec.Type) &&
			strings.EqualFold(relativize(e.Host, name), host) &&
			trimQuotes(e.Answer) == trimQuotes(rec.Content) {
			return nil // already present — idempotent
		}
	}

	ttl := rec.TTL
	if ttl < 300 {
		ttl = 300 // name.com's minimum TTL
	}
	body := map[string]any{"host": host, "type": rec.Type, "answer": rec.Content, "ttl": ttl}
	if err := c.do(ctx, http.MethodPost, "/core/v1/domains/"+name+"/records", body, nil); err != nil {
		return fmt.Errorf("namecom: add record %s %s: %w", rec.Type, host, err)
	}
	return nil
}

// relativize converts a FQDN to name.com's zone-relative host: the apex becomes
// "" and hosts under the domain lose the suffix; already-relative names pass
// through ("@" normalizes to "").
func relativize(name, domain string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	switch {
	case n == "" || n == "@" || n == d:
		return ""
	case strings.HasSuffix(n, "."+d):
		return strings.TrimSuffix(n, "."+d)
	default:
		return n
	}
}

// trimQuotes strips surrounding quotes some providers add to TXT data.
func trimQuotes(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }

// --- Diagnosis helpers (cmd/domainctl) ---------------------------------------

// Hello verifies connectivity + credentials and returns the authenticated
// username and server name.
func (c *Client) Hello(ctx context.Context) (username, server string, err error) {
	var out struct {
		Username   string `json:"username"`
		ServerName string `json:"serverName"`
	}
	if err := c.do(ctx, http.MethodGet, "/core/v1/hello", nil, &out); err != nil {
		return "", "", err
	}
	return out.Username, out.ServerName, nil
}

// AutorenewEnabled reports the domain's current auto-renew flag (diagnosis).
func (c *Client) AutorenewEnabled(ctx context.Context, name string) (bool, error) {
	d, err := c.getDomain(ctx, name)
	if err != nil {
		return false, err
	}
	return d.AutorenewEnabled, nil
}

// DomainSummary is one account domain for the operator survey.
type DomainSummary struct {
	Name      string
	Expire    string
	Autorenew bool
}

// ListDomains returns the account's domains (first 1000 — plenty). Quirk seen
// live 2026-07-18: the list endpoint's autorenewEnabled can report a STALE
// value after a toggle — the per-domain GET (AutorenewEnabled) is the
// authoritative read, and it is what the product logic uses.
func (c *Client) ListDomains(ctx context.Context) ([]DomainSummary, error) {
	var out struct {
		Domains []domainInfo `json:"domains"`
	}
	if err := c.do(ctx, http.MethodGet, "/core/v1/domains?perPage=1000", nil, &out); err != nil {
		return nil, fmt.Errorf("namecom: list domains: %w", err)
	}
	ds := make([]DomainSummary, 0, len(out.Domains))
	for _, d := range out.Domains {
		ds = append(ds, DomainSummary{Name: d.DomainName, Expire: d.ExpireDate, Autorenew: d.AutorenewEnabled})
	}
	return ds, nil
}

// Records returns a domain's DNS records (read-only; cmd/domainctl).
func (c *Client) Records(ctx context.Context, name string) ([]registrar.Record, error) {
	recs, err := c.listRecords(ctx, name)
	if err != nil {
		return nil, err
	}
	out := make([]registrar.Record, 0, len(recs))
	for _, r := range recs {
		out = append(out, registrar.Record{Type: r.Type, Name: r.Host, Content: r.Answer})
	}
	return out, nil
}
