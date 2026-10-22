package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Dependencies between pieces of work (Chief platform control, DESIGN 5.34 rules 1 to
// 8): an edge says its dependent (a plan ticket or a merge request record) waits for its
// prerequisite. The edges are the store's; whether a prerequisite is satisfied is the
// deps package's to judge, and the watcher's to record here.

// The kinds of a dependency's two ends and who set an edge (rule 1).
const (
	DependencyTicket       = "ticket"
	DependencyMergeRequest = "merge_request"

	DependencyPlanner  = "planner"
	DependencyOperator = "operator"
	DependencyChief    = "chief"
)

// Dependency is one edge (rule 1). A ticket end is named TicketNode(plan, ticket).
// SatisfiedAt, Reason and EvaluatedAt are the watcher's last account of the prerequisite
// (rules 4 and 5); ClearedAt is set once the dependent was let go with it satisfied.
type Dependency struct {
	ID               string          `db:"id" json:"id"`
	ProjectID        string          `db:"project_id" json:"project_id"`
	DependentKind    string          `db:"dependent_kind" json:"dependent_kind"`
	DependentID      string          `db:"dependent_id" json:"dependent_id"`
	PrerequisiteKind string          `db:"prerequisite_kind" json:"prerequisite_kind"`
	PrerequisiteID   string          `db:"prerequisite_id" json:"prerequisite_id"`
	Source           string          `db:"source" json:"source"`
	CreatedBy        json.RawMessage `db:"created_by" json:"created_by"`
	CreatedAt        time.Time       `db:"created_at" json:"created_at"`
	SatisfiedAt      *time.Time      `db:"satisfied_at" json:"satisfied_at,omitempty"`
	Reason           *string         `db:"reason" json:"reason,omitempty"`
	EvaluatedAt      *time.Time      `db:"evaluated_at" json:"evaluated_at,omitempty"`
	ClearedAt        *time.Time      `db:"cleared_at" json:"cleared_at,omitempty"`
}

const dependencyColumns = `id, project_id, dependent_kind, dependent_id, prerequisite_kind, prerequisite_id, source, created_by,
	created_at, satisfied_at, reason, evaluated_at, cleared_at`

// DependencyNode is one end of an edge.
type DependencyNode struct {
	Kind string
	ID   string
}

// TicketNode is the id a plan ticket has as an end of an edge: ticket ids are unique only
// within their plan.
func TicketNode(planID, ticketID string) string { return planID + "/" + ticketID }

// SplitTicketNode is TicketNode's inverse.
func SplitTicketNode(id string) (planID, ticketID string, ok bool) {
	return strings.Cut(id, "/")
}

// ErrSelfDependency refuses an edge whose two ends are the same (rule 1).
var ErrSelfDependency = errors.New("a merge request cannot depend on itself")

// DependencyCycleError refuses an edge that would close a cycle (rule 1): Path names the
// ends from the new edge's dependent back to itself.
type DependencyCycleError struct{ Path []string }

func (e *DependencyCycleError) Error() string {
	return "would make a cycle: " + strings.Join(e.Path, " -> ")
}

func collectDependencies(rows pgx.Rows) ([]Dependency, error) {
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Dependency])
	if out == nil {
		out = []Dependency{}
	}
	return out, err
}

// lockDependencies serializes the edge writes of a project for the caller's transaction,
// so two writers cannot each add half of a cycle.
func lockDependencies(ctx context.Context, q Querier, projectID string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('dependencies:' || $1::text, 0))`, projectID)
	return err
}

// AddDependency stores one edge after refusing a self-edge (ErrSelfDependency) and an edge
// that would close a cycle (*DependencyCycleError), rule 1. It reports false, writing
// nothing, when the edge exists. q should be a transaction: the project's edges are locked
// until it ends.
func AddDependency(ctx context.Context, q Querier, d *Dependency) (bool, error) {
	if d.DependentKind == d.PrerequisiteKind && d.DependentID == d.PrerequisiteID {
		return false, ErrSelfDependency
	}
	if err := lockDependencies(ctx, q, d.ProjectID); err != nil {
		return false, err
	}
	from := DependencyNode{d.PrerequisiteKind, d.PrerequisiteID}
	to := DependencyNode{d.DependentKind, d.DependentID}
	path, err := dependencyPath(ctx, q, d.ProjectID, from, to)
	if err != nil {
		return false, err
	}
	if path != nil {
		labels, err := DependencyLabels(ctx, q, append([]DependencyNode{to}, path...))
		if err != nil {
			return false, err
		}
		names := []string{labels[to]}
		for _, n := range path {
			names = append(names, labels[n])
		}
		return false, &DependencyCycleError{Path: names}
	}
	return insertDependency(ctx, q, d)
}

func insertDependency(ctx context.Context, q Querier, d *Dependency) (bool, error) {
	if d.ID == "" {
		d.ID = NewID()
	}
	if len(d.CreatedBy) == 0 {
		d.CreatedBy = json.RawMessage(`{}`)
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	tag, err := q.Exec(ctx, `INSERT INTO dependencies (id, project_id, dependent_kind, dependent_id, prerequisite_kind,
			prerequisite_id, source, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		d.ID, d.ProjectID, d.DependentKind, d.DependentID, d.PrerequisiteKind, d.PrerequisiteID, d.Source, d.CreatedBy, d.CreatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// dependencyPath is the path from `from` to `to` along the project's edges (dependent to
// prerequisite), ending with `to`, or nil when there is none: the edge to -> from would
// close it into a cycle.
func dependencyPath(ctx context.Context, q Querier, projectID string, from, to DependencyNode) ([]DependencyNode, error) {
	rows, err := q.Query(ctx, `SELECT dependent_kind, dependent_id, prerequisite_kind, prerequisite_id FROM dependencies
		WHERE project_id = $1 ORDER BY created_at, id`, projectID)
	if err != nil {
		return nil, err
	}
	next := map[DependencyNode][]DependencyNode{}
	var a, b DependencyNode
	_, err = pgx.ForEachRow(rows, []any{&a.Kind, &a.ID, &b.Kind, &b.ID}, func() error {
		next[a] = append(next[a], b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if from == to {
		return []DependencyNode{to}, nil
	}
	// Breadth first, so the cycle named is a shortest one.
	prev := map[DependencyNode]DependencyNode{from: from}
	queue := []DependencyNode{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, m := range next[n] {
			if _, seen := prev[m]; seen {
				continue
			}
			prev[m] = n
			if m == to {
				path := []DependencyNode{m}
				for c := n; c != from; c = prev[c] {
					path = append(path, c)
				}
				path = append(path, from)
				slices.Reverse(path)
				return path, nil
			}
			queue = append(queue, m)
		}
	}
	return nil, nil
}

// DependencyLabels names edge ends as a person reads them: a merge request "!17" (a GitHub
// pull request "#17"), a ticket its id ("T9"). An end that no longer exists keeps its id.
func DependencyLabels(ctx context.Context, q Querier, nodes []DependencyNode) (map[DependencyNode]string, error) {
	out := make(map[DependencyNode]string, len(nodes))
	var mrs []string
	for _, n := range nodes {
		switch n.Kind {
		case DependencyTicket:
			_, t, _ := SplitTicketNode(n.ID)
			out[n] = t
		default:
			out[n] = n.ID
			mrs = append(mrs, n.ID)
		}
	}
	if len(mrs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, provider, number FROM merge_requests WHERE id = ANY($1)`, mrs)
	if err != nil {
		return nil, err
	}
	var id, provider string
	var number int64
	_, err = pgx.ForEachRow(rows, []any{&id, &provider, &number}, func() error {
		out[DependencyNode{DependencyMergeRequest, id}] = RequestLabel(provider, number)
		return nil
	})
	return out, err
}

// RequestLabel is a request as a message names it: "!17", or "#17" for a GitHub pull
// request.
func RequestLabel(provider string, number int64) string {
	if provider == "github" {
		return fmt.Sprintf("#%d", number)
	}
	return fmt.Sprintf("!%d", number)
}

// ReplaceMergeRequestPrerequisites makes prereqs (merge request ids) the operator and Chief
// prerequisites of a merge request (rule 3): rows for ids no longer listed go, rows already
// there stay as they are, and each new one is added by AddDependency with source and
// createdBy. Planner edges are left alone. It reports whether anything changed; a change
// forgets the watcher's progress on the request (its dependency gate). q should be a
// transaction.
func ReplaceMergeRequestPrerequisites(ctx context.Context, q Querier, projectID, mrID string, prereqs []string, source string, createdBy json.RawMessage) (bool, error) {
	if err := lockDependencies(ctx, q, projectID); err != nil {
		return false, err
	}
	if prereqs == nil {
		prereqs = []string{}
	}
	tag, err := q.Exec(ctx, `DELETE FROM dependencies WHERE dependent_kind = $1 AND dependent_id = $2 AND source <> $3
		AND NOT (prerequisite_kind = $1 AND prerequisite_id = ANY($4))`, DependencyMergeRequest, mrID, DependencyPlanner, prereqs)
	if err != nil {
		return false, err
	}
	changed := tag.RowsAffected() > 0
	for _, p := range prereqs {
		ok, err := AddDependency(ctx, q, &Dependency{ProjectID: projectID, DependentKind: DependencyMergeRequest, DependentID: mrID,
			PrerequisiteKind: DependencyMergeRequest, PrerequisiteID: p, Source: source, CreatedBy: createdBy})
		if err != nil {
			return false, err
		}
		changed = changed || ok
	}
	if changed {
		if err := DeleteDependencyGate(ctx, q, mrID); err != nil {
			return false, err
		}
	}
	return changed, nil
}

// ReplacePlannerDependencies makes edges ([dependent ticket, prerequisite ticket], ticket
// ids of the plan) the Planner's edges of the plan (rule 2): planner rows not listed go, rows
// already there stay, new ones are added. The caller checked the plan (same or earlier
// wave, no cycle). The dependency gates of the merge requests of tickets whose edges
// changed are forgotten.
func ReplacePlannerDependencies(ctx context.Context, q Querier, projectID, planID string, edges [][2]string) error {
	if err := lockDependencies(ctx, q, projectID); err != nil {
		return err
	}
	keep := make([]string, 0, len(edges))
	for _, e := range edges {
		keep = append(keep, TicketNode(planID, e[0])+" "+TicketNode(planID, e[1]))
	}
	rows, err := q.Query(ctx, `DELETE FROM dependencies WHERE dependent_kind = $1 AND source = $2 AND dependent_id LIKE $3 ESCAPE '\'
		AND NOT (dependent_id || ' ' || prerequisite_id = ANY($4)) RETURNING dependent_id`,
		DependencyTicket, DependencyPlanner, escapeLike(planID)+"/%", keep)
	if err != nil {
		return err
	}
	changed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, e := range edges {
		d := &Dependency{ProjectID: projectID, DependentKind: DependencyTicket, DependentID: TicketNode(planID, e[0]),
			PrerequisiteKind: DependencyTicket, PrerequisiteID: TicketNode(planID, e[1]), Source: DependencyPlanner,
			CreatedBy: json.RawMessage(`{"kind":"planner"}`)}
		ok, err := insertDependency(ctx, q, d)
		if err != nil {
			return err
		}
		if ok {
			changed = append(changed, d.DependentID)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	_, err = q.Exec(ctx, `DELETE FROM dependency_gates WHERE merge_request_id IN (SELECT merge_request_id FROM plan_tickets
		WHERE merge_request_id IS NOT NULL AND plan_id || '/' || ticket_id = ANY($1))`, changed)
	return err
}

// DependenciesOf returns the edges whose dependent is (kind, id), oldest first.
func DependenciesOf(ctx context.Context, q Querier, kind, id string) ([]Dependency, error) {
	rows, err := q.Query(ctx, `SELECT `+dependencyColumns+` FROM dependencies WHERE dependent_kind = $1 AND dependent_id = $2
		ORDER BY created_at, id`, kind, id)
	if err != nil {
		return nil, err
	}
	return collectDependencies(rows)
}

// MergeRequestDependencies returns the edges a merge request waits on, oldest first: its own
// and the Planner's edges of the plan ticket it was built for.
func MergeRequestDependencies(ctx context.Context, q Querier, mrID string) ([]Dependency, error) {
	rows, err := q.Query(ctx, `SELECT `+dependencyColumns+` FROM dependencies WHERE dependent_kind = $1 AND dependent_id = $2
		UNION ALL
		SELECT `+qualify("d", dependencyColumns)+` FROM dependencies d JOIN plan_tickets t
			ON d.dependent_kind = $3 AND d.dependent_id = t.plan_id || '/' || t.ticket_id
		WHERE t.merge_request_id = $2
		ORDER BY created_at, id`, DependencyMergeRequest, mrID, DependencyTicket)
	if err != nil {
		return nil, err
	}
	return collectDependencies(rows)
}

// SetDependencyEvaluation records the watcher's judgement of an edge's prerequisite (rules 4
// and 5): satisfied sets satisfied_at, which stays; reason is the rule-5 reason (or the note
// of a satisfied one). An edge satisfied already is left as it is.
func SetDependencyEvaluation(ctx context.Context, q Querier, id string, satisfied bool, reason *string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE dependencies SET evaluated_at = $4::timestamptz, reason = $3::text,
			satisfied_at = CASE WHEN $2::boolean THEN $4::timestamptz ELSE NULL END
		WHERE id = $1 AND satisfied_at IS NULL`, id, satisfied, reason, at.UTC())
	return err
}

// ClearDependencies records that the dependents of the edges ids were let go: each satisfied
// edge not cleared yet gets cleared_at. It returns how many it cleared.
func ClearDependencies(ctx context.Context, q Querier, ids []string, at time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE dependencies SET cleared_at = $2
		WHERE id = ANY($1) AND cleared_at IS NULL AND satisfied_at IS NOT NULL`, ids, at.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ClearTicketDependencies clears the satisfied edges of tickets of a plan that were released
// for intake (rule 6), in the caller's transaction.
func ClearTicketDependencies(ctx context.Context, q Querier, planID string, ticketIDs []string, at time.Time) error {
	if len(ticketIDs) == 0 {
		return nil
	}
	nodes := make([]string, len(ticketIDs))
	for i, t := range ticketIDs {
		nodes[i] = TicketNode(planID, t)
	}
	_, err := q.Exec(ctx, `UPDATE dependencies SET cleared_at = $3
		WHERE dependent_kind = $1 AND dependent_id = ANY($2) AND cleared_at IS NULL AND satisfied_at IS NOT NULL`,
		DependencyTicket, nodes, at.UTC())
	return err
}

// HeldTickets returns the tickets of a plan that an edge not satisfied yet holds (rule 6):
// they are not labelled for intake until the watcher finds every prerequisite satisfied.
func HeldTickets(ctx context.Context, q Querier, planID string) (map[string]bool, error) {
	return planTicketsWhere(ctx, q, planID, `satisfied_at IS NULL AND cleared_at IS NULL`)
}

// TicketsWithOpenDependencies returns the tickets of a plan that have an edge not cleared yet.
func TicketsWithOpenDependencies(ctx context.Context, q Querier, planID string) (map[string]bool, error) {
	return planTicketsWhere(ctx, q, planID, `cleared_at IS NULL`)
}

func planTicketsWhere(ctx context.Context, q Querier, planID, cond string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT dependent_id FROM dependencies
		WHERE dependent_kind = $1 AND dependent_id LIKE $2 ESCAPE '\' AND `+cond, DependencyTicket, escapeLike(planID)+"/%")
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, t, ok := SplitTicketNode(id); ok {
			out[t] = true
		}
	}
	return out, nil
}

// heldMergeRequest is the condition that an edge not cleared yet holds the merge request m:
// one of its own, or one of the plan ticket it was built for.
const heldMergeRequest = `(EXISTS (SELECT 1 FROM dependencies d WHERE d.dependent_kind = 'merge_request' AND d.dependent_id = m.id
		AND d.cleared_at IS NULL)
	OR EXISTS (SELECT 1 FROM plan_tickets t JOIN dependencies d ON d.dependent_kind = 'ticket'
		AND d.dependent_id = t.plan_id || '/' || t.ticket_id WHERE t.merge_request_id = m.id AND d.cleared_at IS NULL))`

// BlockedMergeRequests returns the open merge requests an edge holds (rule 8a), the ones the
// watcher looked at longest ago first, at most limit.
func BlockedMergeRequests(ctx context.Context, q Querier, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT m.id FROM merge_requests m LEFT JOIN dependency_gates g ON g.merge_request_id = m.id
		WHERE m.state = $1 AND `+heldMergeRequest+`
		ORDER BY g.updated_at ASC NULLS FIRST, m.created_at, m.id LIMIT $2`, MergeRequestOpen, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// MergeRequestsWaitingOn returns the open merge requests an edge not cleared yet holds on the
// merge request mrID: edges naming it, or naming the plan ticket it was built for.
func MergeRequestsWaitingOn(ctx context.Context, q Querier, mrID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT m.id FROM merge_requests m
		WHERE m.state = $1 AND m.id IN (
			SELECT d.dependent_id FROM dependencies d WHERE d.dependent_kind = 'merge_request' AND d.cleared_at IS NULL
				AND ((d.prerequisite_kind = 'merge_request' AND d.prerequisite_id = $2)
				  OR (d.prerequisite_kind = 'ticket' AND d.prerequisite_id IN
					(SELECT plan_id || '/' || ticket_id FROM plan_tickets WHERE merge_request_id = $2)))
			UNION
			SELECT t.merge_request_id FROM plan_tickets t JOIN dependencies d ON d.dependent_kind = 'ticket'
				AND d.dependent_id = t.plan_id || '/' || t.ticket_id
			WHERE t.merge_request_id IS NOT NULL AND d.cleared_at IS NULL
				AND ((d.prerequisite_kind = 'merge_request' AND d.prerequisite_id = $2)
				  OR (d.prerequisite_kind = 'ticket' AND d.prerequisite_id IN
					(SELECT plan_id || '/' || ticket_id FROM plan_tickets WHERE merge_request_id = $2))))
		ORDER BY m.created_at, m.id`, MergeRequestOpen, mrID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// GetPlanTicket returns one ticket of a plan, or ErrNotFound.
func GetPlanTicket(ctx context.Context, q Querier, planID, ticketID string) (*PlanTicket, error) {
	rows, err := q.Query(ctx, `SELECT `+planTicketColumns+` FROM plan_tickets WHERE plan_id = $1 AND ticket_id = $2`, planID, ticketID)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[PlanTicket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// DependencyGate is the watcher's progress on a dependent merge request whose prerequisites
// are all satisfied (rules 8a and 12): HeadSHA the head it brought up to date with the
// default branch (Outcome up_to_date or updated), Reason why the request still waits,
// Notice the last thing the operator was told about it.
type DependencyGate struct {
	MergeRequestID string    `db:"merge_request_id"`
	HeadSHA        *string   `db:"head_sha"`
	Outcome        *string   `db:"outcome"`
	Reason         *string   `db:"reason"`
	Notice         *string   `db:"notice"`
	UpdatedAt      time.Time `db:"updated_at"`
}

// GetDependencyGate returns a merge request's gate, nil when it has none.
func GetDependencyGate(ctx context.Context, q Querier, mrID string) (*DependencyGate, error) {
	rows, err := q.Query(ctx, `SELECT merge_request_id, head_sha, outcome, reason, notice, updated_at FROM dependency_gates
		WHERE merge_request_id = $1`, mrID)
	if err != nil {
		return nil, err
	}
	g, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[DependencyGate])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// SetDependencyGate writes a merge request's gate whole, updated_at at.
func SetDependencyGate(ctx context.Context, q Querier, g *DependencyGate, at time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO dependency_gates (merge_request_id, head_sha, outcome, reason, notice, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (merge_request_id) DO UPDATE SET head_sha = $2, outcome = $3, reason = $4, notice = $5, updated_at = $6`,
		g.MergeRequestID, g.HeadSHA, g.Outcome, g.Reason, g.Notice, at.UTC())
	return err
}

// DeleteDependencyGate forgets a merge request's gate.
func DeleteDependencyGate(ctx context.Context, q Querier, mrID string) error {
	_, err := q.Exec(ctx, `DELETE FROM dependency_gates WHERE merge_request_id = $1`, mrID)
	return err
}
