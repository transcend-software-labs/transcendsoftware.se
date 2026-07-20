// Package user holds the account domain types.
package user

import "time"

// User is a customer account on forge.transcendsoftware.se.
type User struct {
	ID         string
	Email      string
	ApprovedAt *time.Time // operator approved this customer to start projects; nil until first-project review
	CreatedAt  time.Time
}

// Approved reports whether the operator has cleared this account to start
// projects. Approval is permanent unless an explicit revocation flow is added.
func (u *User) Approved() bool { return u != nil && u.ApprovedAt != nil }

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
