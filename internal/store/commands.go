package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Recovery commands (DESIGN W2 rules 1 to 6): the receipt of a request to cancel, pause,
// resume, retry or steer a run, and its way to an outcome. The rows are records of what was
// asked and what came of it; they are written by the api and the worker alike and are not
// fenced, except the acknowledgement, which only the worker holding the run's lease gives
// (rule 4).

const commandColumns = `id, run_id, kind, node_id, payload, actor, request_id, received_at, acked_at, applied_at, outcome, detail`

// MaxCommandsListed is how many commands GET /api/runs/{id}/commands lists (rule 3).
const MaxCommandsListed = 100

// InsertRunCommand stores a command as received: pending, with received_at now (the
// database's clock, which orders a run's commands). An empty payload is {}.
func InsertRunCommand(ctx context.Context, q Querier, c *RunCommand) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	if len(c.Payload) == 0 || string(c.Payload) == "null" {
		c.Payload = json.RawMessage(`{}`)
	}
	if len(c.Actor) == 0 {
		c.Actor = json.RawMessage(`{"kind":"platform"}`)
	}
	if c.Outcome == "" {
		c.Outcome = CommandPending
	}
	rows, err := q.Query(ctx, `INSERT INTO run_commands (id, run_id, kind, node_id, payload, actor, request_id, outcome, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+commandColumns,
		c.ID, c.RunID, c.Kind, c.NodeID, c.Payload, c.Actor, c.RequestID, c.Outcome, c.Detail)
	if err != nil {
		return err
	}
	got, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunCommand])
	if err != nil {
		return err
	}
	*c = *got
	return nil
}

func collectCommand(rows pgx.Rows, err error) (*RunCommand, error) {
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToAddrOfStructByName[RunCommand])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// GetRunCommand returns one command of a run; ErrNotFound when the run has no such command.
func GetRunCommand(ctx context.Context, q Querier, runID, id string) (*RunCommand, error) {
	return collectCommand(q.Query(ctx, `SELECT `+commandColumns+` FROM run_commands WHERE run_id=$1 AND id=$2`, runID, id))
}

// ListRunCommands returns a run's commands newest first, at most MaxCommandsListed.
func ListRunCommands(ctx context.Context, q Querier, runID string) ([]RunCommand, error) {
	rows, err := q.Query(ctx, `SELECT `+commandColumns+` FROM run_commands WHERE run_id=$1
		ORDER BY received_at DESC, id DESC LIMIT $2`, runID, MaxCommandsListed)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunCommand])
	if out == nil {
		out = []RunCommand{}
	}
	return out, err
}

// PendingRunCommands returns a run's pending commands in the order they are processed
// (rule 6): received_at, then id.
func PendingRunCommands(ctx context.Context, q Querier, runID string) ([]RunCommand, error) {
	rows, err := q.Query(ctx, `SELECT `+commandColumns+` FROM run_commands WHERE run_id=$1 AND outcome=$2
		ORDER BY received_at, id`, runID, CommandPending)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RunCommand])
}

// PendingCommandsOfRuns returns, per run, its pending commands oldest first (the
// diagnosis's commands_pending).
func PendingCommandsOfRuns(ctx context.Context, q Querier, runIDs []string) (map[string][]RunCommand, error) {
	out := map[string][]RunCommand{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT `+commandColumns+` FROM run_commands WHERE run_id = ANY($1) AND outcome=$2
		ORDER BY received_at, id`, runIDs, CommandPending)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunCommand])
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		out[c.RunID] = append(out[c.RunID], c)
	}
	return out, nil
}

// AckRunCommand records that the worker holding the run's lease took a pending command
// (rule 4): acked_at, once. The acknowledgement carries the lease (rule 8): a worker whose
// epoch moved on gets ErrLeaseLost, and ErrNotFound means the command was settled or taken
// meanwhile. A command's times are the server's clock, as received_at is.
func AckRunCommand(ctx context.Context, q Querier, runID, id string) (*RunCommand, error) {
	l := fence(ctx, runID)
	r, e := fenceArgs(l)
	c, err := collectCommand(q.Query(ctx, `UPDATE run_commands SET acked_at=now() WHERE run_id=$1 AND id=$2 AND outcome=$3
		AND acked_at IS NULL AND `+childFence("run_id", "$4", "$5")+` RETURNING `+commandColumns,
		runID, id, CommandPending, r, e))
	if errors.Is(err, ErrNotFound) {
		return nil, lostOr(ctx, q, l, err)
	}
	return c, err
}

// SettleRunCommand gives a pending command its outcome (applied, rejected, failed or
// superseded) and detail. An applied command gets applied_at; every settled one keeps or
// gets acked_at (a command the api applies itself is acknowledged and applied at once,
// rule 5), except a superseded one, which nobody took. ErrNotFound when it was settled
// already.
func SettleRunCommand(ctx context.Context, q Querier, id, outcome, detail string) (*RunCommand, error) {
	return collectCommand(q.Query(ctx, `UPDATE run_commands SET outcome=$2::text, detail=$3,
		acked_at = CASE WHEN $2::text = $4::text THEN acked_at ELSE COALESCE(acked_at, now()) END,
		applied_at = CASE WHEN $2::text = $5::text THEN now() ELSE applied_at END
		WHERE id=$1 AND outcome=$6 RETURNING `+commandColumns,
		id, outcome, detail, CommandSuperseded, CommandApplied, CommandPending))
}

// ApplyAckedCommands applies the run's acknowledged pending commands of a kind (the cancels
// a confirmed stop applies, rule 11), and returns them.
func ApplyAckedCommands(ctx context.Context, q Querier, runID, kind string) ([]RunCommand, error) {
	rows, err := q.Query(ctx, `UPDATE run_commands SET outcome=$3, applied_at=now() WHERE run_id=$1 AND kind=$2 AND outcome=$4
		AND acked_at IS NOT NULL RETURNING `+commandColumns, runID, kind, CommandApplied, CommandPending)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RunCommand])
}

// SupersedeBefore supersedes the run's pending pause, resume and steer commands received
// before the cancel (rule 6) and returns them.
func SupersedeBefore(ctx context.Context, q Querier, cancel *RunCommand) ([]RunCommand, error) {
	rows, err := q.Query(ctx, `UPDATE run_commands SET outcome=$3, detail=$4 WHERE run_id=$1 AND outcome=$5
		AND kind IN ($6, $7, $8) AND (received_at, id) < ($2::timestamptz, $9::text) RETURNING `+commandColumns,
		cancel.RunID, cancel.ReceivedAt, CommandSuperseded, "superseded by cancel "+cancel.ID, CommandPending,
		CommandPause, CommandResume, CommandSteer, cancel.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RunCommand])
}

// ---- pause and resume (rules 14 and 15) ---------------------------------------------------

// PauseQueuedRun pauses a queued run in place (rule 14): no worker holds it, nothing to park.
// ErrNotFound when the run is not queued.
func PauseQueuedRun(ctx context.Context, q Querier, id string, at time.Time) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET status=$2, paused_at=$3, updated_at=now() WHERE id=$1 AND status=$4
		RETURNING `+runColumns, id, RunPaused, at.UTC(), RunQueued))
}

// ResumePausedRun re-queues a paused run (rule 15), in one statement: the lease epoch goes up
// (rule 7), the paused interval is added to paused_total_ms (a pause does not count toward
// the time limit), and the visit the pause parked, when there is one, is closed succeeded
// without output and marked resumed with the note, so the step's next visit continues it. A
// run parked for a dependency (Chief platform control, DESIGN wave 3 rule 7) loses its park
// reason. ErrNotFound when the run is not paused.
func ResumePausedRun(ctx context.Context, q Querier, id, note string, at time.Time) (*Run, error) {
	return collectRun(q.Query(ctx, `WITH parked AS (
			UPDATE run_nodes SET status=$5, finished_at=$3, resumed_at=$3, resume_note=$4
			WHERE id = (SELECT resume_node_visit_id FROM runs WHERE id=$1 AND status=$2)
			  AND run_id=$1 AND parked_at IS NOT NULL AND park_reason=$6 AND status=$7
			RETURNING id)
		UPDATE runs SET status=$8, lease_epoch=lease_epoch+1,
			paused_total_ms = paused_total_ms + GREATEST(0, COALESCE(EXTRACT(EPOCH FROM ($3::timestamptz - paused_at)) * 1000, 0))::bigint,
			paused_at=NULL, park_reason='', park_detail=NULL, updated_at=now()
		WHERE id=$1 AND status=$2 RETURNING `+runColumns,
		id, RunPaused, at.UTC(), note, NodeSucceeded, ParkPaused, NodeWaitingApproval, RunQueued))
}

// RequeueDependencyRuns re-queues the runs of a merge request that were parked for its
// dependencies (Chief platform control, DESIGN wave 3 rules 7 and 8a), in one statement:
// the lease epoch goes up as for every move to queued (DESIGN W2 rule 7), the park reason
// goes, and note is kept for the run's steps until one succeeds. Such a run was parked
// before it started, so there is no paused interval to count. It returns the runs
// re-queued, none when nothing waited.
func RequeueDependencyRuns(ctx context.Context, q Querier, mrID, note string) ([]Run, error) {
	rows, err := q.Query(ctx, `UPDATE runs SET status=$2, lease_epoch=lease_epoch+1, park_reason='', park_detail=NULL,
		resume_note=$3, paused_at=NULL, updated_at=now()
		WHERE merge_request_id=$1 AND status=$4 AND park_reason=$5 RETURNING `+runColumns,
		mrID, RunQueued, note, RunPaused, RunParkDependency)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Run])
}

// RunStepSucceeded reports whether a step of the run succeeded (a visit that succeeded and
// was not a park closed on its decision or resume): the note of a run the dependency watcher
// re-queued is for the steps before that.
func RunStepSucceeded(ctx context.Context, q Querier, runID string) (bool, error) {
	var yes bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_nodes WHERE run_id=$1 AND status=$2 AND parked_at IS NULL)`,
		runID, NodeSucceeded).Scan(&yes)
	return yes, err
}

// StepResumeNotes returns, oldest first, the resume notes of the step's visits the operator
// paused and resumed since the step last succeeded or was retried (DESIGN W2 rule 15, D-093):
// one entry per resume, "" for a resume without a note. Empty when the step was not resumed.
func StepResumeNotes(ctx context.Context, q Querier, runID, nodeID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT COALESCE(r.resume_note, '') FROM run_nodes r
		WHERE r.run_id=$1 AND r.node_id=$2 AND r.resumed_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM run_nodes s WHERE s.run_id=$1 AND s.node_id=$2 AND s.visit > r.visit
		                  AND s.status=$3 AND s.parked_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM run_nodes t WHERE t.run_id=$1 AND t.node_id=$2 AND t.visit > r.visit
		                  AND t.retried_at IS NOT NULL)
		ORDER BY r.visit`, runID, nodeID, NodeSucceeded)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---- retry from a step (rules 16 and 17) --------------------------------------------------

// StepVisits returns the visits of a node in a run, oldest first.
func StepVisits(ctx context.Context, q Querier, runID, nodeID string) ([]RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE run_id=$1 AND node_id=$2 ORDER BY visit`, runID, nodeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RunNode])
}

// NodesVisitedAfter returns the nodes of the run with a visit that started after the given
// visit did (rule 17: every node visited after the step a retry starts from).
func NodesVisitedAfter(ctx context.Context, q Querier, runID, visitID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT n.node_id FROM run_nodes n, run_nodes f
		WHERE f.id=$2 AND n.run_id=$1 AND n.id <> f.id AND (n.started_at, n.id) > (f.started_at, f.id)
		ORDER BY 1`, runID, visitID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// MarkRetryFrom marks the visit a retry from its step rewinds to (rule 17): the step's next
// visit starts from this one's start commit.
func MarkRetryFrom(ctx context.Context, q Querier, visitID string, at time.Time) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET retry_from_at=$2 WHERE id=$1`, visitID, at.UTC())
	return err
}

// ---- unconfirmed stops (rules 11 to 13) ----------------------------------------------------

// SetRunCancelState records whether the last turn of a stopped run is confirmed stopped.
func SetRunCancelState(ctx context.Context, q Querier, id, state string) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET cancel_state=$2, updated_at=now() WHERE id=$1 RETURNING `+runColumns, id, state))
}

// HasUnconfirmedTurn reports whether a visit of the run has a turn that was not confirmed
// stopped.
func HasUnconfirmedTurn(ctx context.Context, q Querier, runID string) (bool, error) {
	var yes bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed)`, runID).Scan(&yes)
	return yes, err
}

// UnconfirmedTurns returns the run's visits whose turn is not confirmed stopped, oldest first.
func UnconfirmedTurns(ctx context.Context, q Querier, runID string) ([]RunNode, error) {
	rows, err := q.Query(ctx, `SELECT `+runNodeColumns+` FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed
		ORDER BY started_at NULLS LAST, id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RunNode])
}

// ConfirmTurn clears a visit's unconfirmed turn once the gateway shows it stopped.
func ConfirmTurn(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx, `UPDATE run_nodes SET turn_unconfirmed=false WHERE id=$1`, id)
	return err
}

// ListUnconfirmedRuns returns the runs whose cancel_state is unconfirmed, least recently
// updated first, at most limit (the reconcile pass's work, rule 11).
func ListUnconfirmedRuns(ctx context.Context, q Querier, limit int) ([]Run, error) {
	rows, err := q.Query(ctx, `SELECT `+runColumns+` FROM runs WHERE cancel_state=$1 ORDER BY updated_at LIMIT $2`,
		CancelUnconfirmed, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Run])
}

// ConfirmRunStop sets an unconfirmed run's cancel_state confirmed once no visit's turn is
// unconfirmed, and returns the run; ErrNotFound when it is not unconfirmed or a turn still is.
func ConfirmRunStop(ctx context.Context, q Querier, id string) (*Run, error) {
	return collectRun(q.Query(ctx, `UPDATE runs SET cancel_state=$2, updated_at=now() WHERE id=$1 AND cancel_state=$3
		AND NOT EXISTS (SELECT 1 FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed) RETURNING `+runColumns,
		id, CancelConfirmed, CancelUnconfirmed))
}
