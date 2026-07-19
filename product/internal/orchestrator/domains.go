package orchestrator

// Custom-domain lifecycle. A paying customer can attach their own domain (BYOD:
// we show the DNS records, they set them, we verify and Fly issues the cert) or
// buy one in-app (the registrar registers it, we auto-configure DNS + cert).
// The orchestrator owns the whole flow — registrar + Fly certs + Stripe
// billing — behind the DomainRegistrar/domainBiller interfaces, so the
// provider (internal/namecom) is chosen at wiring time and the rest stays
// identical.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/builder"
	"github.com/transcend-software-labs/rasmus-ai/internal/fly"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/registrar"
)

// DomainRegistrar is the registrar surface the orchestrator drives (satisfied
// by *namecom.Client; faked in tests).
type DomainRegistrar interface {
	SearchDomains(ctx context.Context, query string, limit int) ([]registrar.Offer, error)
	CheckDomains(ctx context.Context, names []string) ([]registrar.Offer, error)
	RegisterDomain(ctx context.Context, name string) (string, error)
	RegistrationStatus(ctx context.Context, name string) (string, error)
	ZoneID(ctx context.Context, name string) (string, error)
	EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error
	// DomainExpiry returns the domain's registry expiry, for detecting yearly
	// (auto-)renewals (zero time = unknown). SetAutoRenew toggles the registrar's
	// auto-renew — turned off when a customer detaches a purchased domain so we
	// stop paying to renew it. Providers that don't manage renewals stub these.
	DomainExpiry(ctx context.Context, name string) (time.Time, error)
	SetAutoRenew(ctx context.Context, name string, on bool) error
}

// domainBiller is the Stripe surface a purchased domain needs (satisfied by
// *billing.Client). Nil when Stripe isn't configured — then a domain is comped.
// A purchased domain's actual 1-year cost is added as a one-off item to the
// customer's next invoice (AddInvoiceItem).
type domainBiller interface {
	AddInvoiceItem(ctx context.Context, customerID string, amountMinor int, currency, description string) (string, error)
	// RefundSubscriptionCharge refunds a domain bought upfront in checkout that we
	// then couldn't register (partial refund off the subscription's first payment).
	RefundSubscriptionCharge(ctx context.Context, subscriptionID string, amountMinor int) (string, error)
}

// domainStuckAfter is how long an in-flight domain may sit before the poller
// gives up and marks it failed (registration or DNS never completed).
const domainStuckAfter = 72 * time.Hour

// Errors surfaced to the web layer (shown to the customer, not the operator).
var (
	ErrDomainsDisabled = errors.New("domains not enabled")
	ErrNotEligible     = errors.New("project not eligible for a domain")
	ErrDomainExists    = errors.New("a domain is already attached")
	ErrBadHostname     = errors.New("invalid domain name")
	ErrBuyDisabled     = errors.New("buying domains is not available")
	ErrDomainTooPricey = errors.New("domain price exceeds the allowed maximum")
	ErrNotRegistrable  = errors.New("domain is not available to register")
	ErrNoPrepaidAmount = errors.New("no captured prepaid amount for this domain")
)

// SetDomains wires the custom-domain feature: the registrar client, the Stripe
// biller for a purchased domain's one-off cost (nil to comp), and the
// self-serve price cap in SEK — which also clamps the amount billed. Leaving
// reg nil keeps domains off.
func (o *Orchestrator) SetDomains(reg DomainRegistrar, bill domainBiller, maxPrice float64) {
	o.domains = reg
	o.biller = bill
	o.maxDomainPrice = maxPrice
}

// DomainsEnabled reports whether the feature is wired.
func (o *Orchestrator) DomainsEnabled() bool { return o.domains != nil }

// DomainBuyEnabled reports whether customers can buy a domain in-app (needs a
// biller so we can charge the registration cost — never register one we can't
// bill).
func (o *Orchestrator) DomainBuyEnabled() bool {
	return o.domains != nil && o.biller != nil
}

// SearchDomains proxies a registrable-domain search for the web layer.
func (o *Orchestrator) SearchDomains(ctx context.Context, query string) ([]registrar.Offer, error) {
	if o.domains == nil {
		return nil, ErrDomainsDisabled
	}
	return o.domains.SearchDomains(ctx, query, 12)
}

// MaxDomainPrice is the self-serve price cap, in the registrar's currency.
func (o *Orchestrator) MaxDomainPrice() float64 { return o.maxDomainPrice }

// SetDomainIntent records the domain the customer picked on the subscribe page,
// to be provisioned automatically once their payment settles (Phase B). buy=true
// re-validates registrability + the price cap server-side, so we never start a
// subscription for a domain we can't actually sell; buy=false (BYOD) validates
// only the hostname shape. An empty hostname clears any prior choice.
func (o *Orchestrator) SetDomainIntent(ctx context.Context, projectID, hostname string, buy bool) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(hostname) == "" {
		if p.DomainIntent == "" {
			return nil // nothing to clear
		}
		p.DomainIntent, p.DomainIntentBuy = "", false
		return o.save(ctx, p)
	}
	if o.domains == nil {
		return ErrDomainsDisabled
	}
	host, ok := o.normalizeCustomerHostname(hostname)
	if !ok {
		return ErrBadHostname
	}
	cost, renewal := 0, 0
	if buy {
		if !o.DomainBuyEnabled() {
			return ErrBuyDisabled
		}
		offers, err := o.domains.CheckDomains(ctx, []string{host})
		if err != nil {
			return fmt.Errorf("domain intent: check: %w", err)
		}
		var offer registrar.Offer
		for _, of := range offers {
			if strings.EqualFold(of.Name, host) {
				offer = of
			}
		}
		if !offer.Registrable {
			return ErrNotRegistrable
		}
		if !offer.Buyable(o.maxDomainPrice) {
			return ErrDomainTooPricey
		}
		// Lock BOTH prices in now: the (often discounted) first year is charged
		// upfront in checkout; the renewal price is what every later yearly
		// renewal bills — registrars discount year one, and billing the intro
		// price forever would sell renewals below our own cost.
		cost = domainCostOre(offer.Price, o.maxDomainPrice)
		renewal = domainCostOre(offer.Renewal, o.maxDomainPrice)
	}
	p.DomainIntent, p.DomainIntentBuy, p.DomainCostOre, p.DomainRenewalOre = host, buy, cost, renewal
	return o.save(ctx, p)
}

// provisionDomainIntent acts on a domain the customer chose at checkout, once
// payment has settled: attach it (BYOD) or buy it. Best-effort, run in a
// goroutine off SubscriptionStarted — the base subscription already succeeded,
// so a domain failure only alerts the operator and leaves the customer to retry
// from their project page. The intent is cleared first so a webhook retry can't
// double-provision, and BuyDomain/AttachDomain's own "no existing domain" guard
// makes a concurrent double-fire safe.
func (o *Orchestrator) provisionDomainIntent(projectID string) {
	ctx := context.Background()
	if o.domains == nil {
		return
	}
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		o.log.Error("provision domain intent: load", "project", projectID, "err", err)
		return
	}
	host, buy := p.DomainIntent, p.DomainIntentBuy
	if host == "" {
		return // no intent, or a previous run already provisioned it and cleared it
	}
	// Idempotency + crash recovery: a domain is already in flight or attached for
	// this project, so a previous run (or a concurrent webhook retry) already
	// acted. Don't re-register or re-charge — just drop the now-stale intent.
	if p.HasDomain() {
		o.clearDomainIntent(ctx, projectID)
		return
	}
	prepaidOre, subID := p.DomainCostOre, p.StripeSubID // charged upfront at checkout
	// A bought domain was charged on the checkout's first invoice → mark it so
	// activation doesn't invoice it a second time. Persisted BEFORE we register,
	// so the activation poller can never bill ahead of the flag. The intent is
	// deliberately LEFT SET until we succeed: if the process dies mid-provision,
	// the reaper re-drives this exact call (BuyDomain is idempotent and guards on
	// DomainStatus), so a paid-for domain is never silently dropped. BYOD is free.
	if buy && !p.DomainPrepaid {
		p.DomainPrepaid = true
		if err := o.save(ctx, p); err != nil {
			o.log.Error("provision domain intent: mark prepaid", "project", projectID, "err", err)
			return
		}
	}
	var derr error
	if buy {
		derr = o.BuyDomain(ctx, projectID, host)
	} else {
		derr = o.AttachDomain(ctx, projectID, host)
	}
	if derr == nil {
		// BuyDomain/AttachDomain cleared the intent in the same write that created
		// the domain row, so the reaper's stranded-intent sweep already stops.
		return
	}
	// A concurrent or earlier run may have won the race and registered the domain
	// while this attempt errored (classically ErrDomainExists). Reload: if the
	// domain is now ours, this isn't a failure — clear the intent, don't refund.
	if fresh, ferr := o.store.ProjectByID(ctx, projectID); ferr == nil && fresh.HasDomain() {
		o.clearDomainIntent(ctx, projectID)
		return
	}
	o.log.Error("provision domain intent", "project", projectID, "host", host, "buy", buy, "err", derr)
	// The customer already paid for the domain in checkout, but we couldn't
	// register it — refund the domain amount and clear the prepaid marker (so a
	// later manual buy bills normally). Their subscription stays active.
	// This is money handling — the operator email must say EXACTLY what
	// happened to the charge, especially when the refund itself fails: a
	// silent refund failure means a customer paid for nothing.
	note := "Their subscription is active; they can retry from their project page."
	switch {
	case !buy:
		// BYOD — nothing was charged.
	case o.biller == nil || prepaidOre <= 0 || subID == "":
		note = fmt.Sprintf("NO REFUND ATTEMPTED — no upfront charge on record "+
			"(amount=%d öre, subscription=%q). Check the Stripe invoice manually. ", prepaidOre, subID) + note
	default:
		if _, rerr := o.biller.RefundSubscriptionCharge(ctx, subID, prepaidOre); rerr != nil {
			o.log.Error("provision domain intent: refund", "project", projectID, "err", rerr)
			note = fmt.Sprintf("REFUND FAILED (%v) — refund %d öre manually in Stripe (subscription %s). ",
				rerr, prepaidOre, subID) + note
		} else {
			note = fmt.Sprintf("The %d öre domain charge was auto-refunded. ", prepaidOre) + note
		}
	}
	// Stranded and refunded: clear both the prepaid marker (a later manual buy
	// then bills normally) and the intent (so the reaper's stranded-intent sweep
	// stops re-driving a purchase we've already given up on and refunded).
	if fresh, ferr := o.store.ProjectByID(ctx, projectID); ferr == nil {
		fresh.DomainPrepaid = false
		fresh.DomainIntent, fresh.DomainIntentBuy = "", false
		_ = o.save(ctx, fresh)
	}
	o.notifyOperator(ctx, "Forge: bundled domain provisioning failed",
		fmt.Sprintf("%q chose the domain %s at checkout, but provisioning it failed: %v\n%s\n\n%s",
			p.Name, host, derr, note, o.projectLink(projectID)))
}

// clearDomainIntent drops a project's captured checkout domain intent once
// provisioning has settled (succeeded, already-ours, or refunded), so the
// reaper's stranded-intent sweep stops re-driving it. Best-effort, idempotent.
func (o *Orchestrator) clearDomainIntent(ctx context.Context, projectID string) {
	fresh, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		o.log.Error("clear domain intent: load", "project", projectID, "err", err)
		return
	}
	if fresh.DomainIntent == "" && !fresh.DomainIntentBuy {
		return
	}
	fresh.DomainIntent, fresh.DomainIntentBuy = "", false
	if err := o.save(ctx, fresh); err != nil {
		o.log.Error("clear domain intent: save", "project", projectID, "err", err)
	}
}

// AttachDomain starts the BYOD flow: request a Fly certificate for the
// customer's own hostname, allocate a dedicated IPv6 if it's an apex, and store
// the DNS records for the customer to set. Synchronous — the customer sees the
// records immediately.
func (o *Orchestrator) AttachDomain(ctx context.Context, projectID, hostname string) error {
	if o.domains == nil {
		return ErrDomainsDisabled
	}
	host, ok := o.normalizeCustomerHostname(hostname)
	if !ok {
		return ErrBadHostname
	}
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !p.CanAttachDomain() {
		if p.HasDomain() {
			return ErrDomainExists
		}
		return ErrNotEligible
	}

	app := builder.DeployAppName(p.ID)
	req, err := o.machines.AddCertificate(ctx, app, host)
	if err != nil {
		return fmt.Errorf("attach domain: add certificate: %w", err)
	}
	if err := o.ensureApexIPv6(ctx, p, app, req); err != nil {
		return err
	}
	// If an IPv6 was just allocated, the requirements snapshot above doesn't
	// know it — refresh so the customer is shown EVERY record Fly wants.
	req = o.refreshCertRequirements(ctx, app, host, req)

	p.DomainName = host
	p.DomainKind = project.DomainKindBYOD
	p.DomainStatus = project.DomainPendingDNS
	p.DomainRecords = recordsFor(req.Records)
	p.DomainCreatedAt = time.Now().UTC()
	p.DomainVerifiedAt = time.Time{}
	p.DomainIntent, p.DomainIntentBuy = "", false // see BuyDomain: cleared with the domain row
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: domain attach started",
		fmt.Sprintf("%q attached their own domain %s (awaiting DNS):\n\n%s",
			p.Name, host, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// BuyDomain starts the self-serve purchase flow: re-check price + registrability
// server-side (guarding the cap), register through the registrar, and mark the
// project registering. Provisioning (DNS + cert) then runs via reconcile. The
// customer already saw and acknowledged the price.
func (o *Orchestrator) BuyDomain(ctx context.Context, projectID, domain string) error {
	if !o.DomainBuyEnabled() {
		return ErrBuyDisabled
	}
	host, ok := o.normalizeCustomerHostname(domain)
	if !ok {
		return ErrBadHostname
	}
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !p.CanAttachDomain() {
		if p.HasDomain() {
			return ErrDomainExists
		}
		return ErrNotEligible
	}

	// Server-side price + availability guard: never trust the client's price.
	offers, err := o.domains.CheckDomains(ctx, []string{host})
	if err != nil {
		return fmt.Errorf("buy domain: check: %w", err)
	}
	var offer registrar.Offer
	for _, of := range offers {
		if strings.EqualFold(of.Name, host) {
			offer = of
		}
	}
	// "Not registrable" can mean WE already hold it: an earlier attempt that
	// partially succeeded, or an out-of-band registration during recovery. If
	// our account carries it with a live registrar state, proceed — nothing is
	// bought (RegisterDomain is idempotent and returns the current state
	// without re-ordering), so the price guards don't apply. Anything else
	// (someone else's domain, not in our account) stays refused.
	alreadyOurs := false
	if !offer.Registrable {
		st, serr := o.domains.RegistrationStatus(ctx, host)
		if serr != nil || st != registrar.StateSucceeded {
			return ErrNotRegistrable
		}
		alreadyOurs = true
	}
	if !alreadyOurs && !offer.Buyable(o.maxDomainPrice) {
		return ErrDomainTooPricey
	}

	state, err := o.domains.RegisterDomain(ctx, host)
	if err != nil {
		return fmt.Errorf("buy domain: register: %w", err)
	}

	p.DomainName = host
	p.DomainKind = project.DomainKindPurchased
	p.DomainStatus = project.DomainRegistering
	// Capture the prices the customer is committing to now; the first year is
	// billed once when the domain goes active, the renewal price on each yearly
	// renewal (the domain is taken by then, so neither can be re-fetched).
	// Clamped to the cap. In the already-ours recovery the offer carries no
	// price — keep the amounts captured at checkout instead of zeroing them.
	if offer.Price > 0 {
		p.DomainCostOre = domainCostOre(offer.Price, o.maxDomainPrice)
	}
	if offer.Renewal > 0 {
		p.DomainRenewalOre = domainCostOre(offer.Renewal, o.maxDomainPrice)
	}
	p.DomainCreatedAt = time.Now().UTC()
	p.DomainVerifiedAt = time.Time{}
	// Clear any checkout intent in the SAME write that creates the domain row.
	// From here the domain poller owns recovery (PendingDomainProjects sees
	// 'registering'), so the intent is no longer needed — and clearing it in a
	// separate later save would race the reconcile goroutine spawned below, which
	// would resurrect it. No-op for the self-serve buy path (no intent).
	p.DomainIntent, p.DomainIntentBuy = "", false
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: domain purchased",
		fmt.Sprintf("%q bought %s (%.2f %s) — registering:\n\n%s",
			p.Name, host, offer.Price, offer.Currency, o.baseURLOr("/admin/projects/"+p.ID)))

	// Make immediate progress if registration already completed; otherwise the
	// poller advances it. Best-effort in a goroutine so the request returns fast.
	if state == registrar.StateSucceeded {
		go o.reconcileInBackground(projectID)
	}
	return nil
}

// RetryPaidDomain re-runs provisioning for a bundled domain the customer already
// paid for at checkout but that failed to provision (the GleSYS 404 bug, or any
// transient failure). The operator supplies the hostname because a failed
// provision clears the intent and never stored the name on the project. It
// re-arms the checkout intent and runs the normal provisionDomainIntent path,
// which marks the domain prepaid — so the already-paid registration is NOT
// billed a second time on activation. Requires the captured prepaid amount
// (DomainCostOre) to still be on the project, as a guard against silently
// registering a domain the customer never paid for.
func (o *Orchestrator) RetryPaidDomain(ctx context.Context, projectID, hostname string) error {
	if o.domains == nil {
		return ErrDomainsDisabled
	}
	host, ok := o.normalizeCustomerHostname(hostname)
	if !ok {
		return ErrBadHostname
	}
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !p.Paid {
		return ErrNotPaid
	}
	if !p.CanAttachDomain() {
		if p.HasDomain() {
			return ErrDomainExists
		}
		return ErrNotEligible
	}
	if p.DomainCostOre <= 0 || p.StripeSubID == "" {
		// Without the captured cost + subscription we can't prove it was prepaid,
		// and provisionDomainIntent couldn't refund if this attempt also failed.
		return ErrNoPrepaidAmount
	}
	// Re-arm the intent the checkout captured (cleared when the first attempt
	// failed). Buy=true → provisionDomainIntent sets DomainPrepaid, so activation
	// skips billing. DomainCostOre is left as captured (the prepaid amount).
	p.DomainIntent = host
	p.DomainIntentBuy = true
	if err := o.save(ctx, p); err != nil {
		return err
	}
	go o.provisionDomainIntent(projectID)
	return nil
}

// VerifyDomain re-checks a domain now (the customer's "Verify" button, or after
// they've set their DNS) by running one reconcile pass.
func (o *Orchestrator) VerifyDomain(ctx context.Context, projectID string) error {
	if o.domains == nil {
		return ErrDomainsDisabled
	}
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	return o.reconcileDomain(ctx, p)
}

// DetachDomain removes a project's domain: delete the Fly cert, drop the legacy
// Stripe add-on if one exists, and clear the domain fields. Idempotent and
// best-effort on the external calls so a customer (or operator) can always get
// unstuck. Purchased domains now bill a one-off registration cost that's already
// invoiced — it isn't refunded here (the operator refunds in Stripe if needed);
// only domains bought under the old flat-add-on model carry a DomainSubItemID to
// remove. A purchased domain we own is not transferred away here — manual work.
func (o *Orchestrator) DetachDomain(ctx context.Context, projectID string) error {
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	if !p.HasDomain() {
		return nil
	}
	if p.DomainName != "" {
		if err := o.machines.DeleteCertificate(ctx, builder.DeployAppName(p.ID), p.DomainName); err != nil {
			o.log.Error("detach domain: delete cert", "project", p.ID, "err", err)
		}
		// Stop paying to renew a purchased domain the customer no longer uses —
		// it lapses at the end of the year they already paid for. Best-effort.
		if p.DomainKind == project.DomainKindPurchased && o.domains != nil {
			if err := o.domains.SetAutoRenew(ctx, p.DomainName, false); err != nil {
				o.log.Error("detach domain: disable auto-renew", "project", p.ID, "domain", p.DomainName, "err", err)
			}
		}
	}
	p.DomainName = ""
	p.DomainStatus = project.DomainNone
	p.DomainKind = ""
	p.DomainZoneID = ""
	p.DomainIPv6 = ""
	p.DomainRenewalOre = 0
	p.DomainRecords = nil
	p.DomainCreatedAt = time.Time{}
	p.DomainVerifiedAt = time.Time{}
	// Clear the "already charged at checkout" markers too. Leaving DomainPrepaid
	// set would let the customer detach and then buy a fresh domain that
	// activateDomain skips invoicing — a free domain on repeat. Intent fields go
	// with it: a detached domain has no pending checkout intent.
	p.DomainPrepaid = false
	p.DomainCostOre = 0
	p.DomainIntent = ""
	p.DomainIntentBuy = false
	return o.save(ctx, p)
}

// StartDomainPoller reconciles every in-flight domain on an interval (mirrors
// StartReaper): advance registrations, detect DNS, activate certs, and time out
// anything stuck. All work is idempotent and best-effort.
func (o *Orchestrator) StartDomainPoller(ctx context.Context, interval time.Duration) {
	if o.domains == nil {
		return
	}
	go func() {
		o.reconcileAllDomains(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				o.reconcileAllDomains(ctx)
			}
		}
	}()
}

func (o *Orchestrator) reconcileAllDomains(ctx context.Context) {
	pending, err := o.store.PendingDomainProjects(ctx)
	if err != nil {
		o.log.Error("domain poller: list pending", "err", err)
		return
	}
	for _, p := range pending {
		if err := o.reconcileDomain(ctx, p); err != nil {
			o.log.Error("domain poller: reconcile", "project", p.ID, "err", err)
		}
	}
}

// StartDomainRenewalPoller re-bills domain renewals on a slow interval (daily is
// plenty — renewals happen once a year per domain). GleSYS auto-renews each
// purchased domain and charges us; we detect the renewal (the registry expiry
// advancing) and pass the cost through to the customer, mirroring the initial
// registration charge. No-op without a registrar or a biller.
func (o *Orchestrator) StartDomainRenewalPoller(ctx context.Context, interval time.Duration) {
	if o.domains == nil || o.biller == nil {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				o.rebillDomainRenewals(ctx)
			}
		}
	}()
}

// rebillDomainRenewals scans active purchased domains and bills any that GleSYS
// has auto-renewed since we last billed them.
func (o *Orchestrator) rebillDomainRenewals(ctx context.Context) {
	all, err := o.store.Projects(ctx)
	if err != nil {
		o.log.Error("renewal poller: list projects", "err", err)
		return
	}
	for _, p := range all {
		if p.DomainStatus != project.DomainActive || p.DomainKind != project.DomainKindPurchased {
			continue
		}
		if err := o.rebillDomainRenewal(ctx, p); err != nil {
			o.log.Error("renewal poller: project", "project", p.ID, "domain", p.DomainName, "err", err)
		}
	}
}

// rebillDomainRenewal bills one project for a domain renewal if GleSYS's expiry
// has advanced past what the customer has paid through. The first observation of
// an untracked domain (zero DomainPaidThrough) just anchors the clock without
// billing, so an untracked domain is never charged retroactively.
func (o *Orchestrator) rebillDomainRenewal(ctx context.Context, p *project.Project) error {
	exp, err := o.domains.DomainExpiry(ctx, p.DomainName)
	if err != nil {
		return err
	}
	if exp.IsZero() {
		return nil // unknown expiry — try again next cycle
	}
	if p.DomainPaidThrough.IsZero() {
		p.DomainPaidThrough = exp // establish the anchor, don't bill
		return o.save(ctx, p)
	}
	// A renewal advances the expiry ~a year; the one-month grace ignores any
	// slack between our activation anchor and the real registry date.
	if !exp.After(p.DomainPaidThrough.AddDate(0, 1, 0)) {
		return nil
	}

	// Renewals bill the captured RENEWAL price — the first-year price is often
	// an intro discount. Rows from before renewal capture fall back to the
	// first-year amount (grandfathered). Re-clamped to the current cap.
	renewOre := p.DomainRenewalOre
	if renewOre == 0 {
		renewOre = p.DomainCostOre
	}
	amount := domainCostOre(float64(renewOre)/100, o.maxDomainPrice)
	if o.biller != nil && p.StripeCustomerID != "" && amount > 0 {
		desc := fmt.Sprintf("Domänförnyelse: %s (1 år)", p.DomainName)
		if _, err := o.biller.AddInvoiceItem(ctx, p.StripeCustomerID, amount, "sek", desc); err != nil {
			return fmt.Errorf("bill renewal: %w", err)
		}
		pe := custEmail(p.Locale, "domain_renewed")
		o.notifyCustomer(ctx, p.UserID, pe.Subject,
			fmt.Sprintf(pe.Body, p.DomainName, formatKr(amount)))
	} else {
		o.notifyOperator(ctx, "Forge: domain renewed without billing",
			fmt.Sprintf("%q renewed %s but it couldn't be billed (no Stripe customer / no captured cost) — check manually:\n\n%s",
				p.Name, p.DomainName, o.baseURLOr("/admin/projects/"+p.ID)))
	}
	o.log.Info("domain renewal billed", "project", p.ID, "domain", p.DomainName, "through", exp.Format("2006-01-02"))
	p.DomainPaidThrough = exp
	return o.save(ctx, p)
}

// formatKr renders an öre amount as whole kronor for email copy ("12900" → "129 kr").
func formatKr(ore int) string {
	return fmt.Sprintf("%d kr", (ore+50)/100)
}

func (o *Orchestrator) reconcileInBackground(projectID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := o.store.ProjectByID(ctx, projectID)
	if err != nil {
		o.log.Error("domain reconcile: load", "project", projectID, "err", err)
		return
	}
	if err := o.reconcileDomain(ctx, p); err != nil {
		o.log.Error("domain reconcile", "project", projectID, "err", err)
	}
}

// reconcileDomain advances one project's domain one step. Idempotent: safe to
// call repeatedly from the poller, a background kick, or the Verify button.
func (o *Orchestrator) reconcileDomain(ctx context.Context, p *project.Project) error {
	if o.domains == nil {
		return nil
	}
	// Stuck too long → give up and alert the operator (almost always a missing
	// Cloudflare prerequisite, or the customer never set their DNS).
	if !p.DomainCreatedAt.IsZero() && time.Since(p.DomainCreatedAt) > domainStuckAfter &&
		p.DomainStatus != project.DomainActive {
		return o.failDomain(ctx, p, "timed out (registration or DNS never completed)")
	}

	switch p.DomainStatus {
	case project.DomainRegistering:
		return o.advanceRegistration(ctx, p)
	case project.DomainPendingDNS, project.DomainVerifying:
		return o.advanceCertificate(ctx, p)
	default:
		return nil
	}
}

// advanceRegistration polls a purchased domain's async registration; on success
// it provisions DNS + cert and moves to verifying.
func (o *Orchestrator) advanceRegistration(ctx context.Context, p *project.Project) error {
	state, err := o.domains.RegistrationStatus(ctx, p.DomainName)
	if err != nil {
		return err
	}
	// Log every poll so "stuck at registering" is answerable — a purchased .se
	// sits in 'pending' until the Hostup order settles at the registry, which is
	// normal and otherwise invisible.
	o.log.Info("domain registration poll", "project", p.ID, "domain", p.DomainName, "state", state)
	switch state {
	case registrar.StateSucceeded:
		return o.provisionPurchased(ctx, p)
	case registrar.StatePending, registrar.StateInProgress:
		return nil // keep waiting
	default: // action_required | blocked | failed
		return o.failDomain(ctx, p, "registration "+state)
	}
}

// provisionPurchased configures a freshly registered domain: find its
// auto-created Cloudflare zone, request the Fly cert, allocate an apex IPv6, and
// write every required DNS record (proxied:false). Then it moves to verifying.
func (o *Orchestrator) provisionPurchased(ctx context.Context, p *project.Project) error {
	zoneID := p.DomainZoneID
	if zoneID == "" {
		id, err := o.domains.ZoneID(ctx, p.DomainName)
		if err != nil {
			return err
		}
		if id == "" {
			return nil // zone not created yet — retry next pass
		}
		zoneID = id
		p.DomainZoneID = id
	}

	app := builder.DeployAppName(p.ID)
	req, err := o.machines.AddCertificate(ctx, app, p.DomainName)
	if err != nil {
		return err
	}
	if err := o.ensureApexIPv6(ctx, p, app, req); err != nil {
		return err
	}
	// If an IPv6 was just allocated, the requirements snapshot above doesn't
	// know it — refresh so the DNS writes cover EVERY address Fly validates
	// (a stale snapshot left one of two AAAA targets unwritten, seen live).
	req = o.refreshCertRequirements(ctx, app, p.DomainName, req)
	// Persist the zone id + allocated IPv6 before the DNS writes. Otherwise a
	// failed DNS write returns before the final save, and the next retry reloads
	// the project with an empty DomainIPv6 and allocates a fresh address every
	// pass (a dedicated-IP leak seen live when the CF token lacked DNS edit).
	if err := o.save(ctx, p); err != nil {
		return err
	}
	for _, rec := range req.Records {
		if err := o.domains.EnsureDNSRecord(ctx, zoneID, registrar.Record{
			Type: rec.Type, Name: rec.Name, Content: rec.Value,
		}); err != nil {
			return fmt.Errorf("provision: ensure %s %s: %w", rec.Type, rec.Name, err)
		}
	}

	p.DomainRecords = recordsFor(req.Records)
	p.DomainStatus = project.DomainVerifying
	return o.save(ctx, p)
}

// advanceCertificate checks whether the Fly cert has validated. dns_configured
// + configured → active; dns_configured alone → verifying; neither → stay.
func (o *Orchestrator) advanceCertificate(ctx context.Context, p *project.Project) error {
	st, err := o.machines.CheckCertificate(ctx, builder.DeployAppName(p.ID), p.DomainName)
	if err != nil {
		return err
	}
	switch {
	case st.Configured && st.DNSConfigured:
		return o.activateDomain(ctx, p)
	case st.DNSConfigured && p.DomainStatus != project.DomainVerifying:
		p.DomainStatus = project.DomainVerifying
		return o.save(ctx, p)
	default:
		return nil
	}
}

// activateDomain runs the one-time →active edge: mark active, bill the
// purchased domain's actual registration cost once to the customer's next
// invoice, and email the customer + operator. Guarded by DomainVerifiedAt so
// replays don't re-bill or re-email.
func (o *Orchestrator) activateDomain(ctx context.Context, p *project.Project) error {
	if !p.DomainVerifiedAt.IsZero() {
		return nil // already activated
	}
	p.DomainStatus = project.DomainActive
	p.DomainVerifiedAt = time.Now().UTC()

	// Bill the purchased domain's registration cost as a one-off item on the
	// customer's next invoice (pass-through at cost, captured at buy time and
	// clamped to the cap). BYOD is free; a comped project (no customer / no
	// biller) or a zero cost is noted for the operator.
	if p.DomainKind == project.DomainKindPurchased {
		// Anchor the renewal clock: we registered for one year, so the customer is
		// paid through ~now+1y. The renewal poller re-bills when GleSYS's expiry
		// advances past this (auto-renewal). Approximate is fine — the poller uses
		// a one-month grace, well under a yearly jump.
		p.DomainPaidThrough = time.Now().UTC().AddDate(1, 0, 0)
		switch {
		case p.DomainPrepaid:
			// Already charged on the checkout's first invoice — don't invoice again.
		case o.biller != nil && p.StripeCustomerID != "" && p.DomainCostOre > 0:
			desc := fmt.Sprintf("Domän: %s (1 år)", p.DomainName)
			if _, err := o.biller.AddInvoiceItem(ctx, p.StripeCustomerID, p.DomainCostOre, "sek", desc); err != nil {
				o.log.Error("activate domain: add invoice item", "project", p.ID, "err", err)
			}
		default:
			o.notifyOperator(ctx, "Forge: domain live without billing",
				fmt.Sprintf("%q is live on %s but its registration cost wasn't billed (no Stripe customer, no biller, or no captured cost) — check manually:\n\n%s",
					p.Name, p.DomainName, o.baseURLOr("/admin/projects/"+p.ID)))
		}
	}
	if err := o.save(ctx, p); err != nil {
		return err
	}

	pe := custEmail(p.Locale, "domain_live")
	o.notifyCustomer(ctx, p.UserID, pe.Subject,
		fmt.Sprintf(pe.Body, p.DomainName)+"\n\nhttps://"+p.DomainName)
	o.notifyOperator(ctx, "Forge: domain live",
		fmt.Sprintf("%q is now live on https://%s (%s):\n\n%s",
			p.Name, p.DomainName, p.DomainKind, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// domainCostOre converts a registrar price (SEK) to öre for Stripe, clamped to
// the cap so we never bill more than the self-serve maximum even if a pricier
// offer slipped through. Non-positive → 0 (nothing to bill).
func domainCostOre(price, cap float64) int {
	if cap > 0 && price > cap {
		price = cap
	}
	if price <= 0 {
		return 0
	}
	return int(price*100 + 0.5) // round to the nearest öre
}

// ensureApexIPv6 makes sure an apex domain has a dedicated IPv6 for its AAAA
// record. Fly auto-allocates a dedicated v6 on the app's first deploy, so one
// usually EXISTS already — visible as an AAAA target in the cert's DNS
// requirements. Reuse it; allocating another leaves a second address the
// written records never cover (seen live 2026-07-18: Fly then demanded TWO
// AAAA records while provisioning wrote one, and the cert sat unvalidated).
// Only when the requirements show no AAAA at all is a dedicated v6 allocated —
// A/AAAA). Idempotent via the stored address; subdomains (CNAME) need nothing.
func (o *Orchestrator) ensureApexIPv6(ctx context.Context, p *project.Project, app string, req fly.CertRequirements) error {
	if !req.IsApex || p.DomainIPv6 != "" {
		return nil
	}
	for _, r := range req.Records {
		if strings.EqualFold(r.Type, "AAAA") {
			p.DomainIPv6 = r.Value // the app already has a dedicated v6 — reuse it
			return nil
		}
	}
	addr, err := o.machines.AllocateIPv6(ctx, app)
	if err != nil {
		return fmt.Errorf("allocate apex IPv6: %w", err)
	}
	p.DomainIPv6 = addr
	return nil
}

// refreshCertRequirements re-reads the cert's DNS requirements after any IP
// change, so the record set covers EVERY current address — the requirements
// snapshot from AddCertificate predates an ensureApexIPv6 allocation. Best
// effort: on failure (or an empty answer, e.g. the test fake) the original
// requirements stand.
func (o *Orchestrator) refreshCertRequirements(ctx context.Context, app, hostname string, req fly.CertRequirements) fly.CertRequirements {
	st, err := o.machines.CheckCertificate(ctx, app, hostname)
	if err != nil || len(st.Requirements.Records) == 0 {
		return req
	}
	return st.Requirements
}

// failDomain marks a domain failed and alerts the operator. It clears no fields,
// so the operator can inspect what was attempted.
func (o *Orchestrator) failDomain(ctx context.Context, p *project.Project, reason string) error {
	p.DomainStatus = project.DomainFailed
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: domain failed",
		fmt.Sprintf("Domain %s for %q failed: %s\n\n%s",
			p.DomainName, p.Name, reason, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// recordsFor converts fly cert records into the stored/displayed shape.
func recordsFor(recs []fly.CertRecord) []project.DomainRecord {
	out := make([]project.DomainRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, project.DomainRecord{Type: r.Type, Name: r.Name, Value: r.Value, Note: recordNote(r.Type)})
	}
	return out
}

func recordNote(recType string) string {
	switch recType {
	case "TXT":
		return "ownership check"
	case "CNAME":
		return "DNS-only (grey cloud if you use Cloudflare)"
	default:
		return "DNS-only (grey cloud if you use Cloudflare)"
	}
}

// normalizeCustomerHostname is normalizeHostname plus the branded-preview
// guard: a customer must not attach our own preview domain (or a host under
// it) as their custom domain.
func (o *Orchestrator) normalizeCustomerHostname(in string) (string, bool) {
	h, ok := normalizeHostname(in)
	if !ok {
		return "", false
	}
	if o.previewDomain != "" && (h == o.previewDomain || strings.HasSuffix(h, "."+o.previewDomain)) {
		return "", false
	}
	return h, true
}

// normalizeHostname lowercases, trims, strips a scheme/leading www and trailing
// dot, and validates a plausible registrable hostname. Rejects *.fly.dev (that's
// the preview app, not a custom domain).
func normalizeHostname(in string) (string, bool) {
	h := strings.ToLower(strings.TrimSpace(in))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/")
	h = strings.TrimSuffix(h, ".")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if !validHostname(h) || strings.HasSuffix(h, ".fly.dev") {
		return "", false
	}
	return h, true
}

// validHostname does a lightweight structural check: at least one dot, allowed
// characters, sane label lengths. Registrability is Cloudflare's job.
func validHostname(h string) bool {
	if len(h) < 3 || len(h) > 253 || !strings.Contains(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
