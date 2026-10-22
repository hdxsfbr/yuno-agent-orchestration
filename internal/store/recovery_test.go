package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Recovery (DESIGN W2), store side: lease epochs, fenced writes, commands, pause and resume.

// liveRun stores a running run with one live visit of node "work", claimed the way a worker
// claims it: it returns the run as the claim answered it (with the epoch) and the visit.
func liveRun(t *testing.T) (*Run, *RunNode) {
	t.Helper()
	ctx := context.Background()
	wf := &Workflow{Name: "lease", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, testPool, wf))
	r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual}
	mustDo(t, CreateRun(ctx, testPool, r))
	claimed, err := ClaimQueuedRun(ctx, testPool)
	mustDo(t, err)
	now := time.Now()
	a := &Agent{Name: "Lease " + r.ID[len(r.ID)-6:], Role: "x", Model: "claude-opus-5-5"}
	mustDo(t, CreateAgent(ctx, testPool, a))
	key, turn := "agent:x:yuno-"+r.ID+"-work-1", "turn-1"
	n := &RunNode{RunID: r.ID, NodeID: "work", Visit: 1, Status: NodeRunning, StartedAt: &now, SessionKey: &key, OpenClawRunID: &turn,
		AgentID: &a.ID}
	mustDo(t, CreateRunNode(ctx, testPool, n))
	return claimed, n
}

// Rule 7: the claim and every move to queued increment the epoch.
func TestLeaseEpochMovesWithClaimsAndRequeues(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	run, _ := liveRun(t)
	if run.LeaseEpoch != 1 {
		t.Fatalf("claimed with epoch %d", run.LeaseEpoch)
	}
	requeued, _, err := RequeueStaleRuns(ctx, testPool, -time.Hour)
	mustDo(t, err)
	got, err := GetRun(ctx, testPool, run.ID)
	mustDo(t, err)
	if requeued != 1 || got.Status != RunQueued || got.LeaseEpoch != 2 {
		t.Fatalf("requeued %d: %s epoch %d", requeued, got.Status, got.LeaseEpoch)
	}
	again, err := ClaimQueuedRun(ctx, testPool)
	mustDo(t, err)
	if again.LeaseEpoch != 3 {
		t.Fatalf("claimed again with epoch %d", again.LeaseEpoch)
	}
	mustDo(t, execSQL(ctx, testPool, `UPDATE runs SET status='failed', finished_at=now() WHERE id='`+run.ID+`'`))
	retried, err := RetryRun(ctx, testPool, run.ID, nil, time.Now())
	mustDo(t, err)
	if retried.LeaseEpoch != 4 {
		t.Fatalf("retried with epoch %d", retried.LeaseEpoch)
	}
}

// Rules 8 and 9: every fenced write refuses a worker whose epoch moved on, with
// ErrLeaseLost and the lease marked lost, and changes nothing; the same write with the
// current lease, or with none (the api), goes through.
func TestFencedWritesRefuseALostLease(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	run, node := liveRun(t)
	branch := &RunBranch{RunID: run.ID, ForkVisitID: node.ID, ForkID: "work", Head: "a"}
	mustDo(t, InsertRunBranch(ctx, testPool, branch))
	old := NewLease(run)
	mustDo(t, execSQL(ctx, testPool, `UPDATE runs SET lease_epoch=lease_epoch+1 WHERE id='`+run.ID+`'`))
	at := time.Now()
	writes := map[string]func(context.Context) error{
		"TransitionRun": func(ctx context.Context) error {
			_, err := TransitionRun(ctx, testPool, run.ID, []string{RunRunning}, RunRunning, nil, at)
			return err
		},
		"TouchRun":      func(ctx context.Context) error { return TouchRun(ctx, testPool, run.ID) },
		"TouchRunNode":  func(ctx context.Context) error { return TouchRunNode(ctx, testPool, run.ID, node.ID, at) },
		"SetRunCurrent": func(ctx context.Context) error { return SetRunCurrentNode(ctx, testPool, run.ID, "work") },
		"SetRunStateNode": func(ctx context.Context) error {
			_, err := SetRunStateNode(ctx, testPool, run.ID, "work", json.RawMessage(`{}`), "work")
			return err
		},
		"AddRunUsage": func(ctx context.Context) error {
			_, err := AddRunUsageReturning(ctx, testPool, run.ID, 0, 0, 0, 0, 0, 0, 0)
			return err
		},
		"UpdateRunNodeProgress": func(ctx context.Context) error { return UpdateRunNodeProgress(ctx, testPool, node) },
		"SetRunNodeEvidence": func(ctx context.Context) error {
			return SetRunNodeEvidence(ctx, testPool, node.ID, NodeEvidence{WaitingOn: WaitingOnModel})
		},
		"SetRunNodeRepoSHA": func(ctx context.Context) error { return SetRunNodeRepoSHA(ctx, testPool, node.ID, "abc") },
		"SetBranchCurrent":  func(ctx context.Context) error { return SetBranchCurrent(ctx, testPool, branch.ID, "a") },
		"SetBranchStatus": func(ctx context.Context) error {
			_, err := SetBranchStatus(ctx, testPool, branch.ID, BranchRunning, nil, at)
			return err
		},
		"ParkVisit": func(ctx context.Context) error {
			_, err := ParkVisit(ctx, testPool, run.ID, node.ID, at)
			return err
		},
	}
	for name, write := range writes {
		l := &Lease{RunID: old.RunID, Epoch: old.Epoch}
		if err := write(WithLease(ctx, l)); !errors.Is(err, ErrLeaseLost) || !l.Lost() {
			t.Errorf("%s with the old lease: %v (lost %v)", name, err, l.Lost())
		}
	}
	got, err := GetRun(ctx, testPool, run.ID)
	mustDo(t, err)
	if got.Status != RunRunning || got.CurrentNode != nil {
		t.Fatalf("a refused write changed the run: %s %v", got.Status, got.CurrentNode)
	}
	n, err := GetRunNode(ctx, testPool, node.ID)
	mustDo(t, err)
	if n.RepoSHA != nil || n.ParkedAt != nil {
		t.Fatalf("a refused write changed the visit: %v %v", n.RepoSHA, n.ParkedAt)
	}
	current := &Lease{RunID: run.ID, Epoch: got.LeaseEpoch}
	for _, name := range []string{"TouchRunNode", "SetRunCurrent", "SetRunNodeRepoSHA", "SetBranchCurrent", "UpdateRunNodeProgress"} {
		if err := writes[name](WithLease(ctx, current)); err != nil || current.Lost() {
			t.Errorf("%s with the current lease: %v", name, err)
		}
		if err := writes[name](ctx); err != nil {
			t.Errorf("%s without a lease: %v", name, err)
		}
	}
	// A lease of another run does not fence this one's writes.
	other := &Lease{RunID: "another-run", Epoch: 99}
	if err := TouchRun(WithLease(ctx, other), testPool, run.ID); err != nil || other.Lost() {
		t.Fatalf("another run's lease: %v", err)
	}
}

// Rules 1 to 6: a command's way from receipt to its outcome, and a cancel superseding the
// pause, resume and steer still pending before it.
func TestRunCommandsLifecycle(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	run, _ := liveRun(t)
	pause := &RunCommand{RunID: run.ID, Kind: CommandPause}
	mustDo(t, InsertRunCommand(ctx, testPool, pause))
	if pause.Outcome != CommandPending || pause.ReceivedAt.IsZero() || string(pause.Payload) != "{}" || string(pause.Actor) == "" {
		t.Fatalf("received: %+v", pause)
	}
	// Only the lease holder acknowledges.
	stale := &Lease{RunID: run.ID, Epoch: run.LeaseEpoch - 1}
	if _, err := AckRunCommand(WithLease(ctx, stale), testPool, run.ID, pause.ID); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("ack with an old lease: %v", err)
	}
	acked, err := AckRunCommand(WithLease(ctx, NewLease(run)), testPool, run.ID, pause.ID)
	mustDo(t, err)
	if acked.AckedAt == nil || acked.Outcome != CommandPending {
		t.Fatalf("acknowledged: %+v", acked)
	}
	if _, err := AckRunCommand(ctx, testPool, run.ID, pause.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second ack: %v", err)
	}
	steer := &RunCommand{RunID: run.ID, Kind: CommandSteer, Payload: json.RawMessage(`{"message":"hi"}`)}
	mustDo(t, InsertRunCommand(ctx, testPool, steer))
	pending, err := PendingRunCommands(ctx, testPool, run.ID)
	mustDo(t, err)
	if len(pending) != 2 || pending[0].ID != pause.ID || pending[1].ID != steer.ID {
		t.Fatalf("pending in received order: %+v", pending)
	}
	cancel := &RunCommand{RunID: run.ID, Kind: CommandCancel}
	mustDo(t, InsertRunCommand(ctx, testPool, cancel))
	later := &RunCommand{RunID: run.ID, Kind: CommandSteer}
	mustDo(t, InsertRunCommand(ctx, testPool, later))
	superseded, err := SupersedeBefore(ctx, testPool, cancel)
	mustDo(t, err)
	if len(superseded) != 2 {
		t.Fatalf("superseded %d", len(superseded))
	}
	for _, c := range superseded {
		if c.Outcome != CommandSuperseded || c.Detail != "superseded by cancel "+cancel.ID || c.ID == later.ID {
			t.Fatalf("superseded: %+v", c)
		}
	}
	got, err := GetRunCommand(ctx, testPool, run.ID, steer.ID)
	mustDo(t, err)
	if got.AckedAt != nil {
		t.Fatalf("a superseded command nobody took has acked_at: %+v", got)
	}
	applied, err := SettleRunCommand(ctx, testPool, cancel.ID, CommandApplied, "")
	mustDo(t, err)
	if applied.AckedAt == nil || applied.AppliedAt == nil {
		t.Fatalf("applied at once: %+v", applied)
	}
	if _, err := SettleRunCommand(ctx, testPool, cancel.ID, CommandRejected, "late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settling a settled command: %v", err)
	}
	list, err := ListRunCommands(ctx, testPool, run.ID)
	mustDo(t, err)
	if len(list) != 4 || list[0].ID != later.ID {
		t.Fatalf("newest first: %+v", list)
	}
	byRun, err := PendingCommandsOfRuns(ctx, testPool, []string{run.ID})
	mustDo(t, err)
	if len(byRun[run.ID]) != 1 || byRun[run.ID][0].ID != later.ID {
		t.Fatalf("pending of the run: %+v", byRun)
	}
}

// Rules 14 and 15: a visit pauses with its run, and the resume re-queues the run with a new
// epoch, the paused interval kept out of the time limit, and the visit closed with its note;
// a queued run pauses in place.
func TestPauseVisitAndResume(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	run, node := liveRun(t)
	at := time.Now().Add(-time.Minute)
	paused, err := PauseVisit(WithLease(ctx, NewLease(run)), testPool, run.ID, node.ID, at)
	mustDo(t, err)
	if paused.Status != RunPaused || paused.ResumeNodeVisitID == nil || *paused.ResumeNodeVisitID != node.ID {
		t.Fatalf("paused run: %+v", paused)
	}
	n, err := GetRunNode(ctx, testPool, node.ID)
	mustDo(t, err)
	if n.Status != NodeWaitingApproval || n.ParkedAt == nil || n.ParkReason != ParkPaused {
		t.Fatalf("paused visit: %s %v %q", n.Status, n.ParkedAt, n.ParkReason)
	}
	if loops, err := CountNodeLoopVisits(ctx, testPool, run.ID, "work"); err != nil || loops != 0 {
		t.Fatalf("a paused visit counts as a loop visit: %d %v", loops, err)
	}
	if _, err := ResumePausedRun(ctx, testPool, "no-such-run", "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resume of an unknown run: %v", err)
	}
	resumed, err := ResumePausedRun(ctx, testPool, run.ID, "go on", time.Now())
	mustDo(t, err)
	if resumed.Status != RunQueued || resumed.LeaseEpoch != run.LeaseEpoch+1 || resumed.PausedAt != nil || resumed.PausedTotalMs < 59_000 {
		t.Fatalf("resumed run: %s epoch %d paused %v total %d", resumed.Status, resumed.LeaseEpoch, resumed.PausedAt, resumed.PausedTotalMs)
	}
	n, err = GetRunNode(ctx, testPool, node.ID)
	mustDo(t, err)
	if n.Status != NodeSucceeded || n.ResumedAt == nil || n.ResumeNote == nil || *n.ResumeNote != "go on" {
		t.Fatalf("resumed visit: %s %v %v", n.Status, n.ResumedAt, n.ResumeNote)
	}
	notes, err := StepResumeNotes(ctx, testPool, run.ID, "work")
	mustDo(t, err)
	if len(notes) != 1 || notes[0] != "go on" {
		t.Fatalf("notes = %q", notes)
	}
	if _, err := ResumePausedRun(ctx, testPool, run.ID, "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second resume: %v", err)
	}
	queuedPause, err := PauseQueuedRun(ctx, testPool, run.ID, time.Now())
	mustDo(t, err)
	if queuedPause.Status != RunPaused {
		t.Fatalf("queued run paused: %s", queuedPause.Status)
	}
}

// Rule 12: the stale sweep marks the turn a lost worker left and the run's cancel_state, and
// the reconcile's writes confirm them.
func TestStaleSweepMarksUnconfirmedTurns(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	run, node := liveRun(t)
	_, _, err := RequeueStaleRuns(ctx, testPool, -time.Hour)
	mustDo(t, err)
	got, err := GetRun(ctx, testPool, run.ID)
	mustDo(t, err)
	if got.CancelState != CancelUnconfirmed {
		t.Fatalf("cancel_state = %q", got.CancelState)
	}
	turns, err := UnconfirmedTurns(ctx, testPool, run.ID)
	mustDo(t, err)
	if len(turns) != 1 || turns[0].ID != node.ID {
		t.Fatalf("unconfirmed turns = %+v", turns)
	}
	if _, err := ConfirmRunStop(ctx, testPool, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirmed with a turn still unconfirmed: %v", err)
	}
	mustDo(t, ConfirmTurn(ctx, testPool, node.ID))
	done, err := ConfirmRunStop(ctx, testPool, run.ID)
	mustDo(t, err)
	if done.CancelState != CancelConfirmed {
		t.Fatalf("cancel_state = %q", done.CancelState)
	}
	list, err := ListUnconfirmedRuns(ctx, testPool, 10)
	mustDo(t, err)
	if len(list) != 0 {
		t.Fatalf("still listed: %d", len(list))
	}
}
