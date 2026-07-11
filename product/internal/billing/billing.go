// Package billing talks to the Stripe API for the subscription paywall with a
// small hand-rolled client — this repo keeps external API clients bespoke (see
// internal/imagegen), no vendor SDK. It creates Checkout Sessions for the
// subscribe flow, billing-portal sessions for self-serve management, and reads
// the plan Price for display. Webhook signature verification is in webhook.go.
//
// No Stripe-Version header is sent: the endpoints and parameters used here
// (Checkout Sessions, Billing Portal, Prices, and the three webhook events we
// read) have been stable for years, so the account's default API version
// applies — pinning would add a maintenance surface with no benefit at this
// scope.
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to the Stripe REST API (v1, form-encoded requests, JSON replies).
type Client struct {
	baseURL string
	key     string
	http    *http.Client

	priceMu sync.Mutex
	priceAt time.Time
	price   map[string]Price // id → last fetched price (cached ~1h)
}

// New returns a client. baseURL is https://api.stripe.com in prod (a fake
// httptest server in tests).
func New(baseURL, key string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), key: key,
		http:  &http.Client{Timeout: 15 * time.Second},
		price: map[string]Price{},
	}
}

// LineItem is one Checkout line. Price is a Stripe price id. A future per-domain
// DNS charge is simply an extra LineItem — the checkout encodes the whole slice.
type LineItem struct {
	Price    string
	Quantity int
}

// CheckoutParams configures a subscription Checkout Session.
type CheckoutParams struct {
	ProjectID     string
	CustomerEmail string
	Locale        string
	SuccessURL    string
	CancelURL     string
	LineItems     []LineItem
}

// CreateCheckoutSession opens a subscription Checkout Session and returns its
// hosted URL to redirect the customer to.
func (c *Client) CreateCheckoutSession(ctx context.Context, p CheckoutParams) (string, error) {
	form := url.Values{}
	form.Set("mode", "subscription")
	for i, li := range p.LineItems {
		form.Set(fmt.Sprintf("line_items[%d][price]", i), li.Price)
		q := li.Quantity
		if q == 0 {
			q = 1
		}
		form.Set(fmt.Sprintf("line_items[%d][quantity]", i), strconv.Itoa(q))
	}
	form.Set("client_reference_id", p.ProjectID)
	form.Set("metadata[project_id]", p.ProjectID)
	// Ride the project id onto the Subscription too, so subscription lifecycle
	// events (which don't carry client_reference_id) map back to the project.
	form.Set("subscription_data[metadata][project_id]", p.ProjectID)
	form.Set("success_url", p.SuccessURL)
	form.Set("cancel_url", p.CancelURL)
	if p.CustomerEmail != "" {
		form.Set("customer_email", p.CustomerEmail)
	}
	if p.Locale != "" {
		form.Set("locale", p.Locale)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/checkout/sessions", form, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("billing: checkout session returned no url")
	}
	return out.URL, nil
}

// CreatePortalSession opens a billing-portal session (self-serve cancel / card
// update) and returns its hosted URL.
func (c *Client) CreatePortalSession(ctx context.Context, customerID, returnURL string) (string, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("return_url", returnURL)
	var out struct {
		URL string `json:"url"`
	}
	if err := c.post(ctx, "/v1/billing_portal/sessions", form, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", fmt.Errorf("billing: portal session returned no url")
	}
	return out.URL, nil
}

// Price is the plan's recurring amount, for display ("99 kr/mån").
type Price struct {
	UnitAmount int64  // minor units (öre for SEK)
	Currency   string // "sek"
	Interval   string // "month"
}

// Price fetches a Stripe Price, caching it for an hour. On a refresh failure it
// serves the last good value when it has one, so a transient Stripe hiccup never
// breaks the page — Checkout shows the authoritative amount regardless.
func (c *Client) Price(ctx context.Context, id string) (Price, error) {
	c.priceMu.Lock()
	defer c.priceMu.Unlock()
	if p, ok := c.price[id]; ok && time.Since(c.priceAt) < time.Hour {
		return p, nil
	}
	var out struct {
		UnitAmount int64  `json:"unit_amount"`
		Currency   string `json:"currency"`
		Recurring  struct {
			Interval string `json:"interval"`
		} `json:"recurring"`
	}
	if err := c.get(ctx, "/v1/prices/"+id, &out); err != nil {
		if p, ok := c.price[id]; ok {
			return p, nil // stale but usable
		}
		return Price{}, err
	}
	p := Price{UnitAmount: out.UnitAmount, Currency: out.Currency, Interval: out.Recurring.Interval}
	c.price[id] = p
	c.priceAt = time.Now()
	return p, nil
}

// AddSubscriptionItem appends a recurring line to an existing subscription (the
// flat monthly domain add-on) and returns the new item id. It prorates, so the
// customer is charged only the remaining fraction of the current period from the
// day the domain goes live.
func (c *Client) AddSubscriptionItem(ctx context.Context, subID, priceID string) (string, error) {
	form := url.Values{}
	form.Set("subscription", subID)
	form.Set("price", priceID)
	form.Set("quantity", "1")
	form.Set("proration_behavior", "create_prorations")
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/v1/subscription_items", form, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("billing: subscription item returned no id")
	}
	return out.ID, nil
}

// RemoveSubscriptionItem deletes a subscription line (detaching a domain drops
// its monthly add-on). A 404 — the item is already gone — surfaces as an error;
// callers that treat detach as idempotent may ignore it.
func (c *Client) RemoveSubscriptionItem(ctx context.Context, itemID string) error {
	return c.del(ctx, "/v1/subscription_items/"+itemID)
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("authorization", "Bearer "+c.key)
	return c.do(req, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+c.key)
	return c.do(req, out)
}

func (c *Client) del(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+c.key)
	return c.do(req, nil)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("billing: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var se struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &se)
		if se.Error != nil {
			return fmt.Errorf("billing: %s: %s", se.Error.Type, se.Error.Message)
		}
		return fmt.Errorf("billing: stripe status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("billing: decode (status %d): %w", resp.StatusCode, err)
		}
	}
	return nil
}
