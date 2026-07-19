package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// randToken returns a random hex token.
func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// findOrCreateUser returns the account for email, creating a passwordless one
// (empty hash) if new. The first-ever account becomes admin. Social/magic-link
// logins land in an existing same-email account, so a customer keeps one
// identity across methods.
func (s *Server) findOrCreateUser(r *http.Request, email string) (*user.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if u, err := s.store.UserByEmail(r.Context(), email); err == nil {
		// This login proves control of the address. If the account is still
		// unverified, any password on it was set by someone who never proved
		// ownership — a pre-hijack: attacker password-signs-up as the victim,
		// waits for the victim to log in via Google/magic link, then logs in with
		// the password they set. Wipe that password and verify the account now.
		if !u.Verified {
			if err := s.store.VerifyAndClearPassword(r.Context(), email); err != nil {
				return nil, err
			}
			u.Verified, u.PasswordHash = true, ""
		}
		return u, nil
	}
	// Social login and magic links both prove the address is reachable, so these
	// accounts are created already verified.
	u := &user.User{ID: id.New(), Email: email, Verified: true, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		return nil, err
	}
	return s.store.UserByEmail(r.Context(), email)
}

// --- Social login (OAuth2 authorization code) ---

const oauthStateCookie = "forge_oauth_state"

func (s *Server) redirectURI(r *http.Request, provider string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/auth/" + provider + "/callback"
}

func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauth.Get(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	state := randToken()
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: state, Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})
	http.Redirect(w, r, s.oauth.AuthCodeURL(p, s.redirectURI(r, p.Name), state), http.StatusSeeOther)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauth.Get(r.PathValue("provider"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Anti-CSRF: the state must match the cookie we set.
	c, err := r.Cookie(oauthStateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || c.Value != state {
		s.render(w, http.StatusBadRequest, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.login_failed")))
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.render(w, http.StatusBadRequest, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.login_cancelled")))
		return
	}
	email, err := s.oauth.Email(r.Context(), p, code, s.redirectURI(r, p.Name))
	if err != nil {
		s.log.Error("oauth email", "provider", p.Name, "err", err)
		s.render(w, http.StatusBadGateway, "login", s.authView(r, s.t(r, "login.h1"), fmt.Sprintf(s.t(r, "flash.oauth_failed"), p.Label)))
		return
	}
	u, err := s.findOrCreateUser(r, email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// --- Passwordless "magic link" login ---

const magicTTL = 20 * time.Minute

func (s *Server) handleMagicRequest(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	if !strings.Contains(email, "@") || len(email) > 200 {
		s.render(w, http.StatusBadRequest, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.email_invalid")))
		return
	}
	token := randToken()
	if err := s.store.CreateLoginToken(r.Context(), &user.LoginToken{
		TokenHash: hashToken(token), Email: email,
		ExpiresAt: time.Now().Add(magicTTL).UTC(), CreatedAt: time.Now().UTC(),
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	link := strings.TrimRight(s.cfg.BaseURL, "/") + "/auth/magic?token=" + token
	if err := s.notifier.Send(r.Context(), email, "Your Transcend Forge login link",
		"Click to sign in (valid for 20 minutes):\n\n"+link+"\n\nIf you didn't request this, ignore this email."); err != nil {
		s.log.Error("magic link email", "err", err)
	}
	// Always show the same confirmation, whether or not the address exists.
	s.render(w, http.StatusOK, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.magic_sent")))
}

func (s *Server) handleMagicConsume(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	lt, err := s.store.LoginTokenByHash(r.Context(), hashToken(token))
	if err != nil || time.Now().After(lt.ExpiresAt) {
		s.render(w, http.StatusUnauthorized, "login", s.authView(r, s.t(r, "login.h1"), s.t(r, "flash.magic_invalid")))
		return
	}
	_ = s.store.DeleteLoginToken(r.Context(), lt.TokenHash) // single-use
	u, err := s.findOrCreateUser(r, lt.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}
