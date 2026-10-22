package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Plans, their tickets and their events (DESIGN 5.29 rules 1 and 9; migration 0017).

// Plan statuses.
const (
	PlanPublishing       = "publishing"
	PlanWaitingPlanMerge = "waiting_plan_merge"
	PlanBuilding         = "building"
	PlanShipping         = "shipping"
	PlanAccepting        = "accepting"
	PlanDone             = "done"
	PlanCancelled        = "cancelled"
)

// Plan ticket statuses.
const (
	TicketPending  = "pending"
	TicketReleased = "released"
	TicketBuilding = "building"
	TicketInReview = "in_review"
	TicketMerged   = "merged"
	TicketFailed   = "failed"
	TicketClosed   = "closed"
)

// Plan event kinds the platform writes (rules 2, 6, 8, 9, 12, 14, 15).
const (
	PlanEventPublished        = "published"
	PlanEventWaveReleased     = "wave_released"
	PlanEventSmokeTestWaiting = "smoke_test_waiting"
	PlanEventWaveDone         = "wave_done"
	PlanEventAcceptWaiting    = "accept_waiting"
	PlanEventFeatureDone      = "feature_done"
	PlanEventEscalated        = "escalated"
	PlanEventPaused           = "paused"
	PlanEventResumed          = "resumed"
	PlanEventRetried          = "retried"
	PlanEventCancelled        = "cancelled"
	PlanEventRevised          = "revised"
)

const (
	// DefaultPlanEventsLimit and MaxPlanEventsLimit bound ListPlanEvents.
	DefaultPlanEventsLimit = 100
	MaxPlanEventsLimit     = 500

	planOneOpenIndex     = "plans_one_open_idx"
	planOpenFeatureIndex = "plans_open_feature_idx"
)

var (
	// ErrPlanOpen: the project already has a plan that is not done or cancelled (rule 1);
	// OpenPlanForProject names it.
	ErrPlanOpen = errors.New("the project already has an open plan")
	// ErrPlanExists: the project already has an open plan for this feature.
	ErrPlanExists = errors.New("the project already has a plan for this feature")
	// ErrPlanFinished: the plan is done or cancelled already.
	ErrPlanFinished = errors.New("the plan is finished already")
)

// Plan is one feature's row.
type Plan struct {
	ID          string    `db:"id" json:"id"`
	ProjectID   string    `db:"project_id" json:"project_id"`
	Feature     string    `db:"feature" json:"feature"`
	PlanRunID   string    `db:"plan_run_id" json:"plan_run_id"`
	PlanMRID    *string   `db:"plan_mr_id" json:"plan_mr_id,omitempty"`
	Status      string    `db:"status" json:"status"`
	CurrentWave int       `db:"current_wave" json:"current_wave"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// PlanTicket is one ticket of a plan.
type PlanTicket struct {
	PlanID         string    `db:"plan_id" json:"plan_id"`
	TicketID       string    `db:"ticket_id" json:"ticket_id"`
	Wave           int       `db:"wave" json:"wave"`
	Title          string    `db:"title" json:"title"`
	IssueNumber    *int64    `db:"issue_number" json:"issue_number,omitempty"`
	IssueURL       string    `db:"issue_url" json:"issue_url"`
	RunID          *string   `db:"run_id" json:"run_id,omitempty"`
	MergeRequestID *string   `db:"merge_request_id" json:"merge_request_id,omitempty"`
	Status         string    `db:"status" json:"status"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// PlanEvent is one recorded transition. Wave and TicketID are null for events of the whole
// plan; Detail is a JSON object ({} when empty).
type PlanEvent struct {
	ID       string          `db:"id" json:"id"`
	PlanID   string          `db:"plan_id" json:"plan_id"`
	At       time.Time       `db:"at" json:"at"`
	Kind     string          `db:"kind" json:"kind"`
	Wave     *int            `db:"wave" json:"wave,omitempty"`
	TicketID *string         `db:"ticket_id" json:"ticket_id,omitempty"`
	Detail   json.RawMessage `db:"detail" json:"detail"`
	// NotifiedAt is when the operator was told of the event (rule 8); nil until then.
	NotifiedAt *time.Time `db:"notified_at" json:"notified_at,omitempty"`
}

const planColumns = `id, project_id, feature, plan_run_id, plan_mr_id, status, current_wave, created_at, updated_at`

const planEventColumns = `id, plan_id, at, kind, wave, ticket_id, detail, notified_at`

const planTicketColumns = `plan_id, ticket_id, wave, title, issue_number, issue_url, run_id, merge_request_id, status, updated_at`

// CreatePlan inserts a plan (status defaults to publishing, current_wave to 1). It returns
// ErrPlanOpen when the project has an open plan and ErrPlanExists when it has an open plan
// for the feature already; a finished or cancelled plan frees the name.
func CreatePlan(ctx context.Context, q Querier, p *Plan) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Status == "" {
		p.Status = PlanPublishing
	}
	if p.CurrentWave == 0 {
		p.CurrentWave = 1
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err := q.Exec(ctx, `INSERT INTO plans (`+planColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		p.ID, p.ProjectID, p.Feature, p.PlanRunID, p.PlanMRID, p.Status, p.CurrentWave, p.CreatedAt, p.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case planOneOpenIndex:
			return ErrPlanOpen
		case planOpenFeatureIndex:
			return ErrPlanExists
		}
	}
	return err
}

// GetPlan returns a plan, or ErrNotFound.
func GetPlan(ctx context.Context, q Querier, id string) (*Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planColumns+` FROM plans WHERE id=$1`, id)
	if err != nil {
		return nil, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Plan])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// OpenPlanForProject returns the project's plan whose status is not done or cancelled, or
// nil when it has none.
func OpenPlanForProject(ctx context.Context, q Querier, projectID string) (*Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planColumns+` FROM plans
		WHERE project_id=$1 AND status NOT IN ('done','cancelled')`, projectID)
	if err != nil {
		return nil, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Plan])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// ListPlans returns the project's plans, newest first (never nil).
func ListPlans(ctx context.Context, q Querier, projectID string) ([]Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planColumns+` FROM plans WHERE project_id=$1
		ORDER BY created_at DESC, id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Plan])
	if out == nil {
		out = []Plan{}
	}
	return out, err
}

// ListOpenPlans returns every project's open plan, oldest first: the wave watcher's pass.
func ListOpenPlans(ctx context.Context, q Querier) ([]Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planColumns+` FROM plans WHERE status NOT IN ('done','cancelled')
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Plan])
	if out == nil {
		out = []Plan{}
	}
	return out, err
}

func execPlanUpdate(ctx context.Context, q Querier, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPlanStatus sets a plan's status, ErrNotFound for an unknown plan.
func SetPlanStatus(ctx context.Context, q Querier, id, status string) error {
	return execPlanUpdate(ctx, q, `UPDATE plans SET status=$2, updated_at=now() WHERE id=$1`, id, status)
}

// SetPlanMR records the merge request carrying the plan documents.
func SetPlanMR(ctx context.Context, q Querier, id, mergeRequestID string) error {
	return execPlanUpdate(ctx, q, `UPDATE plans SET plan_mr_id=$2, updated_at=now() WHERE id=$1`, id, mergeRequestID)
}

// SetCurrentWave sets the plan's current wave.
func SetCurrentWave(ctx context.Context, q Querier, id string, wave int) error {
	return execPlanUpdate(ctx, q, `UPDATE plans SET current_wave=$2, updated_at=now() WHERE id=$1`, id, wave)
}

// UpsertPlanTicket inserts a ticket or, when (plan, ticket) exists, replaces its wave, title,
// issue number and issue URL; its status, run and merge request are kept so a revision does
// not undo progress. A new ticket's status defaults to pending. t is refreshed from the row.
func UpsertPlanTicket(ctx context.Context, q Querier, t *PlanTicket) error {
	if t.Status == "" {
		t.Status = TicketPending
	}
	rows, err := q.Query(ctx, `INSERT INTO plan_tickets (`+planTicketColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
		ON CONFLICT (plan_id, ticket_id) DO UPDATE SET wave=EXCLUDED.wave, title=EXCLUDED.title,
			issue_number=EXCLUDED.issue_number, issue_url=EXCLUDED.issue_url, updated_at=now()
		RETURNING `+planTicketColumns,
		t.PlanID, t.TicketID, t.Wave, t.Title, t.IssueNumber, t.IssueURL, t.RunID, t.MergeRequestID, t.Status)
	if err != nil {
		return err
	}
	got, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[PlanTicket])
	if err != nil {
		return err
	}
	*t = got
	return nil
}

// ListPlanTickets returns a plan's tickets by wave and ticket id (never nil).
func ListPlanTickets(ctx context.Context, q Querier, planID string) ([]PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+planTicketColumns+` FROM plan_tickets WHERE plan_id=$1
		ORDER BY wave, length(ticket_id), ticket_id`, planID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[PlanTicket])
	if out == nil {
		out = []PlanTicket{}
	}
	return out, err
}

// TicketByIssue returns the ticket of the project's newest plan that carries the issue
// number, with its plan; ErrNotFound when the issue belongs to no plan.
func TicketByIssue(ctx context.Context, q Querier, projectID string, issueNumber int64) (*PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+qualify("t", planTicketColumns)+` FROM plan_tickets t JOIN plans p ON p.id = t.plan_id
		WHERE p.project_id=$1 AND t.issue_number=$2 ORDER BY p.created_at DESC, p.id DESC LIMIT 1`, projectID, issueNumber)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanTicket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// qualify prefixes each name of a comma-separated column list with an alias.
func qualify(alias, cols string) string {
	names := strings.Split(cols, ",")
	for i, n := range names {
		names[i] = alias + "." + strings.TrimSpace(n)
	}
	return strings.Join(names, ", ")
}

// SetTicketStatus sets a ticket's status, ErrNotFound for an unknown ticket.
func SetTicketStatus(ctx context.Context, q Querier, planID, ticketID, status string) error {
	return execPlanUpdate(ctx, q, `UPDATE plan_tickets SET status=$3, updated_at=now() WHERE plan_id=$1 AND ticket_id=$2`,
		planID, ticketID, status)
}

// SetTicketRun links the run that builds the ticket.
func SetTicketRun(ctx context.Context, q Querier, planID, ticketID, runID string) error {
	return execPlanUpdate(ctx, q, `UPDATE plan_tickets SET run_id=$3, updated_at=now() WHERE plan_id=$1 AND ticket_id=$2`,
		planID, ticketID, runID)
}

// SetTicketMR links the ticket's merge request record.
func SetTicketMR(ctx context.Context, q Querier, planID, ticketID, mergeRequestID string) error {
	return execPlanUpdate(ctx, q, `UPDATE plan_tickets SET merge_request_id=$3, updated_at=now() WHERE plan_id=$1 AND ticket_id=$2`,
		planID, ticketID, mergeRequestID)
}

// SetTicketIssue records the ticket's tracker issue.
func SetTicketIssue(ctx context.Context, q Querier, planID, ticketID string, number int64, url string) error {
	return execPlanUpdate(ctx, q, `UPDATE plan_tickets SET issue_number=$3, issue_url=$4, updated_at=now() WHERE plan_id=$1 AND ticket_id=$2`,
		planID, ticketID, number, url)
}

// ResetFailedTicket sets a failed ticket back to pending and unlinks its run and merge
// request, so the next run that takes its issue starts clean (DESIGN 5.29 rule 16). It
// reports whether the ticket was failed.
func ResetFailedTicket(ctx context.Context, q Querier, planID, ticketID string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plan_tickets SET status='pending', run_id=NULL, merge_request_id=NULL, updated_at=now()
		WHERE plan_id=$1 AND ticket_id=$2 AND status='failed'`, planID, ticketID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AddPlanEvent appends an event (ID, At and an empty Detail are filled in).
func AddPlanEvent(ctx context.Context, q Querier, e *PlanEvent) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if len(e.Detail) == 0 {
		e.Detail = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `INSERT INTO plan_events (id, plan_id, at, kind, wave, ticket_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, e.ID, e.PlanID, e.At, e.Kind, e.Wave, e.TicketID, e.Detail)
	return err
}

// ListPlanEvents returns a plan's events newest first; limit <= 0 means
// DefaultPlanEventsLimit and is capped at MaxPlanEventsLimit (never nil).
func ListPlanEvents(ctx context.Context, q Querier, planID string, limit int) ([]PlanEvent, error) {
	switch {
	case limit <= 0:
		limit = DefaultPlanEventsLimit
	case limit > MaxPlanEventsLimit:
		limit = MaxPlanEventsLimit
	}
	rows, err := q.Query(ctx, `SELECT `+planEventColumns+` FROM plan_events
		WHERE plan_id=$1 ORDER BY at DESC, id DESC LIMIT $2`, planID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[PlanEvent])
	if out == nil {
		out = []PlanEvent{}
	}
	return out, err
}

// SetWavesPaused pauses or resumes a live project's waves (rule 9). Resuming clears the
// reason; pausing stores it. ErrNotFound for an unknown or deleted project. It writes no
// plan event: the caller records one on the open plan, when there is one.
func SetWavesPaused(ctx context.Context, q Querier, projectID string, paused bool, reason string) error {
	var r *string
	if paused {
		r = &reason
	}
	return execPlanUpdate(ctx, q, `UPDATE projects SET waves_paused=$2, waves_paused_reason=$3
		WHERE id=$1 AND deleted_at IS NULL`, projectID, paused, r)
}

// ProjectPause is a project's wave pause as LockProjectPause reads it.
type ProjectPause struct {
	WavesPaused       bool
	WavesPausedReason *string
}

// LockProjectPause reads the project's wave pause and holds its row shared until the
// transaction ends, so a pause set meanwhile waits for the commit (rule 12). ErrNotFound for
// an unknown project.
func LockProjectPause(ctx context.Context, q Querier, projectID string) (ProjectPause, error) {
	var p ProjectPause
	err := q.QueryRow(ctx, `SELECT waves_paused, waves_paused_reason FROM projects WHERE id=$1 FOR SHARE`, projectID).
		Scan(&p.WavesPaused, &p.WavesPausedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// TicketByRun returns the plan ticket the run builds, ErrNotFound for a run that builds none.
func TicketByRun(ctx context.Context, q Querier, runID string) (*PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+planTicketColumns+` FROM plan_tickets WHERE run_id=$1
		ORDER BY updated_at DESC LIMIT 1`, runID)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanTicket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// TicketDocsOfRun returns the feature and the ticket id of the plan ticket the run builds
// (the one TicketByRun returns), "" and "" for a run that builds none: the plan-document
// rule of open_merge_request (DESIGN 5.28 rule 9, D-089).
func TicketDocsOfRun(ctx context.Context, q Querier, runID string) (feature, ticketID string, err error) {
	err = q.QueryRow(ctx, `SELECT p.feature, t.ticket_id FROM plan_tickets t JOIN plans p ON p.id = t.plan_id
		WHERE t.run_id=$1 ORDER BY t.updated_at DESC LIMIT 1`, runID).Scan(&feature, &ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return feature, ticketID, err
}

// LiveRevisionRun returns the id of the oldest run of the project whose input revise_plan
// names the plan and that has not ended, "" when there is none (DESIGN 5.29 rule 12, D-089):
// while one runs, the plan's waves wait.
func LiveRevisionRun(ctx context.Context, q Querier, projectID, planID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM runs WHERE project_id=$1 AND btrim(input->>'revise_plan')=$2
		AND status NOT IN ('succeeded','failed','cancelled','timed_out') ORDER BY created_at, id LIMIT 1`, projectID, planID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// HeldWave reports whether advance_wave closed the wave before wave while a revision of the
// plan ran, and so left wave to be released when it ended (DESIGN 5.29 rule 14, D-089): the
// plan's `wave_done` event whose detail names wave as held_wave.
func HeldWave(ctx context.Context, q Querier, planID string, wave int) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plan_events WHERE plan_id=$1 AND kind=$2 AND detail->>'held_wave'=$3)`,
		planID, PlanEventWaveDone, strconv.Itoa(wave)).Scan(&held)
	return held, err
}

// TicketByMergeRequest returns the plan ticket whose merge request record is mergeRequestID,
// ErrNotFound when none is.
func TicketByMergeRequest(ctx context.Context, q Querier, mergeRequestID string) (*PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+planTicketColumns+` FROM plan_tickets WHERE merge_request_id=$1
		ORDER BY updated_at DESC LIMIT 1`, mergeRequestID)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanTicket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// MoveTicket sets a ticket's status to `to` only when its current status is one of from, and
// reports whether it did: a ticket that moved on meanwhile is left as it is.
func MoveTicket(ctx context.Context, q Querier, planID, ticketID, to string, from ...string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plan_tickets SET status=$3, updated_at=now()
		WHERE plan_id=$1 AND ticket_id=$2 AND status = ANY($4)`, planID, ticketID, to, from)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LinkTicketRun links the run that took the ticket's issue and sets the ticket building
// (rule 10), from pending, released, building or failed (an issue labelled again by hand).
// It reports whether the ticket moved.
func LinkTicketRun(ctx context.Context, q Querier, planID, ticketID, runID string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plan_tickets SET run_id=$3, status='building', updated_at=now()
		WHERE plan_id=$1 AND ticket_id=$2 AND status IN ('pending','released','building','failed')`, planID, ticketID, runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LinkTicketMergeRequest records the merge request of the run's ticket and sets it in
// review (rule 10), from pending, released or building. It returns the ticket's plan id and
// ticket id, "" when the run builds no ticket or the ticket had moved on. InsertMergeRequest
// calls it in the transaction that writes the record.
func LinkTicketMergeRequest(ctx context.Context, q Querier, runID, mergeRequestID string) (planID, ticketID string, err error) {
	rows, err := q.Query(ctx, `UPDATE plan_tickets SET merge_request_id=$2, status='in_review', updated_at=now()
		WHERE run_id=$1 AND status IN ('pending','released','building') RETURNING plan_id, ticket_id`, runID, mergeRequestID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&planID, &ticketID); err != nil {
			return "", "", err
		}
	}
	return planID, ticketID, rows.Err()
}

// CancelPlan sets an open plan cancelled and reports it; ErrNotFound for an unknown plan,
// ErrPlanFinished for one that is done or cancelled already.
func CancelPlan(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `UPDATE plans SET status='cancelled', updated_at=now()
		WHERE id=$1 AND status NOT IN ('done','cancelled')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := GetPlan(ctx, q, id); err != nil {
		return err
	}
	return ErrPlanFinished
}

// RetryTicketOfRun moves the failed ticket the run builds back to building, or to in_review
// when it has a merge request, and records a `retried` event (an operator retried the
// run, DESIGN 5.29 rule 7). It returns the ticket's plan id and project id, "" when the
// run builds no failed ticket of an open plan. Callers run it in the retry's transaction,
// so the run cannot fail again before the ticket is back.
func RetryTicketOfRun(ctx context.Context, q Querier, runID string) (planID, projectID string, err error) {
	rows, err := q.Query(ctx, `UPDATE plan_tickets t SET status = CASE WHEN t.merge_request_id IS NOT NULL THEN 'in_review' ELSE 'building' END,
			updated_at=now()
		FROM plans p WHERE p.id = t.plan_id AND t.run_id=$1 AND t.status='failed' AND p.status NOT IN ('done','cancelled')
		RETURNING t.plan_id, p.project_id, t.ticket_id, t.wave, t.status`, runID)
	if err != nil {
		return "", "", err
	}
	type moved struct {
		planID, projectID, ticketID, status string
		wave                                int
	}
	var ms []moved
	for rows.Next() {
		var m moved
		if err := rows.Scan(&m.planID, &m.projectID, &m.ticketID, &m.wave, &m.status); err != nil {
			rows.Close()
			return "", "", err
		}
		ms = append(ms, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	for _, m := range ms {
		raw, _ := json.Marshal(map[string]any{"run_id": runID, "status": m.status})
		wave, ticket := m.wave, m.ticketID
		if err := AddPlanEvent(ctx, q, &PlanEvent{PlanID: m.planID, Kind: PlanEventRetried, Wave: &wave, TicketID: &ticket, Detail: raw}); err != nil {
			return "", "", err
		}
		planID, projectID = m.planID, m.projectID
	}
	return planID, projectID, nil
}

// MarkPlanEventNotified records that the operator was told of the event.
func MarkPlanEventNotified(ctx context.Context, q Querier, eventID string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE plan_events SET notified_at=$2 WHERE id=$1 AND notified_at IS NULL`, eventID, at.UTC())
	return err
}

// ListUnnotifiedPlanEvents returns the events of the given kinds with notified_at null, at
// least minAge old and at most maxAge old, oldest first, at most limit: the resend sweep's
// work (rule 8).
func ListUnnotifiedPlanEvents(ctx context.Context, q Querier, kinds []string, minAge, maxAge time.Duration, limit int) ([]PlanEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+planEventColumns+` FROM plan_events
		WHERE notified_at IS NULL AND kind = ANY($1) AND at <= now() - make_interval(secs => $2) AND at > now() - make_interval(secs => $3)
		ORDER BY at, id LIMIT $4`, kinds, minAge.Seconds(), maxAge.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[PlanEvent])
	if out == nil {
		out = []PlanEvent{}
	}
	return out, err
}

// MergedTicketsToApply returns the tickets whose merge request record is merged while the
// ticket is still pending, released, building or in review: a merge the ticket did not
// hear of (rule 10). Oldest first, at most limit.
func MergedTicketsToApply(ctx context.Context, q Querier, limit int) ([]PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+qualify("t", planTicketColumns)+` FROM plan_tickets t
		JOIN merge_requests m ON m.id = t.merge_request_id
		JOIN plans p ON p.id = t.plan_id
		WHERE m.state='merged' AND t.status IN ('pending','released','building','in_review')
			AND p.status NOT IN ('done','cancelled')
		ORDER BY t.updated_at, t.plan_id, t.ticket_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[PlanTicket])
	if out == nil {
		out = []PlanTicket{}
	}
	return out, err
}

// PlansWithTicketsInReviewSince returns (plan id, project id) of the open plans that have a
// ticket in review changed at or after since: a link of a merge request to a ticket, for
// publishing plan.updated (the link itself is made in the tool's transaction).
func PlansWithTicketsInReviewSince(ctx context.Context, q Querier, since time.Time) ([][2]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT p.id, p.project_id FROM plan_tickets t JOIN plans p ON p.id = t.plan_id
		WHERE t.status='in_review' AND t.updated_at >= $1 AND p.status NOT IN ('done','cancelled')`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var ids [2]string
		if err := rows.Scan(&ids[0], &ids[1]); err != nil {
			return nil, err
		}
		out = append(out, ids)
	}
	return out, rows.Err()
}

// ShipRef is the trigger_ref of the first ship run of a plan's wave, in the form used before
// attempts were numbered (DESIGN 5.29 rule 12); ParseShipRef still accepts it, as attempt 1.
func ShipRef(planID string, wave int) string {
	return "plan:" + planID + ":wave:" + strconv.Itoa(wave)
}

// ShipAttemptRef is the trigger_ref of attempt k (from 1) at shipping a plan's wave:
// "plan:<plan id>:wave:<n>:<k>". A revision of a wave whose earlier attempt ended without
// closing it lets the watcher start the next attempt (rule 12).
func ShipAttemptRef(planID string, wave, attempt int) string {
	return ShipRef(planID, wave) + ":" + strconv.Itoa(max(attempt, 1))
}

// ParseShipRef splits a ship run's trigger_ref; ok is false for any other text.
func ParseShipRef(ref string) (planID string, wave int, ok bool) {
	planID, wave, _, ok = ParseShipAttempt(ref)
	return planID, wave, ok
}

// ParseShipAttempt is ParseShipRef with the attempt number, 1 for the form without one.
func ParseShipAttempt(ref string) (planID string, wave, attempt int, ok bool) {
	rest, found := strings.CutPrefix(ref, "plan:")
	if !found {
		return "", 0, 0, false
	}
	i := strings.LastIndex(rest, ":wave:")
	if i <= 0 {
		return "", 0, 0, false
	}
	waveText, attemptText, hasAttempt := strings.Cut(rest[i+len(":wave:"):], ":")
	n, err := strconv.Atoi(waveText)
	if err != nil || n < 1 {
		return "", 0, 0, false
	}
	attempt = 1
	if hasAttempt {
		if attempt, err = strconv.Atoi(attemptText); err != nil || attempt < 1 {
			return "", 0, 0, false
		}
	}
	return rest[:i], n, attempt, true
}

// PlanMain is the default branch's status as the wave watcher last read it (rule 11).
type PlanMain struct {
	State string     `json:"state"`
	SHA   string     `json:"sha"`
	At    *time.Time `json:"at,omitempty"`
	URL   string     `json:"url"`
}

// SetPlanMain records the watcher's read of the default branch. It reports whether the
// state, sha or time changed from what was recorded.
func SetPlanMain(ctx context.Context, q Querier, planID string, m PlanMain) (changed bool, err error) {
	var at *time.Time
	if m.At != nil && !m.At.IsZero() {
		t := m.At.UTC()
		at = &t
	}
	prev, err := GetPlanMain(ctx, q, planID)
	if err != nil {
		return false, err
	}
	if err := execPlanUpdate(ctx, q, `UPDATE plans SET main_state=$2, main_sha=$3, main_at=$4, main_url=$5 WHERE id=$1`,
		planID, m.State, m.SHA, at, m.URL); err != nil {
		return false, err
	}
	return prev == nil || prev.State != m.State || prev.SHA != m.SHA || prev.URL != m.URL ||
		(prev.At == nil) != (at == nil) || (at != nil && !prev.At.Equal(*at)), nil
}

// GetPlanMain returns the recorded status of the default branch, nil before the first read.
func GetPlanMain(ctx context.Context, q Querier, planID string) (*PlanMain, error) {
	var state, sha, url *string
	var at *time.Time
	err := q.QueryRow(ctx, `SELECT main_state, main_sha, main_at, main_url FROM plans WHERE id=$1`, planID).Scan(&state, &sha, &at, &url)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil || state == nil {
		return nil, err
	}
	m := &PlanMain{State: *state, At: at}
	if sha != nil {
		m.SHA = *sha
	}
	if url != nil {
		m.URL = *url
	}
	return m, nil
}

// ShipRun is the ship run of one wave of a plan.
type ShipRun struct {
	Wave   int    `json:"wave"`
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// ListShipRuns returns the plan's ship runs by wave (never nil).
func ListShipRuns(ctx context.Context, q Querier, planID string) ([]ShipRun, error) {
	rows, err := q.Query(ctx, `SELECT id, status, trigger_ref FROM runs WHERE trigger=$1 AND trigger_ref LIKE $2
		ORDER BY created_at, id`, TriggerPlan, "plan:"+escapeLike(planID)+":wave:%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// One entry per wave: its newest attempt (a revision can start another, rule 12).
	best := map[int]int{}
	byWave := map[int]ShipRun{}
	for rows.Next() {
		var id, status, ref string
		if err := rows.Scan(&id, &status, &ref); err != nil {
			return nil, err
		}
		if pid, wave, attempt, ok := ParseShipAttempt(ref); ok && pid == planID && attempt >= best[wave] {
			best[wave] = attempt
			byWave[wave] = ShipRun{Wave: wave, RunID: id, Status: status}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ShipRun{}
	for _, r := range byWave {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Wave < out[j].Wave })
	return out, nil
}

// LatestShipRun returns the newest attempt's ship run of the plan's wave and its attempt
// number; ErrNotFound when the wave has none.
func LatestShipRun(ctx context.Context, q Querier, planID string, wave int) (*Run, int, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE trigger=$1 AND (trigger_ref=$2 OR trigger_ref LIKE $3)`,
		TriggerPlan, ShipRef(planID, wave), escapeLike(ShipRef(planID, wave))+`:%`)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if err != nil {
		return nil, 0, err
	}
	var best *Run
	bestAttempt := 0
	for i := range list {
		if list[i].TriggerRef == nil {
			continue
		}
		if pid, w, attempt, ok := ParseShipAttempt(*list[i].TriggerRef); ok && pid == planID && w == wave && attempt >= bestAttempt {
			best, bestAttempt = &list[i], attempt
		}
	}
	if best == nil {
		return nil, 0, ErrNotFound
	}
	return best, bestAttempt, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// LockPlan reads a plan and locks its row until the transaction ends, so the checks and
// the writes that follow are one step (q must be a transaction). ErrNotFound for an unknown plan.
func LockPlan(ctx context.Context, q Querier, id string) (*Plan, error) {
	rows, err := q.Query(ctx, `SELECT `+planColumns+` FROM plans WHERE id=$1 FOR UPDATE`, id)
	if err != nil {
		return nil, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[Plan])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// MovePlan sets a plan's status and current wave only when its status is one of from, and
// reports whether it did.
func MovePlan(ctx context.Context, q Querier, id, to string, wave int, from ...string) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE plans SET status=$2, current_wave=$3, updated_at=now()
		WHERE id=$1 AND status = ANY($4)`, id, to, wave, from)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FindPlanEvent returns the plan's oldest event of the kind, for the wave when wave > 0 and
// for the run named in its detail when runID is not empty; ErrNotFound when there is none.
func FindPlanEvent(ctx context.Context, q Querier, planID, kind string, wave int, runID string) (*PlanEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+planEventColumns+` FROM plan_events
		WHERE plan_id=$1 AND kind=$2 AND ($3 = 0 OR wave=$3) AND ($4 = '' OR detail->>'run_id' = $4)
		ORDER BY at, id LIMIT 1`, planID, kind, wave, runID)
	if err != nil {
		return nil, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanEvent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// FindPlanEventRetry is FindPlanEvent for the event of one ending of a run: runID's event
// whose detail carries retries (events written before the count existed read as 0).
func FindPlanEventRetry(ctx context.Context, q Querier, planID, kind string, wave int, runID string, retries int) (*PlanEvent, error) {
	rows, err := q.Query(ctx, `SELECT `+planEventColumns+` FROM plan_events
		WHERE plan_id=$1 AND kind=$2 AND ($3 = 0 OR wave=$3) AND detail->>'run_id' = $4
			AND COALESCE((detail->>'retries')::int, 0) = $5
		ORDER BY at, id LIMIT 1`, planID, kind, wave, runID, retries)
	if err != nil {
		return nil, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanEvent])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// WaveTicketMerge is a ticket of a wave with what its merge request record says.
type WaveTicketMerge struct {
	PlanTicket
	// MRState and MRNumber are those of the ticket's record; "" and 0 without one.
	MRState  string
	MRNumber int64
	// MergedAt is when a merged record merged (closed_at, else updated_at); nil otherwise.
	MergedAt *time.Time
	// MRTitle and MRURL are the record's, "" without one; MergeSHA is the commit the
	// provider says the merge made, "" when it said none.
	MRTitle  string
	MRURL    string
	MergeSHA string
}

// ListWaveTickets returns the tickets of one wave of a plan with their merge request
// records (rule 11), by ticket id.
func ListWaveTickets(ctx context.Context, q Querier, planID string, wave int) ([]WaveTicketMerge, error) {
	rows, err := q.Query(ctx, `SELECT `+qualify("t", planTicketColumns)+`, COALESCE(m.state, ''), COALESCE(m.number, 0),
			CASE WHEN m.state = 'merged' THEN COALESCE(m.closed_at, m.updated_at) END,
			COALESCE(m.title, ''), COALESCE(m.url, ''), COALESCE(m.merge_sha, '')
		FROM plan_tickets t LEFT JOIN merge_requests m ON m.id = t.merge_request_id
		WHERE t.plan_id=$1 AND t.wave=$2 ORDER BY length(t.ticket_id), t.ticket_id`, planID, wave)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaveTicketMerge{}
	for rows.Next() {
		var w WaveTicketMerge
		t := &w.PlanTicket
		if err := rows.Scan(&t.PlanID, &t.TicketID, &t.Wave, &t.Title, &t.IssueNumber, &t.IssueURL, &t.RunID, &t.MergeRequestID,
			&t.Status, &t.UpdatedAt, &w.MRState, &w.MRNumber, &w.MergedAt, &w.MRTitle, &w.MRURL, &w.MergeSHA); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
