package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const workflowColumns = `id, name, description, definition, version, template_key, created_at, updated_at, deleted_at,
	from_template, seeded_hash`

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
	_, err := q.Exec(ctx, `INSERT INTO workflows (`+workflowColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL,$9,$10)`,
		w.ID, w.Name, w.Description, w.Definition, w.Version, w.TemplateKey, w.CreatedAt, w.UpdatedAt, w.FromTemplate, w.SeededHash)
	return err
}

// WorkflowFromTemplateExists reports whether a live workflow was instantiated from the
// template with key (DESIGN 5.15 rule 13).
func WorkflowFromTemplateExists(ctx context.Context, q Querier, key string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflows WHERE from_template = $1 AND deleted_at IS NULL)`, key).Scan(&ok)
	return ok, err
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

// DeleteWorkflowIfIdle soft-deletes the workflow in one statement, only while it is still
// at the given version and has no run queued, running or waiting for approval, so a run
// or an edit that starts between a caller's check and the delete cannot be lost. It
// reports whether the row was deleted.
func DeleteWorkflowIfIdle(ctx context.Context, q Querier, id string, version int) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE workflows SET deleted_at=now() WHERE id=$1 AND version=$2 AND deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM runs WHERE workflow_id = $1 AND status IN ($3, $4, $5))`,
		id, version, RunQueued, RunRunning, RunWaitingApproval)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
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

// skillColumns are the columns a skill is written with; skillSelect also reads the count
// of its bundle files.
const skillColumns = `id, name, description, body, source, source_url, source_ref, license, created_at, updated_at`
const skillSelect = `id, name, description, body, source, source_url, source_ref, license,
	(SELECT count(*)::int FROM skill_files f WHERE f.skill_id = skills.id) AS files_count,
	created_at, updated_at`

func CreateSkill(ctx context.Context, q Querier, s *Skill) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	if s.Source == "" {
		s.Source = SkillSourceUser
	}
	_, err := q.Exec(ctx, `INSERT INTO skills (`+skillColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		s.ID, s.Name, s.Description, s.Body, s.Source, s.SourceURL, s.SourceRef, s.License, s.CreatedAt, s.UpdatedAt)
	return err
}

func GetSkill(ctx context.Context, q Querier, id string) (*Skill, error) {
	rows, err := q.Query(ctx, `SELECT `+skillSelect+` FROM skills WHERE id=$1`, id)
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
	rows, err := q.Query(ctx, `SELECT `+skillSelect+` FROM skills ORDER BY name`)
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
	rows, err := q.Query(ctx, `SELECT `+skillSelect+` FROM skills WHERE name = ANY($1) ORDER BY name`, names)
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

// SetSkillSource records where a skill came from (an import that replaced an existing
// skill, DESIGN 5.27 rule 6). UpdateSkill leaves these columns alone.
func SetSkillSource(ctx context.Context, q Querier, id, source, sourceURL, sourceRef, license string) error {
	tag, err := q.Exec(ctx, `UPDATE skills SET source=$2, source_url=$3, source_ref=$4, license=$5 WHERE id=$1`,
		id, source, sourceURL, sourceRef, license)
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

// GetWorkflowByFromTemplate returns the oldest live workflow instantiated from the template
// key (the seed's copy of a shipped template), ErrNotFound when there is none.
func GetWorkflowByFromTemplate(ctx context.Context, q Querier, key string) (*Workflow, error) {
	rows, err := q.Query(ctx, `SELECT `+workflowColumns+` FROM workflows WHERE from_template=$1 AND deleted_at IS NULL
		ORDER BY created_at, id LIMIT 1`, key)
	if err != nil {
		return nil, err
	}
	w, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Workflow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// DefinitionHash is the sha256 (hex) the seed records for a workflow definition. Hash the
// definition as read back from the database, which is what a later seed compares.
func DefinitionHash(def json.RawMessage) string {
	sum := sha256.Sum256(def)
	return hex.EncodeToString(sum[:])
}

// SetWorkflowSeededHash records the hash of the definition the seed last wrote to the
// workflow, without touching its version. It does not change the workflow's definition.
func SetWorkflowSeededHash(ctx context.Context, q Querier, id, hash string) error {
	tag, err := q.Exec(ctx, `UPDATE workflows SET seeded_hash=$2 WHERE id=$1 AND deleted_at IS NULL`, id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
