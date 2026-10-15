package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const workflowColumns = `id, name, description, definition, version, template_key, created_at, updated_at, deleted_at`

func CreateWorkflow(ctx context.Context, q Querier, w *Workflow) error {
	if w.ID == "" {
		w.ID = NewID()
	}
	if w.Version == 0 {
		w.Version = 1
	}
	if len(w.Definition) == 0 {
		w.Definition = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	w.CreatedAt, w.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO workflows (`+workflowColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL)`,
		w.ID, w.Name, w.Description, w.Definition, w.Version, w.TemplateKey, w.CreatedAt, w.UpdatedAt)
	return err
}

func GetWorkflow(ctx context.Context, q Querier, id string) (*Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowColumns+` FROM workflows WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	w, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Workflow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

func GetWorkflowByTemplateKey(ctx context.Context, q Querier, key string) (*Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowColumns+` FROM workflows WHERE template_key=$1 AND deleted_at IS NULL`, key)
	if err != nil {
		return nil, err
	}
	w, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Workflow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

func ListWorkflows(ctx context.Context, q Querier) ([]Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowColumns+` FROM workflows WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Workflow])
	if out == nil {
		out = []Workflow{}
	}
	return out, err
}

// UpdateWorkflow replaces name, description and definition and bumps the version
// (DESIGN section 4: runs pin workflow_version).
func UpdateWorkflow(ctx context.Context, q Querier, w *Workflow) error {
	w.UpdatedAt = time.Now().UTC()
	rows, err := q.Query(ctx, `UPDATE workflows SET name=$2, description=$3, definition=$4, version=version+1, updated_at=$5
		WHERE id=$1 AND deleted_at IS NULL RETURNING version`, w.ID, w.Name, w.Description, w.Definition, w.UpdatedAt)
	if err != nil {
		return err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[int])
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	w.Version = v
	return nil
}

func DeleteWorkflow(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE workflows SET deleted_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const skillColumns = `id, name, description, body, created_at, updated_at`

func CreateSkill(ctx context.Context, q Querier, s *Skill) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO skills (`+skillColumns+`) VALUES ($1,$2,$3,$4,$5,$6)`,
		s.ID, s.Name, s.Description, s.Body, s.CreatedAt, s.UpdatedAt)
	return err
}

func GetSkill(ctx context.Context, q Querier, id string) (*Skill, error) {
	rows, err := q.Query(ctx, `SELECT `+skillColumns+` FROM skills WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	s, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Skill])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

func ListSkills(ctx context.Context, q Querier) ([]Skill, error) {
	rows, err := q.Query(ctx, `SELECT `+skillColumns+` FROM skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Skill])
	if out == nil {
		out = []Skill{}
	}
	return out, err
}

// ListSkillsByName returns the skills whose names are in the set, in name order.
func ListSkillsByName(ctx context.Context, q Querier, names []string) ([]Skill, error) {
	if len(names) == 0 {
		return []Skill{}, nil
	}
	rows, err := q.Query(ctx, `SELECT `+skillColumns+` FROM skills WHERE name = ANY($1) ORDER BY name`, names)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Skill])
	if out == nil {
		out = []Skill{}
	}
	return out, err
}

func UpdateSkill(ctx context.Context, q Querier, s *Skill) error {
	s.UpdatedAt = time.Now().UTC()
	tag, err := q.Exec(ctx, `UPDATE skills SET name=$2, description=$3, body=$4, updated_at=$5 WHERE id=$1`,
		s.ID, s.Name, s.Description, s.Body, s.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func DeleteSkill(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `DELETE FROM skills WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
