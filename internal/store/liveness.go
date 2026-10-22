package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/events"
)

// Liveness evidence (DESIGN 5.12, liveness from evidence): the run_nodes columns a live
// visit's goroutine writes, and the reads the diagnosis makes of them.

// Waiting-on values of run_nodes.waiting_on (the liveness package names them too).
const (
	WaitingOnModel = "model"
	WaitingOnTool  = "tool"
	WaitingOnHuman = "human"
	WaitingOnSetup = "setup"
)

// NodeEvidence is what a visit writes when its evidence changes: waiting_on,
// model_wait_since (nil unless waiting on the model), progress_at (nil keeps the stored
// value) and open_calls (a JSON array).
type NodeEvidence struct {
	WaitingOn      string
	ModelWaitSince *time.Time
	ProgressAt     *time.Time
	OpenCalls      json.RawMessage
}

// StartRunNodeEvidence records a visit's start: its first heartbeat and progress at `at`,
// waiting on setup, no open call. Fenced by the worker's lease (DESIGN W2 rule 8).
func StartRunNodeEvidence(ctx context.Context, q Querier, id string, at time.Time) error {
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET heartbeat_at=$2, progress_at=$2, waiting_on=$3, model_wait_since=NULL,
		open_calls='[]' WHERE id=$1 AND `+childFence("run_id", "$4", "$5"), id, at.UTC(), WaitingOnSetup, r, e)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// TouchRunNode is TouchRun with the visit's heartbeat in the same statement: the run's
// updated_at (the stale-run sweeper's evidence, DESIGN 5.1 rule 8) and the visit's
// heartbeat_at move together. The heartbeat renews the worker's lease (DESIGN W2 rule 8):
// both writes carry the epoch, and ErrLeaseLost says the run is another worker's now.
func TouchRunNode(ctx context.Context, q Querier, runID, runNodeID string, at time.Time) error {
	l := fence(ctx, runID)
	_, epoch := fenceArgs(l)
	tag, err := q.Exec(ctx, `WITH node AS (UPDATE run_nodes SET heartbeat_at=$3 WHERE id=$2 AND run_id=$1
			AND EXISTS (SELECT 1 FROM runs lr WHERE lr.id=$1 AND `+runFence("$4")+`) RETURNING id)
		UPDATE runs SET updated_at=now() WHERE id=$1 AND `+runFence("$4"), runID, runNodeID, at.UTC(), epoch)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// SetRunNodeEvidence writes a visit's evidence after a change, fenced by the worker's lease
// (DESIGN W2 rule 8).
func SetRunNodeEvidence(ctx context.Context, q Querier, id string, ev NodeEvidence) error {
	calls := ev.OpenCalls
	if len(calls) == 0 {
		calls = json.RawMessage(`[]`)
	}
	var progress any
	if ev.ProgressAt != nil {
		progress = ev.ProgressAt.UTC()
	}
	var since any
	if ev.ModelWaitSince != nil {
		since = ev.ModelWaitSince.UTC()
	}
	l := LeaseOf(ctx)
	r, e := fenceArgs(l)
	tag, err := q.Exec(ctx, `UPDATE run_nodes SET waiting_on=$2, model_wait_since=$3, open_calls=$4,
		progress_at=COALESCE($5::timestamptz, progress_at) WHERE id=$1 AND `+childFence("run_id", "$6", "$7"),
		id, ev.WaitingOn, since, calls, progress, r, e)
	if err == nil && tag.RowsAffected() == 0 {
		return lostOr(ctx, q, l, nil)
	}
	return err
}

// LiveNode is a live agent visit with its evidence: agent is the agent's name (its id when
// the row is gone), parked says the visit parked on a decision (DESIGN 5.24 rule 5), and
// the last event is the newest run event about the visit (its node.status, model_call,
// tool_call or approval events), nil when there is none.
type LiveNode struct {
	ID             string
	RunID          string
	NodeID         string
	Visit          int
	BranchID       *string
	AgentID        string
	Agent          string
	Status         string
	Parked         bool
	ParkReason     string // "paused" for a visit the operator paused (DESIGN W2 rule 14)
	HeartbeatAt    *time.Time
	ProgressAt     *time.Time
	WaitingOn      string
	ModelWaitSince *time.Time
	OpenCalls      json.RawMessage
	LastEventSeq   *int64
	LastEvent      *string
	LastEventAt    *time.Time
}

// ListLiveNodes returns the live agent visits of the runs: rows with an agent (fork, join
// and approval node rows have none and no visit goroutine) that have not finished, oldest
// first per run.
func ListLiveNodes(ctx context.Context, q Querier, runIDs []string) ([]LiveNode, error) {
	if len(runIDs) == 0 {
		return []LiveNode{}, nil
	}
	rows, err := q.Query(ctx, `SELECT n.id, n.run_id, n.node_id, n.visit, n.branch_id, n.agent_id, COALESCE(a.name, n.agent_id),
			n.status, n.parked_at IS NOT NULL, n.park_reason, n.heartbeat_at, n.progress_at, n.waiting_on, n.model_wait_since, n.open_calls,
			le.seq, le.event, le.created_at
		FROM run_nodes n
		LEFT JOIN agents a ON a.id = n.agent_id
		LEFT JOIN LATERAL (SELECT e.seq, e.event, e.created_at FROM run_events e
			WHERE e.run_id = n.run_id AND (e.data->'node'->>'id' = n.id OR e.data->'model_call'->>'run_node_id' = n.id
				OR e.data->'tool_call'->>'run_node_id' = n.id OR e.data->'approval'->>'run_node_id' = n.id)
			ORDER BY e.seq DESC LIMIT 1) le ON true
		WHERE n.run_id = ANY($1) AND n.agent_id IS NOT NULL AND n.finished_at IS NULL AND n.status IN ($2, $3, $4)
		ORDER BY n.run_id, n.started_at NULLS LAST, n.id`, runIDs, NodePending, NodeRunning, NodeWaitingApproval)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LiveNode, error) {
		var n LiveNode
		err := r.Scan(&n.ID, &n.RunID, &n.NodeID, &n.Visit, &n.BranchID, &n.AgentID, &n.Agent, &n.Status, &n.Parked, &n.ParkReason,
			&n.HeartbeatAt, &n.ProgressAt, &n.WaitingOn, &n.ModelWaitSince, &n.OpenCalls, &n.LastEventSeq, &n.LastEvent, &n.LastEventAt)
		return n, err
	})
	if out == nil {
		out = []LiveNode{}
	}
	return out, err
}

// ListPendingApprovalsOfRuns returns the pending approvals and questions of the runs,
// oldest first.
func ListPendingApprovalsOfRuns(ctx context.Context, q Querier, runIDs []string) ([]Approval, error) {
	if len(runIDs) == 0 {
		return []Approval{}, nil
	}
	rows, err := q.Query(ctx, `SELECT `+approvalSelect+` FROM approvals WHERE run_id = ANY($1) AND status=$2
		ORDER BY requested_at, id`, runIDs, ApprovalPending)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Approval])
	if out == nil {
		out = []Approval{}
	}
	return out, err
}

// ListLiveRuns returns the runs that have not ended (queued, running, waiting_approval,
// paused), newest first, optionally only those of a project; limit outside 1..500 is 100.
func ListLiveRuns(ctx context.Context, q Querier, projectID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE status IN ($1, $2, $3, $6) AND ($4 = '' OR project_id = $4)
		ORDER BY created_at DESC LIMIT $5`, RunQueued, RunRunning, RunWaitingApproval, projectID, limit, RunPaused)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if out == nil {
		out = []Run{}
	}
	return out, err
}

// RunEventsPage returns up to limit events of a run with seq > after, oldest first, only
// of the given types when any are given.
func RunEventsPage(ctx context.Context, q Querier, runID string, after int64, limit int, types []string) ([]events.Event, error) {
	if types == nil {
		types = []string{}
	}
	rows, err := q.Query(ctx, `SELECT seq, run_id, event, data, created_at FROM run_events
		WHERE run_id = $1 AND seq > $2 AND (cardinality($3::text[]) = 0 OR event = ANY($3::text[]))
		ORDER BY seq LIMIT $4`, runID, after, types, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (events.Event, error) {
		var e events.Event
		err := r.Scan(&e.Seq, &e.RunID, &e.Type, &e.Data, &e.At)
		return e, err
	})
	if out == nil {
		out = []events.Event{}
	}
	return out, err
}
