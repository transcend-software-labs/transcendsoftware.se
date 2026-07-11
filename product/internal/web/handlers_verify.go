package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/internal/web/i18n"
)

// verifyTTL is how long an email-verification link stays valid. Longer than a
// magic link — people confirm on their own schedule, sometimes days later.
const verifyTTL = 7 * 24 * time.Hour

// sendVerificationEmail issues a single-use verification token for email and
// mails a confirmation link in the given language. It reuses the login-token
// table: a verification link is just proof the address is reachable.
func (s *Server) sendVerificationEmail(ctx context.Context, email, lang string) {
	token := randToken()
	if err := s.store.CreateLoginToken(ctx, &user.LoginToken{
		TokenHash: hashToken(token), Email: strings.ToLower(email),
		ExpiresAt: time.Now().Add(verifyTTL).UTC(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		s.log.Error("verify token", "err", err)
		return
	}
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/verify?token=" + token
	body := fmt.Sprintf(i18n.T(lang, "verify.email.body"), link)
	if err := s.notifier.Send(ctx, email, i18n.T(lang, "verify.email.subject"), body); err != nil {
		s.log.Error("verify email", "err", err)
	}
}

// handleVerify consumes a verification link: it confirms the address, signs the
// person in (so a link opened on another device just works), and lands them on
// the dashboard.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	lt, err := s.store.LoginTokenByHash(r.Context(), hashToken(token))
	if err != nil || time.Now().After(lt.ExpiresAt) {
		s.render(w, http.StatusUnauthorized, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.verify_invalid")))
		return
	}
	_ = s.store.DeleteLoginToken(r.Context(), lt.TokenHash) // single-use
	if err := s.store.MarkUserVerified(r.Context(), lt.Email); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Sign them in on the confirming device, then land on the dashboard.
	if u, err := s.store.UserByEmail(r.Context(), lt.Email); err == nil {
		if !s.startSession(w, r, u.ID) {
			return
		}
	}
	http.Redirect(w, r, "/dashboard?verified=1", http.StatusSeeOther)
}

// handleResendVerification re-sends the confirmation link to the logged-in,
// still-unverified user.
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request, u *user.User) {
	if !u.Verified {
		s.sendVerificationEmail(r.Context(), u.Email, s.lang(r))
	}
	http.Redirect(w, r, "/dashboard?verify_sent=1", http.StatusSeeOther)
}
