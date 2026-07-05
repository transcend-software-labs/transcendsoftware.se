// Package store defines persistence for accounts, projects and iterations,
// with an in-memory implementation (memory.go) and a Postgres implementation
// (postgres.go) behind a single interface.
package store

import (
	"context"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// Store is the persistence boundary for the whole product.
type Store interface {
	// Users
	CreateUser(ctx context.Context, u *user.User) error
	UserByEmail(ctx context.Context, email string) (*user.User, error)
	UserByID(ctx context.Context, id string) (*user.User, error)

	// Sessions (cookie tokens are stored hashed; see user.Session)
	CreateSession(ctx context.Context, s *user.Session) error
	SessionByTokenHash(ctx context.Context, tokenHash string) (*user.Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteExpiredSessions removes sessions past their expiry (housekeeping).
	DeleteExpiredSessions(ctx context.Context) error

	// Projects
	CreateProject(ctx context.Context, p *project.Project) error
	UpdateProject(ctx context.Context, p *project.Project) error
	ProjectByID(ctx context.Context, id string) (*project.Project, error)
	ProjectsByUser(ctx context.Context, userID string) ([]*project.Project, error)
	// EscalatedProjects returns projects awaiting operator review, newest first.
	EscalatedProjects(ctx context.Context) ([]*project.Project, error)

	// Iterations
	CreateIteration(ctx context.Context, it *project.Iteration) error
	UpdateIteration(ctx context.Context, it *project.Iteration) error
	IterationsByProject(ctx context.Context, projectID string) ([]*project.Iteration, error)
	// ActiveIterations returns build passes currently in the building state
	// (for the active-builds view and startup recovery).
	ActiveIterations(ctx context.Context) ([]*project.Iteration, error)

	// Assets (metadata; bytes live in object storage)
	CreateAsset(ctx context.Context, a *project.Asset) error
	AssetsByProject(ctx context.Context, projectID string) ([]*project.Asset, error)

	// Close releases resources (no-op for the in-memory store).
	Close() error
}
