package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/cloudflare"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// fakeCF is an in-memory DomainRegistrar for orchestrator tests.
type fakeCF struct {
	offers      []cloudflare.DomainOffer
	regState    string // RegisterDomain result (default succeeded)
	statusState string // RegistrationStatus result (default succeeded)
	zoneID      string // ZoneID result (default "zone1")
	ensured     []cloudflare.DNSRecord
	registered  []string
}

func (f *fakeCF) SearchDomains(context.Context, string, int) ([]cloudflare.DomainOffer, error) {
	return f.offers, nil
}
func (f *fakeCF) CheckDomains(context.Context, []string) ([]cloudflare.DomainOffer, error) {
	return f.offers, nil
}
func (f *fakeCF) RegisterDomain(_ context.Context, name string) (string, error) {
	f.registered = append(f.registered, name)
	if f.regState == "" {
		return cloudflare.StateSucceeded, nil
	}
	return f.regState, nil
}
func (f *fakeCF) RegistrationStatus(context.Context, string) (string, error) {
	if f.statusState == "" {
		return cloudflare.StateSucceeded, nil
	}
	return f.statusState, nil
}
func (f *fakeCF) ZoneID(context.Context, string) (string, error) {
	if f.zoneID == "" {
		return "zone1", nil
	}
	return f.zoneID, nil
}
func (f *fakeCF) EnsureDNSRecord(_ context.Context, _ string, rec cloudflare.DNSRecord) error {
	f.ensured = append(f.ensured, rec)
	return nil
}

// fakeBiller records the Stripe sub-item calls.
type fakeBiller struct {
	added   []string // subscription ids an item was added to
	removed []string // item ids removed
}

func (b *fakeBiller) AddSubscriptionItem(_ context.Context, subID, _ string) (string, error) {
	b.added = append(b.added, subID)
	return "si_dom", nil
}
func (b *fakeBiller) RemoveSubscriptionItem(_ context.Context, itemID string) error {
	b.removed = append(b.removed, itemID)
	return nil
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
		Locale: "sv", Paid: true, PaidVia: "stripe", StripeSubID: "sub_1",
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
	orch.SetDomains(&fakeCF{}, biller, "price_dom", 100)
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

	// DNS + cert now validated → active, one-time emails, no add-on (BYOD is free).
	fake.SetCertActive(true)
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("verify (active): %v", err)
	}
	got, _ = st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainActive || got.DomainVerifiedAt.IsZero() {
		t.Fatalf("expected active+verified, got %+v", got)
	}
	if len(biller.added) != 0 {
		t.Errorf("BYOD must not add a billing item, added: %v", biller.added)
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

func TestBuyDomain_Lifecycle_AddsSubItem(t *testing.T) {
	st := store.NewMemory()
	orch, fake := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{
		offers:   []cloudflare.DomainOffer{{Name: "acme.se", Registrable: true, Price: 12, Currency: "USD"}},
		regState: cloudflare.StatePending, // avoid the async success goroutine; drive reconcile by hand
	}
	orch.SetDomains(cf, biller, "price_dom", 100)
	seedDomainProject(t, st, nil)
	ctx := context.Background()

	if err := orch.BuyDomain(ctx, "p1", "acme.se"); err != nil {
		t.Fatalf("buy: %v", err)
	}
	if len(cf.registered) != 1 {
		t.Fatalf("expected a registration call, got %v", cf.registered)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.DomainStatus != project.DomainRegistering {
		t.Fatalf("after buy: %s", got.DomainStatus)
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

	// Cert validates → active → the flat monthly add-on lands on the sub.
	fake.SetCertActive(true)
	if err := orch.VerifyDomain(ctx, "p1"); err != nil {
		t.Fatalf("reconcile (active): %v", err)
	}
	got, _ = st.ProjectByID(ctx, "p1")
	if got.DomainStatus != project.DomainActive || got.DomainSubItemID != "si_dom" {
		t.Fatalf("expected active+sub-item, got %+v", got)
	}
	if len(biller.added) != 1 || biller.added[0] != "sub_1" {
		t.Errorf("expected add-on on sub_1, got %v", biller.added)
	}
}

func TestBuyDomain_PriceCap(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	cf := &fakeCF{offers: []cloudflare.DomainOffer{{Name: "acme.se", Registrable: true, Price: 500, Currency: "USD"}}}
	orch.SetDomains(cf, &fakeBiller{}, "price_dom", 100)
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

func TestBuyDomain_NotRegistrable(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	cf := &fakeCF{offers: []cloudflare.DomainOffer{{Name: "acme.se", Registrable: false}}}
	orch.SetDomains(cf, &fakeBiller{}, "price_dom", 100)
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
	orch.SetDomains(&fakeCF{}, &fakeBiller{}, "price_dom", 100)
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
	orch.SetDomains(&fakeCF{}, biller, "price_dom", 100)
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
	orch.SetDomains(&fakeCF{}, &fakeBiller{}, "price_dom", 100)
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
