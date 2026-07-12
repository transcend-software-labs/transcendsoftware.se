// Package hostup talks to the Hostup REST API v2 (developer.hostup.se) for
// custom domains: it checks and registers domains — including the Swedish
// ccTLDs .se/.nu that Cloudflare Registrar can't sell — and manages the DNS
// records that point a domain at a customer's Fly app. Bespoke net/http client
// in the house style (see internal/billing, internal/cloudflare), no SDK.
//
// It implements the same orchestrator.DomainRegistrar surface as
// internal/cloudflare, so the provider is chosen at wiring time
// (cmd/server/main.go) and the two can be compared side by side.
//
// API notes (from developer.hostup.se):
//   - Auth: `Authorization: Bearer <token>`; keys minted at
//     cloud.hostup.se/api-management (scoped; needs domains, dns, orders).
//   - Errors: RFC 7807 problem documents with a stable `code`.
//   - Availability may return 202 + a poll URL for slow batches.
//   - Registration is an ORDER (POST /api/v2/orders); it completes when the
//     invoice settles, so the account needs an auto-payable method. The
//     registrant defaults to the account owner (Hostup account contact) —
//     which is the model we want: Forge owns/manages the domain, the customer
//     pays the monthly add-on.
//   - DNS record names are zone-relative ("@" for the root), so FQDNs from
//     Fly's cert requirements are relativized before writing.
package hostup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

// searchTLDs are the endings offered when a customer types a bare name in the
// domain search. Leading with .se/.nu — the ccTLDs that motivated Hostup.
var searchTLDs = []string{"se", "nu", "com", "net", "org", "eu", "io", "dev", "app", "online"}

// Client talks to the Hostup REST API v2 (JSON in/out, Bearer auth).
type Client struct {
	baseURL string
	token   string
	payment string // order payment method (card|swish|bankgiro|sepa|invoice)
	http    *http.Client

	mu        sync.Mutex
	zoneNames map[string]string // zone id → zone name, for relativizing records
}

// New returns a client. baseURL is Hostup's API host in prod (a fake httptest
// server in tests); paymentMethod is how registration orders are settled
// (empty → "invoice").
func New(baseURL, token, paymentMethod string) *Client {
	if paymentMethod == "" {
		paymentMethod = "invoice"
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		payment:   paymentMethod,
		http:      &http.Client{Timeout: 25 * time.Second},
		zoneNames: map[string]string{},
	}
}

// availabilityItem is one entry in POST /api/v2/domains/availability's data.
type availabilityItem struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Premium   bool   `json:"premium"`
	Actions   struct {
		CanRegister struct {
			Allowed bool `json:"allowed"`
		} `json:"canRegister"`
	} `json:"actions"`
	Billing struct {
		Amount       float64 `json:"amount"`
		CurrencyCode string  `json:"currencyCode"`
	} `json:"billing"`
	RenewalAmount               float64 `json:"renewalAmount"`
	ExistingDomainID            string  `json:"existingDomainId"`
	ExistingDomainServiceStatus string  `json:"existingDomainServiceStatus"`
}

// checkAvailability runs the availability check, following the async 202+poll
// contract for slow batches.
func (c *Client) checkAvailability(ctx context.Context, names []string) ([]availabilityItem, error) {
	type envelope struct {
		Data      []availabilityItem `json:"data"`
		Operation *struct {
			Status  string `json:"status"`
			PollURL string `json:"pollUrl"`
		} `json:"operation"`
	}
	var out envelope
	if err := c.do(ctx, http.MethodPost, "/api/v2/domains/availability",
		map[string]any{"names": names}, &out); err != nil {
		return nil, err
	}
	// Async: poll the returned URL until the result carries data.
	for tries := 0; out.Data == nil && out.Operation != nil && out.Operation.PollURL != ""; tries++ {
		if tries >= 10 {
			return nil, fmt.Errorf("hostup: availability check still %s after polling", out.Operation.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
		poll := out.Operation.PollURL
		out = envelope{}
		if err := c.do(ctx, http.MethodGet, poll, nil, &out); err != nil {
			return nil, err
		}
	}
	return out.Data, nil
}

func (i availabilityItem) offer() registrar.Offer {
	return registrar.Offer{
		Name:        i.Name,
		Registrable: i.Available && i.Actions.CanRegister.Allowed,
		Premium:     i.Premium,
		Price:       i.Billing.Amount,
		Renewal:     i.RenewalAmount,        // first years are often discounted; the cap checks this too
		Currency:    i.Billing.CurrencyCode, // SEK
	}
}

// SearchDomains suggests registrable domains for a keyword: Hostup has no
// suggestion endpoint, so a bare name fans out across searchTLDs and an exact
// domain is checked as typed.
func (c *Client) SearchDomains(ctx context.Context, query string, limit int) ([]registrar.Offer, error) {
	if limit <= 0 {
		limit = 20
	}
	var names []string
	if q := strings.ToLower(strings.TrimSpace(query)); strings.Contains(q, ".") {
		names = []string{q}
	} else if base := slugify(query); base != "" {
		for _, tld := range searchTLDs {
			names = append(names, base+"."+tld)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	items, err := c.checkAvailability(ctx, names)
	if err != nil {
		return nil, err
	}
	offers := make([]registrar.Offer, 0, len(items))
	for _, it := range items {
		offers = append(offers, it.offer())
		if len(offers) == limit {
			break
		}
	}
	return offers, nil
}

// CheckDomains returns availability + price (SEK) for specific domain names.
func (c *Client) CheckDomains(ctx context.Context, names []string) ([]registrar.Offer, error) {
	items, err := c.checkAvailability(ctx, names)
	if err != nil {
		return nil, err
	}
	offers := make([]registrar.Offer, 0, len(items))
	for _, it := range items {
		offers = append(offers, it.offer())
	}
	return offers, nil
}

// RegisterDomain places a registration order and maps the order status onto
// the neutral workflow states. The registrant is the Hostup account owner
// (Forge), so no per-customer identity is needed — .se/.nu just require the
// registry terms to be accepted on the order.
func (c *Client) RegisterDomain(ctx context.Context, name string) (string, error) {
	item := map[string]any{
		"type":       "domain",
		"action":     "register",
		"domainName": name,
		"years":      1,
	}
	if tld := name[strings.LastIndex(name, ".")+1:]; tld == "se" || tld == "nu" {
		item["acceptedTerms"] = []string{tld + "_registration_terms"}
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"` // pending|active|completed|cancelled|failed
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/orders",
		map[string]any{"paymentMethod": c.payment, "items": []any{item}}, &out); err != nil {
		return "", err
	}
	switch out.Status {
	case "completed", "active":
		return registrar.StateSucceeded, nil
	case "cancelled", "failed":
		return registrar.StateFailed, fmt.Errorf("hostup: registration order %s %s", out.ID, out.Status)
	case "pending":
		return registrar.StatePending, nil
	default:
		return registrar.StateInProgress, nil
	}
}

// RegistrationStatus reports how far a registration has come, via the
// availability endpoint's view of the account: once the domain exists in the
// account (existingDomainId) it is ours, and its service status says whether
// it's live. Still-available means the order hasn't reached the registry yet
// (e.g. the invoice hasn't settled) — pending, until the 72h stuck-timeout.
func (c *Client) RegistrationStatus(ctx context.Context, name string) (string, error) {
	items, err := c.checkAvailability(ctx, []string{name})
	if err != nil {
		return "", err
	}
	for _, it := range items {
		if !strings.EqualFold(it.Name, name) {
			continue
		}
		switch {
		case it.ExistingDomainID != "" && it.ExistingDomainServiceStatus == "active":
			return registrar.StateSucceeded, nil
		case it.ExistingDomainID != "":
			return registrar.StateInProgress, nil
		case it.Available:
			return registrar.StatePending, nil
		default:
			// Taken but not (yet) in our account — registration may be settling.
			return registrar.StateInProgress, nil
		}
	}
	return "", fmt.Errorf("hostup: availability returned no result for %s", name)
}

// zoneListItem is one entry in GET /api/v2/dns-zones.
type zoneListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ZoneID returns the DNS-zone id for an exact domain name. Empty id (with no
// error) means no zone yet — the caller retries (registration auto-creates it).
func (c *Client) ZoneID(ctx context.Context, name string) (string, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("limit", "1")
	var out struct {
		Data []zoneListItem `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v2/dns-zones?"+q.Encode(), nil, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 {
		return "", nil
	}
	c.mu.Lock()
	c.zoneNames[out.Data[0].ID] = out.Data[0].Name
	c.mu.Unlock()
	return out.Data[0].ID, nil
}

// zoneName resolves a zone id to its domain name (needed to relativize record
// names), from cache or GET /api/v2/dns-zones/{id}.
func (c *Client) zoneName(ctx context.Context, zoneID string) (string, error) {
	c.mu.Lock()
	name := c.zoneNames[zoneID]
	c.mu.Unlock()
	if name != "" {
		return name, nil
	}
	// Tolerate both a bare zone object and a {data: {...}} envelope.
	var out struct {
		ID   string        `json:"id"`
		Name string        `json:"name"`
		Data *zoneListItem `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v2/dns-zones/"+url.PathEscape(zoneID), nil, &out); err != nil {
		return "", err
	}
	name = out.Name
	if name == "" && out.Data != nil {
		name = out.Data.Name
	}
	if name == "" {
		return "", fmt.Errorf("hostup: zone %s has no name", zoneID)
	}
	c.mu.Lock()
	c.zoneNames[zoneID] = name
	c.mu.Unlock()
	return name, nil
}

// recordWire is Hostup's DNS-record shape (create body + list items).
type recordWire struct {
	ID    string `json:"id,omitempty"`
	Type  string `json:"type"`
	Name  string `json:"name"` // zone-relative; "@" for the root
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

// EnsureDNSRecord creates rec in the zone if an identical one (type+name+value)
// isn't already present, so it is safe to re-run. Record names are relativized
// to the zone ("gutka.org" → "@", "_acme-challenge.gutka.org" → "_acme-challenge").
func (c *Client) EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error {
	zone, err := c.zoneName(ctx, zoneID)
	if err != nil {
		return err
	}
	rel := relativize(rec.Name, zone)

	var existing struct {
		Data []recordWire `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/api/v2/dns-zones/"+url.PathEscape(zoneID)+"/records?limit=1000", nil, &existing); err != nil {
		return err
	}
	for _, e := range existing.Data {
		if strings.EqualFold(e.Type, rec.Type) &&
			strings.EqualFold(relativize(e.Name, zone), rel) &&
			trimQuotes(e.Value) == trimQuotes(rec.Content) {
			return nil // already present — idempotent
		}
	}

	ttl := rec.TTL
	if ttl == 0 {
		ttl = 300 // low default so validation records propagate fast
	}
	return c.do(ctx, http.MethodPost, "/api/v2/dns-zones/"+url.PathEscape(zoneID)+"/records",
		recordWire{Type: rec.Type, Name: rel, Value: rec.Content, TTL: ttl}, nil)
}

// relativize converts a FQDN to a zone-relative record name: the zone apex
// becomes "@", hosts under the zone lose the zone suffix, and anything already
// relative passes through.
func relativize(name, zone string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	z := strings.ToLower(strings.TrimSuffix(zone, "."))
	switch {
	case n == "" || n == "@" || n == z:
		return "@"
	case strings.HasSuffix(n, "."+z):
		return strings.TrimSuffix(n, "."+z)
	default:
		return n
	}
}

// trimQuotes strips surrounding quotes for value comparison — Hostup applies
// DNS-safe quoting to TXT values server-side.
func trimQuotes(s string) string { return strings.Trim(s, `"`) }

// slugify reduces a search phrase to a DNS-usable label (å/ä→a, ö→o, keep
// [a-z0-9-]). "" when nothing usable remains.
func slugify(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	var b strings.Builder
	for _, r := range q {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-':
			b.WriteRune(r)
		case r == 'å' || r == 'ä':
			b.WriteByte('a')
		case r == 'ö':
			b.WriteByte('o')
			// spaces and anything else are dropped
		}
	}
	return strings.Trim(b.String(), "-")
}

// do performs one API call. Non-2xx responses are RFC 7807 problem documents;
// their title/detail/code are surfaced in the error.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+c.token)
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hostup: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var prob struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Code   string `json:"code"`
		}
		_ = json.Unmarshal(raw, &prob)
		if prob.Title != "" || prob.Detail != "" {
			return fmt.Errorf("hostup: %s: %s (code %s)", prob.Title, prob.Detail, prob.Code)
		}
		return fmt.Errorf("hostup: status %d", resp.StatusCode)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("hostup: decode (status %d): %w", resp.StatusCode, err)
		}
	}
	return nil
}
