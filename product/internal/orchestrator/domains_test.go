package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// waitForDomain polls until ok(project) holds or the deadline passes — for the
// async domain provisioning fired off SubscriptionStarted.
func waitForDomain(t *testing.T, st store.Store, id string, ok func(*project.Project) bool) *project.Project {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, err := st.ProjectByID(context.Background(), id); err == nil && ok(p) {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := st.ProjectByID(context.Background(), id)
	t.Fatalf("domain condition not met; status=%s kind=%s intent=%q", p.DomainStatus, p.DomainKind, p.DomainIntent)
	return nil
}

// fakeCF is an in-memory DomainRegistrar for orchestrator tests.
type fakeCF struct {
	offers       []registrar.Offer
	regState     string // RegisterDomain result (default succeeded)
	statusState  string // RegistrationStatus result (default succeeded)
	zoneID       string // ZoneID result (default "zone1")
	ensured      []registrar.Record
	registered   []string
	expiry       time.Time // DomainExpiry result (zero = unknown)
	autoRenewOff []string  // domains SetAutoRenew(false) was called on
}

func (f *fakeCF) SearchDomains(context.Context, string, int) ([]registrar.Offer, error) {
	return f.offers, nil
}
func (f *fakeCF) CheckDomains(context.Context, []string) ([]registrar.Offer, error) {
	return f.offers, nil
}
func (f *fakeCF) RegisterDomain(_ context.Context, name string) (string, error) {
	f.registered = append(f.registered, name)
	if f.regState == "" {
		return registrar.StateSucceeded, nil
	}
	return f.regState, nil
}
func (f *fakeCF) RegistrationStatus(context.Context, string) (string, error) {
	if f.statusState == "" {
		return registrar.StateSucceeded, nil
	}
	return f.statusState, nil
}
func (f *fakeCF) ZoneID(context.Context, string) (string, error) {
	if f.zoneID == "" {
		return "zone1", nil
	}
	return f.zoneID, nil
}
func (f *fakeCF) EnsureDNSRecord(_ context.Context, _ string, rec registrar.Record) error {
	f.ensured = append(f.ensured, rec)
	return nil
}
func (f *fakeCF) DomainExpiry(context.Context, string) (time.Time, error) {
	return f.expiry, nil
}
func (f *fakeCF) SetAutoRenew(_ context.Context, name string, on bool) error {
	if !on {
		f.autoRenewOff = append(f.autoRenewOff, name)
	}
	return nil
}

// fakeBiller records the Stripe calls: one-off invoice items for a purchased
// domain's registration cost, and legacy sub-item removals on detach.
type fakeBiller struct {
	invoiced  []invoiceItem // one-off invoice items added
	removed   []string      // legacy sub-item ids removed
	refunds   []refundCall  // upfront-domain refunds issued
	refundErr error         // when set, RefundSubscriptionCharge fails
}

type refundCall struct {
	sub    string
	amount int
}

type invoiceItem struct {
	customer string
	amount   int
	currency string
}

func (b *fakeBiller) AddInvoiceItem(_ context.Context, customerID string, amountMinor int, currency, _ string) (string, error) {
	b.invoiced = append(b.invoiced, invoiceItem{customer: customerID, amount: amountMinor, currency: currency})
	return "ii_dom", nil
}
func (b *fakeBiller) RemoveSubscriptionItem(_ context.Context, itemID string) error {
	b.removed = append(b.removed, itemID)
	return nil
}
func (b *fakeBiller) RefundSubscriptionCharge(_ context.Context, subscriptionID string, amountMinor int) (string, error) {
	if b.refundErr != nil {
		return "", b.refundErr
	}
	b.refunds = append(b.refunds, refundCall{sub: subscriptionID, amount: amountMinor})
	return "re_dom", nil
}

// seedDomainProject creates a paid, preview_ready project + owner, ready to
// attach a domain. mutate lets a test tweak fields before it's stored.
func seedDomainProject(t *testing.T, st store.Store, mutate func(*project.Project)) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &user.User{ID: "u1", Email: "cust@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{
		ID: "p1", UserID: "u1", Name: "Bageri", Status: project.StatusPreviewReady,
		Locale: "sv", Paid: true, PaidVia: "stripe", StripeCustomerID: "cus_1", StripeSubID: "sub_1",
		PreviewURL: "https://forge-p1.fly.dev", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if mutate != nil {
		mutate(p)
	}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("project: %v", err)
	}
}

func TestAttachDomain_BYOD_LifecycleAndEmailsOnce(t *testing.T) {
	st := store.NewMemory()
	orch, fake := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	orch.SetDomains(&fakeCF{}, biller, 100)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	if err := orch.AttachDomain(ctx, "p1", "acme.se"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainPendingDNS || got.DomainKind != project.DomainKindBYOD {
		t.Fatalf("after attach: %+v", got)
	}
	if len(got.DomainRecords) == 0 {
		t.Fatal("expected DNS records to show the customer")
	}
	if got.DomainIPv6 == "" || len(fake.AllocatedIPv6()) == 0 {
		t.Fatal("apex domain should allocate a dedicated IPv6")
	}

	// DNS not visible yet → stays pending_dns.
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("verify (pending): %v", err)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.DomainStatus != project.DomainPendingDNS {
		t.Fatalf("should stay pending_dns until DNS resolves, got %s", got.DomainStatus)
	}

	// DNS + cert now validated → active, one-time emails, no charge (BYOD is free).
	fake.SetCertActive(true)
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("verify (active): %v", err)
	}
	got, _ = st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainActive || got.DomainVerifiedAt.IsZero() {
		t.Fatalf("expected active+verified, got %+v", got)
	}
	if len(biller.invoiced) != 0 {
		t.Errorf("BYOD must not bill, invoiced: %v", biller.invoiced)
	}
	if !sentTo(rec, "cust@example.com", "domän") { // Swedish "Din domän är live"
		t.Errorf("no localized domain-live email; sent %+v", rec.all())
	}
	if !sentTo(rec, "rasmus@example.com", "domain live") {
		t.Errorf("no operator domain-live email; sent %+v", rec.all())
	}

	// Replay must not re-email or re-bill.
	n := len(rec.all())
	if err := orch.VerifyDomain(ctx, "p1"); err != nil || len(rec.all()) != n {
		t.Errorf("replay should be a no-op: emails %d→%d err=%v", n, len(rec.all()), err)
	}
}

// A domain bought upfront in checkout (DomainPrepaid) must NOT be invoiced again
// when it goes active — it was already charged on the first invoice.
func TestActivateDomain_SkipsBillingWhenPrepaid(t *testing.T) {
	st := store.NewMemory()
	orch, fake := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{
		offers:   []registrar.Offer{{Name: "acme.se", Registrable: true, Price: 129, Renewal: 129, Currency: "SEK"}},
		regState: registrar.StatePending,
	}
	orch.SetDomains(cf, biller, 300)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	if err := orch.BuyDomain(ctx, "p1", "acme.se"); err != nil {
		t.Fatalf("buy: %v", err)
	}
	p, _ := st.ProjectByID(ctx, "p1") // mark prepaid, as the checkout-bundled path does
	p.DomainPrepaid = true
	if err := st.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	_ = orch.VerifyDomain(ctx, "p1") // → verifying
	fake.SetCertActive(true)
	if err := orch.VerifyDomain(ctx, "p1"); err != nil { // → active
		t.Fatalf("reconcile active: %v", err)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.DomainStatus != project.DomainActive {
		t.Fatalf("expected active, got %s", got.DomainStatus)
	}
	if len(biller.invoiced) != 0 {
		t.Errorf("prepaid domain must not be invoiced on activation, got %v", biller.invoiced)
	}
}

// When a bundled (prepaid) domain can't be registered, the upfront charge is
// auto-refunded and the prepaid marker cleared (so a later manual buy bills).
func TestProvisionDomainIntent_RefundsOnFailure(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{offers: []registrar.Offer{{Name: "acme.se", Registrable: false}}} // taken since checkout
	orch.SetDomains(cf, biller, 300)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	p, _ := st.ProjectByID(ctx, "p1")
	p.DomainIntent, p.DomainIntentBuy, p.DomainCostOre, p.StripeSubID = "acme.se", true, 12900, "sub_1"
	if err := st.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}

	orch.provisionDomainIntent("p1")

	if len(biller.refunds) != 1 || biller.refunds[0].sub != "sub_1" || biller.refunds[0].amount != 12900 {
		t.Fatalf("expected a 12900 refund on sub_1, got %v", biller.refunds)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.DomainPrepaid {
		t.Errorf("prepaid marker must be cleared after a refunded failure")
	}
	if len(biller.invoiced) != 0 {
		t.Errorf("a failed domain must not be invoiced, got %v", biller.invoiced)
	}
}

// TestRetryPaidDomain reproduces the ippfnotti.nu recovery: a bundled domain
// paid at checkout but failed to register. The operator retries with the
// hostname; it registers on the PREPAID path (DomainPrepaid=true), so no second
// charge — and guards reject a project that can't be shown to have prepaid.
func TestRetryPaidDomain(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{
		offers:   []registrar.Offer{{Name: "ippfnotti.nu", Registrable: true, Price: 280, Renewal: 280, Currency: "SEK"}},
		regState: registrar.StatePending, // no async success goroutine
	}
	orch.SetDomains(cf, biller, 300)
	// A paid project whose bundled-domain buy failed: the cost was captured at
	// checkout but no domain is attached and the intent was cleared.
	seedDomainProject(t, st, func(p *project.Project) { p.DomainCostOre = 28000 })
	ctx := context.Background()

	if err := orch.RetryPaidDomain(ctx, "p1", "ippfnotti.nu"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	// provisionDomainIntent runs async → wait for the registration + prepaid mark.
	p := waitForDomain(t, st, "p1", func(p *project.Project) bool {
		return p.DomainStatus == project.DomainRegistering && p.DomainPrepaid
	})
	if p.DomainName != "ippfnotti.nu" {
		t.Errorf("domain name = %q, want ippfnotti.nu", p.DomainName)
	}
	if len(cf.registered) != 1 || cf.registered[0] != "ippfnotti.nu" {
		t.Fatalf("expected one registration of ippfnotti.nu, got %v", cf.registered)
	}
	// Prepaid → activation must not add an invoice item (already covered in
	// depth by the prepaid-activation test; here we just confirm the flag).
	if !p.DomainPrepaid {
		t.Error("domain must be marked prepaid so activation skips billing")
	}

	// Guards: an unpaid project, and a paid one with no captured cost, both refuse.
	seedGuard := func(id string, mutate func(*project.Project)) {
		u := &user.User{ID: "u-" + id, Email: id + "@x.se", CreatedAt: time.Now().UTC()}
		_ = st.CreateUser(ctx, u)
		gp := &project.Project{ID: id, UserID: u.ID, Name: id, Status: project.StatusPreviewReady,
			PreviewURL: "https://x.fly.dev", StripeSubID: "sub_x", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		mutate(gp)
		_ = st.CreateProject(ctx, gp)
	}
	seedGuard("unpaid", func(p *project.Project) { p.Paid = false; p.DomainCostOre = 28000 })
	if err := orch.RetryPaidDomain(ctx, "unpaid", "x.se"); !errors.Is(err, ErrNotPaid) {
		t.Errorf("unpaid retry: got %v, want ErrNotPaid", err)
	}
	seedGuard("nocost", func(p *project.Project) { p.Paid = true; p.DomainCostOre = 0 })
	if err := orch.RetryPaidDomain(ctx, "nocost", "x.se"); !errors.Is(err, ErrNoPrepaidAmount) {
		t.Errorf("no-cost retry: got %v, want ErrNoPrepaidAmount", err)
	}
}

func TestBuyDomain_Lifecycle_BillsRegistrationCost(t *testing.T) {
	st := store.NewMemory()
	orch, fake := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{
		offers:   []registrar.Offer{{Name: "acme.se", Registrable: true, Price: 129, Renewal: 129, Currency: "SEK"}},
		regState: registrar.StatePending, // avoid the async success goroutine; drive reconcile by hand
	}
	orch.SetDomains(cf, biller, 300)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	if err := orch.BuyDomain(ctx, "p1", "acme.se"); err != nil {
		t.Fatalf("buy: %v", err)
	}
	if len(cf.registered) != 1 {
		t.Fatalf("expected a registration call, got %v", cf.registered)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.DomainStatus != project.DomainRegistering || got.DomainCostOre != 12900 {
		t.Fatalf("after buy: status=%s cost=%d (want registering / 12900 öre)", got.DomainStatus, got.DomainCostOre)
	}

	// Registration reported succeeded → provision DNS + cert → verifying.
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("reconcile (provision): %v", err)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainVerifying || got.DomainZoneID != "zone1" {
		t.Fatalf("expected verifying+zone, got %+v", got)
	}
	if len(cf.ensured) == 0 {
		t.Fatal("expected DNS records written to the zone")
	}

	// Cert validates → active → the registration cost lands once on the next invoice.
	fake.SetCertActive(true)
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("reconcile (active): %v", err)
	}
	got, _ = st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainActive {
		t.Fatalf("expected active, got %+v", got)
	}
	if len(biller.invoiced) != 1 {
		t.Fatalf("expected one invoice item, got %v", biller.invoiced)
	}
	if ii := biller.invoiced[0]; ii.customer != "cus_1" || ii.amount != 12900 || ii.currency != "sek" {
		t.Errorf("invoice item = %+v, want {cus_1 12900 sek}", ii)
	}
}

func TestBuyDomain_PriceCap(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	cf := &fakeCF{offers: []registrar.Offer{{Name: "acme.se", Registrable: true, Price: 500, Currency: "USD"}}}
	orch.SetDomains(cf, &fakeBiller{}, 100)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	if err := orch.BuyDomain(ctx, "p1", "acme.se"); err != ErrDomainTooPricey {
		t.Fatalf("expected price-cap rejection, got %v", err)
	}
	if len(cf.registered) != 0 {
		t.Fatal("must not register a domain over the cap")
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.HasDomain() {
		t.Fatal("rejected buy must leave no domain")
	}
}

func TestBuyDomain_RenewalCap(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	// Cheap first year, pricey renewal — must be refused like an over-cap price.
	cf := &fakeCF{offers: []registrar.Offer{{Name: "acme.se", Registrable: true, Price: 9, Renewal: 899, Currency: "SEK"}}}
	orch.SetDomains(cf, &fakeBiller{}, 300)
	seedDomainProject(t, st, nil)

	if err := orch.BuyDomain(context.Background(), "p1", "acme.se"); err != ErrDomainTooPricey {
		t.Fatalf("expected renewal-cap rejection, got %v", err)
	}
	if len(cf.registered) != 0 {
		t.Fatal("must not register a domain whose renewal exceeds the cap")
	}
}

func TestBuyDomain_NotRegistrable(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	cf := &fakeCF{offers: []registrar.Offer{{Name: "acme.se", Registrable: false}}}
	orch.SetDomains(cf, &fakeBiller{}, 100)
	seedDomainProject(t, st, nil)

	if err := orch.BuyDomain(context.Background(), "p1", "acme.se"); err != ErrNotRegistrable {
		t.Fatalf("expected not-registrable, got %v", err)
	}
}

func TestReconcileDomain_StuckTimesOut(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	orch.SetDomains(&fakeCF{}, &fakeBiller{}, 100)
	seedDomainProject(t, st, func(p *project.Project) {
		p.DomainName = "acme.se"
		p.DomainKind = project.DomainKindPurchased
		p.DomainStatus = project.DomainRegistering
		p.DomainCreatedAt = time.Now().Add(-80 * time.Hour) // past the 72h limit
	})
	ctx := context.Background()

	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.DomainStatus != project.DomainFailed {
		t.Fatalf("stuck domain should fail, got %s", got.DomainStatus)
	}
	if !sentTo(rec, "rasmus@example.com", "domain failed") {
		t.Errorf("no operator failure alert; sent %+v", rec.all())
	}
}

func TestDetachDomain_RemovesSubItemAndClears(t *testing.T) {
	st := store.NewMemory()
	orch, fake := newTestOrchWithVerifier(st, NoopVerifier{})
	biller := &fakeBiller{}
	orch.SetDomains(&fakeCF{}, biller, 100)
	seedDomainProject(t, st, func(p *project.Project) {
		p.DomainName = "acme.se"
		p.DomainKind = project.DomainKindPurchased
		p.DomainStatus = project.DomainActive
		p.DomainSubItemID = "si_dom"
		p.DomainVerifiedAt = time.Now().UTC()
	})
	ctx := context.Background()

	if err := orch.DetachDomain(ctx, "p1"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.HasDomain() || got.DomainName != "" || got.DomainSubItemID != "" {
		t.Fatalf("detach should clear all domain fields, got %+v", got)
	}
	if len(biller.removed) != 1 || biller.removed[0] != "si_dom" {
		t.Errorf("expected the add-on removed, got %v", biller.removed)
	}
	if fake.HasCert(builder.DeployAppName("p1"), "acme.se") {
		t.Error("cert should have been deleted")
	}
}

func TestAttachDomain_Guards(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetDomains(&fakeCF{}, &fakeBiller{}, 100)
	ctx := context.Background()

	// Malformed names + the preview host are rejected before the project even
	// loads (hostname validation is first).
	if err := orch.AttachDomain(ctx, "p1", "forge-p1.fly.dev"); err != ErrBadHostname {
		t.Fatalf("fly.dev host should be rejected, got %v", err)
	}
	if err := orch.AttachDomain(ctx, "p1", "notadomain"); err != ErrBadHostname {
		t.Fatalf("bare label should be rejected, got %v", err)
	}
	// Unpaid project can't attach even a valid hostname.
	seedDomainProject(t, st, func(p *project.Project) { p.Paid = false })
	if err := orch.AttachDomain(ctx, "p1", "acme.se"); err != ErrNotEligible {
		t.Fatalf("unpaid should be ineligible, got %v", err)
	}
}

// TestSubscriptionStarted_ProvisionsBundledBYOD: a customer who chose to bring
// their own domain at checkout has it attached automatically once they pay.
func TestSubscriptionStarted_ProvisionsBundledBYOD(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	orch.SetDomains(&fakeCF{}, &fakeBiller{}, 100)
	seedDomainProject(t, st, func(p *project.Project) {
		p.Paid, p.PaidVia, p.StripeSubID = false, "", ""
		p.DomainIntent, p.DomainIntentBuy = "acme.se", false
	})

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("subscription started: %v", err)
	}
	got := waitForDomain(t, st, "p1", func(p *project.Project) bool {
		return p.DomainStatus == project.DomainPendingDNS
	})
	if got.DomainKind != project.DomainKindBYOD {
		t.Fatalf("expected BYOD attach, got kind %q", got.DomainKind)
	}
	if got.DomainIntent != "" {
		t.Errorf("intent should be cleared after provisioning, got %q", got.DomainIntent)
	}
}

// TestSubscriptionStarted_ProvisionsBundledBuy: a customer who chose to buy a
// domain at checkout has it registered automatically once they pay.
func TestSubscriptionStarted_ProvisionsBundledBuy(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	orch.SetDomains(&fakeCF{offers: []registrar.Offer{
		{Name: "acme.se", Registrable: true, Price: 12, Renewal: 12, Currency: "USD"},
	}}, &fakeBiller{}, 100)
	seedDomainProject(t, st, func(p *project.Project) {
		p.Paid, p.PaidVia, p.StripeSubID = false, "", ""
		p.DomainIntent, p.DomainIntentBuy = "acme.se", true
	})

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("subscription started: %v", err)
	}
	got := waitForDomain(t, st, "p1", func(p *project.Project) bool {
		return p.DomainKind == project.DomainKindPurchased && p.DomainName == "acme.se"
	})
	if got.DomainIntent != "" {
		t.Errorf("intent should be cleared after provisioning, got %q", got.DomainIntent)
	}
}

// TestSetDomainIntent_Validation: a buy intent is price-cap-checked, BYOD is
// stored on hostname shape alone, and an empty hostname clears the choice.
func TestSetDomainIntent_Validation(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	orch.SetDomains(&fakeCF{offers: []registrar.Offer{
		{Name: "acme.se", Registrable: true, Price: 200, Renewal: 200, Currency: "USD"}, // over the cap
	}}, &fakeBiller{}, 100)
	seedDomainProject(t, st, func(p *project.Project) { p.Paid = false })
	ctx := context.Background()

	if err := orch.SetDomainIntent(ctx, "p1", "acme.se", true); !errors.Is(err, ErrDomainTooPricey) {
		t.Fatalf("over-cap buy intent should be refused, got %v", err)
	}
	if err := orch.SetDomainIntent(ctx, "p1", "myown.se", false); err != nil {
		t.Fatalf("byod intent: %v", err)
	}
	if p, _ := st.ProjectByID(ctx, "p1"); p.DomainIntent != "myown.se" || p.DomainIntentBuy {
		t.Fatalf("byod intent not stored: intent=%q buy=%v", p.DomainIntent, p.DomainIntentBuy)
	}
	if err := orch.SetDomainIntent(ctx, "p1", "", false); err != nil {
		t.Fatalf("clear intent: %v", err)
	}
	if p, _ := st.ProjectByID(ctx, "p1"); p.DomainIntent != "" {
		t.Fatalf("intent not cleared, got %q", p.DomainIntent)
	}
}
