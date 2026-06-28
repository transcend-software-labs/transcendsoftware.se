// Package auth provides password hashing and cookie-based sessions.
//
// Sessions are kept in memory: fine for a single instance, but they reset on
// restart and won't work across multiple app instances. Move to a shared store
// (Postgres/Redis) before scaling horizontally.
package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"golang.org/x/crypto/bcrypt"
)

// CookieName is the session cookie.
const CookieName = "rasmus_session"

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether pw matches the bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

type entry struct {
	userID  string
	csrf    string
	expires time.Time
}

// Sessions is an in-memory session manager.
type Sessions struct {
	mu  sync.RWMutex
	m   map[string]entry
	ttl time.Duration
}

// NewSessions returns a session manager with the given lifetime.
func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{m: make(map[string]entry), ttl: ttl}
}

// Create starts a session for userID and returns its token.
func (s *Sessions) Create(userID string) string {
	token := id.New()
	s.mu.Lock()
	s.m[token] = entry{userID: userID, csrf: id.New(), expires: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return token
}

// CSRF returns the per-session CSRF token, or false if missing/expired.
func (s *Sessions) CSRF(token string) (string, bool) {
	s.mu.RLock()
	e, ok := s.m[token]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.csrf, true
}

// UserID returns the user for a token, or false if missing/expired.
func (s *Sessions) UserID(token string) (string, bool) {
	s.mu.RLock()
	e, ok := s.m[token]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		s.Destroy(token)
		return "", false
	}
	return e.userID, true
}

// Destroy ends a session.
func (s *Sessions) Destroy(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// SetCookie writes the session cookie on the response.
func (s *Sessions) SetCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(s.ttl),
	})
}

// ClearCookie removes the session cookie.
func (s *Sessions) ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
