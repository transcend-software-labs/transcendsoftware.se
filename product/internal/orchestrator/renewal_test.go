package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

// activePurchasedDomain seeds p1 as a live purchased domain paid through
// `paidThrough`, with the registrar reporting `expiry`.
func activePurchasedDomain(t *testing.T, expiry, paidThrough time.Time) (*Orchestrator, *fakeBiller, *fakeCF, *recordingNotifier, store.Store) {
	t.Helper()
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	biller := &fakeBiller{}
	cf := &fakeCF{expiry: expiry}
	orch.SetDomains(cf, biller, 300)
	seedDomainProject(t, st, func(p *project.Project) {
		p.DomainName = "acme.se"
		p.DomainKind = project.DomainKindPurchased
		p.DomainStatus = project.DomainActive
		p.DomainCostOre = 12900
		p.DomainVerifiedAt = time.Now().UTC()
		p.DomainPaidThrough = paidThrough
	})
	return orch, biller, cf, rec, st
}

func TestRebillDomainRenewal_BillsOnExpiryAdvance(t *testing.T) {
	now := time.Now().UTC()
	orch, biller, _, rec, st := activePurchasedDomain(t, now.AddDate(2, 0, 0), now.AddDate(1, 0, 0))
	ctx := context.Background()

	orch.rebillDomainRenewals(ctx)

	if len(biller.invoiced) != 1 {
		t.Fatalf("expected one renewal invoice item, got %v", biller.invoiced)
	}
	if ii := biller.invoiced[0]; ii.customer != "cus_1" || ii.amount != 12900 || ii.currency != "sek" {
		t.Errorf("renewal invoice item = %+v, want {cus_1 12900 sek}", ii)
	}
	if !sentTo(rec, "cust@example.com", "förnyats") { // sv "Din domän har förnyats"
		t.Errorf("no renewal email to the customer; sent %+v", rec.all())
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if !got.DomainPaidThrough.Equal(now.AddDate(2, 0, 0)) {
		t.Errorf("paid-through should advance to the new expiry, got %v", got.DomainPaidThrough)
	}

	// Idempotent: a second pass with the same expiry must not double-bill.
	orch.rebillDomainRenewals(ctx)
	if len(biller.invoiced) != 1 {
		t.Errorf("second pass double-billed: %v", biller.invoiced)
	}
}

func TestRebillDomainRenewal_NoRenewalNoBill(t *testing.T) {
	now := time.Now().UTC()
	// Expiry equals what they've paid through — nothing renewed.
	orch, biller, _, _, _ := activePurchasedDomain(t, now.AddDate(1, 0, 0), now.AddDate(1, 0, 0))
	orch.rebillDomainRenewals(context.Background())
	if len(biller.invoiced) != 0 {
		t.Errorf("no renewal should mean no bill, got %v", biller.invoiced)
	}
}

func TestRebillDomainRenewal_FirstObservationAnchors(t *testing.T) {
	now := time.Now().UTC()
	// Legacy domain: no paid-through recorded yet.
	orch, biller, _, _, st := activePurchasedDomain(t, now.AddDate(1, 0, 0), time.Time{})
	orch.rebillDomainRenewals(context.Background())
	if len(biller.invoiced) != 0 {
		t.Errorf("first observation must anchor, not bill; got %v", biller.invoiced)
	}
	got, _ := st.ProjectByID(context.Background(), "p1")
	if !got.DomainPaidThrough.Equal(now.AddDate(1, 0, 0)) {
		t.Errorf("anchor not set to the observed expiry, got %v", got.DomainPaidThrough)
	}
}

func TestDetachDomain_DisablesAutoRenewForPurchased(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	biller := &fakeBiller{}
	cf := &fakeCF{}
	orch.SetDomains(cf, biller, 300)
	seedDomainProject(t, st, func(p *project.Project) {
		p.DomainName = "acme.se"
		p.DomainKind = project.DomainKindPurchased
		p.DomainStatus = project.DomainActive
		p.DomainVerifiedAt = time.Now().UTC()
	})
	if err := orch.DetachDomain(context.Background(), "p1"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if len(cf.autoRenewOff) != 1 || cf.autoRenewOff[0] != "acme.se" {
		t.Errorf("detach should disable auto-renew for a purchased domain, got %v", cf.autoRenewOff)
	}
}

func TestDetachDomain_BYODLeavesAutoRenewAlone(t *testing.T) {
	st := store.NewMemory()
	orch, _ := newTestOrchWithVerifier(st, NoopVerifier{})
	cf := &fakeCF{}
	orch.SetDomains(cf, &fakeBiller{}, 300)
	seedDomainProject(t, st, func(p *project.Project) {
		p.DomainName = "acme.se"
		p.DomainKind = project.DomainKindBYOD
		p.DomainStatus = project.DomainActive
		p.DomainVerifiedAt = time.Now().UTC()
	})
	if err := orch.DetachDomain(context.Background(), "p1"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if len(cf.autoRenewOff) != 0 {
		t.Errorf("BYOD detach must not touch registrar auto-renew, got %v", cf.autoRenewOff)
	}
}
