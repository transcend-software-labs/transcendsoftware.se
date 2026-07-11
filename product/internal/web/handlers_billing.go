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

// handleSubscribe starts a Stripe subscription Checkout for the customer's site.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request, u *user.User) {
	p, ok := s.ownedProject(w, r, u)
	if !ok {
		return
	}
	if s.billing == nil || p.Paid || !subscribable(p) {
		http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
		return
	}
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	url, err := s.billing.CreateCheckoutSession(r.Context(), billing.CheckoutParams{
		ProjectID:     p.ID,
		CustomerEmail: u.Email,
		Locale:        s.lang(r),
		SuccessURL:    base + "/projects/" + p.ID + "?sub=success",
		CancelURL:     base + "/projects/" + p.ID + "?sub=cancel",
		// A per-domain DNS line will be appended here once domains are provisioned.
		LineItems: []billing.LineItem{{Price: s.cfg.StripePriceID}},
	})
	if err != nil {
		s.log.Error("stripe checkout", "project", p.ID, "err", err)
		http.Redirect(w, r, "/projects/"+p.ID+"?sub=error", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
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
