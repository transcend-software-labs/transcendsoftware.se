// Package user holds the account domain types.
package user

import "time"

// User is a customer account on app.transcendsoftware.se.
type User struct {
	ID           string
	Email        string
	PasswordHash string
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
