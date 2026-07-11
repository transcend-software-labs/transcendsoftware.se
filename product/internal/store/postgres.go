package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
	"github.com/transcend-software-labs/rasmus-ai/migrations"
)

// Postgres is a Store backed by PostgreSQL via pgx.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres connects to the database at dsn, verifies the connection, and
// applies any pending schema migrations.
//
// Fly Managed Postgres hands out a PgBouncer (transaction-pooling) endpoint,
// which is incompatible with pgx's default prepared-statement caching — a
// cached statement lives on one server connection, but the next query may be
// routed to another. QueryExecModeExec uses the unnamed statement per query
// (no cross-transaction cache), which is the PgBouncer-safe mode.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// migrationLockKey serializes concurrent migrators (e.g. two instances booting
// during a deploy). Arbitrary but fixed; transaction-scoped advisory locks work
// behind PgBouncer's transaction pooling.
const migrationLockKey = "4242000001"

// migrate applies embedded migrations in filename order inside one
// transaction, tracking applied versions in schema_migrations. Databases that
// were migrated manually (pre-tracking) backfill safely because every
// migration is idempotent — see migrations/migrations.go for the rules.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// No-arg Execs run over the simple protocol, which allows the
	// multi-statement bodies our migration files contain.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+migrationLockKey+`)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
		   version    text PRIMARY KEY,
		   applied_at timestamptz NOT NULL DEFAULT now()
		 )`); err != nil {
		return err
	}

	for _, name := range names {
		var applied int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM schema_migrations WHERE version = $1`, name).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			return err
		}
		slog.Info("store: applied migration", "version", name)
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (p *Postgres) CreateUser(ctx context.Context, u *user.User) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash, verified, created_at) VALUES ($1, $2, $3, $4, $5)`,
		u.ID, u.Email, u.PasswordHash, u.Verified, u.CreatedAt)
	if isUniqueViolation(err) {
		return ErrEmailTaken
	}
	return err
}

func (p *Postgres) UserByEmail(ctx context.Context, email string) (*user.User, error) {
	var u user.User
	err := p.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, verified, created_at FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Verified, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (p *Postgres) UserByID(ctx context.Context, id string) (*user.User, error) {
	var u user.User
	err := p.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, verified, created_at FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Verified, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// MarkUserVerified flips the verified flag for the account with this email.
func (p *Postgres) MarkUserVerified(ctx context.Context, email string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE users SET verified = true WHERE lower(email) = lower($1)`, email)
	return err
}

func (p *Postgres) CreateSession(ctx context.Context, s *user.Session) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, csrf, expires_at, created_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		s.TokenHash, s.UserID, s.CSRF, s.ExpiresAt, s.CreatedAt)
	return err
}

func (p *Postgres) SessionByTokenHash(ctx context.Context, tokenHash string) (*user.Session, error) {
	var s user.Session
	err := p.pool.QueryRow(ctx,
		`SELECT token_hash, user_id, csrf, expires_at, created_at FROM sessions WHERE token_hash = $1`,
		tokenHash).Scan(&s.TokenHash, &s.UserID, &s.CSRF, &s.ExpiresAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (p *Postgres) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (p *Postgres) DeleteExpiredSessions(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	return err
}

func (p *Postgres) CreateLoginToken(ctx context.Context, t *user.LoginToken) error {
	// Opportunistic housekeeping.
	_, _ = p.pool.Exec(ctx, `DELETE FROM login_tokens WHERE expires_at < now()`)
	_, err := p.pool.Exec(ctx,
		`INSERT INTO login_tokens (token_hash, email, expires_at, created_at) VALUES ($1,$2,$3,$4)`,
		t.TokenHash, t.Email, t.ExpiresAt, t.CreatedAt)
	return err
}

func (p *Postgres) LoginTokenByHash(ctx context.Context, tokenHash string) (*user.LoginToken, error) {
	var t user.LoginToken
	err := p.pool.QueryRow(ctx,
		`SELECT token_hash, email, expires_at, created_at FROM login_tokens WHERE token_hash = $1`,
		tokenHash).Scan(&t.TokenHash, &t.Email, &t.ExpiresAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (p *Postgres) DeleteLoginToken(ctx context.Context, tokenHash string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM login_tokens WHERE token_hash = $1`, tokenHash)
	return err
}

func (p *Postgres) CreateProject(ctx context.Context, pr *project.Project) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO projects
		   (id, user_id, name, brief, status, questions, design_options, design_brief,
		    answers, plan, verdict, reject_reason, preview_url, repo_url, snapshot_key,
		    screenshots, findings, critique, iterations_used, created_at, updated_at, plan_spec, locale, content_answers, content_rosters, pending_images, image_gen_count)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		pr.ID, pr.UserID, pr.Name, pr.Brief, pr.Status, marshalQuestions(pr.Questions),
		marshalJSON(pr.DesignOptions), pr.DesignBrief,
		pr.Answers, pr.Plan, pr.Verdict, pr.RejectReason, pr.PreviewURL, pr.RepoURL,
		pr.SnapshotKey, marshalJSON(pr.Screenshots), marshalJSON(pr.Findings), pr.Critique, pr.IterationsUsed, pr.CreatedAt, pr.UpdatedAt,
		marshalObj(pr.Spec), localeOr(pr.Locale), marshalObj(pr.ContentAnswers), marshalObj(pr.ContentRosters), marshalObj(pr.PendingImages), pr.ImageGenCount)
	return err
}

func (p *Postgres) UpdateProject(ctx context.Context, pr *project.Project) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE projects SET
		   name=$2, brief=$3, status=$4, questions=$5, design_options=$6, design_brief=$7,
		   answers=$8, plan=$9, verdict=$10, reject_reason=$11, preview_url=$12,
		   repo_url=$13, snapshot_key=$14, screenshots=$15, findings=$16, critique=$17, iterations_used=$18, updated_at=$19, plan_spec=$20, locale=$21, content_answers=$22, content_rosters=$23, pending_images=$24, image_gen_count=$25
		 WHERE id=$1`,
		pr.ID, pr.Name, pr.Brief, pr.Status, marshalQuestions(pr.Questions),
		marshalJSON(pr.DesignOptions), pr.DesignBrief, pr.Answers,
		pr.Plan, pr.Verdict, pr.RejectReason, pr.PreviewURL, pr.RepoURL,
		pr.SnapshotKey, marshalJSON(pr.Screenshots), marshalJSON(pr.Findings), pr.Critique, pr.IterationsUsed, pr.UpdatedAt,
		marshalObj(pr.Spec), localeOr(pr.Locale), marshalObj(pr.ContentAnswers), marshalObj(pr.ContentRosters), marshalObj(pr.PendingImages), pr.ImageGenCount)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

func marshalQuestions(qs []string) string {
	if len(qs) == 0 {
		return "[]"
	}
	b, err := json.Marshal(qs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// DeleteProject removes a project and everything hanging off it (its
// iterations and asset rows) in one transaction. Object-storage blobs
// (snapshots, screenshots, assets) are left to lifecycle/reaper cleanup.
func (p *Postgres) DeleteProject(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM assets WHERE project_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM iterations WHERE project_id = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return tx.Commit(ctx)
}

func (p *Postgres) ProjectByID(ctx context.Context, id string) (*project.Project, error) {
	pr, err := scanProject(p.pool.QueryRow(ctx, projectColumns+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	return pr, err
}

func (p *Postgres) ProjectsByUser(ctx context.Context, userID string) ([]*project.Project, error) {
	rows, err := p.pool.Query(ctx, projectColumns+` WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Project
	for rows.Next() {
		pr, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (p *Postgres) Projects(ctx context.Context) ([]*project.Project, error) {
	rows, err := p.pool.Query(ctx, projectColumns+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Project
	for rows.Next() {
		pr, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

func (p *Postgres) EscalatedProjects(ctx context.Context) ([]*project.Project, error) {
	rows, err := p.pool.Query(ctx, projectColumns+` WHERE status = $1 ORDER BY created_at DESC`,
		project.StatusEscalated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Project
	for rows.Next() {
		pr, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

const projectColumns = `SELECT id, user_id, name, brief, status, questions, design_options, design_brief,
	answers, plan, verdict, reject_reason, preview_url, repo_url, snapshot_key,
	screenshots, findings, critique, iterations_used, created_at, updated_at, plan_spec, locale, content_answers, content_rosters, pending_images, image_gen_count
	FROM projects`

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (*project.Project, error) {
	var pr project.Project
	var questionsJSON, designJSON, screenshotsJSON, findingsJSON, specJSON, contentJSON, rostersJSON, pendingJSON string
	err := row.Scan(&pr.ID, &pr.UserID, &pr.Name, &pr.Brief, &pr.Status, &questionsJSON,
		&designJSON, &pr.DesignBrief,
		&pr.Answers, &pr.Plan, &pr.Verdict, &pr.RejectReason, &pr.PreviewURL, &pr.RepoURL,
		&pr.SnapshotKey, &screenshotsJSON, &findingsJSON, &pr.Critique, &pr.IterationsUsed, &pr.CreatedAt, &pr.UpdatedAt, &specJSON, &pr.Locale, &contentJSON, &rostersJSON, &pendingJSON, &pr.ImageGenCount)
	if err != nil {
		return nil, err
	}
	if specJSON != "" && specJSON != "{}" {
		_ = json.Unmarshal([]byte(specJSON), &pr.Spec)
	}
	if contentJSON != "" && contentJSON != "{}" {
		_ = json.Unmarshal([]byte(contentJSON), &pr.ContentAnswers)
	}
	if rostersJSON != "" && rostersJSON != "{}" {
		_ = json.Unmarshal([]byte(rostersJSON), &pr.ContentRosters)
	}
	if pendingJSON != "" && pendingJSON != "{}" {
		_ = json.Unmarshal([]byte(pendingJSON), &pr.PendingImages)
	}
	if questionsJSON != "" && questionsJSON != "[]" {
		_ = json.Unmarshal([]byte(questionsJSON), &pr.Questions)
	}
	if designJSON != "" && designJSON != "[]" {
		_ = json.Unmarshal([]byte(designJSON), &pr.DesignOptions)
	}
	if screenshotsJSON != "" && screenshotsJSON != "[]" {
		_ = json.Unmarshal([]byte(screenshotsJSON), &pr.Screenshots)
	}
	if findingsJSON != "" && findingsJSON != "[]" {
		_ = json.Unmarshal([]byte(findingsJSON), &pr.Findings)
	}
	return &pr, nil
}

// localeOr defaults an empty locale to English so the NOT NULL column is happy.
func localeOr(l string) string {
	if l == "" {
		return "en"
	}
	return l
}

// marshalObj renders v as a JSON object for a jsonb/text column, "{}" on failure.
func marshalObj(v any) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "{}"
	}
	return string(b)
}

// marshalJSON renders v as JSON for a text column, "[]" on failure/empty.
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func (p *Postgres) CreateAsset(ctx context.Context, a *project.Asset) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO assets (id, project_id, object_key, filename, content_type, description, slot, generated, size, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.ID, a.ProjectID, a.Key, a.Filename, a.ContentType, a.Description, a.Slot, a.Generated, a.Size, a.CreatedAt)
	return err
}

func (p *Postgres) AssetsByProject(ctx context.Context, projectID string) ([]*project.Asset, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, project_id, object_key, filename, content_type, description, slot, generated, size, created_at
		 FROM assets WHERE project_id = $1 ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Asset
	for rows.Next() {
		var a project.Asset
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Key, &a.Filename,
			&a.ContentType, &a.Description, &a.Slot, &a.Generated, &a.Size, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateIteration(ctx context.Context, it *project.Iteration) error {
	hb := it.HeartbeatAt
	if hb.IsZero() {
		hb = it.CreatedAt
	}
	_, err := p.pool.Exec(ctx,
		`INSERT INTO iterations
		   (id, project_id, number, prompt, preview_url, status, log, machine_id, session_id, sandbox_addr, heartbeat_at, tokens, impl_model, planner_model, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		it.ID, it.ProjectID, it.Number, it.Prompt, it.PreviewURL, it.Status, validUTF8(it.Log),
		it.MachineID, it.SessionID, it.SandboxAddr, hb, it.Tokens, it.ImplModel, it.PlannerModel, it.CreatedAt)
	return err
}

// validUTF8 scrubs invalid UTF-8 so a text write can't fail on binary bytes —
// e.g. a build log that captured the agent reading a binary file. Postgres
// rejects invalid UTF-8 (SQLSTATE 22021), which would fail an otherwise-good
// build at the persistence step.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "") }

func (p *Postgres) UpdateIteration(ctx context.Context, it *project.Iteration) error {
	hb := it.HeartbeatAt
	if hb.IsZero() {
		hb = it.CreatedAt
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE iterations SET prompt=$2, preview_url=$3, status=$4, log=$5,
		   machine_id=$6, session_id=$7, sandbox_addr=$8, heartbeat_at=$9, tokens=$10 WHERE id=$1`,
		it.ID, it.Prompt, it.PreviewURL, it.Status, validUTF8(it.Log),
		it.MachineID, it.SessionID, it.SandboxAddr, hb, it.Tokens)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

const iterationColumns = `SELECT id, project_id, number, prompt, preview_url, status,
	log, machine_id, session_id, sandbox_addr, heartbeat_at, tokens, impl_model, planner_model, created_at FROM iterations`

func scanIteration(row rowScanner) (*project.Iteration, error) {
	var it project.Iteration
	err := row.Scan(&it.ID, &it.ProjectID, &it.Number, &it.Prompt, &it.PreviewURL,
		&it.Status, &it.Log, &it.MachineID, &it.SessionID, &it.SandboxAddr, &it.HeartbeatAt, &it.Tokens, &it.ImplModel, &it.PlannerModel, &it.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (p *Postgres) IterationsByProject(ctx context.Context, projectID string) ([]*project.Iteration, error) {
	rows, err := p.pool.Query(ctx, iterationColumns+` WHERE project_id = $1 ORDER BY number ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Iteration
	for rows.Next() {
		it, err := scanIteration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (p *Postgres) IterationsSince(ctx context.Context, t time.Time) ([]*project.Iteration, error) {
	rows, err := p.pool.Query(ctx, iterationColumns+` WHERE created_at >= $1 ORDER BY created_at DESC`, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Iteration
	for rows.Next() {
		it, err := scanIteration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (p *Postgres) ActiveIterations(ctx context.Context) ([]*project.Iteration, error) {
	rows, err := p.pool.Query(ctx, iterationColumns+` WHERE status = $1 ORDER BY created_at DESC`,
		project.StatusBuilding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Iteration
	for rows.Next() {
		it, err := scanIteration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
