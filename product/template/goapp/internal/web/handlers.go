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
// lengths are capped and the owner reads messages on /app.
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

// message is a contact-form entry shown to the owner.
type message struct {
	Name, Email, Body string
	At                time.Time
}

type dashboardView struct {
	Messages []message
}

// handleDashboard is the logged-in area. The site owner (first account) also
// sees messages from the contact form.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var v dashboardView
	if u.IsAdmin {
		rows, err := s.db.QueryContext(r.Context(),
			`SELECT name, email, body, created_at FROM messages ORDER BY created_at DESC LIMIT 50`)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var m message
			var at int64
			if err := rows.Scan(&m.Name, &m.Email, &m.Body, &at); err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			m.At = time.Unix(at, 0).UTC()
			v.Messages = append(v.Messages, m)
		}
	}
	s.render(w, http.StatusOK, "dashboard", s.view(r, "Dashboard", v))
}
