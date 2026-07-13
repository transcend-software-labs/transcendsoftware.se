package web

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/billing"
	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// subscribable reports whether a project is at a point where paying makes sense:
// the customer has a preview to pay for (and delivery is still pending).
func subscribable(p *project.Project) bool {
	return p.Status == project.StatusPreviewReady || p.Status == project.StatusAccepted
}

// domainSelectable reports whether the customer may choose a domain now — either
// after paying (the post-pay panel attaches immediately) or before paying (Phase
// B: bundle it with the subscription). Both need a live preview and no domain yet.
func domainSelectable(p *project.Project) bool {
	return p.PreviewURL != "" && !p.HasDomain() && (p.Paid || subscribable(p))
}

// handleSubscribe starts a Stripe subscription Checkout for the customer's site.
// If the customer bundled a domain choice (Phase B), it's recorded now and
// provisioned automatically once the payment settles.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if s.billing == nil || p.Paid || !subscribable(p) {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	// Capture the (optional) bundled-domain choice before the Stripe redirect. A
	// bad/unbuyable choice bounces the customer back to pick again rather than
	// taking payment for something we can't deliver.
	if s.orch.DomainsEnabled() && domainSelectable(p) {
		if err := s.applyDomainChoice(r, p); err != nil {
			// Log the real cause: the customer only sees a generic "pick another
			// domain" flash, so without this a validation/registrar/save failure
			// is invisible (a store binding bug once hid here as "not registrable").
			s.log.Error("subscribe: domain choice rejected", "project", p.ID,
				"mode", r.FormValue("domain_mode"), "err", err)
			http.Redirect(w, r, "/projects/"+p.ID+"?sub=domainbad", http.StatusSeeOther)
			return
		}
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	url, err := s.billing.CreateCheckoutSession(r.Context(), billing.CheckoutParams{
		ProjectID:     p.ID,
		CustomerEmail: u.Email,
		Locale:        s.lang(r),
		SuccessURL:    base + "/projects/" + p.ID + "?sub=success",
		CancelURL:     base + "/projects/" + p.ID + "?sub=cancel",
		LineItems:     []billing.LineItem{{Price: s.cfg.StripePriceID}},
	})
	if err != nil {
		s.log.Error("stripe checkout", "project", p.ID, "err", err)
		http.Redirect(w, r, "/projects/"+p.ID+"?sub=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// applyDomainChoice reads the subscribe form's optional domain fields and records
// the intent (buy or BYOD), to be provisioned once payment settles. "none" (or an
// empty choice) clears any prior intent. The buy path is re-validated server-side
// inside SetDomainIntent.
func (s *Server) applyDomainChoice(r *http.Request, p *project.Project) error {
	switch r.FormValue("domain_mode") {
	case "byod":
		return s.orch.SetDomainIntent(r.Context(), p.ID, r.FormValue("byod_host"), false)
	case "buy":
		return s.orch.SetDomainIntent(r.Context(), p.ID, r.FormValue("buy_domain"), true)
	default:
		return s.orch.SetDomainIntent(r.Context(), p.ID, "", false) // clear
	}
}

// handleBillingPortal sends a subscribed customer to Stripe's self-serve portal
// (change card, cancel).
func (s *Server) handleBillingPortal(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if s.billing == nil || p.StripeCustomerID == "" {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	url, err := s.billing.CreatePortalSession(r.Context(), p.StripeCustomerID, base+"/projects/"+p.ID)
	if err != nil {
		s.log.Error("stripe portal", "project", p.ID, "err", err)
		http.Redirect(w, r, "/projects/"+p.ID+"?sub=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// handleStripeWebhook receives Stripe subscription events. Bare route: the
// signature is the authentication (no session, no CSRF). Unmapped/unknown events
// return 200 so Stripe doesn't retry the unresolvable ones for days.
func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil || s.cfg.StripeWebhookSecret == "" {
		http.NotFound(w, r) // feature invisible when unconfigured
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := billing.VerifySignature(body, r.Header.Get("Stripe-Signature"), s.cfg.StripeWebhookSecret, time.Now(), 5*time.Minute); err != nil {
		s.log.Warn("stripe webhook: bad signature", "err", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	ev, err := billing.ParseEvent(body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	switch ev.Type {
	case "checkout.session.completed":
		pid := ev.ProjectID()
		if pid == "" {
			s.log.Warn("stripe webhook: checkout without project id", "session", ev.Object.ID)
			break
		}
		if err := s.orch.SubscriptionStarted(pid, ev.Object.Customer, ev.Object.Subscription); err != nil {
			s.log.Error("stripe webhook: subscription started", "project", pid, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError) // Stripe retries
			return
		}
	case "customer.subscription.deleted":
		if pid := ev.ProjectID(); pid != "" {
			if err := s.orch.SubscriptionEnded(pid, ev.Object.ID); err != nil {
				s.log.Error("stripe webhook: subscription ended", "project", pid, "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
	case "invoice.payment_failed":
		s.orch.SubscriptionPaymentFailed(ev.Object.Customer, ev.Object.Subscription)
	default:
		s.log.Debug("stripe webhook: ignored event", "type", ev.Type)
	}
	w.WriteHeader(http.StatusOK)
}

// handleTerms renders the (draft) terms of service. Public, works logged-out.
func (s *Server) handleTerms(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "terms", s.view(r, s.t(r, "terms.title"), nil))
}
