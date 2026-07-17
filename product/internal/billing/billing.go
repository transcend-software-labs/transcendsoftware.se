// Package billing talks to the Stripe API for the subscription paywall with a
// small hand-rolled client — this repo keeps external API clients bespoke (see
// internal/imagegen), no vendor SDK. It creates Checkout Sessions for the
// subscribe flow, billing-portal sessions for self-serve management, and reads
// the plan Price for display. Webhook signature verification is in webhook.go.
//
// Most calls send no Stripe-Version header: the endpoints and parameters used
// here (Checkout Sessions, Billing Portal, Prices, and the three webhook events
// we read) have been stable for years, so the account's default API version
// applies. The one exception is the invoice read in RefundSubscriptionCharge,
// which pins a version (stripeBasilVersion) because the Invoice payment linkage
// moved between API versions — see there.
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

// LineItem is one Checkout line. Either a recurring plan (Price = a Stripe price
// id) or a one-time inline charge (AmountMinor>0, with Currency+Name). In a
// subscription Checkout a one-time item lands on the FIRST invoice — that's how a
// bundled domain is charged upfront alongside the plan, in a single payment.
type LineItem struct {
	Price       string
	Quantity    int
	AmountMinor int    // one-time inline price (minor units); when >0, Price is ignored
	Currency    string // one-time currency, e.g. "sek"
	Name        string // one-time product name shown on checkout + the invoice
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
		q := li.Quantity
		if q == 0 {
			q = 1
		}
		form.Set(fmt.Sprintf("line_items[%d][quantity]", i), strconv.Itoa(q))
		if li.AmountMinor > 0 {
			// One-time inline price → charged on the subscription's first invoice.
			form.Set(fmt.Sprintf("line_items[%d][price_data][currency]", i), li.Currency)
			form.Set(fmt.Sprintf("line_items[%d][price_data][unit_amount]", i), strconv.Itoa(li.AmountMinor))
			form.Set(fmt.Sprintf("line_items[%d][price_data][product_data][name]", i), li.Name)
			continue
		}
		form.Set(fmt.Sprintf("line_items[%d][price]", i), li.Price)
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

// AddInvoiceItem creates a pending invoice item on the customer for a flat,
// one-off amount (an extra change beyond the monthly allowance). With no invoice
// specified, Stripe attaches it to the customer's next scheduled invoice — so
// overage rides along on the next monthly subscription charge, no separate
// payment. amountMinor is in the currency's minor unit (öre for SEK). Returns the
// invoice-item id.
func (c *Client) AddInvoiceItem(ctx context.Context, customerID string, amountMinor int, currency, description string) (string, error) {
	form := url.Values{}
	form.Set("customer", customerID)
	form.Set("amount", strconv.Itoa(amountMinor))
	form.Set("currency", currency)
	if description != "" {
		form.Set("description", description)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/v1/invoiceitems", form, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("billing: invoice item returned no id")
	}
	return out.ID, nil
}

// RefundSubscriptionCharge partially refunds the payment that settled a
// subscription's latest invoice — used to auto-refund a bundled domain we
// charged upfront but couldn't register. It resolves subscription → latest
// invoice → payment intent, then refunds amountMinor. Best-effort.
func (c *Client) RefundSubscriptionCharge(ctx context.Context, subscriptionID string, amountMinor int) (string, error) {
	var sub struct {
		LatestInvoice string `json:"latest_invoice"`
	}
	if err := c.get(ctx, "/v1/subscriptions/"+subscriptionID, &sub); err != nil {
		return "", fmt.Errorf("refund: get subscription: %w", err)
	}
	if sub.LatestInvoice == "" {
		return "", fmt.Errorf("refund: subscription %s has no invoice yet", subscriptionID)
	}
	// Resolve the invoice's payment. Stripe's "basil" API versions (2025-03-31+,
	// now the account default) removed the top-level invoice.payment_intent /
	// invoice.charge and moved the linkage into an invoice.payments sub-list that
	// must be expanded — reading the old field is why this returned "no payment
	// intent" and the refund never fired. Pin basil so the shape is deterministic
	// regardless of the account default, and expand the list.
	var inv struct {
		PaymentIntent string `json:"payment_intent"` // pre-basil (kept as a fallback)
		Charge        string `json:"charge"`         // pre-basil
		Payments      struct {
			Data []struct {
				Payment struct {
					PaymentIntent string `json:"payment_intent"`
					Charge        string `json:"charge"`
				} `json:"payment"`
			} `json:"data"`
		} `json:"payments"` // basil+
	}
	if err := c.getVersioned(ctx, "/v1/invoices/"+sub.LatestInvoice+"?expand[]=payments", stripeBasilVersion, &inv); err != nil {
		return "", fmt.Errorf("refund: get invoice: %w", err)
	}
	paymentIntent, charge := inv.PaymentIntent, inv.Charge
	for _, pm := range inv.Payments.Data {
		if paymentIntent == "" {
			paymentIntent = pm.Payment.PaymentIntent
		}
		if charge == "" {
			charge = pm.Payment.Charge
		}
	}
	// Refunds accept either a payment_intent or a charge; prefer the PI.
	form := url.Values{}
	switch {
	case paymentIntent != "":
		form.Set("payment_intent", paymentIntent)
	case charge != "":
		form.Set("charge", charge)
	default:
		return "", fmt.Errorf("refund: invoice %s has no resolvable payment (checked payment_intent, charge, payments)", sub.LatestInvoice)
	}
	form.Set("amount", strconv.Itoa(amountMinor))
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/v1/refunds", form, &out); err != nil {
		return "", fmt.Errorf("refund: create: %w", err)
	}
	return out.ID, nil
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
	return c.getVersioned(ctx, path, "", out)
}

// stripeBasilVersion pins the invoice read in RefundSubscriptionCharge to a
// version whose Invoice object exposes the payments sub-list, so the refund
// doesn't depend on (or break with) the account's default API version.
const stripeBasilVersion = "2025-03-31.basil"

// getVersioned is get with an optional Stripe-Version header ("" = account
// default). Pinning a version makes a response shape deterministic for the few
// calls that read version-sensitive fields.
func (c *Client) getVersioned(ctx context.Context, path, apiVersion string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+c.key)
	if apiVersion != "" {
		req.Header.Set("Stripe-Version", apiVersion)
	}
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
