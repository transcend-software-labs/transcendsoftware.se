// Package cloudflare talks to the Cloudflare API v4 for custom domains: it
// searches and registers domains through Cloudflare Registrar and manages the
// DNS records that point a domain at a customer's Fly app. Bespoke net/http
// client in the house style (see internal/billing, internal/imagegen) — no
// vendor SDK.
//
// Every DNS record we create is proxied:false. Cloudflare's orange-cloud proxy
// would terminate TLS itself and break Fly's ACME challenge and certificate —
// so unproxied is a hard invariant, enforced in EnsureDNSRecord.
//
// The Registrar endpoints (search/check/register) are Cloudflare's beta
// Registrar API (shipped ~April 2026): registration is asynchronous and returns
// a workflow `state`. Zones and DNS-records are the long-stable GA endpoints.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Cloudflare v4 REST API (JSON in/out, Bearer auth).
type Client struct {
	baseURL   string
	token     string
	accountID string
	http      *http.Client
}

// New returns a client. baseURL is https://api.cloudflare.com/client/v4 in prod
// (a fake httptest server in tests).
func New(baseURL, token, accountID string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		accountID: accountID,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Registration workflow states returned by RegisterDomain / RegistrationStatus.
const (
	StateSucceeded      = "succeeded"
	StatePending        = "pending"
	StateInProgress     = "in_progress"
	StateActionRequired = "action_required"
	StateBlocked        = "blocked"
	StateFailed         = "failed"
)

// DomainOffer is a domain we can (or can't) register, with its price. Price is
// the one-year registration cost parsed from Cloudflare's string amount; it is
// 0 when the domain isn't registrable or carries no price.
type DomainOffer struct {
	Name        string
	Registrable bool
	Premium     bool
	Price       float64
	Currency    string // e.g. "USD"
}

// DNSRecord is a single DNS record in a zone. Proxied is always sent as false
// for the records we manage.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
	Proxied bool   `json:"proxied"`
}

// pricing mirrors Cloudflare's nested price object (amounts are JSON strings).
type pricing struct {
	Currency         string `json:"currency"`
	RegistrationCost string `json:"registration_cost"`
	RenewalCost      string `json:"renewal_cost"`
}

// domainResult is one item in a search or check response.
type domainResult struct {
	Name        string  `json:"name"`
	Registrable bool    `json:"registrable"`
	Tier        string  `json:"tier"` // "standard" | "premium"
	Pricing     pricing `json:"pricing"`
	Reason      string  `json:"reason"` // present when !registrable
}

func (d domainResult) offer() DomainOffer {
	price, _ := strconv.ParseFloat(d.Pricing.RegistrationCost, 64)
	return DomainOffer{
		Name:        d.Name,
		Registrable: d.Registrable,
		Premium:     d.Tier == "premium",
		Price:       price,
		Currency:    d.Pricing.Currency,
	}
}

// SearchDomains suggests registrable domains for a keyword/phrase, with prices.
func (c *Client) SearchDomains(ctx context.Context, query string, limit int) ([]DomainOffer, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", strconv.Itoa(limit))
	var out struct {
		Domains []domainResult `json:"domains"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/accounts/"+c.accountID+"/registrar/domain-search?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	offers := make([]DomainOffer, 0, len(out.Domains))
	for _, d := range out.Domains {
		offers = append(offers, d.offer())
	}
	return offers, nil
}

// CheckDomains returns availability + price for specific domain names (1–20).
// Used server-side to re-verify price and registrability just before buying.
func (c *Client) CheckDomains(ctx context.Context, names []string) ([]DomainOffer, error) {
	var out struct {
		Domains []domainResult `json:"domains"`
	}
	if err := c.do(ctx, http.MethodPost,
		"/accounts/"+c.accountID+"/registrar/domain-check",
		map[string]any{"domains": names}, &out); err != nil {
		return nil, err
	}
	offers := make([]DomainOffer, 0, len(out.Domains))
	for _, d := range out.Domains {
		offers = append(offers, d.offer())
	}
	return offers, nil
}

// RegisterDomain starts registering a domain and returns the workflow state
// (StateSucceeded when it completed synchronously, StatePending/StateInProgress
// while it's still processing, or a terminal-bad state). auto_renew is set true
// so the domain doesn't lapse — the customer's recurring add-on covers it — and
// privacy redaction is on. The registrant contact + payment method + agreement
// are account-level prerequisites the owner configures in the dashboard.
func (c *Client) RegisterDomain(ctx context.Context, name string) (string, error) {
	body := map[string]any{
		"domain_name":  name,
		"years":        1,
		"auto_renew":   true,
		"privacy_mode": "redaction",
	}
	var out struct {
		State string `json:"state"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.do(ctx, http.MethodPost,
		"/accounts/"+c.accountID+"/registrar/registrations", body, &out); err != nil {
		return "", err
	}
	if out.State == StateFailed && out.Error != nil {
		return StateFailed, fmt.Errorf("cloudflare: registration failed: %s (%s)", out.Error.Message, out.Error.Code)
	}
	if out.State == "" {
		return "", fmt.Errorf("cloudflare: registration returned no state")
	}
	return out.State, nil
}

// RegistrationStatus polls the async registration workflow for a domain.
func (c *Client) RegistrationStatus(ctx context.Context, name string) (string, error) {
	var out struct {
		State string `json:"state"`
	}
	if err := c.do(ctx, http.MethodGet,
		"/accounts/"+c.accountID+"/registrar/registrations/"+url.PathEscape(name)+"/registration-status",
		nil, &out); err != nil {
		return "", err
	}
	if out.State == "" {
		return "", fmt.Errorf("cloudflare: registration-status returned no state")
	}
	return out.State, nil
}

// ZoneID returns the Cloudflare zone id for an exact domain name. Registering a
// domain through Cloudflare Registrar auto-creates its zone, so this is how we
// find the zone to add DNS records to. Empty id (with no error) means no zone
// exists yet — the caller retries.
func (c *Client) ZoneID(ctx context.Context, name string) (string, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("match", "all")
	var zones []struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?"+q.Encode(), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", nil
	}
	return zones[0].ID, nil
}

// ListDNSRecords returns every DNS record in a zone.
func (c *Client) ListDNSRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	var recs []DNSRecord
	if err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records", nil, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// EnsureDNSRecord creates rec in the zone if an identical one (type+name+content)
// isn't already present, so it is safe to re-run. Proxied is forced false — the
// orange-cloud proxy breaks Fly's ACME challenge and TLS.
func (c *Client) EnsureDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) error {
	rec.Proxied = false
	if rec.TTL == 0 {
		rec.TTL = 1 // Cloudflare: 1 = automatic
	}
	existing, err := c.ListDNSRecords(ctx, zoneID)
	if err != nil {
		return err
	}
	for _, e := range existing {
		if strings.EqualFold(e.Type, rec.Type) &&
			strings.EqualFold(e.Name, rec.Name) &&
			e.Content == rec.Content {
			return nil // already present — idempotent
		}
	}
	return c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", rec, nil)
}

// apiEnvelope is the standard Cloudflare v4 wrapper around every response.
type apiEnvelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

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
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env apiEnvelope
	// A non-JSON body (proxy error page, etc.) still yields a useful status error.
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 || !env.Success {
		if len(env.Errors) > 0 {
			return fmt.Errorf("cloudflare: %s (code %d)", env.Errors[0].Message, env.Errors[0].Code)
		}
		return fmt.Errorf("cloudflare: status %d", resp.StatusCode)
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare: decode result: %w", err)
		}
	}
	return nil
}
