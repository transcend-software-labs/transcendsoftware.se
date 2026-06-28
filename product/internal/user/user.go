// Package user holds the account domain type.
package user

import "time"

// User is a customer account on app.transcendsoftware.se.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}
