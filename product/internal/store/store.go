// Package store defines persistence for accounts, projects and iterations,
// with an in-memory implementation (memory.go) and a Postgres implementation
// (postgres.go) behind a single interface.
package store

import (
	"context"
	"time"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// WithdrawalRequest is the durable audit record created by the public
// statutory withdrawal function. It deliberately stores only what is needed to
// identify and act on the request.
type WithdrawalRequest struct {
	ID        string
	Email     string
	ProjectID string
	CreatedAt time.Time
}

// MarketingEvent is one anonymous acquisition step. Persistence aggregates it
// by UTC day and campaign dimensions; it never stores a visitor identifier.
type MarketingEvent struct {
	Kind       string
	Source     string
	Medium     string
	Campaign   string
	OccurredAt time.Time
}

const (
	MarketingLandingView = "landing_view"
	MarketingStart       = "start"
	MarketingSignupView  = "signup_view"
)

// MarketingSource summarizes anonymous public-funnel activity for one campaign
// tuple. Empty dimensions mean direct or unattributed traffic.
type MarketingSource struct {
	Source       string
	Medium       string
	Campaign     string
	LandingViews int
	Starts       int
	SignupViews  int
}

// MarketingFunnel combines anonymous public-page counters with distinct
// customer progress from the product's existing account/project records.
type MarketingFunnel struct {
	LandingViews int
	Starts       int
	SignupViews  int
	Signups      int
	Briefs       int
	Approved     int
	Previews     int
	Paid         int
	Sources      []MarketingSource
}

// Store is the persistence boundary for the whole product.
type Store interface {
	// Health verifies the persistence dependency used by readiness checks.
	Health(ctx context.Context) error

	// Users
	CreateUser(ctx context.Context, u *user.User) error
	UserByEmail(ctx context.Context, email string) (*user.User, error)
	UserByID(ctx context.Context, id string) (*user.User, error)
	// DeleteUser removes the account, sessions and login tokens. Callers must
	// purge each project through the orchestrator first so external resources and
	// object-storage data are removed before the database cascade.
	DeleteUser(ctx context.Context, id string) error
	// MarkUserApproved permanently clears a customer to start projects after
	// the operator reviews their first brief. It preserves the first approval
	// timestamp when called more than once.
	MarkUserApproved(ctx context.Context, id string, approvedAt time.Time) error

	// Sessions (cookie tokens are stored hashed; see user.Session)
	CreateSession(ctx context.Context, s *user.Session) error
	SessionByTokenHash(ctx context.Context, tokenHash string) (*user.Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteExpiredSessions removes sessions past their expiry (housekeeping).
	DeleteExpiredSessions(ctx context.Context) error

	// Magic-link login tokens (single-use; stored hashed)
	CreateLoginToken(ctx context.Context, t *user.LoginToken) error
	LoginTokenByHash(ctx context.Context, tokenHash string) (*user.LoginToken, error)
	DeleteLoginToken(ctx context.Context, tokenHash string) error
	// ConsumeLoginToken atomically returns and deletes one unexpired token.
	ConsumeLoginToken(ctx context.Context, tokenHash string, now time.Time) (*user.LoginToken, error)

	// Projects
	CreateProject(ctx context.Context, p *project.Project) error
	UpdateProject(ctx context.Context, p *project.Project) error
	// DeleteProject removes a project and its iterations + assets (operator cleanup).
	DeleteProject(ctx context.Context, id string) error
	ProjectByID(ctx context.Context, id string) (*project.Project, error)
	// ProjectByPreviewHost resolves a branded preview subdomain label
	// ("bageriet-a1fa81") to its project, for the preview reverse proxy.
	ProjectByPreviewHost(ctx context.Context, host string) (*project.Project, error)
	ProjectsByUser(ctx context.Context, userID string) ([]*project.Project, error)
	// EscalatedProjects returns projects awaiting operator review, newest first.
	EscalatedProjects(ctx context.Context) ([]*project.Project, error)
	// PendingDomainProjects returns projects whose domain is still in flight
	// (registering / pending_dns / verifying), for the reconcile poller.
	PendingDomainProjects(ctx context.Context) ([]*project.Project, error)
	// Projects returns every project, newest first (reaper + operator views;
	// fine at current scale, paginate before it isn't).
	Projects(ctx context.Context) ([]*project.Project, error)

	// Iterations
	CreateIteration(ctx context.Context, it *project.Iteration) error
	// ReserveIteration atomically enforces global concurrent/daily build limits
	// and creates the building iteration when capacity remains. A non-positive
	// limit disables that dimension.
	ReserveIteration(ctx context.Context, it *project.Iteration, maxConcurrent, maxDaily int) error
	UpdateIteration(ctx context.Context, it *project.Iteration) error
	IterationsByProject(ctx context.Context, projectID string) ([]*project.Iteration, error)
	// ActiveIterations returns build passes currently in the building state
	// (for the active-builds view and startup recovery).
	ActiveIterations(ctx context.Context) ([]*project.Iteration, error)
	// IterationsSince returns build passes created at or after t, newest first
	// (for the operator's recent-build stats).
	IterationsSince(ctx context.Context, t time.Time) ([]*project.Iteration, error)

	// Assets (metadata; bytes live in object storage)
	CreateAsset(ctx context.Context, a *project.Asset) error
	AssetsByProject(ctx context.Context, projectID string) ([]*project.Asset, error)
	// SetAssetDescription updates one asset's caption (which recipe/item a photo
	// is for), so the build can pair it correctly.
	SetAssetDescription(ctx context.Context, assetID, description string) error

	// Consumer requests
	RecordWithdrawalRequest(ctx context.Context, request WithdrawalRequest) error

	// Anonymous acquisition metrics. Public events are stored only as daily
	// aggregate counters; MarketingFunnel joins those totals with distinct
	// customer progress already present in users/projects.
	RecordMarketingEvent(ctx context.Context, event MarketingEvent) error
	MarketingFunnel(ctx context.Context, since time.Time) (MarketingFunnel, error)

	// Close releases resources (no-op for the in-memory store).
	Close() error
}
