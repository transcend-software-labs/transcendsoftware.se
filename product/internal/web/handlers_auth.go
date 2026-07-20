package web

import (
	"net/http"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
)

// landingView carries the public pricing block: the monthly base price (from
// Stripe), the included-changes allowance, the flat overage per extra change,
// and whether the domain-buy feature is live (so we only advertise it when
// wired). PriceStr is "" when billing is unconfigured or Stripe is unreachable
// — the template then shows a copy fallback.
type landingView struct {
	PriceStr        string
	IncludedChanges int
	OverageStr      string
	DomainBuy       bool
	Examples        []landingExample
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.recordMarketing(r, store.MarketingLandingView)
	priceStr, priceAmt, priceCur := s.monthlyPrice(r)
	lv := landingView{
		PriceStr:        priceStr,
		IncludedChanges: s.orch.ChangesPerMonth(),
		OverageStr:      formatPrice(int64(s.orch.OverageOre()), "sek"),
		DomainBuy:       s.orch.DomainBuyEnabled(),
		Examples:        landingExamples(s.lang(r)),
	}
	v := s.view(r, s.t(r, "title.landing"), lv)
	v.StartURL = withCampaign("/start", r)
	v.JSONLD = s.landingJSONLD(r, priceAmt, priceCur) // Service graph, with an Offer when the price is known
	s.render(w, http.StatusOK, "landing", v)
}

// monthlyPrice returns the base-plan price — formatted (e.g. "299 kr") plus
// the numeric minor-unit amount and currency for structured data — cached for
// an hour so the public landing page never blocks on — or hard-depends on — a
// live Stripe call. Returns zero values when billing is disabled or the first
// fetch fails (the template falls back to copy; the JSON-LD omits the Offer).
func (s *Server) monthlyPrice(r *http.Request) (string, int64, string) {
	if s.billing == nil || s.cfg.StripePriceID == "" {
		return "", 0, ""
	}
	s.priceMu.Lock()
	defer s.priceMu.Unlock()
	if s.priceCache != "" && time.Since(s.priceAt) < time.Hour {
		return s.priceCache, s.priceAmt, s.priceCur
	}
	pr, err := s.billing.Price(r.Context(), s.cfg.StripePriceID)
	if err != nil {
		return s.priceCache, s.priceAmt, s.priceCur // stale values if we have them, else "" → copy fallback
	}
	s.priceCache = formatPrice(pr.UnitAmount, pr.Currency)
	s.priceAmt, s.priceCur = pr.UnitAmount, pr.Currency
	s.priceAt = time.Now()
	return s.priceCache, s.priceAmt, s.priceCur
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "login", s.view(r, s.t(r, "login.h1"), nil))
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) == nil {
		s.recordMarketing(r, store.MarketingSignupView)
	}
	s.render(w, http.StatusOK, "signup", s.view(r, s.t(r, "signup.h1"), nil))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.sessions.Destroy(r.Context(), c.Value)
	}
	s.sessions.ClearCookie(w, s.cfg.SecureCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession creates a session and sets its cookie; it reports success so
// callers only redirect when a session actually exists.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) bool {
	token, err := s.sessions.Create(r.Context(), userID)
	if err != nil {
		s.log.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	s.sessions.SetCookie(w, token, s.cfg.SecureCookie)
	return true
}
