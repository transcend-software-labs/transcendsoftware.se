package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
