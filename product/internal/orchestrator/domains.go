package orchestrator

// Custom-domain lifecycle. A paying customer can attach their own domain (BYOD:
// we show the DNS records, they set them, we verify and Fly issues the cert) or
// buy one in-app (the registrar registers it, we auto-configure DNS + cert).
// The orchestrator owns the whole flow — registrar + Fly certs + Stripe add-on
// — behind the DomainRegistrar/domainBiller interfaces, so the provider
// (internal/cloudflare or internal/hostup) is chosen at wiring time and the
// rest stays identical.

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
// by *cloudflare.Client and *hostup.Client; faked in tests).
type DomainRegistrar interface {
	SearchDomains(ctx context.Context, query string, limit int) ([]registrar.Offer, error)
	CheckDomains(ctx context.Context, names []string) ([]registrar.Offer, error)
	RegisterDomain(ctx context.Context, name string) (string, error)
	RegistrationStatus(ctx context.Context, name string) (string, error)
	ZoneID(ctx context.Context, name string) (string, error)
	EnsureDNSRecord(ctx context.Context, zoneID string, rec registrar.Record) error
}

// domainBiller is the Stripe surface the domain add-on needs (satisfied by
// *billing.Client). Nil when Stripe isn't configured — then a domain is comped.
type domainBiller interface {
	AddSubscriptionItem(ctx context.Context, subID, priceID string) (string, error)
	RemoveSubscriptionItem(ctx context.Context, itemID string) error
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
)

// SetDomains wires the custom-domain feature: the registrar client, the Stripe
// biller for the monthly add-on (nil to comp), the add-on price id, and the
// self-serve price cap in the registrar's own currency (USD for Cloudflare,
// SEK for Hostup). Leaving reg nil keeps domains off.
func (o *Orchestrator) SetDomains(reg DomainRegistrar, bill domainBiller, priceID string, maxPrice float64) {
	o.domains = reg
	o.biller = bill
	o.domainPriceID = priceID
	o.maxDomainPrice = maxPrice
}

// DomainsEnabled reports whether the feature is wired.
func (o *Orchestrator) DomainsEnabled() bool { return o.domains != nil }

// DomainBuyEnabled reports whether customers can buy a domain in-app (needs the
// add-on price so we never register a domain we can't bill).
func (o *Orchestrator) DomainBuyEnabled() bool {
	return o.domains != nil && o.domainPriceID != ""
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

// AttachDomain starts the BYOD flow: request a Fly certificate for the
// customer's own hostname, allocate a dedicated IPv6 if it's an apex, and store
// the DNS records for the customer to set. Synchronous — the customer sees the
// records immediately.
func (o *Orchestrator) AttachDomain(ctx context.Context, projectID, hostname string) error {
	if o.domains == nil {
		return ErrDomainsDisabled
	}
	host, ok := normalizeHostname(hostname)
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
	if err := o.ensureApexIPv6(ctx, p, app, req.IsApex); err != nil {
		return err
	}

	p.DomainName = host
	p.DomainKind = project.DomainKindBYOD
	p.DomainStatus = project.DomainPendingDNS
	p.DomainRecords = recordsFor(req.Records)
	p.DomainCreatedAt = time.Now().UTC()
	p.DomainVerifiedAt = time.Time{}
	if err := o.save(ctx, p); err != nil {
		return err
	}
	o.notifyOperator(ctx, "Forge: domain attach started",
		fmt.Sprintf("%q attached their own domain %s (awaiting DNS):\n\n%s",
			p.Name, host, o.baseURLOr("/admin/projects/"+p.ID)))
	return nil
}

// BuyDomain starts the self-serve purchase flow: re-check price + registrability
// server-side (guarding the cap), register through Cloudflare, and mark the
// project registering. Provisioning (DNS + cert) then runs via reconcile. The
// customer already saw and acknowledged the price.
func (o *Orchestrator) BuyDomain(ctx context.Context, projectID, domain string) error {
	if !o.DomainBuyEnabled() {
		return ErrBuyDisabled
	}
	host, ok := normalizeHostname(domain)
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
	if !offer.Registrable {
		return ErrNotRegistrable
	}
	if !offer.Buyable(o.maxDomainPrice) {
		return ErrDomainTooPricey
	}

	state, err := o.domains.RegisterDomain(ctx, host)
	if err != nil {
		return fmt.Errorf("buy domain: register: %w", err)
	}

	p.DomainName = host
	p.DomainKind = project.DomainKindPurchased
	p.DomainStatus = project.DomainRegistering
	p.DomainCreatedAt = time.Now().UTC()
	p.DomainVerifiedAt = time.Time{}
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

// DetachDomain removes a project's domain: delete the Fly cert, drop the Stripe
// add-on, and clear the domain fields. Idempotent and best-effort on the
// external calls so a customer (or operator) can always get unstuck. A purchased
// domain we own is not transferred away here — that's manual, future work.
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
	}
	if p.DomainSubItemID != "" && o.biller != nil {
		if err := o.biller.RemoveSubscriptionItem(ctx, p.DomainSubItemID); err != nil {
			o.log.Error("detach domain: remove sub item", "project", p.ID, "err", err)
		}
	}
	p.DomainName = ""
	p.DomainStatus = project.DomainNone
	p.DomainKind = ""
	p.DomainZoneID = ""
	p.DomainIPv6 = ""
	p.DomainSubItemID = ""
	p.DomainRecords = nil
	p.DomainCreatedAt = time.Time{}
	p.DomainVerifiedAt = time.Time{}
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
	if err := o.ensureApexIPv6(ctx, p, app, req.IsApex); err != nil {
		return err
	}
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

// activateDomain runs the one-time →active edge: mark active, add the Stripe
// monthly add-on (purchased + live sub only), and email the customer + operator.
// Guarded by DomainVerifiedAt so replays don't re-bill or re-email.
func (o *Orchestrator) activateDomain(ctx context.Context, p *project.Project) error {
	if !p.DomainVerifiedAt.IsZero() {
		return nil // already activated
	}
	p.DomainStatus = project.DomainActive
	p.DomainVerifiedAt = time.Now().UTC()

	// Bill the flat monthly add-on for a purchased domain on a live subscription.
	// BYOD is free; a comped project (no sub / no biller) is noted for the operator.
	if p.DomainKind == project.DomainKindPurchased {
		switch {
		case o.biller != nil && p.StripeSubID != "" && o.domainPriceID != "":
			itemID, err := o.biller.AddSubscriptionItem(ctx, p.StripeSubID, o.domainPriceID)
			if err != nil {
				o.log.Error("activate domain: add sub item", "project", p.ID, "err", err)
			} else {
				p.DomainSubItemID = itemID
			}
		default:
			o.notifyOperator(ctx, "Forge: domain live without add-on billing",
				fmt.Sprintf("%q is live on %s but has no live subscription to bill the domain add-on to — check manually:\n\n%s",
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

// ensureApexIPv6 allocates a dedicated IPv6 once for an apex domain (needed for
// A/AAAA). Idempotent via the stored address; subdomains (CNAME) need nothing.
func (o *Orchestrator) ensureApexIPv6(ctx context.Context, p *project.Project, app string, isApex bool) error {
	if !isApex || p.DomainIPv6 != "" {
		return nil
	}
	addr, err := o.machines.AllocateIPv6(ctx, app)
	if err != nil {
		return fmt.Errorf("allocate apex IPv6: %w", err)
	}
	p.DomainIPv6 = addr
	return nil
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
