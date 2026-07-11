// Package user holds the account domain types.
package user

import "time"

// User is a customer account on forge.transcendsoftware.se.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Verified     bool // email confirmed (password signups); magic-link/OAuth are inherently verified
	CreatedAt    time.Time
}

// Session is a login session. Only a hash of the cookie token is stored, so a
// leaked database yields no valid cookies.
type Session struct {
	TokenHash string // hex SHA-256 of the cookie token
	UserID    string
	CSRF      string // per-session CSRF token
	ExpiresAt time.Time
	CreatedAt time.Time
}

// LoginToken is a single-use passwordless ("magic link") login token. Only a
// hash of the emailed token is stored.
type LoginToken struct {
	TokenHash string // hex SHA-256 of the token in the link
	Email     string // the address the link was sent to
	ExpiresAt time.Time
	CreatedAt time.Time
}
