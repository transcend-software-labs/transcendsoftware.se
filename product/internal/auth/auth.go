// Package auth provides password hashing and cookie-based sessions.
//
// Sessions live in the store (Postgres in production), so logins survive
// deploys and work across instances. The cookie holds a random token; the
// store only ever sees its SHA-256 hash.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/id"
	"github.com/transcend-software-labs/rasmus-ai/internal/store"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
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

// Sessions manages login sessions on top of the store.
type Sessions struct {
	store store.Store
	ttl   time.Duration
}

// NewSessions returns a session manager with the given lifetime.
func NewSessions(st store.Store, ttl time.Duration) *Sessions {
	return &Sessions{store: st, ttl: ttl}
}

// hashToken is the one-way mapping from cookie token to stored key.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create starts a session for userID and returns its cookie token.
func (s *Sessions) Create(ctx context.Context, userID string) (string, error) {
	// Opportunistic housekeeping; an error here must not block a login.
	_ = s.store.DeleteExpiredSessions(ctx)

	token := id.New()
	now := time.Now().UTC()
	err := s.store.CreateSession(ctx, &user.Session{
		TokenHash: hashToken(token),
		UserID:    userID,
		CSRF:      id.New(),
		ExpiresAt: now.Add(s.ttl),
		CreatedAt: now,
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// lookup returns the live session for a token, or nil.
func (s *Sessions) lookup(ctx context.Context, token string) *user.Session {
	sess, err := s.store.SessionByTokenHash(ctx, hashToken(token))
	if err != nil {
		return nil
	}
	if time.Now().After(sess.ExpiresAt) {
		_ = s.store.DeleteSession(ctx, sess.TokenHash)
		return nil
	}
	return sess
}

// CSRF returns the per-session CSRF token, or false if missing/expired.
func (s *Sessions) CSRF(ctx context.Context, token string) (string, bool) {
	if sess := s.lookup(ctx, token); sess != nil {
		return sess.CSRF, true
	}
	return "", false
}

// UserID returns the user for a token, or false if missing/expired.
func (s *Sessions) UserID(ctx context.Context, token string) (string, bool) {
	if sess := s.lookup(ctx, token); sess != nil {
		return sess.UserID, true
	}
	return "", false
}

// Destroy ends a session.
func (s *Sessions) Destroy(ctx context.Context, token string) {
	_ = s.store.DeleteSession(ctx, hashToken(token))
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
