package store

import (
	"context"
	"encoding/json"
	"errors"
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
