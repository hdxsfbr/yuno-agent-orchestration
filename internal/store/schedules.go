package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Schedule kinds (DESIGN 5.9).
const (
	ScheduleCron  = "cron"
	ScheduleDelay = "delay"
)

// Schedule fires runs of a workflow on a cron cadence or once at run_at (DESIGN 5.9).
// NextRunAt is nil while the schedule is disabled. ProjectID is the project its runs
// work on (rule 6), nil for a workflow without one; ProjectName is joined for the list
// and survives the project's deletion like WorkflowName.
type Schedule struct {
	ID           string          `db:"id" json:"id"`
	WorkflowID   string          `db:"workflow_id" json:"workflow_id"`
	WorkflowName string          `db:"workflow_name" json:"workflow_name"`
	ProjectID    *string         `db:"project_id" json:"project_id,omitempty"`
	ProjectName  *string         `db:"project_name" json:"project_name,omitempty"`
	Kind         string          `db:"kind" json:"kind"`
	CronExpr     *string         `db:"cron_expr" json:"cron_expr,omitempty"`
	TZ           string          `db:"tz" json:"tz"`
	RunAt        *time.Time      `db:"run_at" json:"run_at,omitempty"`
	Enabled      bool            `db:"enabled" json:"enabled"`
	NextRunAt    *time.Time      `db:"next_run_at" json:"next_run_at,omitempty"`
	LastRunAt    *time.Time      `db:"last_run_at" json:"last_run_at,omitempty"`
	LastRunID    *string         `db:"last_run_id" json:"last_run_id,omitempty"`
	LastError    *string         `db:"last_error" json:"last_error,omitempty"`
	Input        json.RawMessage `db:"input" json:"input"`
	CreatedAt    time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time       `db:"updated_at" json:"updated_at"`
}

// DueSchedule is a schedule the tick locked, with whether its workflow is gone.
type DueSchedule struct {
	Schedule
	WorkflowGone bool `db:"workflow_gone"`
}

// ScheduleOutcome is what one tick records for a due schedule. LastRunAt and LastRunID
// are only set when a run was created; LastError nil clears the previous error.
type ScheduleOutcome struct {
	Enabled   bool
	NextRunAt *time.Time
	LastRunAt *time.Time
	LastRunID *string
	LastError *string
	At        time.Time
}

// The workflow and project names are joined for the list and the form; a soft-deleted
// workflow or project keeps its name so the schedule still says what it ran.
const scheduleColumns = `s.id, s.workflow_id, COALESCE(w.name, '') AS workflow_name, s.project_id, p.name AS project_name,
	s.kind, s.cron_expr, s.tz, s.run_at, s.enabled, s.next_run_at, s.last_run_at, s.last_run_id, s.last_error, s.input,
	s.created_at, s.updated_at`

const scheduleJoins = ` FROM schedules s LEFT JOIN workflows w ON w.id = s.workflow_id LEFT JOIN projects p ON p.id = s.project_id`

const scheduleSelect = `SELECT ` + scheduleColumns + scheduleJoins

func CreateSchedule(ctx context.Context, q Querier, s *Schedule) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if len(s.Input) == 0 {
		s.Input = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO schedules (id, workflow_id, project_id, kind, cron_expr, tz, run_at, enabled, next_run_at,
		last_run_at, last_run_id, last_error, input, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14)`,
		s.ID, s.WorkflowID, s.ProjectID, s.Kind, s.CronExpr, s.TZ, s.RunAt, s.Enabled, s.NextRunAt,
		s.LastRunAt, s.LastRunID, s.LastError, s.Input, now)
	return err
}

func GetSchedule(ctx context.Context, q Querier, id string) (*Schedule, error) {
	rows, err := q.Query(ctx, scheduleSelect+` WHERE s.id = $1`, id)
	if err != nil {
		return nil, err
	}
	s, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Schedule])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// ListSchedules returns every schedule, oldest first.
func ListSchedules(ctx context.Context, q Querier) ([]Schedule, error) {
	rows, err := q.Query(ctx, scheduleSelect+` ORDER BY s.created_at, s.id`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Schedule])
	if out == nil {
		out = []Schedule{}
	}
	return out, err
}

// UpdateSchedule replaces the configuration and the bookkeeping fields the caller set.
func UpdateSchedule(ctx context.Context, q Querier, s *Schedule) error {
	s.UpdatedAt = time.Now().UTC()
	tag, err := q.Exec(ctx, `UPDATE schedules SET workflow_id=$2, kind=$3, cron_expr=$4, tz=$5, run_at=$6, enabled=$7,
		next_run_at=$8, last_error=$9, input=$10, updated_at=$11, project_id=$12 WHERE id=$1`,
		s.ID, s.WorkflowID, s.Kind, s.CronExpr, s.TZ, s.RunAt, s.Enabled, s.NextRunAt, s.LastError, s.Input, s.UpdatedAt, s.ProjectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func DeleteSchedule(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `DELETE FROM schedules WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NextScheduleDue is the earliest next_run_at of the enabled schedules, nil when none is
// enabled.
func NextScheduleDue(ctx context.Context, q Querier) (*time.Time, error) {
	var next *time.Time
	err := q.QueryRow(ctx, `SELECT min(next_run_at) FROM schedules WHERE enabled`).Scan(&next)
	return next, err
}

// ScheduleOccurrenceRef is the trigger_ref of the run a schedule fires for one
// occurrence: "<schedule id>@<occurrence, RFC 3339 UTC>". It names the occurrence, not
// the attempt, so firing it again cannot create a second run (DESIGN 5.9 rule 2).
func ScheduleOccurrenceRef(scheduleID string, occurrence time.Time) string {
	return scheduleID + "@" + occurrence.UTC().Format(time.RFC3339)
}

// ScheduleIDOfRef is the schedule id in a scheduled run's trigger_ref.
func ScheduleIDOfRef(ref string) string {
	id, _, _ := strings.Cut(ref, "@")
	return id
}

// LockDueSchedules selects the enabled schedules due at now, oldest due first, and
// locks them for the caller's transaction; rows another transaction holds are skipped
// (DESIGN 5.9 rule 2), so two processes never fire the same row.
func LockDueSchedules(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]DueSchedule, error) {
	rows, err := tx.Query(ctx, `SELECT `+scheduleColumns+`, (w.id IS NULL OR w.deleted_at IS NOT NULL) AS workflow_gone`+
		scheduleJoins+`
		WHERE s.enabled AND s.next_run_at <= $1
		ORDER BY s.next_run_at, s.id LIMIT $2
		FOR UPDATE OF s SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DueSchedule])
}

// RecordScheduleOutcome writes what a tick did with a due schedule.
func RecordScheduleOutcome(ctx context.Context, q Querier, id string, o ScheduleOutcome) error {
	_, err := q.Exec(ctx, `UPDATE schedules SET enabled=$2, next_run_at=$3, last_run_at=COALESCE($4, last_run_at),
		last_run_id=COALESCE($5, last_run_id), last_error=$6, updated_at=$7 WHERE id=$1`,
		id, o.Enabled, o.NextRunAt, o.LastRunAt, o.LastRunID, o.LastError, o.At)
	return err
}
