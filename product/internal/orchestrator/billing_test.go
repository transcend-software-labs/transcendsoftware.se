package orchestrator

import (
	"context"
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
