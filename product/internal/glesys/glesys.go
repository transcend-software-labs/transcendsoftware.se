// Package glesys drives custom domains through the GleSYS API for .se and the
// other TLDs a customer might want: availability + price, registration,
// registrar-state polling, and the DNS records that point a domain at a
// customer's Fly app.
//
// It talks to the GleSYS REST API directly (bespoke net/http, house style —
// see internal/hostup, internal/cloudflare) rather than through the official
// github.com/glesys/glesys-go SDK. The SDK's request for domain/available omits
// the required "search" argument (sends a bare string), and — more
// fundamentally — its response structs type fields as Go bool/int/float while
// the live API returns them as JSON strings ("available":"yes", "amount":"129"),
// so it fails to decode real responses. We decode only the fields we need, with
// string-tolerant scalars, and let the rest be ignored.
//
// It implements the same orchestrator.DomainRegistrar surface as
// internal/cloudflare and internal/hostup, so the provider is chosen at wiring
// time (cmd/server/main.go).
//
// GleSYS notes:
//   - Auth is HTTP Basic: username = the project key ("CL12345"), password =
//     the API key. Endpoints are POST https://api.glesys.com/<module>/<func>.
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// Client talks to the GleSYS REST API (JSON in, Basic auth).
type Client struct {
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
	return &Client{
		http:       &http.Client{Timeout: 25 * time.Second},
		baseURL:    strings.TrimRight(defaultBaseURL, "/"),
		project:    project,
		apiKey:     apiKey,
		registrant: reg,
	}
}

// newTest points the client at a test server (httptest) with dummy creds.
func newTest(baseURL string, reg Registrant) *Client {
	return &Client{
		http:       &http.Client{Timeout: 10 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
		project:    "CLtest",
		apiKey:     "test-key",
		registrant: reg,
	}
}

// apiError is a non-200 GleSYS response, carrying the HTTP status so callers
// can special-case 404 (a domain not in the account — "not found / not yet").
type apiError struct {
	fn     string
	status int
	text   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("glesys %s: status %d: %s", e.fn, e.status, e.text)
}

// isNotFound reports whether err is a GleSYS 404 (domain not in the account).
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.status == http.StatusNotFound
}

// do performs one GleSYS API call: POST /<fn> with a JSON body, Basic auth, and
// decodes the JSON response into out (which should target the "response"
// envelope). Non-200 responses surface the API's status text as an *apiError.
func (c *Client) do(ctx context.Context, fn string, params, out any) error {
	var body io.Reader
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+fn, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.project, c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("glesys %s: %w", fn, err)
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
		return &apiError{fn: fn, status: resp.StatusCode, text: strings.TrimSpace(e.Response.Status.Text)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("glesys %s: decode: %w", fn, err)
		}
	}
	return nil
}

// --- string-tolerant scalars: GleSYS returns bools/numbers as JSON strings ---

// flexBool unmarshals a GleSYS boolean that arrives as a string ("yes"/"true"/
// "1") or a real JSON bool.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.ToLower(strings.Trim(strings.TrimSpace(string(data)), `"`))
	*b = flexBool(s == "true" || s == "yes" || s == "1" || s == "available")
	return nil
}

// flexFloat unmarshals a number that may arrive as a JSON string.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("flexFloat %q: %w", s, err)
	}
	*f = flexFloat(v)
	return nil
}

// flexInt unmarshals an integer that may arrive as a JSON string (or "1.0").
type flexInt int

func (i *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("flexInt %q: %w", s, err)
	}
	*i = flexInt(int(v))
	return nil
}

// registrarState extracts registrarinfo.state + expiry, tolerating GleSYS
// returning registrarinfo as either an object ({"state":"OK","expire":"…"}) or
// the string "None".
type registrarState struct {
	State  string
	Expire string // "2027-01-19" (YYYY-MM-DD); "" when unknown
}

func (r *registrarState) UnmarshalJSON(data []byte) error {
	t := strings.TrimSpace(string(data))
	if len(t) == 0 || t == "null" || t[0] == '"' {
		return nil // a string like "None" — no registrar state
	}
	var o struct {
		State  string `json:"state"`
		Expire string `json:"expire"`
	}
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	r.State, r.Expire = o.State, o.Expire
	return nil
}

// available checks a query against domain/available with the correctly-named
// "search" argument. A full domain incl. TLD returns just that domain; a bare
// keyword fans out across every TLD (slow — the UI requires a full domain).
func (c *Client) available(ctx context.Context, search string) ([]registrar.Offer, error) {
	var out struct {
		Response struct {
			Domain []struct {
				Name      string   `json:"domainname"`
				Available flexBool `json:"available"`
				Prices    []struct {
					Amount   flexFloat `json:"amount"`
					Currency string    `json:"currency"`
					Years    flexInt   `json:"years"`
				} `json:"prices"`
			} `json:"domain"`
		} `json:"response"`
	}
	if err := c.do(ctx, "domain/available", map[string]string{"search": search}, &out); err != nil {
		return nil, err
	}
	offers := make([]registrar.Offer, 0, len(out.Response.Domain))
	for _, d := range out.Response.Domain {
		o := registrar.Offer{Name: d.Name, Registrable: bool(d.Available)}
		for _, p := range d.Prices {
			if int(p.Years) == 1 {
				o.Price = float64(p.Amount)
				o.Currency = p.Currency
				// GleSYS reports the registration price; renewals bill at cost
				// yearly, so use the same figure for the renewal cap check.
				o.Renewal = float64(p.Amount)
				break
			}
		}
		// The billing chain (Stripe subscription + the invoice item) is SEK, and
		// we don't convert — a non-SEK offer isn't sellable.
		if !strings.EqualFold(o.Currency, "SEK") {
			o.Registrable = false
		}
		offers = append(offers, o)
	}
	return offers, nil
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
		os, err := c.available(ctx, name)
		if err != nil {
			return nil, err
		}
		offers = append(offers, os...)
	}
	return offers, nil
}

// RegisterDomain registers a domain to Forge's contact for one year and turns
// on auto-renew so a customer site never lapses (re-billing renewals is a
// follow-up). It returns the neutral workflow state from the registrar.
func (c *Client) RegisterDomain(ctx context.Context, name string) (string, error) {
	params := map[string]any{
		"domainname":   name,
		"numyears":     1,
		"firstname":    c.registrant.Firstname,
		"lastname":     c.registrant.Lastname,
		"organization": c.registrant.Organization,
		"nationalid":   c.registrant.NationalID,
		"address":      c.registrant.Address,
		"city":         c.registrant.City,
		"zipcode":      c.registrant.ZipCode,
		"country":      c.registrant.Country,
		"email":        c.registrant.Email,
		"phonenumber":  c.registrant.PhoneNumber,
	}
	var out struct {
		Response struct {
			Domain struct {
				RegistrarInfo registrarState `json:"registrarinfo"`
			} `json:"domain"`
		} `json:"response"`
	}
	if err := c.do(ctx, "domain/register", params, &out); err != nil {
		return "", fmt.Errorf("glesys: register %q: %w", name, err)
	}
	// Best-effort: keep the domain from lapsing. A fresh .se may still be
	// settling, so a failure here is logged, not fatal — GleSYS also defaults new
	// registrations to auto-renew.
	if err := c.SetAutoRenew(ctx, name, true); err != nil {
		slog.Warn("glesys set auto-renew", "domain", name, "err", err)
	}
	return mapState(name, out.Response.Domain.RegistrarInfo.State), nil
}

// SetAutoRenew turns GleSYS auto-renew on or off for a domain. Off is used when
// a customer detaches a purchased domain, so we stop paying to renew it.
func (c *Client) SetAutoRenew(ctx context.Context, name string, on bool) error {
	v := "no"
	if on {
		v = "yes"
	}
	return c.do(ctx, "domain/setautorenew", map[string]any{"domainname": name, "setautorenew": v}, nil)
}

// DomainExpiry returns the domain's current registry expiry date, for detecting
// (auto-)renewals. Zero time (no error) when unknown or the domain isn't in the
// account yet.
func (c *Client) DomainExpiry(ctx context.Context, name string) (time.Time, error) {
	ri, err := c.details(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("glesys: details %q: %w", name, err)
	}
	if ri.Expire == "" {
		return time.Time{}, nil
	}
	t, perr := time.Parse("2006-01-02", strings.TrimSpace(ri.Expire))
	if perr != nil {
		slog.Warn("glesys: unparsable expiry", "domain", name, "expire", ri.Expire)
		return time.Time{}, nil
	}
	return t, nil
}

// details fetches a domain's registrar state. A 404 (domain not in the account)
// is returned as an *apiError so callers can distinguish "not yet" from a real
// failure.
func (c *Client) details(ctx context.Context, name string) (registrarState, error) {
	var out struct {
		Response struct {
			Domain struct {
				RegistrarInfo registrarState `json:"registrarinfo"`
			} `json:"domain"`
		} `json:"response"`
	}
	err := c.do(ctx, "domain/details", map[string]string{"domainname": name}, &out)
	return out.Response.Domain.RegistrarInfo, err
}

// RegistrationStatus reports how far a registration has come. A 404 means the
// domain isn't in the account yet (order still settling) → pending.
func (c *Client) RegistrationStatus(ctx context.Context, name string) (string, error) {
	ri, err := c.details(ctx, name)
	if err != nil {
		if isNotFound(err) {
			slog.Info("glesys registration status", "domain", name, "state", "not-in-account")
			return registrar.StatePending, nil
		}
		return "", fmt.Errorf("glesys: details %q: %w", name, err)
	}
	return mapState(name, ri.State), nil
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
	if _, err := c.details(ctx, name); err != nil {
		if isNotFound(err) {
			return "", nil // not in the account yet — retry next pass
		}
		slog.Info("glesys zone not ready", "domain", name, "err", err)
		return "", nil
	}
	return name, nil
}

// dnsRecord is one existing DNS record — only the fields we compare (GleSYS
// returns recordid/ttl as strings too, so we simply don't decode them).
type dnsRecord struct {
	Host string `json:"host"`
	Type string `json:"type"`
	Data string `json:"data"`
}

// listRecords returns a domain's DNS records.
func (c *Client) listRecords(ctx context.Context, name string) ([]dnsRecord, error) {
	var out struct {
		Response struct {
			Records []dnsRecord `json:"records"`
		} `json:"response"`
	}
	err := c.do(ctx, "domain/listrecords", map[string]string{"domainname": name}, &out)
	return out.Response.Records, err
}

// EnsureDNSRecord creates rec in the domain if an identical one (type+host+data)
// isn't already present, so it is safe to re-run. Record names are relativized
// to the GleSYS "host" form ("acme.se" → "@", "_acme-challenge.acme.se" →
// "_acme-challenge"). zoneID is the domain name (see ZoneID).
func (c *Client) EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error {
	name := zoneID
	host := relativize(rec.Name, name)

	existing, err := c.listRecords(ctx, name)
	if err != nil {
		return fmt.Errorf("glesys: list records %q: %w", name, err)
	}
	for _, e := range existing {
		if strings.EqualFold(e.Type, rec.Type) &&
			strings.EqualFold(relativize(e.Host, name), host) &&
			trimQuotes(e.Data) == trimQuotes(rec.Content) {
			return nil // already present — idempotent
		}
	}

	ttl := rec.TTL
	if ttl == 0 {
		ttl = 300 // low default so validation records propagate fast
	}
	if err := c.do(ctx, "domain/addrecord", map[string]any{
		"domainname": name, "host": host, "type": rec.Type, "data": rec.Content, "ttl": ttl,
	}, nil); err != nil {
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
