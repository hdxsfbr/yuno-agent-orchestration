package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Projects are the repositories runs work on (DESIGN 5.17). The git token lives in
// projects.token_enc as an opaque blob the project package seals and opens; nothing in
// this package reads it as text, and no Project value carries it.

// ProjectSettings are a project's settings without its token: the columns a run
// snapshots when it is queued (DESIGN 5.17 rule 4).
type ProjectSettings struct {
	Name           string `json:"name"`
	RepoURL        string `json:"repo_url"`
	Provider       string `json:"provider"`
	DefaultBranch  string `json:"default_branch"`
	SandboxNetwork bool   `json:"sandbox_network"`
}

// Project is one projects row as the API serves it: TokenSet instead of the token.
type Project struct {
	ID             string     `db:"id" json:"id"`
	Name           string     `db:"name" json:"name"`
	RepoURL        string     `db:"repo_url" json:"repo_url"`
	Provider       string     `db:"provider" json:"provider"`
	DefaultBranch  string     `db:"default_branch" json:"default_branch"`
	SandboxNetwork bool       `db:"sandbox_network" json:"sandbox_network"`
	Description    string     `db:"description" json:"description"`
	TokenSet       bool       `db:"token_set" json:"token_set"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt      *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
	// LastRun is not a column; FillProjectLastRuns sets it for the API's views.
	LastRun *ProjectLastRun `db:"-" json:"last_run,omitempty"`
}

// Settings is the project's snapshot for a run.
func (p *Project) Settings() ProjectSettings {
	return ProjectSettings{Name: p.Name, RepoURL: p.RepoURL, Provider: p.Provider, DefaultBranch: p.DefaultBranch, SandboxNetwork: p.SandboxNetwork}
}

// ProjectLastRun is the newest run that names a project (DESIGN 5.17 rule 3).
type ProjectLastRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Toolchain  *string    `json:"toolchain,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// projectColumns reads a project for scanning; token_set stands in for the blob.
const projectColumns = `id, name, repo_url, provider, default_branch, sandbox_network, description,
	(token_enc IS NOT NULL) AS token_set, created_at, updated_at, deleted_at`

// CreateProject stores a project with its sealed token (nil stores none).
func CreateProject(ctx context.Context, q Querier, p *Project, tokenEnc []byte) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	p.TokenSet = len(tokenEnc) > 0
	_, err := q.Exec(ctx, `INSERT INTO projects (id, name, repo_url, provider, default_branch, sandbox_network, description,
		token_enc, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		p.ID, p.Name, p.RepoURL, p.Provider, p.DefaultBranch, p.SandboxNetwork, p.Description, tokenEnc, p.CreatedAt, p.UpdatedAt)
	return err
}

func scanProject(rows pgx.Rows) (*Project, error) {
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Project])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// GetProject returns a live project; ErrNotFound for an unknown or deleted id.
func GetProject(ctx context.Context, q Querier, id string) (*Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// GetProjectByName finds the live project with this name, ignoring case and surrounding
// spaces (DESIGN 5.17 rule 8); the unique index keeps that to one row.
func GetProjectByName(ctx context.Context, q Querier, name string) (*Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE lower(name)=lower($1) AND deleted_at IS NULL
		ORDER BY created_at LIMIT 1`, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// GetProjectByRepoURL finds the oldest live project with this clone URL.
func GetProjectByRepoURL(ctx context.Context, q Querier, repoURL string) (*Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE repo_url=$1 AND deleted_at IS NULL
		ORDER BY created_at LIMIT 1`, strings.TrimSpace(repoURL))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// FindProjectByRepoURL is GetProjectByRepoURL over every row, a live one first: the
// bootstrap's idempotency check (DESIGN 5.17 rule 9), which must not recreate a project
// the operator deleted.
func FindProjectByRepoURL(ctx context.Context, q Querier, repoURL string) (*Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE repo_url=$1
		ORDER BY (deleted_at IS NOT NULL), created_at LIMIT 1`, strings.TrimSpace(repoURL))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// ListProjects returns the live projects by name.
func ListProjects(ctx context.Context, q Querier) ([]Project, error) {
	rows, err := q.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE deleted_at IS NULL ORDER BY lower(name), created_at`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Project])
	if out == nil {
		out = []Project{}
	}
	return out, err
}

// UpdateProject replaces a live project's settings and description; the token is
// changed with SetProjectToken.
func UpdateProject(ctx context.Context, q Querier, p *Project) error {
	p.UpdatedAt = time.Now().UTC()
	tag, err := q.Exec(ctx, `UPDATE projects SET name=$2, repo_url=$3, provider=$4, default_branch=$5, sandbox_network=$6,
		description=$7, updated_at=$8 WHERE id=$1 AND deleted_at IS NULL`,
		p.ID, p.Name, p.RepoURL, p.Provider, p.DefaultBranch, p.SandboxNetwork, p.Description, p.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetProjectToken replaces a live project's sealed token.
func SetProjectToken(ctx context.Context, q Querier, id string, tokenEnc []byte) error {
	tag, err := q.Exec(ctx, `UPDATE projects SET token_enc=$2, updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id, tokenEnc)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProject soft-deletes: runs keep their snapshot and the row keeps its token, so
// a run in flight still clones and pushes (DESIGN 5.17 rule 3).
func DeleteProject(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE projects SET deleted_at=now(), updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetProjectTokenEnc reads the sealed token of any project, deleted or not; nil when
// the row has none, ErrNotFound when there is no row.
func GetProjectTokenEnc(ctx context.Context, q Querier, id string) ([]byte, error) {
	var blob []byte
	err := q.QueryRow(ctx, `SELECT token_enc FROM projects WHERE id=$1`, id).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return blob, err
}

// LatestRunsByProject returns the newest run of every project that has one.
func LatestRunsByProject(ctx context.Context, q Querier) (map[string]ProjectLastRun, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (project_id) project_id, id, status, toolchain, created_at, finished_at
		FROM runs WHERE project_id IS NOT NULL ORDER BY project_id, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProjectLastRun{}
	for rows.Next() {
		var pid string
		var r ProjectLastRun
		if err := rows.Scan(&pid, &r.ID, &r.Status, &r.Toolchain, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out[pid] = r
	}
	return out, rows.Err()
}

// FillProjectLastRuns sets LastRun on each project from LatestRunsByProject.
func FillProjectLastRuns(ctx context.Context, q Querier, projects []Project) error {
	if len(projects) == 0 {
		return nil
	}
	latest, err := LatestRunsByProject(ctx, q)
	if err != nil {
		return err
	}
	for i := range projects {
		if r, ok := latest[projects[i].ID]; ok {
			lr := r
			projects[i].LastRun = &lr
		}
	}
	return nil
}
