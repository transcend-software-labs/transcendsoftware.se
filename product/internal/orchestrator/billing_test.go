package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// seedSubTestProject creates an unpaid preview_ready project + its owner.
func seedSubTestProject(t *testing.T, st store.Store, locale string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &user.User{ID: "u1", Email: "cust@example.com", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("user: %v", err)
	}
	p := &project.Project{ID: "p1", UserID: "u1", Name: "Bageri", Status: project.StatusPreviewReady,
		Locale: locale, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatalf("project: %v", err)
	}
}

func TestSubscriptionStarted(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "sv")
	ctx := context.Background()

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if !got.Paid || got.PaidVia != "stripe" || got.StripeCustomerID != "cus_1" || got.StripeSubID != "sub_1" {
		t.Fatalf("state after started: %+v", got)
	}
	// Paying IS accepting: the preview moves into the review queue on payment.
	if got.Status != project.StatusAccepted {
		t.Fatalf("status after payment = %q, want accepted", got.Status)
	}
	if !sentTo(rec, "cust@example.com", "prenumeration") { // Swedish subject
		t.Errorf("no localized customer email; sent: %+v", rec.all())
	}
	if !sentTo(rec, "rasmus@example.com", "review & deliver") {
		t.Errorf("no operator email; sent: %+v", rec.all())
	}
	// Replay (Stripe retry) = no duplicate emails.
	n := len(rec.all())
	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil || len(rec.all()) != n {
		t.Errorf("replay should be a no-op: emails %d→%d err=%v", n, len(rec.all()), err)
	}
}

func TestSubscriptionStarted_Comped(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "en")
	ctx := context.Background()
	if err := orch.MarkPaid("p1", "manual"); err != nil { // comped by Rasmus
		t.Fatalf("comp: %v", err)
	}
	n := len(rec.all())
	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.PaidVia != "manual" || got.StripeSubID != "sub_1" {
		t.Fatalf("comped project should keep manual + record the IDs: %+v", got)
	}
	if len(rec.all()) != n {
		t.Errorf("comped subscribe should not email; sent: %+v", rec.all())
	}
	// Auto-accept rides the unpaid→paid transition only: a comped project keeps
	// its explicit accept step (the accept button stays visible for it).
	if got.Status != project.StatusPreviewReady {
		t.Fatalf("comped subscribe must not auto-accept, status = %q", got.Status)
	}
}

func TestSubscriptionEnded(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	rec := &recordingNotifier{}
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "en")
	ctx := context.Background()
	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	// A stale delete (a different, older sub id) must not un-pay.
	if err := orch.SubscriptionEnded("p1", "sub_OLD"); err != nil {
		t.Fatalf("ended stale: %v", err)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); !got.Paid {
		t.Error("stale subscription.deleted must not un-pay a live subscription")
	}
	// The matching delete un-pays and alerts the operator.
	if err := orch.SubscriptionEnded("p1", "sub_1"); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if got, _ := st.ProjectByID(ctx, "p1"); got.Paid {
		t.Error("matching subscription.deleted should un-pay")
	}
	if !sentTo(rec, "rasmus@example.com", "cancelled") {
		t.Errorf("no cancellation email; sent: %+v", rec.all())
	}
}

// seedPurchasedDomain attaches a live, Forge-bought domain to the seeded project.
func seedPurchasedDomain(t *testing.T, st store.Store, kind string) {
	t.Helper()
	ctx := context.Background()
	p, err := st.ProjectByID(ctx, "p1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p.DomainName = "acme.se"
	p.DomainKind = kind
	p.DomainStatus = project.DomainActive
	p.DomainPaidThrough = time.Date(2027, 3, 4, 0, 0, 0, 0, time.UTC)
	if err := st.UpdateProject(ctx, p); err != nil {
		t.Fatalf("save: %v", err)
	}
}

// Churn must stop us paying the registrar: a bought domain auto-renews on OUR
// card, so cancelling has to switch that off. The domain itself stays attached —
// the customer paid through the year and may come back before it lapses.
func TestSubscriptionEnded_StopsPayingForPurchasedDomain(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	cf := &fakeCF{}
	orch.SetDomains(cf, &fakeBiller{}, 100)
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "sv")
	ctx := context.Background()

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	seedPurchasedDomain(t, st, project.DomainKindPurchased)
	if err := orch.SubscriptionEnded("p1", "sub_1"); err != nil {
		t.Fatalf("ended: %v", err)
	}

	if len(cf.autoRenewOff) != 1 || cf.autoRenewOff[0] != "acme.se" {
		t.Errorf("cancellation must stop registrar auto-renew, got %v", cf.autoRenewOff)
	}
	got, _ := st.ProjectByID(ctx, "p1")
	if got.DomainName != "acme.se" {
		t.Error("the domain stays attached until it lapses; only renewal stops")
	}
}

// A domain the customer owns elsewhere is not ours to touch, whatever happens
// to their subscription.
func TestSubscriptionEnded_LeavesBYODAlone(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	cf := &fakeCF{}
	orch.SetDomains(cf, &fakeBiller{}, 100)
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "sv")

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	seedPurchasedDomain(t, st, project.DomainKindBYOD)
	if err := orch.SubscriptionEnded("p1", "sub_1"); err != nil {
		t.Fatalf("ended: %v", err)
	}

	if len(cf.autoRenewOff) != 0 {
		t.Errorf("BYOD domains live at the customer's registrar, got %v", cf.autoRenewOff)
	}
}

// A registrar outage must not break the billing event: the project still un-pays
// and the operator still hears about it.
func TestSubscriptionEnded_SurvivesRegistrarFailure(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	rec := &recordingNotifier{}
	orch.SetDomains(&fakeCF{autoRenewErr: errors.New("registrar down")}, &fakeBiller{}, 100)
	orch.SetNotifications(rec, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "sv")

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	seedPurchasedDomain(t, st, project.DomainKindPurchased)
	if err := orch.SubscriptionEnded("p1", "sub_1"); err != nil {
		t.Fatalf("ended: %v", err)
	}

	if got, _ := st.ProjectByID(context.Background(), "p1"); got.Paid {
		t.Error("a registrar failure must not block un-paying")
	}
	if !sentTo(rec, "rasmus@example.com", "cancelled") {
		t.Errorf("no cancellation email; sent %+v", rec.all())
	}
}

// Coming back before the domain lapses has to switch renewal back on, or the
// site would go dark a year later with the customer paying.
func TestSubscriptionStarted_ResumesRenewalForReturningCustomer(t *testing.T) {
	st := store.NewMemory()
	orch := newTestOrch(st)
	cf := &fakeCF{}
	orch.SetDomains(cf, &fakeBiller{}, 100)
	orch.SetNotifications(&recordingNotifier{}, "rasmus@example.com", "https://forge.example")
	seedSubTestProject(t, st, "sv")

	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_1"); err != nil {
		t.Fatalf("started: %v", err)
	}
	seedPurchasedDomain(t, st, project.DomainKindPurchased)
	if err := orch.SubscriptionEnded("p1", "sub_1"); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if err := orch.SubscriptionStarted("p1", "cus_1", "sub_2"); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}

	if len(cf.autoRenewOn) != 1 || cf.autoRenewOn[0] != "acme.se" {
		t.Errorf("resubscribing must resume registrar auto-renew, got %v", cf.autoRenewOn)
	}
}
