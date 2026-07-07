package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"app/internal/auth"
)

// maxFieldLen caps user-provided form fields.
const maxFieldLen = 4000

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "landing", s.view(r, "Welcome", nil))
}

// handleContact stores a message from the public contact form. It is
// deliberately open (no login, no CSRF — there is no session to bind to);
// lengths are capped and the owner reads messages in the site admin (/admin).
func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	email := strings.TrimSpace(r.FormValue("email"))
	body := strings.TrimSpace(r.FormValue("message"))
	if name == "" || body == "" || len(name) > 200 || len(email) > 200 || len(body) > maxFieldLen {
		s.render(w, http.StatusBadRequest, "landing", View{Title: "Welcome",
			Flash: "Please fill in your name and a message."})
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO messages (id, name, email, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		auth.NewID(), name, email, body, time.Now().Unix())
	if err != nil {
		s.log.Error("store message", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.render(w, http.StatusOK, "landing", View{Title: "Welcome",
		Flash: "Thanks — your message has been sent!"})
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "signup", s.view(r, "Create account", nil))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	if !strings.Contains(email, "@") || len(password) < 8 || len(email) > 200 {
		s.render(w, http.StatusBadRequest, "signup", View{Title: "Create account",
			Flash: "Enter a valid email and a password of at least 8 characters."})
		return
	}
	// The first account becomes the site owner. When OWNER_EMAIL is set (Forge
	// injects the ordering customer's address), reserve that first account for
	// it — otherwise whoever signs up first would own the site and its data.
	if s.ownerEmail != "" {
		var count int
		if err := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if count == 0 && !strings.EqualFold(email, s.ownerEmail) {
			s.render(w, http.StatusForbidden, "signup", View{Title: "Create account",
				Flash: "The first account is reserved for the site owner — sign up with the email address the site was ordered with."})
			return
		}
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u, err := auth.CreateUser(r.Context(), s.db, email, hash)
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			s.render(w, http.StatusConflict, "signup", View{Title: "Create account",
				Flash: "That email is already registered."})
			return
		}
		s.log.Error("create user", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "login", s.view(r, "Log in", nil))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	u, err := auth.UserByEmail(r.Context(), s.db, email)
	if err != nil || !auth.CheckPassword(u.PasswordHash, password) {
		s.render(w, http.StatusUnauthorized, "login", View{Title: "Log in",
			Flash: "Wrong email or password."})
		return
	}
	if !s.startSession(w, r, u.ID) {
		return
	}
	http.Redirect(w, r, "/app", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.sessions.Destroy(r.Context(), c.Value)
	}
	s.sessions.ClearCookie(w, s.secureCookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession creates a session and sets its cookie; reports success so
// callers only redirect when a session actually exists.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) bool {
	token, err := s.sessions.Create(r.Context(), userID)
	if err != nil {
		s.log.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	s.sessions.SetCookie(w, token, s.secureCookie)
	return true
}

// handleDashboard is the logged-in area. The owner's real home is the site
// admin (which renders all data by introspection), so they are sent there;
// other accounts get a plain account page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, u *auth.User) {
	if u.IsAdmin {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "dashboard", s.view(r, "Your account", nil))
}
