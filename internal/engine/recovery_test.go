package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/liveness"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Recovery (DESIGN W2): commands, leases, cancel confirmation, pause and resume, retry from
// a step and steering, engine side. The fake gateway answers chat.abort with "aborted" by
// default (the confirming chat event); abortEmits off and the sessions it lists stand for a
// gateway that does not confirm, or shows the turn still running.

// commandRow reads a command of the run.
func (env *testEnv) commandRow(runID, id string) *store.RunCommand {
	env.t.Helper()
	c, err := store.GetRunCommand(context.Background(), env.pool, runID, id)
	if err != nil {
		env.t.Fatal(err)
	}
	return c
}

// awaitCommand waits for a command of the run to satisfy ok.
func (env *testEnv) awaitCommand(runID, id, what string, ok func(*store.RunCommand) bool) *store.RunCommand {
	env.t.Helper()
	var c *store.RunCommand
	eventually(env.t, what, func() bool {
		got, err := store.GetRunCommand(context.Background(), env.pool, runID, id)
		if err != nil {
			return false
		}
		c = got
		return ok(got)
	})
	return c
}

// commandEvents counts the run.command events of a command.
func (env *testEnv) commandEvents(runID, id string) int {
	return env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.command' AND data->'command'->>'id'=$2`, runID, id)
}

// leaseEpoch reads the run's lease epoch.
func (env *testEnv) leaseEpoch(runID string) int64 {
	env.t.Helper()
	var e int64
	if err := env.pool.QueryRow(context.Background(), `SELECT lease_epoch FROM runs WHERE id=$1`, runID).Scan(&e); err != nil {
		env.t.Fatal(err)
	}
	return e
}

// sessionShows makes sessions.list show the session with an active run (the turn) or none.
func (env *testEnv) sessionShows(key, turn string, active bool) {
	env.fc.mu.Lock()
	defer env.fc.mu.Unlock()
	row := openclaw.SessionRow{Key: key, AgentID: "x", HasActiveRun: active}
	if active {
		row.ActiveRunIDs = []string{turn}
	}
	env.fc.sessions = []openclaw.SessionRow{row}
}

// heldRun starts a quick run whose first turn starts and is held open; the returned channel
// gives that turn's send.
func (env *testEnv) heldRun() (*store.Run, <-chan struct{}, openclaw.ChatSendParams) {
	env.t.Helper()
	a := env.agent(nil)
	started := make(chan openclaw.ChatSendParams, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n == 1 {
			started <- p
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	p := recv(env.t, started, "the first turn")
	eventually(env.t, "the visit's loop", func() bool { return len(env.e.activeVisits(run.ID)) == 1 })
	return run, done, p
}

// TestCommandLifecycle: a command is received (pending, not acknowledged), acknowledged by
// the worker holding the run, then applied once its effect is confirmed; each change is a
// run.command event (DESIGN W2 rules 1, 2 and 4, Outputs).
func TestCommandLifecycle(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fc.abortEmits = false
	run, done, p := env.heldRun()
	got, err := env.e.Pause(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	c := got.Command
	if c == nil || c.Kind != store.CommandPause || c.Outcome != store.CommandPending || c.AckedAt != nil || c.AppliedAt != nil {
		t.Fatalf("received: %+v", c)
	}
	acked := env.awaitCommand(run.ID, c.ID, "the acknowledgement", func(c *store.RunCommand) bool { return c.AckedAt != nil })
	if acked.Outcome != store.CommandPending || acked.AppliedAt != nil || acked.AckedAt.Before(acked.ReceivedAt) {
		t.Fatalf("acknowledged: %+v", acked)
	}
	eventually(t, "the abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "aborted", map[string]any{"stopReason": "rpc"}))
	if r := env.wait(done, run.ID); r.Status != store.RunPaused {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	applied := env.commandRow(run.ID, c.ID)
	if applied.Outcome != store.CommandApplied || applied.AppliedAt == nil || applied.AppliedAt.Before(*applied.AckedAt) {
		t.Fatalf("applied: %+v", applied)
	}
	if n := env.commandEvents(run.ID, c.ID); n != 3 {
		t.Fatalf("run.command events = %d, want received, acknowledged, applied", n)
	}
	list, err := store.ListRunCommands(ctx, env.pool, run.ID)
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("commands = %+v %v", list, err)
	}
}

// TestCancelQueued (rule 10): a queued run is cancelled at once, the command acknowledged
// and applied in the same step, with nothing to confirm.
func TestCancelQueued(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	c := got.Command
	if got.Status != store.RunCancelled || got.CancelState != store.CancelConfirmed || deref(got.FailureReason) != "cancelled by andre" {
		t.Fatalf("run = %s %q %q", got.Status, got.CancelState, deref(got.FailureReason))
	}
	if c == nil || c.Outcome != store.CommandApplied || c.AckedAt == nil || c.AppliedAt == nil {
		t.Fatalf("command = %+v", c)
	}
	// A cancel of the finished run is rejected and recorded.
	again, err := env.e.Cancel(ctx, run.ID, "andre")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != CodeRunFinished || ce.Message != "the run already ended (cancelled)" || again == nil ||
		again.Command.Outcome != store.CommandRejected {
		t.Fatalf("second cancel = %+v %v", again, err)
	}
}

// TestCancelLiveConfirmedByEvent (rule 11): the worker acknowledges the cancel and aborts the
// turn; the gateway's "aborted" event confirms it, and the run ends cancelled, confirmed, with
// the command applied.
func TestCancelLiveConfirmedByEvent(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run, done, p := env.heldRun()
	got, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil || got.Command.Outcome != store.CommandPending {
		t.Fatalf("cancel = %+v %v", got, err)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled || r.CancelState != store.CancelConfirmed {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	c := env.commandRow(run.ID, got.Command.ID)
	if c.Outcome != store.CommandApplied || c.AckedAt == nil || c.AppliedAt == nil {
		t.Fatalf("command = %+v", c)
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), p.IdempotencyKey) {
		t.Fatalf("aborts = %s", aborts)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed`, run.ID); n != 0 {
		t.Fatalf("%d unconfirmed turns", n)
	}
}

// TestCancelLiveConfirmedBySession (rule 11): no "aborted" event arrives, and after abortWait
// sessions.list shows no active run for the session: confirmed.
func TestCancelLiveConfirmedBySession(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fc.abortEmits = false
	run, done, p := env.heldRun()
	env.sessionShows(p.SessionKey, p.IdempotencyKey, false)
	got, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
	env.clock.Advance(env.e.abortWait + time.Second)
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled || r.CancelState != store.CancelConfirmed {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	if c := env.commandRow(run.ID, got.Command.ID); c.Outcome != store.CommandApplied {
		t.Fatalf("command = %+v", c)
	}
	if len(env.fc.callsFor("sessions.list")) == 0 {
		t.Fatal("the session was not checked")
	}
}

// unconfirmedCancel cancels a live run whose turn the gateway keeps showing as running: the
// run ends cancelled with cancel_state unconfirmed and the command acknowledged, not applied.
func unconfirmedCancel(t *testing.T, env *testEnv) (*store.Run, openclaw.ChatSendParams, *store.RunCommand) {
	t.Helper()
	ctx := context.Background()
	env.fc.abortEmits = false
	run, done, p := env.heldRun()
	env.sessionShows(p.SessionKey, p.IdempotencyKey, true)
	got, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
	env.clock.Advance(env.e.abortWait + time.Second)
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled || r.CancelState != store.CancelUnconfirmed {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	c := env.commandRow(run.ID, got.Command.ID)
	if c.Outcome != store.CommandPending || c.AckedAt == nil || c.AppliedAt != nil {
		t.Fatalf("command = %+v, want acknowledged and not applied", c)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed`, run.ID); n != 1 {
		t.Fatalf("%d unconfirmed turns", n)
	}
	return r, p, c
}

// TestCancelUnconfirmedThenReconciled (rule 11): the reconcile pass repeats chat.abort and
// the session check while the gateway shows the turn; once it shows none the stop is
// confirmed and the command applied.
func TestCancelUnconfirmedThenReconciled(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run, p, c := unconfirmedCancel(t, env)
	aborts := len(env.fc.callsFor("chat.abort"))
	env.e.reconcileStops(ctx)
	if r := env.run(run.ID); r.CancelState != store.CancelUnconfirmed {
		t.Fatalf("still running on the gateway, yet cancel_state = %q", r.CancelState)
	}
	if n := len(env.fc.callsFor("chat.abort")); n != aborts+1 {
		t.Fatalf("aborts = %d, want one more", n)
	}
	// A gateway that is away confirms nothing.
	env.sessionShows(p.SessionKey, p.IdempotencyKey, false)
	env.fc.setConnected(false)
	env.e.reconcileStops(ctx)
	if r := env.run(run.ID); r.CancelState != store.CancelUnconfirmed {
		t.Fatalf("gateway away, yet cancel_state = %q", r.CancelState)
	}
	env.fc.setConnected(true)
	env.e.reconcileStops(ctx)
	r := env.run(run.ID)
	if r.CancelState != store.CancelConfirmed || r.Status != store.RunCancelled {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	if got := env.commandRow(run.ID, c.ID); got.Outcome != store.CommandApplied || got.AppliedAt == nil {
		t.Fatalf("command = %+v", got)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed`, run.ID); n != 0 {
		t.Fatalf("%d unconfirmed turns", n)
	}
}

// Failure handling: a gateway that is down during the cancel leaves the stop unconfirmed,
// never confirmed.
func TestCancelGatewayDownLeavesUnconfirmed(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run, done, _ := env.heldRun()
	env.fc.setConnected(false)
	got, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	env.awaitCommand(run.ID, got.Command.ID, "the acknowledgement", func(c *store.RunCommand) bool { return c.AckedAt != nil })
	env.clock.Advance(env.e.abortWait + time.Second)
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled || r.CancelState != store.CancelUnconfirmed {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	if c := env.commandRow(run.ID, got.Command.ID); c.Outcome != store.CommandPending {
		t.Fatalf("command = %+v", c)
	}
}

// Edge case: a cancel that loses the race with a finishing turn is rejected, "the run already
// ended (succeeded)"; so is every other command the run leaves pending.
func TestCancelRacingTheEnd(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run := env.endedRun(store.RunSucceeded, "", "")
	cancel := &store.RunCommand{RunID: run.ID, Kind: store.CommandCancel}
	steer := &store.RunCommand{RunID: run.ID, Kind: store.CommandSteer}
	for _, c := range []*store.RunCommand{cancel, steer} {
		if err := store.InsertRunCommand(ctx, env.pool, c); err != nil {
			t.Fatal(err)
		}
	}
	env.e.settleAtEnd(ctx, run)
	for _, c := range []*store.RunCommand{cancel, steer} {
		if got := env.commandRow(run.ID, c.ID); got.Outcome != store.CommandRejected || got.Detail != "the run already ended (succeeded)" {
			t.Fatalf("%s = %+v", c.Kind, got)
		}
	}
}

// TestRetryRefusedUnconfirmedCancel (rule 13): retry and resume answer cancel_unconfirmed,
// naming the session and the turn, until the reconcile pass confirms the stop.
func TestRetryRefusedUnconfirmedCancel(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run, p, _ := unconfirmedCancel(t, env)
	_, err := env.e.Retry(ctx, run.ID, "andre", nil)
	var re *RetryError
	if !errors.As(err, &re) || re.Code != CodeCancelUnconfirmed || !strings.Contains(re.Message, p.SessionKey) ||
		!strings.Contains(re.Message, p.IdempotencyKey) {
		t.Fatalf("retry = %v", err)
	}
	if r := env.run(run.ID); r.Status != store.RunCancelled || r.Retries != 0 {
		t.Fatalf("a refused retry changed the run: %s retries %d", r.Status, r.Retries)
	}
	rejected := env.count(`SELECT count(*) FROM run_commands WHERE run_id=$1 AND kind='retry' AND outcome='rejected'`, run.ID)
	if rejected != 1 {
		t.Fatalf("rejected retries = %d", rejected)
	}
	env.sessionShows(p.SessionKey, p.IdempotencyKey, false)
	env.e.reconcileStops(ctx)
	got, err := env.e.Retry(ctx, run.ID, "andre", nil)
	if err != nil || got.Status != store.RunQueued || got.Command == nil || got.Command.Outcome != store.CommandApplied {
		t.Fatalf("retry after the reconcile = %+v %v", got, err)
	}
}

// Rule 13: a resume is refused like a retry while the run's last turn is not confirmed
// stopped, naming the session and the turn.
func TestResumeRefusedUnconfirmedStop(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Pause(ctx, run.ID, "andre"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	key, turn := "agent:x:yuno-"+run.ID+"-agent-1", run.ID+"-turn"
	n := &store.RunNode{RunID: run.ID, NodeID: "agent", AgentID: &a.ID, Visit: 1, Status: store.NodeFailed, StartedAt: &now,
		FinishedAt: &now, SessionKey: &key, OpenClawRunID: &turn}
	if err := store.CreateRunNode(ctx, env.pool, n); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnUnconfirmed(ctx, env.pool, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRunCancelState(ctx, env.pool, run.ID, store.CancelUnconfirmed); err != nil {
		t.Fatal(err)
	}
	_, err = env.e.Resume(ctx, run.ID, "andre", "")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != CodeCancelUnconfirmed || !strings.Contains(ce.Message, key) || !strings.Contains(ce.Message, turn) {
		t.Fatalf("resume = %v", err)
	}
	if r := env.run(run.ID); r.Status != store.RunPaused {
		t.Fatalf("run = %s", r.Status)
	}
}

// TestSweeperLeavesUnconfirmed (rule 12): a run the stale sweeper re-queues or fails as
// "worker lost" gets cancel_state unconfirmed for the turn its lost worker left, and its
// lease epoch moves on (rule 7); the retry of a failed one waits for the reconcile pass.
func TestSweeperLeavesUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		attempt int
		status  string
	}{{"re-queued", 1, store.RunQueued}, {"failed as worker lost", 2, store.RunFailed}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			ctx := context.Background()
			a := env.agent(nil)
			started := make(chan openclaw.ChatSendParams, 1)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				started <- p
				return startedTurn(p)
			}
			run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.ClaimQueuedRun(ctx, env.pool)
			if err != nil {
				t.Fatal(err)
			}
			// The worker's process goes away with the turn running: abortEmits off, so its
			// abort on the way out is not confirmed either.
			env.fc.abortEmits = false
			wctx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.e.runRun(wctx, claimed)
			}()
			p := recv(t, started, "the turn")
			stop()
			<-done
			if _, err := env.pool.Exec(ctx, `UPDATE runs SET attempt=$2 WHERE id=$1`, run.ID, tc.attempt); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil {
				t.Fatal(err)
			}
			r := env.run(run.ID)
			if r.Status != tc.status || r.CancelState != store.CancelUnconfirmed || r.LeaseEpoch != claimed.LeaseEpoch+1 {
				t.Fatalf("swept run = %s %q epoch %d (claimed with %d)", r.Status, r.CancelState, r.LeaseEpoch, claimed.LeaseEpoch)
			}
			if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND turn_unconfirmed AND openclaw_run_id=$2`, run.ID, p.IdempotencyKey); n != 1 {
				t.Fatalf("unconfirmed turns = %d", n)
			}
			if tc.status != store.RunFailed {
				return
			}
			var re *RetryError
			if _, err := env.e.Retry(ctx, run.ID, "andre", nil); !errors.As(err, &re) || re.Code != CodeCancelUnconfirmed {
				t.Fatalf("retry before the reconcile = %v", err)
			}
			env.e.reconcileStops(ctx) // sessions.list does not show the session: no active run
			if got, err := env.e.Retry(ctx, run.ID, "andre", nil); err != nil || got.Status != store.RunQueued {
				t.Fatalf("retry after the reconcile = %+v %v", got, err)
			}
		})
	}
}

// TestLeaseFencing (rules 7 to 9): once the run is re-queued, a write with the old epoch is
// refused, and the old visit stops itself at its next heartbeat: it aborts its turn, logs
// that it lost its lease, and writes nothing more, so it does not end the run, which the
// next claim finishes.
func TestLeaseFencing(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.heartbeat = 20 * time.Millisecond
	a := env.agent(nil)
	started := make(chan openclaw.ChatSendParams, 2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n > 1 { // the next claim's visit: a session and a token of its own
			go finishThenFinal(t, env, p, `{"summary":"done"}`)
		}
		started <- p
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	old := store.NewLease(claimed)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	p := recv(t, started, "the first turn")
	lost := runNodes(t, env, run.ID)[0]
	if _, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil {
		t.Fatal(err)
	}
	// A write with the old epoch is refused: the lease check and the write are one statement.
	if _, err := store.TransitionRun(store.WithLease(ctx, &store.Lease{RunID: run.ID, Epoch: old.Epoch}), env.pool, run.ID,
		[]string{store.RunQueued}, store.RunFailed, nil, time.Now()); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("transition with the old lease = %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the old worker did not stop")
	}
	r := env.run(run.ID)
	if r.Status != store.RunQueued || r.FinishedAt != nil {
		t.Fatalf("the old worker ended the run: %s", r.Status)
	}
	if n := env.logged(run.ID, "worker lost its lease; stopped"); n != 1 {
		t.Fatalf("lease lines = %d", n)
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) == 0 || !strings.Contains(string(aborts[len(aborts)-1]), p.IdempotencyKey) {
		t.Fatalf("the old turn was not aborted: %s", aborts)
	}
	if n := runNodes(t, env, run.ID)[0]; n.Status != store.NodeRunning || n.FinishedAt != nil || n.ID != lost.ID {
		t.Fatalf("the old visit wrote its end: %s", n.Status)
	}
	// The next claim holds the run and finishes it.
	if got := env.resume(env.e, run.ID); got.Status != store.RunSucceeded {
		t.Fatalf("reclaimed run = %s %q", got.Status, deref(got.FailureReason))
	}
}

// TestBranchFencing (rule 8, Failure handling): branch rows are fenced through their run;
// once the run is re-queued each branch visit stops itself, nothing stops its siblings as a
// failure would, and the fork writes nothing.
func TestBranchFencing(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.heartbeat = 20 * time.Millisecond
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	h := newHolder(2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if node, _ := visitOf(p.SessionKey); node == "coder" {
			go finishThenFinal(t, env, p, `{"code":"done"}`)
		} else {
			h.add(p)
		}
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	recv(t, h.all, "both branch turns")
	before := branchRows(t, env, run.ID)
	if _, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil {
		t.Fatal(err)
	}
	oldCtx := store.WithLease(ctx, store.NewLease(claimed))
	if _, err := store.SetBranchStatus(oldCtx, env.pool, before["a"].ID, store.BranchFailed, nil, time.Now()); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("branch status with the old lease = %v", err)
	}
	if err := store.SetBranchCurrent(oldCtx, env.pool, before["a"].ID, "merge"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("branch position with the old lease = %v", err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the old worker did not stop")
	}
	after := branchRows(t, env, run.ID)
	for _, head := range []string{"a", "b"} {
		if after[head].Status != store.BranchRunning || after[head].FinishedAt != nil || after[head].CurrentNode != head {
			t.Fatalf("branch %s after the lost lease: %+v", head, after[head])
		}
	}
	if r := env.run(run.ID); r.Status != store.RunQueued {
		t.Fatalf("run = %s", r.Status)
	}
	if n := env.logged(run.ID, "failed"); n != 0 {
		t.Fatalf("a branch was taken for failed (%d lines)", n)
	}
	if n := env.logged(run.ID, "worker lost its lease; stopped"); n != 1 {
		t.Fatalf("lease lines = %d", n)
	}
	if n := len(env.fc.callsFor("chat.abort")); n < 2 {
		t.Fatalf("aborts = %d, want both branch turns", n)
	}
}

// TestPauseResume (rules 14 and 15): a live step is paused (turn stopped, checkout saved,
// visit parked, run paused, worker released); the resume re-queues it with a new lease, and
// the step continues in a new visit from the paused visit's checkout, whose task carries the
// notice and the note. The paused visit does not count against max_loop_visits.
func TestPauseResume(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	ctx := context.Background()
	a := env.agent(nil)
	def := line(workflow.Node{ID: "work", AgentID: a.ID, Task: "write"})
	def.Project = true
	def.Limits = store.Limits{MaxLoopVisits: 1, MaxDurationSeconds: 3600}
	wf := env.createWorkflow(def)
	var mu sync.Mutex
	seen := map[int]string{}
	messages := map[int]string{}
	held := make(chan openclaw.ChatSendParams, 1)
	env.fc.onSend = func(sp openclaw.ChatSendParams, n int) (any, error) {
		_, visit := visitOf(sp.SessionKey)
		repo := filepath.Join(turnWS(env, sp), "runs")
		mu.Lock()
		defer mu.Unlock()
		entries, _ := os.ReadDir(repo)
		readme := ""
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(repo, e.Name(), "repo", "README.md")); err == nil {
				readme = string(b)
				writeFile(t, filepath.Join(repo, e.Name(), "repo", "README.md"), "half done\n")
			}
		}
		seen[visit], messages[visit] = readme, sp.Message
		if visit == 1 {
			held <- sp
		} else {
			go finishThenFinal(t, env, sp, `{"ok":true}`)
		}
		return startedTurn(sp)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	recv(t, held, "the first turn")
	eventually(t, "the visit's loop", func() bool { return len(env.e.activeVisits(run.ID)) == 1 })
	got, err := env.e.Pause(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunPaused || r.PausedAt == nil {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if c := env.commandRow(run.ID, got.Command.ID); c.Outcome != store.CommandApplied {
		t.Fatalf("pause = %+v", c)
	}
	paused := runNodes(t, env, run.ID)[0]
	if paused.Status != store.NodeWaitingApproval || paused.ParkedAt == nil || paused.ParkReason != store.ParkPaused ||
		deref(paused.RepoSHA) == "" || deref(paused.RepoSHA) == deref(paused.StartSHA) {
		t.Fatalf("paused visit = %s parked %v reason %q at %q from %q", paused.Status, paused.ParkedAt, paused.ParkReason,
			deref(paused.RepoSHA), deref(paused.StartSHA))
	}
	epoch := env.leaseEpoch(run.ID)
	resumed, err := env.e.Resume(ctx, run.ID, "andre", "  keep the README short  ")
	if err != nil || resumed.Status != store.RunQueued || resumed.Command.Outcome != store.CommandApplied || resumed.LeaseEpoch != epoch+1 {
		t.Fatalf("resume = %+v %v", resumed, err)
	}
	if again, err := env.e.Resume(ctx, run.ID, "andre", ""); err == nil || again.Command.Detail != "the run is not paused" {
		t.Fatalf("second resume = %+v %v", again, err)
	}
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("resumed run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[2] != "half done\n" {
		t.Fatalf("the continuation's checkout held %q", seen[2])
	}
	want := resumedNotice + "\nThe operator's note: keep the README short"
	if !strings.Contains(messages[2], want) || strings.Contains(messages[1], resumedNotice) {
		t.Fatalf("continuation task = %q", messages[2])
	}
	closed := runNodes(t, env, run.ID)[0]
	if closed.Status != store.NodeSucceeded || closed.ResumedAt == nil || deref(closed.ResumeNote) != "keep the README short" {
		t.Fatalf("the paused visit after the resume: %s %v %q", closed.Status, closed.ResumedAt, deref(closed.ResumeNote))
	}

	// A queued run pauses at once and resumes to queued.
	queued, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := env.e.Pause(ctx, queued.ID, "andre"); err != nil || got.Status != store.RunPaused || got.Command.Outcome != store.CommandApplied {
		t.Fatalf("pause of a queued run = %+v %v", got, err)
	}
	if _, err := store.ClaimQueuedRun(ctx, env.pool); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a paused run was claimed: %v", err)
	}
	if got, err := env.e.Resume(ctx, queued.ID, "andre", ""); err != nil || got.Status != store.RunQueued {
		t.Fatalf("resume of a run paused before it started = %+v %v", got, err)
	}
}

// TestPauseOnWaitingRejected (rule 14): a run waiting on a decision is not paused; the
// command is rejected and says what the run waits on.
func TestPauseOnWaitingRejected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	wf := gated(t, env, false, store.Limits{})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	ap := pendingApproval(t, env, r.ID)
	got, err := env.e.Pause(ctx, r.ID, "andre")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != CodeCommandRejected || ce.Message != "the run is waiting on approval "+ap.ID {
		t.Fatalf("pause = %v", err)
	}
	if got.Command.Outcome != store.CommandRejected || got.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s, command %+v", got.Status, got.Command)
	}
}

// TestPauseUnconfirmedFails (rule 14): a turn not confirmed stopped within abortWait fails
// the pause, "could not confirm the turn stopped", and the step goes on to its result.
func TestPauseUnconfirmedFails(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fc.abortEmits = false
	run, done, p := env.heldRun()
	env.sessionShows(p.SessionKey, p.IdempotencyKey, true)
	got, err := env.e.Pause(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
	env.clock.Advance(env.e.abortWait + time.Second)
	c := env.awaitCommand(run.ID, got.Command.ID, "the failed pause", func(c *store.RunCommand) bool { return c.Outcome != store.CommandPending })
	if c.Outcome != store.CommandFailed || c.Detail != "could not confirm the turn stopped" {
		t.Fatalf("pause = %+v", c)
	}
	if r := env.run(run.ID); r.Status != store.RunRunning {
		t.Fatalf("run = %s", r.Status)
	}
	finishThenFinal(t, env, p, `{"summary":"done"}`)
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// retryFromFlow is a project run one (records its checkout's README, then edits it) → two
// (fails its first visit) → end.
type retryFromFlow struct {
	env  *testEnv
	run  *store.Run
	mu   sync.Mutex
	seen map[string]string // "<node>#<visit>" → README.md when its turn started
	msgs map[string]string
}

func newRetryFromFlow(t *testing.T) *retryFromFlow {
	t.Helper()
	env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
	f := &retryFromFlow{env: env, seen: map[string]string{}, msgs: map[string]string{}}
	coder, reviewer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: reviewer.ID, Task: "review"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.run = run
	env.fc.onSend = func(sp openclaw.ChatSendParams, n int) (any, error) {
		node, visit := visitOf(sp.SessionKey)
		readme := filepath.Join(turnWS(env, sp), "runs", run.ID, "repo", "README.md")
		b, _ := os.ReadFile(readme)
		key := node + "#" + string(rune('0'+visit))
		f.mu.Lock()
		f.seen[key], f.msgs[key] = string(b), sp.Message
		f.mu.Unlock()
		switch {
		case node == "one":
			writeFile(t, readme, "edited by one, visit "+string(rune('0'+visit))+"\n")
			go finishThenFinal(t, env, sp, `{"ok":true}`)
		case visit == 1:
			go env.fc.push("chat", chatEvent(sp.SessionKey, sp.IdempotencyKey, "error", map[string]any{
				"errorKind": "invalid_request", "errorMessage": "messages: bad shape",
				"errorDetail": map[string]any{"provider": "scripted", "model": "scripted-1", "httpStatus": 400}}))
		default:
			go finishThenFinal(t, env, sp, `{"ok":true}`)
		}
		return startedTurn(sp)
	}
	return f
}

func (f *retryFromFlow) found(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[key]
}

// TestRetryFromStep (rules 16 and 17): a retry from an earlier step moves the run there,
// marks the latest visit of it and of every node after it retried, and starts the step from
// the commit its latest visit started from (a fresh attempt); the later steps run again.
func TestRetryFromStep(t *testing.T) {
	f := newRetryFromFlow(t)
	env := f.env
	ctx := context.Background()
	failed := env.resume(env.e, f.run.ID)
	if failed.Status != store.RunFailed || deref(failed.CurrentNode) != "two" {
		t.Fatalf("first attempt = %s at %q", failed.Status, deref(failed.CurrentNode))
	}
	if f.found("one#1") != "# demo\n" || f.found("two#1") != "edited by one, visit 1\n" {
		t.Fatalf("first attempt saw %q and %q", f.found("one#1"), f.found("two#1"))
	}
	env.clock.Advance(time.Hour)
	got, err := env.e.RetryFrom(ctx, f.run.ID, "andre", "one", nil)
	if err != nil || got.Status != store.RunQueued || deref(got.CurrentNode) != "one" || got.Command.Outcome != store.CommandApplied ||
		deref(got.Command.NodeID) != "one" {
		t.Fatalf("retry from one = %+v %v", got, err)
	}
	marked := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND retried_at IS NOT NULL`, f.run.ID)
	rewound := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND node_id='one' AND retry_from_at IS NOT NULL`, f.run.ID)
	if marked != 2 || rewound != 1 {
		t.Fatalf("marked %d visits retried, %d rewound", marked, rewound)
	}
	done := env.resume(env.e, f.run.ID)
	if done.Status != store.RunSucceeded {
		t.Fatalf("retried run = %s %q", done.Status, deref(done.FailureReason))
	}
	// one starts again from the clone, not from its own earlier edit; two runs again after it.
	if f.found("one#2") != "# demo\n" || f.found("two#2") != "edited by one, visit 2\n" {
		t.Fatalf("after the retry one saw %q and two %q", f.found("one#2"), f.found("two#2"))
	}
	nodes := runNodes(t, env, f.run.ID)
	one1, one2 := nodeByID(nodes, "one", 1), nodeByID(nodes, "one", 2)
	if one2 == nil || deref(one2.StartSHA) == "" || deref(one2.StartSHA) != deref(one1.StartSHA) {
		t.Fatalf("one#2 started at %q, one#1 at %q", deref(one2.StartSHA), deref(one1.StartSHA))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.msgs["one#2"], retriedNotice) || !strings.Contains(f.msgs["two#2"], retriedNotice) {
		t.Fatal("the retried steps were not told they were retried")
	}
}

// TestRetryFromStepRejected (rule 16): from_node must be a step the run reached, outside a
// fork branch; nothing changes and the command is rejected.
func TestRetryFromStepRejected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	run := env.endedRun(store.RunFailed, "two", "")
	env.visit(run.ID, "one", 1, store.NodeSucceeded)
	now := time.Now()
	fork := &store.RunNode{RunID: run.ID, NodeID: "panel", Visit: 1, Status: store.NodeFailed, StartedAt: &now}
	branch := &store.RunBranch{RunID: run.ID, ForkID: "panel", Head: "a", Status: store.BranchFailed, CurrentNode: "a", StartedAt: now}
	if err := store.CreateForkVisit(ctx, env.pool, fork, []*store.RunBranch{branch}); err != nil {
		t.Fatal(err)
	}
	inBranch := &store.RunNode{RunID: run.ID, NodeID: "a", Visit: 1, Status: store.NodeFailed, StartedAt: &now, BranchID: &branch.ID}
	if err := store.CreateRunNode(ctx, env.pool, inBranch); err != nil {
		t.Fatal(err)
	}
	for from, msg := range map[string]string{"nope": "from_node must be a step this run reached", "a": "retry the fork step instead"} {
		_, err := env.e.RetryFrom(ctx, run.ID, "andre", from, nil)
		var re *RetryError
		if !errors.As(err, &re) || re.Code != RetryNotRetryable || re.Message != msg {
			t.Fatalf("retry from %s = %v", from, err)
		}
	}
	if r := env.run(run.ID); r.Status != store.RunFailed || deref(r.CurrentNode) != "two" || r.Retries != 0 {
		t.Fatalf("a refused retry changed the run: %s at %q", r.Status, deref(r.CurrentNode))
	}
	if n := env.count(`SELECT count(*) FROM run_commands WHERE run_id=$1 AND outcome='rejected'`, run.ID); n != 2 {
		t.Fatalf("rejected commands = %d", n)
	}
}

// steerEntry is the session.message of a user transcript entry, as the gateway records a
// message sent with an idempotency key.
func steerEntry(key, idem, text string) map[string]any {
	id := nextMsgID()
	return map[string]any{"sessionKey": key, "messageId": id, "message": map[string]any{
		"role": "user", "content": text, "timestamp": time.Now().UnixMilli(), "idempotencyKey": idem + ":user",
		"__openclaw": map[string]any{"id": id, "idempotencyKey": idem + ":user"}}}
}

// TestSteerLive (rule 18): the worker sends "Operator: <message>" with queueMode steer and the
// key <visit id>:steer:<command id>; the turn's id stays; the command is applied when the
// transcript shows the message.
func TestSteerLive(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	var mu sync.Mutex
	var first openclaw.ChatSendParams
	started := make(chan openclaw.ChatSendParams, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if p.QueueMode == openclaw.QueueSteer {
			mu.Lock()
			turn := first.IdempotencyKey
			mu.Unlock()
			go env.fc.push("session.message", steerEntry(p.SessionKey, p.IdempotencyKey, p.Message))
			return openclaw.ChatSendResult{RunID: turn, Status: "started"}, nil
		}
		mu.Lock()
		first = p
		mu.Unlock()
		started <- p
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	p := recv(t, started, "the turn")
	eventually(t, "the visit's loop", func() bool { return len(env.e.activeVisits(run.ID)) == 1 })
	got, err := env.e.Steer(ctx, run.ID, "andre", "", "  Look at README.md first.  ")
	if err != nil || got.Command.Outcome != store.CommandPending {
		t.Fatalf("steer = %+v %v", got, err)
	}
	c := env.awaitCommand(run.ID, got.Command.ID, "the steer applied", func(c *store.RunCommand) bool { return c.Outcome == store.CommandApplied })
	if c.AckedAt == nil {
		t.Fatalf("steer = %+v", c)
	}
	var steered []openclaw.ChatSendParams
	for _, s := range env.fc.sendParams() {
		if s.QueueMode == openclaw.QueueSteer {
			steered = append(steered, s)
		}
	}
	visit := runNodes(t, env, run.ID)[0]
	if len(steered) != 1 || steered[0].Message != "Operator: Look at README.md first." || steered[0].SessionKey != p.SessionKey ||
		steered[0].IdempotencyKey != visit.ID+":steer:"+c.ID {
		t.Fatalf("steered sends = %+v", steered)
	}
	if deref(visit.OpenClawRunID) != p.IdempotencyKey {
		t.Fatalf("the turn id changed to %q", deref(visit.OpenClawRunID))
	}
	finishThenFinal(t, env, p, `{"summary":"done"}`)
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// TestSteerBetweenTurns (rule 18): a turn that ends before it took the message (the steered
// send never showed in its transcript) is followed by a turn that opens with the message; the
// command is applied when that turn starts.
func TestSteerBetweenTurns(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	var mu sync.Mutex
	var first openclaw.ChatSendParams
	started := make(chan openclaw.ChatSendParams, 4)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if p.QueueMode == openclaw.QueueSteer {
			mu.Lock()
			turn := first.IdempotencyKey
			mu.Unlock()
			return openclaw.ChatSendResult{RunID: turn, Status: "started"}, nil // queued, never taken
		}
		mu.Lock()
		if first.IdempotencyKey == "" {
			first = p
		} else {
			msg := p.Message
			p.Message = first.Message
			mu.Unlock()
			started <- openclaw.ChatSendParams{Message: msg}
			go finishThenFinal(t, env, p, `{"summary":"done"}`)
			return startedTurn(p)
		}
		mu.Unlock()
		started <- p
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	p := recv(t, started, "the first turn")
	eventually(t, "the visit's loop", func() bool { return len(env.e.activeVisits(run.ID)) == 1 })
	got, err := env.e.Steer(ctx, run.ID, "andre", "", "Stop and summarise.")
	if err != nil {
		t.Fatal(err)
	}
	env.awaitCommand(run.ID, got.Command.ID, "the steer taken", func(c *store.RunCommand) bool { return c.AckedAt != nil })
	eventually(t, "the steered send", func() bool { return len(env.fc.sendParams()) == 2 })
	if c := env.commandRow(run.ID, got.Command.ID); c.Outcome != store.CommandPending {
		t.Fatalf("applied before the turn took it: %+v", c)
	}
	// The turn ends without taking the message, and without a result.
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart("Thinking.")}, "stop", 10, 1, 0))
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
		"message": map[string]any{"role": "assistant", "content": []any{textPart("Thinking.")}}}))
	next := recv(t, started, "the next turn")
	if !strings.HasPrefix(next.Message, continueMessage) || !strings.HasSuffix(next.Message, "\n\nOperator: Stop and summarise.") {
		t.Fatalf("next turn = %q", next.Message)
	}
	env.awaitCommand(run.ID, got.Command.ID, "the steer applied with the turn", func(c *store.RunCommand) bool { return c.Outcome == store.CommandApplied })
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// TestSteerRejected (rule 18): no live visit, several live visits and no step named, a step
// that is not running, and a message out of bounds.
func TestSteerRejected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	queued, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var ce *CommandError
	if _, err := env.e.Steer(ctx, queued.ID, "andre", "", "hello"); !errors.As(err, &ce) || ce.Message != nothingRunning {
		t.Fatalf("steer of a queued run = %v", err)
	}
	var ve *ValidationError
	for _, msg := range []string{"", "   ", strings.Repeat("é", MaxSteerChars+1)} {
		if _, err := env.e.Steer(ctx, queued.ID, "andre", "", msg); !errors.As(err, &ve) {
			t.Fatalf("steer %d characters = %v", len(msg), err)
		}
	}
	if n := env.count(`SELECT count(*) FROM run_commands WHERE run_id=$1`, queued.ID); n != 1 {
		t.Fatalf("commands recorded = %d; a refused body is not a command", n)
	}
	if _, err := env.e.Cancel(ctx, queued.ID, "andre"); err != nil {
		t.Fatal(err)
	}

	// Two live branches: the step must be named, and be one of them.
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	h := newHolder(2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if node, _ := visitOf(p.SessionKey); node == "coder" {
			go finishThenFinal(t, env, p, `{"code":"done"}`)
		} else if p.QueueMode == "" {
			h.add(p)
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{}`)})
	recv(t, h.all, "both branch turns")
	eventually(t, "both branch loops", func() bool { return len(env.e.activeVisits(run.ID)) == 2 })
	if _, err := env.e.Steer(ctx, run.ID, "andre", "", "hello"); !errors.As(err, &ce) || ce.Message != "name the step: a, b" {
		t.Fatalf("steer of two live steps = %v", err)
	}
	if _, err := env.e.Steer(ctx, run.ID, "andre", "lead", "hello"); !errors.As(err, &ce) || ce.Message != "lead is not running; running: a, b" {
		t.Fatalf("steer of a step that is not running = %v", err)
	}
	if got, err := env.e.Steer(ctx, run.ID, "andre", "a", "hello"); err != nil || got.Command.Outcome != store.CommandPending {
		t.Fatalf("steer of a named step = %+v %v", got, err)
	}
	if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
		t.Fatal(err)
	}
	if r := env.wait(done, run.ID); r.Status != store.RunCancelled {
		t.Fatalf("run = %s", r.Status)
	}
}

// TestCancelSupersedes (rule 6, edge cases): a cancel supersedes the pause still pending
// before it; a pause or a resume after it is rejected, the cancel winning.
func TestCancelSupersedes(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Claimed, and held by a worker that has not reached its step yet.
	if _, err := store.ClaimQueuedRun(ctx, env.pool); err != nil {
		t.Fatal(err)
	}
	pause, err := env.e.Pause(ctx, run.ID, "andre")
	if err != nil || pause.Command.Outcome != store.CommandPending {
		t.Fatalf("pause = %+v %v", pause, err)
	}
	cancel, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil || cancel.Command.Outcome != store.CommandPending {
		t.Fatalf("cancel = %+v %v", cancel, err)
	}
	superseded := env.commandRow(run.ID, pause.Command.ID)
	if superseded.Outcome != store.CommandSuperseded || superseded.Detail != "superseded by cancel "+cancel.Command.ID || superseded.AckedAt != nil {
		t.Fatalf("pause after the cancel = %+v", superseded)
	}
	var ce *CommandError
	if _, err := env.e.Pause(ctx, run.ID, "andre"); !errors.As(err, &ce) || ce.Message != "the run is being cancelled" {
		t.Fatalf("pause after a cancel = %v", err)
	}
	if _, err := env.e.Resume(ctx, run.ID, "andre", ""); !errors.As(err, &ce) || ce.Message != "the run is not paused" {
		t.Fatalf("resume after a cancel = %v", err)
	}
	// The worker reaches the step: the cancel ends the run before the step starts, and is
	// applied with it.
	claimed := env.run(run.ID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	if r := env.wait(done, run.ID); r.Status != store.RunCancelled || r.CancelState != store.CancelConfirmed {
		t.Fatalf("run = %s %q", r.Status, r.CancelState)
	}
	if c := env.commandRow(run.ID, cancel.Command.ID); c.Outcome != store.CommandApplied || c.AckedAt == nil {
		t.Fatalf("cancel = %+v", c)
	}
}

// TestSetupHeartbeat: a visit's setup (here a materialisation the gateway does not answer)
// writes the visit's heartbeat and the run's updated_at on the heartbeat interval, so a long
// setup does not read as a silent worker; the lease fences it like the loop's heartbeat.
func TestSetupHeartbeat(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.heartbeat = 20 * time.Millisecond
	env.e.setupTimeout = 2 * time.Second
	a := env.agent(nil)
	env.fc.mu.Lock()
	env.fc.block["agents.list"] = true
	env.fc.mu.Unlock()
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	eventually(t, "the setup waiting on the gateway", func() bool { return len(env.fc.callsFor("agents.list")) > 0 })
	var before time.Time
	if err := env.pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id=$1`, run.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	later := env.clock.Now().Add(time.Minute)
	env.clock.Advance(time.Minute)
	eventually(t, "a heartbeat during setup", func() bool {
		r := env.evidence(run.ID)
		return r.waitingOn == store.WaitingOnSetup && sameInstant(r.heartbeat, later)
	})
	var after time.Time
	if err := env.pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id=$1`, run.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.After(before) {
		t.Fatalf("updated_at did not move: %v then %v", before, after)
	}
	if len(env.fc.sendParams()) != 0 {
		t.Fatal("a turn started during the blocked setup")
	}
	env.fc.mu.Lock()
	delete(env.fc.block, "agents.list")
	env.fc.mu.Unlock()
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "agent materialisation failed") {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// TestStalledStepRecovery is the acceptance test of DESIGN W2: the gateway hangs a turn; the
// diagnosis says the step is stalled, with the evidence; a cancel is acknowledged, the turn
// aborted and the command applied; the retry starts a new lease, and the hung visit's late
// write is refused.
func TestStalledStepRecovery(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.heartbeat = 20 * time.Millisecond
	// The engine's own restart of a stalled turn is off: the operator recovers the step.
	env.e.modelStall = 0
	a := env.agent(nil)
	started := make(chan openclaw.ChatSendParams, 2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if n > 1 { // the retried visit: a session and a token of its own
			go finishThenFinal(t, env, p, `{"summary":"done"}`)
		}
		started <- p
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "x", Limits: hourLimit})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	hung := recv(t, started, "the hung turn")
	eventually(t, "waiting on the model", func() bool { return env.evidence(run.ID).waitingOn == store.WaitingOnModel })
	t0 := env.clock.Now()
	env.clock.Advance(11 * time.Minute)
	eventually(t, "a fresh heartbeat", func() bool { return sameInstant(env.evidence(run.ID).heartbeat, t0.Add(11*time.Minute)) })

	// The diagnosis, as GET /api/runs/{id}/diagnosis classifies it with MODEL_STALL_AFTER 600 s.
	live, err := store.ListLiveNodes(ctx, env.pool, []string{run.ID})
	if err != nil || len(live) != 1 {
		t.Fatalf("live nodes = %+v %v", live, err)
	}
	n := live[0]
	var calls []liveness.OpenCall
	_ = json.Unmarshal(n.OpenCalls, &calls)
	th := liveness.NewThresholds(10*time.Minute, 5*time.Minute)
	v := liveness.ClassifyNode(liveness.Node{NodeID: n.NodeID, HeartbeatAt: n.HeartbeatAt, ProgressAt: n.ProgressAt,
		WaitingOn: n.WaitingOn, ModelWaitSince: n.ModelWaitSince, OpenCalls: calls}, th, env.clock.Now())
	if v.Liveness != liveness.Stalled || v.Reason != "waiting on the model 11m0s, past the 10m0s restart" ||
		v.ModelWaitAgeS == nil || *v.ModelWaitAgeS != 660 || v.HeartbeatAgeS == nil || *v.HeartbeatAgeS > 1 {
		t.Fatalf("diagnosis = %+v", v)
	}
	runLive, reason := liveness.ClassifyRun(liveness.Run{Status: store.RunRunning, Nodes: []liveness.Verdict{v}}, th, env.clock.Now())
	if runLive != liveness.Stalled || !strings.HasPrefix(reason, n.NodeID+": ") {
		t.Fatalf("run diagnosis = %s %q", runLive, reason)
	}

	// The cancel: acknowledged, the turn aborted (the gateway confirms), applied.
	cancel, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil || cancel.Command.Outcome != store.CommandPending {
		t.Fatalf("cancel = %+v %v", cancel, err)
	}
	cancelled := env.wait(done, run.ID)
	if cancelled.Status != store.RunCancelled || cancelled.CancelState != store.CancelConfirmed {
		t.Fatalf("run = %s %q", cancelled.Status, cancelled.CancelState)
	}
	c := env.commandRow(run.ID, cancel.Command.ID)
	if c.Outcome != store.CommandApplied || c.AckedAt == nil || c.AppliedAt == nil {
		t.Fatalf("cancel command = %+v", c)
	}
	aborts := env.fc.callsFor("chat.abort")
	if len(aborts) != 1 || !strings.Contains(string(aborts[0]), hung.IdempotencyKey) {
		t.Fatalf("aborts = %s", aborts)
	}

	// The retry: a new lease, then the claim's.
	retried, err := env.e.Retry(ctx, run.ID, "andre", nil)
	if err != nil || retried.LeaseEpoch <= claimed.LeaseEpoch {
		t.Fatalf("retry = %+v %v (claimed with epoch %d)", retried, err, claimed.LeaseEpoch)
	}
	// The hung visit's late writes are refused.
	old := store.WithLease(ctx, store.NewLease(claimed))
	hungVisit := runNodes(t, env, run.ID)[0]
	if err := store.TouchRunNode(old, env.pool, run.ID, hungVisit.ID, env.clock.Now().Add(time.Hour)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("the hung visit's heartbeat = %v", err)
	}
	if _, err := store.TransitionRun(old, env.pool, run.ID, []string{store.RunQueued}, store.RunFailed, nil, env.clock.Now()); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("the hung visit's transition = %v", err)
	}
	if r := env.run(run.ID); r.Status != store.RunQueued {
		t.Fatalf("run after the refused writes = %s", r.Status)
	}
	if got := env.resume(env.e, run.ID); got.Status != store.RunSucceeded {
		t.Fatalf("the retried run = %s %q", got.Status, deref(got.FailureReason))
	}
}
