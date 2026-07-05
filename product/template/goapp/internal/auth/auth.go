// Package auth provides password hashing, user accounts, and cookie sessions
// backed by the SQLite database. The cookie holds a random token; the database
// only ever sees its SHA-256 hash.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// CookieName is the session cookie.
const CookieName = "app_session"

// ErrEmailTaken is returned when signing up with an existing email.
var ErrEmailTaken = errors.New("email already registered")

// User is an account on this site.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	IsAdmin      bool
	CreatedAt    time.Time
}

// HashPassword returns a bcrypt hash of pw.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword reports whether pw matches the bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// NewID returns a random 128-bit hex ID (used for users, messages, tokens).
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not a recoverable situation
	}
	return hex.EncodeToString(b)
}

// CreateUser inserts a new account. The first account ever created becomes the
// site owner (admin) — it's the customer setting up their own site.
func CreateUser(ctx context.Context, db *sql.DB, email, passwordHash string) (*User, error) {
	u := &User{ID: NewID(), Email: email, PasswordHash: passwordHash, CreatedAt: time.Now().UTC()}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return nil, err
	}
	u.IsAdmin = count == 0
	_, err := db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, is_admin, created_at) VALUES (?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.IsAdmin, u.CreatedAt.Unix())
	if err != nil {
		// UNIQUE violation on email → taken.
		var already int
		if db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email = ?`, email).Scan(&already) == nil && already > 0 {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	return u, nil
}

// UserByEmail looks an account up by email (case-insensitive).
func UserByEmail(ctx context.Context, db *sql.DB, email string) (*User, error) {
	return scanUser(db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, is_admin, created_at FROM users WHERE email = ?`, email))
}

// UserByID looks an account up by id.
func UserByID(ctx context.Context, db *sql.DB, id string) (*User, error) {
	return scanUser(db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, is_admin, created_at FROM users WHERE id = ?`, id))
}

func scanUser(row *sql.Row) (*User, error) {
	var u User
	var created int64
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &created); err != nil {
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	return &u, nil
}

// Sessions manages login sessions in the database.
type Sessions struct {
	db  *sql.DB
	ttl time.Duration
}

// NewSessions returns a session manager with the given lifetime.
func NewSessions(db *sql.DB, ttl time.Duration) *Sessions {
	return &Sessions{db: db, ttl: ttl}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create starts a session for userID and returns its cookie token.
func (s *Sessions) Create(ctx context.Context, userID string) (string, error) {
	// Opportunistic housekeeping; must never block a login.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())

	token := NewID() + NewID() // 256 bits
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(token), userID, NewID(), now.Add(s.ttl).Unix(), now.Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// lookup returns (userID, csrf) for a live session, or ok=false.
func (s *Sessions) lookup(ctx context.Context, token string) (userID, csrf string, ok bool) {
	var expires int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, csrf, expires_at FROM sessions WHERE token_hash = ?`,
		hashToken(token)).Scan(&userID, &csrf, &expires)
	if err != nil {
		return "", "", false
	}
	if time.Now().Unix() > expires {
		s.Destroy(ctx, token)
		return "", "", false
	}
	return userID, csrf, true
}

// UserID returns the user for a token, or false if missing/expired.
func (s *Sessions) UserID(ctx context.Context, token string) (string, bool) {
	uid, _, ok := s.lookup(ctx, token)
	return uid, ok
}

// CSRF returns the per-session CSRF token, or false if missing/expired.
func (s *Sessions) CSRF(ctx context.Context, token string) (string, bool) {
	_, csrf, ok := s.lookup(ctx, token)
	return csrf, ok
}

// Destroy ends a session.
func (s *Sessions) Destroy(ctx context.Context, token string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
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
