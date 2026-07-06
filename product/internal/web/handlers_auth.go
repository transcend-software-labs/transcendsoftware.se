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

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "landing", s.view(r, "Websites, built & guaranteed", nil))
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "login", s.view(r, "Log in", nil))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	u, err := s.store.UserByEmail(r.Context(), email)
	if err != nil || !auth.CheckPassword(u.PasswordHash, password) {
		s.render(w, http.StatusUnauthorized, "login", s.authView(r, "Log in", "Wrong email or password."))
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "signup", s.view(r, "Get started", nil))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	if !strings.Contains(email, "@") || len(password) < 8 {
		s.render(w, http.StatusBadRequest, "signup", s.authView(r, "Get started",
			"Enter a valid email and a password of at least 8 characters."))
		return
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u := &user.User{ID: id.New(), Email: email, PasswordHash: hash, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			s.render(w, http.StatusConflict, "signup", s.authView(r, "Get started",
				"That email is already registered."))
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
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
