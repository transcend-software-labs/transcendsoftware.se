package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateCheckoutSession_OneTimeDomainItem(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBody = r.Form.Encode()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_1","url":"https://pay"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk_test_x")
	if _, err := c.CreateCheckoutSession(context.Background(), CheckoutParams{
		ProjectID: "p1",
		LineItems: []LineItem{
			{Price: "price_base"},
			{AmountMinor: 12900, Currency: "sek", Name: "Domän: acme.se (1 år)"}, // bundled domain, upfront
		},
	}); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	for _, want := range []string{
		"line_items%5B0%5D%5Bprice%5D=price_base",                  // recurring plan
		"line_items%5B1%5D%5Bprice_data%5D%5Bcurrency%5D=sek",      // one-time domain
		"line_items%5B1%5D%5Bprice_data%5D%5Bunit_amount%5D=12900", // its cost
		"line_items%5B1%5D%5Bprice_data%5D%5Bproduct_data%5D%5Bname%5D=",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("checkout body missing %q\nbody: %s", want, gotBody)
		}
	}
	if strings.Contains(gotBody, "line_items%5B1%5D%5Bprice%5D=") {
		t.Errorf("one-time item must use price_data, not a price id:\n%s", gotBody)
	}
}

func TestCreateCheckoutSession(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		_ = r.ParseForm()
		gotBody = r.Form.Encode()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_123","url":"https://checkout.stripe.com/c/pay/cs_123"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk_test_x")
	url, err := c.CreateCheckoutSession(context.Background(), CheckoutParams{
		ProjectID: "proj-1", CustomerEmail: "a@b.se", Locale: "sv",
		SuccessURL: "https://f.se/projects/proj-1?sub=success",
		CancelURL:  "https://f.se/projects/proj-1?sub=cancel",
		LineItems:  []LineItem{{Price: "price_base"}, {Price: "price_dns", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if url != "https://checkout.stripe.com/c/pay/cs_123" {
		t.Fatalf("url = %q", url)
	}
	if gotPath != "/v1/checkout/sessions" || gotAuth != "Bearer sk_test_x" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	for _, want := range []string{
		"mode=subscription",
		"client_reference_id=proj-1",
		"metadata%5Bproject_id%5D=proj-1",
		"subscription_data%5Bmetadata%5D%5Bproject_id%5D=proj-1",
		"line_items%5B0%5D%5Bprice%5D=price_base",
		"line_items%5B0%5D%5Bquantity%5D=1",      // defaulted from 0
		"line_items%5B1%5D%5Bprice%5D=price_dns", // the future DNS line rides the same slice
		"customer_email=a%40b.se",
		"locale=sv",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("checkout body missing %q\nbody: %s", want, gotBody)
		}
	}
}

func TestCreatePortalSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/v1/billing_portal/sessions" || r.Form.Get("customer") != "cus_1" {
			t.Errorf("path=%q customer=%q", r.URL.Path, r.Form.Get("customer"))
		}
		_, _ = w.Write([]byte(`{"url":"https://billing.stripe.com/p/session/x"}`))
	}))
	defer srv.Close()
	u, err := New(srv.URL, "sk_test_x").CreatePortalSession(context.Background(), "cus_1", "https://f.se/projects/p")
	if err != nil || u != "https://billing.stripe.com/p/session/x" {
		t.Fatalf("portal url=%q err=%v", u, err)
	}
}

func TestPrice_CachesAndDegrades(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			_, _ = w.Write([]byte(`{"unit_amount":9900,"currency":"sek","recurring":{"interval":"month"}}`))
			return
		}
		http.Error(w, `{"error":{"type":"api_error","message":"boom"}}`, 500) // later fetches fail
	}))
	defer srv.Close()

	c := New(srv.URL, "sk_test_x")
	p, err := c.Price(context.Background(), "price_base")
	if err != nil || p.UnitAmount != 9900 || p.Currency != "sek" || p.Interval != "month" {
		t.Fatalf("price=%+v err=%v", p, err)
	}
	// Second call inside the hour is served from cache (server not hit again).
	if _, err := c.Price(context.Background(), "price_base"); err != nil || hits != 1 {
		t.Fatalf("expected cache hit, hits=%d err=%v", hits, err)
	}
	// Force a refresh by expiring the cache; the fetch fails but the stale value serves.
	c.priceAt = c.priceAt.Add(-2 * 60 * 60 * 1e9) // -2h
	if p2, err := c.Price(context.Background(), "price_base"); err != nil || p2.UnitAmount != 9900 {
		t.Fatalf("stale degrade failed: p=%+v err=%v (hits=%d)", p2, err, hits)
	}
}

func TestCheckout_StripeErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"No such price"}}`, 400)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "sk_test_x").CreateCheckoutSession(context.Background(), CheckoutParams{
		ProjectID: "p", LineItems: []LineItem{{Price: "price_bad"}},
	})
	if err == nil || !strings.Contains(err.Error(), "No such price") {
		t.Fatalf("expected surfaced stripe error, got %v", err)
	}
}

func TestAddSubscriptionItem(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = r.ParseForm()
		gotBody = r.Form.Encode()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"si_123","object":"subscription_item"}`))
	}))
	defer srv.Close()

	id, err := New(srv.URL, "sk_test_x").AddSubscriptionItem(context.Background(), "sub_1", "price_dom")
	if err != nil {
		t.Fatalf("add item: %v", err)
	}
	if id != "si_123" {
		t.Fatalf("id = %q", id)
	}
	if gotPath != "/v1/subscription_items" || gotMethod != http.MethodPost {
		t.Fatalf("path=%q method=%q", gotPath, gotMethod)
	}
	for _, want := range []string{
		"subscription=sub_1",
		"price=price_dom",
		"quantity=1",
		"proration_behavior=create_prorations",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("add-item body missing %q\nbody: %s", want, gotBody)
		}
	}
}

func TestAddInvoiceItem(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = r.ParseForm()
		gotBody = r.Form.Encode()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ii_123","object":"invoiceitem"}`))
	}))
	defer srv.Close()

	id, err := New(srv.URL, "sk_test_x").AddInvoiceItem(context.Background(), "cus_1", 4900, "sek", "Extra change")
	if err != nil {
		t.Fatalf("add invoice item: %v", err)
	}
	if id != "ii_123" {
		t.Fatalf("id = %q", id)
	}
	if gotPath != "/v1/invoiceitems" || gotMethod != http.MethodPost {
		t.Fatalf("path=%q method=%q", gotPath, gotMethod)
	}
	for _, want := range []string{
		"customer=cus_1",
		"amount=4900",
		"currency=sek",
		"description=Extra+change",
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("invoice-item body missing %q\nbody: %s", want, gotBody)
		}
	}
}

// stubStripe routes GET subscription → invoice → POST refund for the refund
// tests. invoiceJSON is the body returned for the invoice retrieve; it captures
// the Stripe-Version header and refund form for assertions.
type refundStub struct {
	invoiceJSON    string
	gotVersion     string
	gotInvoicePath string
	gotRefundForm  string
}

func (s *refundStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			_, _ = w.Write([]byte(`{"id":"sub_1","latest_invoice":"in_1"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/invoices/"):
			s.gotVersion = r.Header.Get("Stripe-Version")
			s.gotInvoicePath = r.URL.RequestURI()
			_, _ = w.Write([]byte(s.invoiceJSON))
		case r.URL.Path == "/v1/refunds":
			_ = r.ParseForm()
			s.gotRefundForm = r.Form.Encode()
			_, _ = w.Write([]byte(`{"id":"re_123","object":"refund"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRefund_BasilPaymentsShape: on basil+ accounts the invoice has no top-level
// payment_intent — the id lives in payments.data[].payment.payment_intent. The
// refund must pin the basil version, expand payments, and refund that PI. This
// is the prod bug: reading the removed field gave "no payment intent".
func TestRefund_BasilPaymentsShape(t *testing.T) {
	s := &refundStub{invoiceJSON: `{"id":"in_1","payment_intent":null,"charge":null,` +
		`"payments":{"object":"list","data":[{"payment":{"type":"payment_intent","payment_intent":"pi_basil","charge":null}}]}}`}
	srv := s.server(t)

	id, err := New(srv.URL, "sk_test_x").RefundSubscriptionCharge(context.Background(), "sub_1", 28000)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if id != "re_123" {
		t.Fatalf("refund id = %q", id)
	}
	if s.gotVersion != stripeBasilVersion {
		t.Errorf("invoice read must pin %q, got version %q", stripeBasilVersion, s.gotVersion)
	}
	if !strings.Contains(s.gotInvoicePath, "expand") || !strings.Contains(s.gotInvoicePath, "payments") {
		t.Errorf("invoice read must expand payments, got %q", s.gotInvoicePath)
	}
	for _, want := range []string{"payment_intent=pi_basil", "amount=28000"} {
		if !strings.Contains(s.gotRefundForm, want) {
			t.Errorf("refund form missing %q\nform: %s", want, s.gotRefundForm)
		}
	}
}

// TestRefund_ChargeFallback: a payment recorded as a bare charge (no PI) must
// still refund, via the charge parameter.
func TestRefund_ChargeFallback(t *testing.T) {
	s := &refundStub{invoiceJSON: `{"id":"in_1",` +
		`"payments":{"data":[{"payment":{"type":"charge","payment_intent":null,"charge":"ch_bare"}}]}}`}
	srv := s.server(t)

	if _, err := New(srv.URL, "sk_test_x").RefundSubscriptionCharge(context.Background(), "sub_1", 28000); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if !strings.Contains(s.gotRefundForm, "charge=ch_bare") {
		t.Errorf("refund should fall back to the charge, got form: %s", s.gotRefundForm)
	}
	if strings.Contains(s.gotRefundForm, "payment_intent=") {
		t.Errorf("no PI available — must not send payment_intent; form: %s", s.gotRefundForm)
	}
}

// TestRefund_LegacyPaymentIntent: a pre-basil account still returns the
// top-level payment_intent; the fallback must use it.
func TestRefund_LegacyPaymentIntent(t *testing.T) {
	s := &refundStub{invoiceJSON: `{"id":"in_1","payment_intent":"pi_legacy","payments":{"data":[]}}`}
	srv := s.server(t)

	if _, err := New(srv.URL, "sk_test_x").RefundSubscriptionCharge(context.Background(), "sub_1", 100); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if !strings.Contains(s.gotRefundForm, "payment_intent=pi_legacy") {
		t.Errorf("legacy top-level payment_intent should be used, got form: %s", s.gotRefundForm)
	}
}

// TestRefund_NoPaymentResolvable: when neither a PI nor a charge can be found,
// the refund errors clearly instead of silently no-op-ing.
func TestRefund_NoPaymentResolvable(t *testing.T) {
	s := &refundStub{invoiceJSON: `{"id":"in_1","payments":{"data":[]}}`}
	srv := s.server(t)

	_, err := New(srv.URL, "sk_test_x").RefundSubscriptionCharge(context.Background(), "sub_1", 100)
	if err == nil || !strings.Contains(err.Error(), "no resolvable payment") {
		t.Fatalf("want a clear no-payment error, got %v", err)
	}
	if s.gotRefundForm != "" {
		t.Error("no refund should be attempted when no payment resolves")
	}
}

func TestRemoveSubscriptionItem(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"si_123","deleted":true}`))
	}))
	defer srv.Close()

	if err := New(srv.URL, "sk_test_x").RemoveSubscriptionItem(context.Background(), "si_123"); err != nil {
		t.Fatalf("remove item: %v", err)
	}
	if gotPath != "/v1/subscription_items/si_123" || gotMethod != http.MethodDelete {
		t.Fatalf("path=%q method=%q", gotPath, gotMethod)
	}
}
