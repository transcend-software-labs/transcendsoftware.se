package store

import "errors"

// ErrEmailTaken is returned by CreateUser when the email already exists.
var ErrEmailTaken = errors.New("email already registered")

// ErrAccessApprovalPending is returned when an unapproved account already has
// a first project waiting for operator approval. The database enforces this as
// well as the web layer so concurrent submissions cannot create a queue flood.
var ErrAccessApprovalPending = errors.New("an access approval is already pending")

// ErrConflict means an attempted project update was based on an older revision
// than the current row. Callers must reload and reapply only their owned fields.
var ErrConflict = errors.New("project changed concurrently")

// Build reservation errors are returned before an iteration is created. The
// database implementation checks them under one advisory transaction lock, so
// simultaneous requests cannot all pass the same wallet guard.
var (
	ErrBuildCapacity = errors.New("build capacity reached; try again later")
	ErrBuildDailyCap = errors.New("daily build budget reached; try again tomorrow")
)
