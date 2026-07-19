package orchestrator

import (
	"context"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
)

// The Forge Pro change model. A paying subscriber gets a monthly allowance of
// "changes" (fixes/tweaks driven by a change request). Changes within the
// allowance are free; each one beyond it adds a flat, one-off charge to the
// customer's next Stripe invoice. Everything here is expressed in "changes" —
// the customer never sees AI/token cost. A change to an already-delivered site
// goes live when the customer accepts the new preview, with no operator review
// (see AcceptPreview's fast path).

// changeBiller is the Stripe surface the overage charge needs (satisfied by
// *billing.Client). Nil when Stripe isn't configured — then overage is comped.
type changeBiller interface {
	AddInvoiceItem(ctx context.Context, customerID string, amountMinor int, currency, description string) (string, error)
}

// overageCurrency is the minor-unit currency the subscription (and thus any
// overage) is billed in.
const overageCurrency = "sek"

// SetChangePolicy wires the monthly change allowance and the flat overage price
// (in minor units, öre). bill may be nil — then overage changes still proceed
// but are comped with an operator note instead of billed.
func (o *Orchestrator) SetChangePolicy(bill changeBiller, changesPerMonth, overageOre int) {
	o.changeBiller = bill
	o.changesPerMonth = changesPerMonth
	o.overageOre = overageOre
}

// ChangesPerMonth is the included monthly change allowance (for the web view).
func (o *Orchestrator) ChangesPerMonth() int { return o.changesPerMonth }

// OverageOre is the flat price of an extra change, in öre (for the web view).
func (o *Orchestrator) OverageOre() int { return o.overageOre }

// billOverage adds the flat overage charge to the customer's next invoice.
// Best-effort: with no Stripe customer (a comped project) or no biller it's
// waived with an operator note; a Stripe error is logged and flagged, never
// surfaced to the customer.
func (o *Orchestrator) billOverage(ctx context.Context, p *project.Project) {
	if o.changeBiller == nil || p.StripeCustomerID == "" {
		o.notifyOperator(ctx, "Forge: extra change not billed",
			"\""+p.Name+"\" used a change beyond its monthly allowance, but there's no Stripe "+
				"customer on file — it was comped.\n\n"+o.projectLink(p.ID))
		return
	}
	desc := "Extra website change — " + p.Name
	if _, err := o.changeBiller.AddInvoiceItem(ctx, p.StripeCustomerID, o.overageOre, overageCurrency, desc); err != nil {
		o.log.Error("overage: add invoice item", "project", p.ID, "err", err)
		o.notifyOperator(ctx, "Forge: overage billing failed",
			"Couldn't add the overage charge for \""+p.Name+"\": "+err.Error()+"\n\n"+o.projectLink(p.ID))
	}
}
