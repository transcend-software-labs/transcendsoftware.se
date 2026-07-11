package web_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/config"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/llm"
	"github.com/transcend-software-labs/rasmus-ai/internal/opencode"
	"github.com/transcend-software-labs/rasmus-ai/internal/orchestrator"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/storage"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/stream"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web"
)

const webhookSecret = "whsec_test"

func mustUser(id, email string) *user.User {
	return &user.User{ID: id, Email: email, Verified: true, CreatedAt: time.Now().UTC()}
}

// storeFor returns the memory store registered for a test server's URL.
func storeFor(base string) store.Store {
	v, _ := testStores.Load(base)
	return v.(store.Store)
}

// fakeStripe stands in for the Stripe API: checkout/portal sessions and price.
func fakeStripe(t *testing.T, captured *url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/prices/"):
			_, _ = w.Write([]byte(`{"unit_amount":9900,"currency":"sek","recurring":{"interval":"month"}}`))
		case r.URL.Path == "/v1/checkout/sessions":
			if captured != nil {
				*captured = r.Form
			}
			_, _ = w.Write([]byte(`{"id":"cs_1","url":"https://checkout.stripe.com/pay/cs_1"}`))
		case r.URL.Path == "/v1/billing_portal/sessions":
			_, _ = w.Write([]byte(`{"url":"https://billing.stripe.com/p/x"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// newBillingServer builds a test server with the Stripe paywall wired to a fake
// Stripe. Mirrors newTestServerAuth but adds SetBilling.
func newBillingServer(t *testing.T, stripeURL string) (*httptest.Server, store.Store) {
	t.Helper()
	st := store.NewMemory()
	fake := llm.NewFake()
	machines := fly.NewFake()
	b := builder.NewSandbox(machines, func(string) opencode.Driver { return opencode.NewFake() }, builder.Config{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := stream.NewBroker(100)
	assets := storage.NewMemory()
	orch := orchestrator.New(st, fake, fake, fake, b, machines, assets, broker, orchestrator.NoopVerifier{}, log)
	cfg := config.Config{
		AdminEmail: "admin@example.com", BaseURL: "https://forge.example",
		StripeSecretKey: "sk_test_x", StripePriceID: "price_base", StripeWebhookSecret: webhookSecret,
	}
	sessions := auth.NewSessions(st, time.Hour)
	srv, err := web.NewServer(cfg, st, sessions, orch, broker, assets, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.SetBilling(billing.New(stripeURL, "sk_test_x"))
	ts := httptest.NewServer(srv.Handler())
	testStores.Store(ts.URL, st)
	return ts, st
}

func signStripe(body string, at time.Time) string {
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, base, body, sig string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/webhooks/stripe", strings.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook post: %v", err)
	}
	return resp
}

func TestSubscribe_RedirectsToCheckout(t *testing.T) {
	var captured url.Values
	stripe := fakeStripe(t, &captured)
	defer stripe.Close()
	srv, st := newBillingServer(t, stripe.URL)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := t.Context()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	p := &project.Project{ID: "pay1", UserID: u.ID, Name: "Bakery", Status: project.StatusPreviewReady, PreviewURL: "https://x"}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tok := csrfToken(t, c, srv.URL)

	// The project page shows the subscribe panel with the price.
	page, _ := c.Get(srv.URL + "/projects/pay1")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if !strings.Contains(string(body), "99 kr") || !strings.Contains(string(body), "/projects/pay1/subscribe") {
		t.Error("project page missing the subscribe panel / price")
	}

	// Subscribe → 303 to the Stripe Checkout URL (don't follow the external redirect).
	noRedir := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedir.PostForm(srv.URL+"/projects/pay1/subscribe", url.Values{"csrf_token": {tok}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://checkout.stripe.com/pay/cs_1" {
		t.Fatalf("subscribe should 303 to checkout, got %d / %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if captured.Get("client_reference_id") != "pay1" || captured.Get("mode") != "subscription" {
		t.Errorf("checkout params wrong: %v", captured.Encode())
	}
}

func TestSubscribe_GuardedWhileBuilding(t *testing.T) {
	var captured url.Values
	stripe := fakeStripe(t, &captured)
	defer stripe.Close()
	srv, st := newBillingServer(t, stripe.URL)
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := t.Context()
	u, _ := st.UserByEmail(ctx, "neighbour@example.com")
	_ = st.CreateProject(ctx, &project.Project{ID: "b1", UserID: u.ID, Name: "X", Status: project.StatusBuilding})
	tok := csrfToken(t, c, srv.URL)
	noRedir := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := noRedir.PostForm(srv.URL+"/projects/b1/subscribe", url.Values{"csrf_token": {tok}})
	resp.Body.Close()
	if resp.Header.Get("Location") != "/projects/b1" || captured != nil {
		t.Fatalf("building project should not reach Stripe; loc=%q captured=%v", resp.Header.Get("Location"), captured)
	}
}

func TestWebhook_PaysAndGates(t *testing.T) {
	stripe := fakeStripe(t, nil)
	defer stripe.Close()
	srv, st := newBillingServer(t, stripe.URL)
	defer srv.Close()
	ctx := t.Context()
	_ = st.CreateUser(ctx, mustUser("u1", "cust@example.com"))
	// An accepted project (customer already accepted) awaiting delivery.
	_ = st.CreateProject(ctx, &project.Project{ID: "acc1", UserID: "u1", Name: "Bakery", Status: project.StatusAccepted, PreviewURL: "https://x"})

	body := `{"type":"checkout.session.completed","data":{"object":{"id":"cs_1",` +
		`"client_reference_id":"acc1","customer":"cus_1","subscription":"sub_1",` +
		`"payment_status":"paid","metadata":{"project_id":"acc1"}}}}`

	// Bad signature → 400, no change.
	bad := postWebhook(t, srv.URL, body, "t=1,v1=deadbeef")
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad signature should 400, got %d", bad.StatusCode)
	}
	if got, _ := st.ProjectByID(ctx, "acc1"); got.Paid {
		t.Fatal("bad-signature webhook must not pay")
	}

	// Valid signature → 200, project paid with the stripe ids.
	ok := postWebhook(t, srv.URL, body, signStripe(body, time.Now()))
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("valid webhook should 200, got %d", ok.StatusCode)
	}
	got, _ := st.ProjectByID(ctx, "acc1")
	if !got.Paid || got.PaidVia != "stripe" || got.StripeSubID != "sub_1" || got.StripeCustomerID != "cus_1" {
		t.Fatalf("webhook did not pay correctly: %+v", got)
	}

	// The full gate: now that it's accepted + paid, the operator can deliver.
	adm := signedInAdminClient(t, srv.URL)
	atok := csrfToken(t, adm, srv.URL)
	dresp, _ := adm.PostForm(srv.URL+"/admin/projects/acc1/deliver", url.Values{"csrf_token": {atok}})
	dresp.Body.Close()
	if got, _ := st.ProjectByID(ctx, "acc1"); got.Status != project.StatusDelivered {
		t.Fatalf("paid+accepted should deliver, got %q", got.Status)
	}
}

func TestWebhook_SubscriptionDeletedAndUnknown(t *testing.T) {
	stripe := fakeStripe(t, nil)
	defer stripe.Close()
	srv, st := newBillingServer(t, stripe.URL)
	defer srv.Close()
	ctx := t.Context()
	_ = st.CreateUser(ctx, mustUser("u1", "cust@example.com"))
	_ = st.CreateProject(ctx, &project.Project{ID: "p1", UserID: "u1", Name: "X",
		Status: project.StatusPreviewReady, Paid: true, PaidVia: "stripe", StripeSubID: "sub_1", StripeCustomerID: "cus_1"})

	// A stale sub id must not un-pay.
	stale := `{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_OLD","metadata":{"project_id":"p1"}}}}`
	r := postWebhook(t, srv.URL, stale, signStripe(stale, time.Now()))
	r.Body.Close()
	if got, _ := st.ProjectByID(ctx, "p1"); !got.Paid {
		t.Error("stale subscription.deleted must not un-pay")
	}
	// The matching sub id un-pays.
	del := `{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_1","metadata":{"project_id":"p1"}}}}`
	r2 := postWebhook(t, srv.URL, del, signStripe(del, time.Now()))
	r2.Body.Close()
	if got, _ := st.ProjectByID(ctx, "p1"); got.Paid {
		t.Error("matching subscription.deleted should un-pay")
	}
	// Unknown event type → 200.
	unk := `{"type":"charge.refunded","data":{"object":{}}}`
	r3 := postWebhook(t, srv.URL, unk, signStripe(unk, time.Now()))
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Errorf("unknown event should 200, got %d", r3.StatusCode)
	}
}

func TestBilling_InvisibleWhenUnconfigured(t *testing.T) {
	srv := newTestServer(t) // no SetBilling
	defer srv.Close()
	c := signedInClient(t, srv.URL)
	ctx := t.Context()
	st := storeFor(srv.URL)
	usr, _ := st.UserByEmail(ctx, "neighbour@example.com")
	_ = st.CreateProject(ctx, &project.Project{ID: "np", UserID: usr.ID, Name: "X", Status: project.StatusPreviewReady, PreviewURL: "https://x"})

	page, _ := c.Get(srv.URL + "/projects/np")
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if strings.Contains(string(body), "/projects/np/subscribe") {
		t.Error("unconfigured billing must not show the subscribe panel")
	}
	// Webhook route 404s.
	r := postWebhook(t, srv.URL, `{"type":"x"}`, "t=1,v1=x")
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("unconfigured webhook should 404, got %d", r.StatusCode)
	}
}
