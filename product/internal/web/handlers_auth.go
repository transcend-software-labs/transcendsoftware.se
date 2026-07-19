package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/auth"
	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
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
	Domains         bool
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	priceStr, priceAmt, priceCur := s.monthlyPrice(r)
	lv := landingView{
		PriceStr:        priceStr,
		IncludedChanges: s.orch.ChangesPerMonth(),
		OverageStr:      formatPrice(int64(s.orch.OverageOre()), "sek"),
		Domains:         s.orch.DomainsEnabled(),
	}
	v := s.view(r, s.t(r, "title.landing"), lv)
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	u, err := s.store.UserByEmail(r.Context(), email)
	if err != nil || !auth.CheckPassword(u.PasswordHash, password) {
		s.render(w, http.StatusUnauthorized, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.wrong_login")))
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "signup", s.view(r, s.t(r, "signup.h1"), nil))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	// Store lowercased: the users table's UNIQUE(email) is case-sensitive, so
	// without this "Victim@x.com" would slip past a "victim@x.com" row (every
	// lookup uses lower(email)) and create a colliding duplicate account.
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	password := r.FormValue("password")

	if !strings.Contains(email, "@") || len(password) < 8 {
		s.render(w, http.StatusBadRequest, "signup", s.authView(r, s.t(r, "signup.h1"), s.t(r, "flash.signup_invalid")))
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Password signups start unverified — they must confirm the address before
	// they can spend money (create a project). Social/magic-link logins prove
	// the address inherently and are created verified elsewhere.
	u := &user.User{ID: id.New(), Email: email, PasswordHash: hash, Verified: false, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			s.render(w, http.StatusConflict, "signup", s.authView(r, s.t(r, "signup.h1"), s.t(r, "flash.email_taken")))
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.sendVerificationEmail(r.Context(), u.Email, s.lang(r))
	if !s.startSession(w, r, u.ID) {
		return
	}
	// Signed in, but the dashboard shows a "confirm your email" banner and
	// project creation stays blocked until they do.
	http.Redirect(w, r, "/dashboard?verify_sent=1", http.StatusSeeOther)
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
