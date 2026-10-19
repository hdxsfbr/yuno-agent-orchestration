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
	ID             string `db:"id" json:"id"`
	Name           string `db:"name" json:"name"`
	RepoURL        string `db:"repo_url" json:"repo_url"`
	Provider       string `db:"provider" json:"provider"`
	DefaultBranch  string `db:"default_branch" json:"default_branch"`
	SandboxNetwork bool   `db:"sandbox_network" json:"sandbox_network"`
	Description    string `db:"description" json:"description"`
	// Instructions are the operator's standing instructions rendered into every visit
	// of a run on the project (DESIGN 5.18 rule 1). The budget caps are micro-USD per
	// UTC day and UTC month, nil for none (DESIGN 5.19 rule 1); the API shows USD.
	Instructions         string     `db:"instructions" json:"instructions"`
	BudgetDayUSDMicros   *USDMicros `db:"budget_day_usd_micros" json:"max_cost_usd_per_day,omitempty"`
	BudgetMonthUSDMicros *USDMicros `db:"budget_month_usd_micros" json:"max_cost_usd_per_month,omitempty"`
	TokenSet             bool       `db:"token_set" json:"token_set"`
	CreatedAt            time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time  `db:"updated_at" json:"updated_at"`
	DeletedAt            *time.Time `db:"deleted_at" json:"deleted_at,omitempty"`
	// The intake settings and the poll's bookkeeping (DESIGN 5.23 rule 1, migration
	// 0013). The columns never reach the API as such: Intake is their view, filled by
	// fillIntake after every scan. IntakeWorkflowName is joined from the workflow and
	// kept when the workflow was soft-deleted; the poll columns are written by the
	// intake loop's own queries, not by CreateProject or UpdateProject.
	IntakeEnabled      bool       `db:"intake_enabled" json:"-"`
	IntakeLabel        string     `db:"intake_label" json:"-"`
	IntakeWorkflowID   *string    `db:"intake_workflow_id" json:"-"`
	IntakePolledAt     *time.Time `db:"intake_polled_at" json:"-"`
	IntakeLastError    *string    `db:"intake_last_error" json:"-"`
	IntakeWorkflowName *string    `db:"intake_workflow_name" json:"-"`
	// WavesPaused stops a plan's next wave release and ship run; the reason is the
	// operator's or the escalation's (DESIGN 5.29 rule 1, migration 0017). Written by
	// SetWavesPaused only.
	WavesPaused       bool    `db:"waves_paused" json:"waves_paused"`
	WavesPausedReason *string `db:"waves_paused_reason" json:"waves_paused_reason,omitempty"`
	// LastRun, Spend and Intake are not columns; FillProjectLastRuns and
	// FillProjectSpend set the first two for the API's views, fillIntake the third.
	LastRun *ProjectLastRun `db:"-" json:"last_run,omitempty"`
	Spend   *ProjectSpend   `db:"-" json:"spend,omitempty"`
	Intake  *ProjectIntake  `db:"-" json:"intake,omitempty"`
}

// DefaultIntakeLabel is the label intake watches until the operator chooses another
// (DESIGN 5.23 rule 1: projects.intake_label defaults to it).
const DefaultIntakeLabel = "yuno"

// ProjectIntake is the intake view of a project as the API serves it (DESIGN 5.23 rule
// 1): the operator's settings, the workflow's name and the poll's last time and error.
type ProjectIntake struct {
	Enabled      bool       `json:"enabled"`
	Label        string     `json:"label"`
	WorkflowID   *string    `json:"workflow_id,omitempty"`
	WorkflowName *string    `json:"workflow_name,omitempty"`
	PolledAt     *time.Time `json:"polled_at,omitempty"`
	LastError    *string    `json:"last_error,omitempty"`
}

// fillIntake sets Intake from the intake columns just scanned.
func (p *Project) fillIntake() {
	p.Intake = &ProjectIntake{Enabled: p.IntakeEnabled, Label: p.IntakeLabel, WorkflowID: p.IntakeWorkflowID,
		WorkflowName: p.IntakeWorkflowName, PolledAt: p.IntakePolledAt, LastError: p.IntakeLastError}
}

// ProjectSpend is what a project's runs spent in the current UTC day and month,
// whatever their status, and when each window ends (DESIGN 5.19 rules 2 and 7).
type ProjectSpend struct {
	DayUSDMicros   USDMicros `json:"day_usd"`
	MonthUSDMicros USDMicros `json:"month_usd"`
	DayUntil       time.Time `json:"day_until"`
	MonthUntil     time.Time `json:"month_until"`
}

// ProjectFact is a note an agent saved for a project (DESIGN 5.18 rule 2): shared by
// every agent of a run on the project, keyed by (project, key), expiring like an
// agent's memory fact. AgentName is joined for the API's list (null when the agent is
// gone); queries that do not join it select NULL::text AS agent_name.
type ProjectFact struct {
	ID          string     `db:"id" json:"id"`
	ProjectID   string     `db:"project_id" json:"project_id"`
	Key         string     `db:"key" json:"key"`
	Value       string     `db:"value" json:"value"`
	AgentID     *string    `db:"agent_id" json:"agent_id,omitempty"`
	AgentName   *string    `db:"agent_name" json:"agent_name,omitempty"`
	SourceRunID *string    `db:"source_run_id" json:"source_run_id,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at,omitempty"`
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

// projectColumns reads a project for scanning; token_set stands in for the blob, and the
// intake workflow's name comes from the join of projectJoins. The columns are qualified
// with the alias p, so a query that uses them selects FROM projectFrom and names p.id,
// p.name and the rest in its conditions (workflows has columns of the same names).
const projectColumns = `p.id, p.name, p.repo_url, p.provider, p.default_branch, p.sandbox_network, p.description,
	p.instructions, p.budget_day_usd_micros, p.budget_month_usd_micros,
	(p.token_enc IS NOT NULL) AS token_set, p.created_at, p.updated_at, p.deleted_at,
	p.intake_enabled, p.intake_label, p.intake_workflow_id, p.intake_polled_at, p.intake_last_error,
	p.waves_paused, p.waves_paused_reason,
	w.name AS intake_workflow_name`

// projectFrom is the table projectColumns reads: the project and its intake workflow. The
// join leaves out a soft-deleted workflow, so a project whose workflow was deleted shows
// no name for it (intake_workflow_id stays set: the API soft-deletes workflows, so the
// column's set null never fires, and the loop finds the workflow gone, DESIGN 5.23 rule 2).
const projectFrom = ` FROM projects p LEFT JOIN workflows w ON w.id = p.intake_workflow_id AND w.deleted_at IS NULL`

// projectSelect is the statement every single-project read starts from; append a WHERE.
const projectSelect = `SELECT ` + projectColumns + projectFrom

// CreateProject stores a project with its sealed token (nil stores none).
func CreateProject(ctx context.Context, q Querier, p *Project, tokenEnc []byte) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	p.TokenSet = len(tokenEnc) > 0
	if p.IntakeLabel == "" {
		p.IntakeLabel = DefaultIntakeLabel
	}
	_, err := q.Exec(ctx, `INSERT INTO projects (id, name, repo_url, provider, default_branch, sandbox_network, description,
		token_enc, created_at, updated_at, instructions, budget_day_usd_micros, budget_month_usd_micros,
		intake_enabled, intake_label, intake_workflow_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		p.ID, p.Name, p.RepoURL, p.Provider, p.DefaultBranch, p.SandboxNetwork, p.Description, tokenEnc, p.CreatedAt, p.UpdatedAt,
		p.Instructions, p.BudgetDayUSDMicros, p.BudgetMonthUSDMicros,
		p.IntakeEnabled, p.IntakeLabel, p.IntakeWorkflowID)
	return err
}

// scanProject reads the one project a query returned and fills its Intake view.
func scanProject(rows pgx.Rows) (*Project, error) {
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Project])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.fillIntake()
	return p, nil
}

// GetProject returns a live project; ErrNotFound for an unknown or deleted id.
func GetProject(ctx context.Context, q Querier, id string) (*Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE p.id=$1 AND p.deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// GetProjectByName finds the live project with this name, ignoring case and surrounding
// spaces (DESIGN 5.17 rule 8); the unique index keeps that to one row.
func GetProjectByName(ctx context.Context, q Querier, name string) (*Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE lower(p.name)=lower($1) AND p.deleted_at IS NULL
		ORDER BY p.created_at LIMIT 1`, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// GetProjectByRepoURL finds the oldest live project with this clone URL.
func GetProjectByRepoURL(ctx context.Context, q Querier, repoURL string) (*Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE p.repo_url=$1 AND p.deleted_at IS NULL
		ORDER BY p.created_at LIMIT 1`, strings.TrimSpace(repoURL))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// FindProjectByRepoURL is GetProjectByRepoURL over every row, a live one first: the
// bootstrap's idempotency check (DESIGN 5.17 rule 9), which must not recreate a project
// the operator deleted.
func FindProjectByRepoURL(ctx context.Context, q Querier, repoURL string) (*Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE p.repo_url=$1
		ORDER BY (p.deleted_at IS NOT NULL), p.created_at LIMIT 1`, strings.TrimSpace(repoURL))
	if err != nil {
		return nil, err
	}
	return scanProject(rows)
}

// ListProjects returns the live projects by name.
func ListProjects(ctx context.Context, q Querier) ([]Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE p.deleted_at IS NULL ORDER BY lower(p.name), p.created_at`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Project])
	if out == nil {
		out = []Project{}
	}
	for i := range out {
		out[i].fillIntake()
	}
	return out, err
}

// UpdateProject replaces a live project's settings and description, the intake settings
// among them (enabled, label, workflow: DESIGN 5.23 rule 1); the token is changed with
// SetProjectToken and the poll's bookkeeping (intake_polled_at, intake_last_error) is
// left alone. An empty IntakeLabel keeps the stored one: no valid label is empty.
func UpdateProject(ctx context.Context, q Querier, p *Project) error {
	p.UpdatedAt = time.Now().UTC()
	tag, err := q.Exec(ctx, `UPDATE projects SET name=$2, repo_url=$3, provider=$4, default_branch=$5, sandbox_network=$6,
		description=$7, updated_at=$8, instructions=$9, budget_day_usd_micros=$10, budget_month_usd_micros=$11,
		intake_enabled=$12, intake_label=COALESCE(NULLIF($13, ''), intake_label), intake_workflow_id=$14
		WHERE id=$1 AND deleted_at IS NULL`,
		p.ID, p.Name, p.RepoURL, p.Provider, p.DefaultBranch, p.SandboxNetwork, p.Description, p.UpdatedAt,
		p.Instructions, p.BudgetDayUSDMicros, p.BudgetMonthUSDMicros,
		p.IntakeEnabled, p.IntakeLabel, p.IntakeWorkflowID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListIntakeProjects returns the live projects the intake loop passes over (DESIGN 5.23
// rule 2): intake enabled and a workflow set, oldest first. A project whose workflow was
// soft-deleted is still listed: the loop finds that out and says so in intake_last_error.
func ListIntakeProjects(ctx context.Context, q Querier) ([]Project, error) {
	rows, err := q.Query(ctx, projectSelect+` WHERE p.deleted_at IS NULL AND p.intake_enabled
		AND p.intake_workflow_id IS NOT NULL ORDER BY p.created_at, p.id`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Project])
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].fillIntake()
	}
	return out, nil
}

// SetProjectIntakePoll records what a pass over a project did (DESIGN 5.23 rule 2): when
// it ran and its first error, nil for a clean pass. It is the poll's bookkeeping, not a
// change of settings, so updated_at stays; the row may be soft-deleted (a project deleted
// during the pass). ErrNotFound when there is no row.
func SetProjectIntakePoll(ctx context.Context, q Querier, id string, at time.Time, lastError *string) error {
	tag, err := q.Exec(ctx, `UPDATE projects SET intake_polled_at=$2, intake_last_error=$3 WHERE id=$1`,
		id, at.UTC(), lastError)
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

// ---- instructions and budgets (DESIGN 5.18, 5.19) -----------------------------------

// ProjectInstructions reads the operator's instructions of a project row, deleted or
// not: a run in flight on a project deleted since still renders them (DESIGN 5.18 rule
// 4). A missing row has none.
func ProjectInstructions(ctx context.Context, q Querier, projectID string) (string, error) {
	var instructions string
	err := q.QueryRow(ctx, `SELECT instructions FROM projects WHERE id=$1`, projectID).Scan(&instructions)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return instructions, err
}

// DayBounds is the UTC calendar day that contains now, [00:00, next 00:00): the end is
// exclusive (DESIGN 5.19 units).
func DayBounds(now time.Time) (start, end time.Time) {
	n := now.UTC()
	start = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 0, 1)
}

// MonthBounds is the UTC calendar month that contains now, [1st 00:00, next 1st 00:00).
func MonthBounds(now time.Time) (start, end time.Time) {
	n := now.UTC()
	start = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// SumProjectSpend is what the project's runs cost in micro-USD, counting the runs
// created in [from, to) whatever their status (DESIGN 5.19 rule 2): a running run counts
// what it has spent so far, and the runs of a deleted project keep counting. One indexed
// query (runs (project_id, created_at)).
func SumProjectSpend(ctx context.Context, q Querier, projectID string, from, to time.Time) (int64, error) {
	var sum int64
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd_micros), 0)::bigint FROM runs
		WHERE project_id = $1 AND created_at >= $2 AND created_at < $3`, projectID, from, to).Scan(&sum)
	return sum, err
}

// spendByProject is SumProjectSpend for several projects in one query; a project with
// no runs in the window is absent from the map.
func spendByProject(ctx context.Context, q Querier, ids []string, from, to time.Time) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT project_id, COALESCE(sum(cost_usd_micros), 0)::bigint FROM runs
		WHERE project_id = ANY($1) AND created_at >= $2 AND created_at < $3 GROUP BY project_id`, ids, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, err
		}
		out[id] = sum
	}
	return out, rows.Err()
}

// FillProjectSpend sets Spend on each project: the current UTC day's and month's spend
// at now and when each window ends (DESIGN 5.19 rule 7). Two queries however many
// projects there are, one per window.
func FillProjectSpend(ctx context.Context, q Querier, projects []Project, now time.Time) error {
	if len(projects) == 0 {
		return nil
	}
	ids := make([]string, len(projects))
	for i := range projects {
		ids[i] = projects[i].ID
	}
	dayStart, dayEnd := DayBounds(now)
	monthStart, monthEnd := MonthBounds(now)
	day, err := spendByProject(ctx, q, ids, dayStart, dayEnd)
	if err != nil {
		return err
	}
	month, err := spendByProject(ctx, q, ids, monthStart, monthEnd)
	if err != nil {
		return err
	}
	for i := range projects {
		id := projects[i].ID
		projects[i].Spend = &ProjectSpend{DayUSDMicros: USDMicros(day[id]), MonthUSDMicros: USDMicros(month[id]),
			DayUntil: dayEnd, MonthUntil: monthEnd}
	}
	return nil
}

// Budget alert windows and levels (DESIGN 5.19 rule 5).
const (
	BudgetWindowDay    = "day"
	BudgetWindowMonth  = "month"
	BudgetAlertWarning = "warning"
	BudgetAlertReached = "reached"
)

// SentBudgetAlerts returns the levels already recorded for a project's window.
func SentBudgetAlerts(ctx context.Context, q Querier, projectID, windowKind string, windowStart time.Time) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT level FROM project_budget_alerts
		WHERE project_id = $1 AND window_kind = $2 AND window_start = $3`, projectID, windowKind, windowStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var level string
		if err := rows.Scan(&level); err != nil {
			return nil, err
		}
		out[level] = true
	}
	return out, rows.Err()
}

// RecordBudgetAlert writes the row of an alert that was sent (or had no destination),
// once: a repeat is a no-op, so a crash between the send and this write re-sends at most
// once under the same idempotency key (DESIGN 5.19 consistency).
func RecordBudgetAlert(ctx context.Context, q Querier, projectID, windowKind string, windowStart time.Time, level string, sentAt time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO project_budget_alerts (project_id, window_kind, window_start, level, sent_at)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, projectID, windowKind, windowStart, level, sentAt)
	return err
}
