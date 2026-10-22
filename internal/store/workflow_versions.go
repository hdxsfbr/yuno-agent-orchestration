package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/actor"
)

// ErrVersionConflict is UpdateWorkflowIfVersion's answer when the workflow is no longer at
// the version the caller read (DESIGN W4 rule 3).
var ErrVersionConflict = errors.New("the workflow changed since it was read")

// WorkflowVersionInfo is one stored version of a workflow without its definition, as the
// version list shows it (DESIGN W4 rules 1 and 2). CreatedBy is the actor that wrote it
// (internal/actor), {"kind":"platform"} for a write no request made, or {"kind":"backfill"}
// for a version migration 0031 recovered.
type WorkflowVersionInfo struct {
	WorkflowID  string          `db:"workflow_id" json:"workflow_id"`
	Version     int             `db:"version" json:"version"`
	Name        string          `db:"name" json:"name"`
	Description string          `db:"description" json:"description"`
	CreatedAt   time.Time       `db:"created_at" json:"created_at"`
	CreatedBy   json.RawMessage `db:"created_by" json:"created_by"`
}

// WorkflowVersion is one stored version with its definition.
type WorkflowVersion struct {
	WorkflowVersionInfo
	Definition json.RawMessage `db:"definition" json:"definition"`
}

// platformAuthor is created_by of a version no request wrote: the seed, the template
// loader, an agent's quick-run workflow.
var platformAuthor = json.RawMessage(`{"kind":"platform"}`)

// versionAuthor is created_by of a version written in ctx: the request's actor, or the
// platform when ctx carries none.
func versionAuthor(ctx context.Context) json.RawMessage {
	a, ok := actor.From(ctx)
	if !ok {
		return platformAuthor
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return platformAuthor
	}
	return raw
}

// versionInsert writes the version a workflow statement returned (the CTE w) into
// workflow_versions, in the same statement, so the workflow and its version are one write
// (DESIGN W4 rule 1). The caller binds created_by to $1.
const versionInsert = ` INSERT INTO workflow_versions (workflow_id, version, name, description, definition, created_at, created_by)
	SELECT id, version, name, description, definition, updated_at, $1::jsonb FROM w RETURNING version`

// ListWorkflowVersions returns the workflow's versions newest first, without definitions.
func ListWorkflowVersions(ctx context.Context, q Querier, workflowID string) ([]WorkflowVersionInfo, error) {
	rows, err := q.Query(ctx, `SELECT workflow_id, version, name, description, created_at, created_by
		FROM workflow_versions WHERE workflow_id = $1 ORDER BY version DESC`, workflowID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[WorkflowVersionInfo])
	if out == nil {
		out = []WorkflowVersionInfo{}
	}
	return out, err
}

// GetWorkflowVersion returns one version with its definition, ErrNotFound when the
// workflow has no such version.
func GetWorkflowVersion(ctx context.Context, q Querier, workflowID string, version int) (*WorkflowVersion, error) {
	rows, err := q.Query(ctx, `SELECT workflow_id, version, name, description, created_at, created_by, definition
		FROM workflow_versions WHERE workflow_id = $1 AND version = $2`, workflowID, version)
	if err != nil {
		return nil, err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[WorkflowVersion])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// UpdateWorkflowIfVersion is UpdateWorkflow only while the workflow is still at version
// expected, checked in the statement that writes it (DESIGN W4 rule 3). It answers
// ErrVersionConflict when the workflow moved on and ErrNotFound when it is gone.
func UpdateWorkflowIfVersion(ctx context.Context, q Querier, w *Workflow, expected int) error {
	w.UpdatedAt = time.Now().UTC()
	rows, err := q.Query(ctx, `WITH w AS (UPDATE workflows SET name=$3, description=$4, definition=$5, version=version+1, updated_at=$6
		WHERE id=$2 AND deleted_at IS NULL AND version=$7 RETURNING id, version, name, description, definition, updated_at)`+versionInsert,
		versionAuthor(ctx), w.ID, w.Name, w.Description, w.Definition, w.UpdatedAt, expected)
	if err != nil {
		return err
	}
	v, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[int])
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := GetWorkflow(ctx, q, w.ID); err != nil {
			return err
		}
		return ErrVersionConflict
	}
	if err != nil {
		return err
	}
	w.Version = v
	return nil
}
