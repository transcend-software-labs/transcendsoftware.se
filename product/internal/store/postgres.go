package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/transcend-software-labs/rasmus-ai/internal/project"
	"github.com/transcend-software-labs/rasmus-ai/internal/user"
)

// Postgres is a Store backed by PostgreSQL via pgx.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres connects to the database at dsn and verifies the connection.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
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
		`INSERT INTO users (id, email, password_hash, created_at) VALUES ($1, $2, $3, $4)`,
		u.ID, u.Email, u.PasswordHash, u.CreatedAt)
	if isUniqueViolation(err) {
		return ErrEmailTaken
	}
	return err
}

func (p *Postgres) UserByEmail(ctx context.Context, email string) (*user.User, error) {
	var u user.User
	err := p.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE lower(email) = lower($1)`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
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
		`SELECT id, email, password_hash, created_at FROM users WHERE id = $1`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (p *Postgres) CreateProject(ctx context.Context, pr *project.Project) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO projects
		   (id, user_id, name, brief, status, questions, answers, plan, verdict,
		    reject_reason, preview_url, repo_url, iterations_used, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		pr.ID, pr.UserID, pr.Name, pr.Brief, pr.Status, marshalQuestions(pr.Questions),
		pr.Answers, pr.Plan, pr.Verdict, pr.RejectReason, pr.PreviewURL, pr.RepoURL,
		pr.IterationsUsed, pr.CreatedAt, pr.UpdatedAt)
	return err
}

func (p *Postgres) UpdateProject(ctx context.Context, pr *project.Project) error {
	tag, err := p.pool.Exec(ctx,
		`UPDATE projects SET
		   name=$2, brief=$3, status=$4, questions=$5, answers=$6, plan=$7, verdict=$8,
		   reject_reason=$9, preview_url=$10, repo_url=$11, iterations_used=$12, updated_at=$13
		 WHERE id=$1`,
		pr.ID, pr.Name, pr.Brief, pr.Status, marshalQuestions(pr.Questions), pr.Answers,
		pr.Plan, pr.Verdict, pr.RejectReason, pr.PreviewURL, pr.RepoURL,
		pr.IterationsUsed, pr.UpdatedAt)
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

const projectColumns = `SELECT id, user_id, name, brief, status, questions, answers, plan, verdict,
	reject_reason, preview_url, repo_url, iterations_used, created_at, updated_at
	FROM projects`

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (*project.Project, error) {
	var pr project.Project
	var questionsJSON string
	err := row.Scan(&pr.ID, &pr.UserID, &pr.Name, &pr.Brief, &pr.Status, &questionsJSON,
		&pr.Answers, &pr.Plan, &pr.Verdict, &pr.RejectReason, &pr.PreviewURL, &pr.RepoURL,
		&pr.IterationsUsed, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if questionsJSON != "" && questionsJSON != "[]" {
		_ = json.Unmarshal([]byte(questionsJSON), &pr.Questions)
	}
	return &pr, nil
}

func (p *Postgres) CreateAsset(ctx context.Context, a *project.Asset) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO assets (id, project_id, object_key, filename, content_type, size, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.ProjectID, a.Key, a.Filename, a.ContentType, a.Size, a.CreatedAt)
	return err
}

func (p *Postgres) AssetsByProject(ctx context.Context, projectID string) ([]*project.Asset, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, project_id, object_key, filename, content_type, size, created_at
		 FROM assets WHERE project_id = $1 ORDER BY created_at ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*project.Asset
	for rows.Next() {
		var a project.Asset
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Key, &a.Filename,
			&a.ContentType, &a.Size, &a.CreatedAt); err != nil {
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
		   (id, project_id, number, prompt, preview_url, status, log, machine_id, heartbeat_at, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		it.ID, it.ProjectID, it.Number, it.Prompt, it.PreviewURL, it.Status, it.Log,
		it.MachineID, hb, it.CreatedAt)
	return err
}

func (p *Postgres) UpdateIteration(ctx context.Context, it *project.Iteration) error {
	hb := it.HeartbeatAt
	if hb.IsZero() {
		hb = it.CreatedAt
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE iterations SET prompt=$2, preview_url=$3, status=$4, log=$5,
		   machine_id=$6, heartbeat_at=$7 WHERE id=$1`,
		it.ID, it.Prompt, it.PreviewURL, it.Status, it.Log, it.MachineID, hb)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return project.ErrNotFound
	}
	return nil
}

const iterationColumns = `SELECT id, project_id, number, prompt, preview_url, status,
	log, machine_id, heartbeat_at, created_at FROM iterations`

func scanIteration(row rowScanner) (*project.Iteration, error) {
	var it project.Iteration
	err := row.Scan(&it.ID, &it.ProjectID, &it.Number, &it.Prompt, &it.PreviewURL,
		&it.Status, &it.Log, &it.MachineID, &it.HeartbeatAt, &it.CreatedAt)
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
