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
		s.render(w, http.StatusUnauthorized, "login", View{Title: "Log in", Flash: "Wrong email or password."})
		return
	}
	s.startSession(w, u.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "signup", s.view(r, "Get started", nil))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	if !strings.Contains(email, "@") || len(password) < 8 {
		s.render(w, http.StatusBadRequest, "signup", View{Title: "Get started",
			Flash: "Enter a valid email and a password of at least 8 characters."})
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
			s.render(w, http.StatusConflict, "signup", View{Title: "Get started",
				Flash: "That email is already registered."})
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.startSession(w, u.ID)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.sessions.Destroy(c.Value)
	}
	s.sessions.ClearCookie(w, s.cfg.SecureCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, userID string) {
	token := s.sessions.Create(userID)
	s.sessions.SetCookie(w, token, s.cfg.SecureCookie)
}
