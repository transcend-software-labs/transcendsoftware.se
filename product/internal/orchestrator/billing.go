package orchestrator

import (
	"context"
	"fmt"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// SubscriptionStarted records a Stripe subscription for a project: it persists
// the Stripe identifiers, marks the project paid through the MarkPaid
// choke-point (unlocking Rasmus's manual delivery), and — only on the
// unpaid→paid transition — emails the customer and the operator. Idempotent:
// replays of the same subscription's checkout.session.completed are no-ops.
func (o *Orchestrator) SubscriptionStarted(projectID, customerID, subID string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.Paid && p.StripeSubID == subID {
		return nil // Stripe retry / duplicate delivery
	}
	wasPaid := p.Paid // a project comped via "manual" stays that way; we just record the IDs
	p.StripeCustomerID = customerID
	p.StripeSubID = subID
	if err := o.save(ctx, p); err != nil {
		return err
	}
	if err := o.MarkPaid(projectID, "stripe"); err != nil {
		return err
	}
	if wasPaid {
		return nil // already paid (comp) — don't re-announce
	}
	// Paying IS accepting: subscribing is the customer's strongest "ship it"
	// signal, so a preview_ready project moves straight into Rasmus's review
	// queue — no separate accept click. (The paid email below already tells the
	// operator to review & deliver, so no extra accept notification.)
	if cur, err := o.store.ProjectByID(ctx, projectID); err == nil && cur.Status == project.StatusPreviewReady {
		cur.Status = project.StatusAccepted
		if err := o.save(ctx, cur); err != nil {
			o.log.Error("subscription started: auto-accept", "project", projectID, "err", err)
		}
	}
	pe := custEmail(p.Locale, "subscription_active")
	o.notifyCustomer(ctx, p.UserID, pe.Subject,
		fmt.Sprintf(pe.Body, p.Name)+"\n\n"+o.projectLink(p.ID))
	o.notifyOperator(ctx, "Forge: project paid — review & deliver",
		fmt.Sprintf("%q is now paid (subscription). Review and deliver:\n\n%s",
			p.Name, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// SubscriptionEnded reflects a cancelled/expired Stripe subscription: clears the
// paid flag and alerts the operator. Guarded on the stored subscription id so a
// stale delete (after the customer already re-subscribed) can't un-pay a live
// subscription. Site suspension on cancel is future work — this only flips the
// flag and tells Rasmus.
func (o *Orchestrator) SubscriptionEnded(projectID, subID string) error {
	ctx := context.Background()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p.StripeSubID == "" || p.StripeSubID != subID || !p.Paid {
		return nil // stale/mismatched event, or already unpaid
	}
	status := p.Status
	if err := o.MarkUnpaid(projectID); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: subscription cancelled",
		fmt.Sprintf("The subscription for %q (status: %s) has ended — no longer marked paid:\n\n%s",
			p.Name, status, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// SubscriptionPaymentFailed alerts the operator only; Stripe's own retries and
// dunning handle the customer, and a terminal failure arrives later as
// SubscriptionEnded. Invoices don't carry our project metadata, so we pass the
// Stripe ids for the operator to look up.
func (o *Orchestrator) SubscriptionPaymentFailed(customerID, subID string) {
	o.notifyOperator(context.Background(), "Forge: subscription payment failed",
		fmt.Sprintf("A subscription payment failed (customer %s, subscription %s). Stripe will retry; check the dashboard if it doesn't recover.", customerID, subID))
}
