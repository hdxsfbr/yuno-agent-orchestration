package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Recovery (DESIGN W2): the operator or the Chief stops, holds, restarts and redirects a
// run through commands. Each request is a run_commands row from receipt to its outcome
// (rules 1 to 6): the api records it, then decides it under the run's row lock. A run no
// worker holds (queued, paused, parked, or ended) has it applied or rejected at once; a run
// a worker holds has it left pending, the worker woken, and the worker acknowledges it and
// carries it out (node.go, visit.commands). The worker's own writes are fenced by its lease
// (store.Lease, rules 7 to 9).

// Error codes of a command the run's state refuses (409).
const (
	CodeRunFinished       = "run_finished"
	CodeCommandRejected   = "command_rejected"
	CodeCancelUnconfirmed = "cancel_unconfirmed"
)

const (
	// MaxSteerChars and MaxResumeNoteChars cap a steer's message (rule 18) and a resume's
	// note (rule 15), in characters.
	MaxSteerChars      = 4000
	MaxResumeNoteChars = 2000
	// resumedNotice follows the task of a step the operator paused and resumed (rule 15).
	resumedNotice = "The operator paused this step and resumed it."
	// steerPrefix starts an operator's message in the turn (rule 18).
	steerPrefix = "Operator: "
	// commandReadEvery is how often a live visit reads its run's pending commands without
	// being woken (rule 5).
	commandReadEvery = 2 * time.Second
	// reconcileBatch caps the runs one reconcile pass checks (rule 11).
	reconcileBatch = 100
)

// ErrCommandNotTaken is the answer when a command's receipt could not be written: the
// request is not taken (Failure handling, 503).
var ErrCommandNotTaken = errors.New("the command could not be recorded; nothing was done")

// CommandError is a command the run's state refuses (rule 4: rejected): Code is the API's
// error code, Message the command's detail, Command the recorded row.
type CommandError struct {
	Code    string
	Message string
	Command *store.RunCommand
}

func (e *CommandError) Error() string { return e.Message }

// verdict is how a command was decided under the run's lock: applied, rejected (with the
// code and detail) or pending for the worker that holds the run. run is the run after it;
// after runs once the transaction committed (the housekeeping and the events).
type verdict struct {
	outcome string
	code    string
	detail  string
	run     *store.Run
	after   func(context.Context)
}

func reject(code, detail string) verdict {
	return verdict{outcome: store.CommandRejected, code: code, detail: detail}
}

// endedDetail is a command's detail for a run that ended before the command applied.
func endedDetail(status string) string { return "the run already ended (" + status + ")" }

const nothingRunning = "nothing is running; retry or resume with a note"

// commandActor is who sent a command: the request's actor, or the operator named by.
func commandActor(ctx context.Context, by string) json.RawMessage {
	if a, ok := actor.From(ctx); ok {
		if b, err := json.Marshal(a); err == nil {
			return b
		}
	}
	b, _ := json.Marshal(map[string]string{"kind": string(actor.Operator), "name": by})
	return b
}

// newCommand is a command as ctx's request sends it: its actor and request id, the step it
// names and its own fields.
func newCommand(ctx context.Context, runID, kind, by, nodeID string, payload map[string]any) *store.RunCommand {
	raw := json.RawMessage(`{}`)
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	c := &store.RunCommand{RunID: runID, Kind: kind, Payload: raw, Actor: commandActor(ctx, by), RequestID: obs.RequestID(ctx)}
	if nodeID != "" {
		c.NodeID = &nodeID
	}
	return c
}

// record writes a command's receipt on its own (rule 2), for a retry, whose checks run in a
// transaction of their own. An unknown run wraps store.ErrNotFound; a failed write is
// ErrCommandNotTaken.
func (e *Engine) record(ctx context.Context, runID, kind, by string, nodeID string, payload map[string]any) (*store.RunCommand, error) {
	if _, err := store.GetRun(ctx, e.deps.Pool, runID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("run %s: %w", runID, err)
		}
		e.log.Warn("command not taken: run unreadable", "run_id", runID, "kind", kind, "error", err)
		return nil, ErrCommandNotTaken
	}
	c := newCommand(ctx, runID, kind, by, nodeID, payload)
	if err := store.InsertRunCommand(ctx, e.deps.Pool, c); err != nil {
		e.log.Warn("command not taken", "run_id", runID, "kind", kind, "error", err)
		return nil, ErrCommandNotTaken
	}
	e.emitCommand(ctx, c)
	return c, nil
}

// receive takes a command from receipt to the answer of rule 5, in one transaction that
// holds the run's row (serialised with a claim and with the worker's own reading of its
// commands, Consistency): the receipt, the decision, and the outcome when it is the api's
// to give; then the housekeeping. A rejection answers the run and a *CommandError. A
// transaction that fails leaves no receipt: the request is not taken (ErrCommandNotTaken).
func (e *Engine) receive(ctx context.Context, runID, kind, by, nodeID string, payload map[string]any,
	decide func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error)) (*store.Run, error) {
	c := newCommand(ctx, runID, kind, by, nodeID, payload)
	received := false
	var v verdict
	err := pgx.BeginFunc(ctx, e.deps.Pool, func(tx pgx.Tx) error {
		run, err := store.GetRunForUpdate(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := store.InsertRunCommand(ctx, tx, c); err != nil {
			return err
		}
		received = true
		return e.decideIn(ctx, tx, run, c, decide, &v)
	})
	if errors.Is(err, store.ErrNotFound) && !received {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	if err != nil {
		e.log.Warn("command not taken", "run_id", runID, "kind", kind, "error", err)
		return nil, ErrCommandNotTaken
	}
	wctx := context.WithoutCancel(ctx)
	e.emit(wctx, runID, "run.command", map[string]any{"command": store.RunCommand{ID: c.ID, RunID: c.RunID, Kind: c.Kind,
		NodeID: c.NodeID, Payload: c.Payload, Actor: c.Actor, RequestID: c.RequestID, ReceivedAt: c.ReceivedAt, Outcome: store.CommandPending}})
	return e.answer(wctx, c, v)
}

// process decides, under the run's lock, a command that was recorded earlier and is still
// pending (settleUnheld).
func (e *Engine) process(ctx context.Context, c *store.RunCommand,
	decide func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error)) (*store.Run, error) {
	var v verdict
	err := pgx.BeginFunc(ctx, e.deps.Pool, func(tx pgx.Tx) error {
		run, err := store.GetRunForUpdate(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		return e.decideIn(ctx, tx, run, c, decide, &v)
	})
	if err != nil {
		e.settleCommand(ctx, c, store.CommandFailed, obs.Redact(err.Error()))
		return nil, err
	}
	return e.answer(context.WithoutCancel(ctx), c, v)
}

// decideIn runs decide in the transaction and settles the command when the outcome is the
// api's to give.
func (e *Engine) decideIn(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand,
	decide func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error), v *verdict) error {
	var err error
	if *v, err = decide(ctx, tx, run, c); err != nil {
		return err
	}
	if v.run == nil {
		v.run = run
	}
	if v.outcome == store.CommandApplied || v.outcome == store.CommandRejected {
		settled, err := store.SettleRunCommand(ctx, tx, c.ID, v.outcome, v.detail)
		if err != nil {
			return err
		}
		*c = *settled
	}
	return nil
}

// answer publishes a decided command, runs its housekeeping and returns the run with it.
func (e *Engine) answer(ctx context.Context, c *store.RunCommand, v verdict) (*store.Run, error) {
	if c.Outcome != store.CommandPending {
		e.emitCommand(ctx, c)
	}
	if v.after != nil {
		v.after(ctx)
	}
	run := v.run
	if fresh, err := store.GetRun(ctx, e.deps.Pool, c.RunID); err == nil {
		run = fresh
	}
	run.Command = c
	if c.Outcome == store.CommandRejected {
		return run, &CommandError{Code: v.code, Message: c.Detail, Command: c}
	}
	return run, nil
}

// emitCommand publishes a command's state (Outputs: run.command on every change).
func (e *Engine) emitCommand(ctx context.Context, c *store.RunCommand) {
	e.emit(ctx, c.RunID, "run.command", map[string]any{"command": c})
}

// settleCommand gives a pending command its outcome and publishes it; one settled
// meanwhile is left as it is. It returns the settled row, nil when it was not pending.
func (e *Engine) settleCommand(ctx context.Context, c *store.RunCommand, outcome, detail string) *store.RunCommand {
	if c == nil {
		return nil
	}
	settled, err := store.SettleRunCommand(context.WithoutCancel(ctx), e.deps.Pool, c.ID, outcome, detail)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.log.Warn("settle command failed", "run_id", c.RunID, "command_id", c.ID, "error", err)
		}
		return nil
	}
	e.emitCommand(ctx, settled)
	return settled
}

// ackCommand records that this worker took a command (rule 4) and publishes it; nil when
// it was taken or settled meanwhile, or the lease is gone.
func (e *Engine) ackCommand(ctx context.Context, c *store.RunCommand) *store.RunCommand {
	acked, err := store.AckRunCommand(context.WithoutCancel(ctx), e.deps.Pool, c.RunID, c.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrLeaseLost) {
			e.log.Warn("acknowledge command failed", "run_id", c.RunID, "command_id", c.ID, "error", err)
		}
		return nil
	}
	e.emitCommand(ctx, acked)
	return acked
}

// held reports whether a worker of this process holds the run: it is running, or waits on
// an approval with a live visit (an exec approval, a gated tool's call) whose turn goes on.
func (e *Engine) held(run *store.Run) bool {
	switch run.Status {
	case store.RunRunning:
		return true
	case store.RunWaitingApproval:
		return len(e.activeVisits(run.ID)) > 0
	}
	return false
}

// wakeCommands tells the run's live visits a command waits for them (rule 5).
func (e *Engine) wakeCommands(runID string) {
	for _, v := range e.activeVisits(runID) {
		v.cmdWake.Store(true)
		v.wake()
	}
}

// liveSteps lists the nodes of the run's live visits, sorted.
func (e *Engine) liveSteps(runID string) []string {
	var ids []string
	for _, v := range e.activeVisits(runID) {
		if !slices.Contains(ids, v.node.ID) {
			ids = append(ids, v.node.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// waitingDetail names what a waiting_approval run waits on: its oldest pending approval or
// question.
func waitingDetail(ctx context.Context, q store.Querier, runID string) string {
	pending, err := store.ListPendingApprovals(ctx, q, runID)
	if err != nil || len(pending) == 0 {
		return "the run is waiting on a decision"
	}
	a := pending[0]
	switch a.Kind {
	case store.ApprovalKindQuestion:
		return "the run is waiting on question " + a.ID
	case store.ApprovalKindNode:
		return "the run is waiting on approval " + a.ID
	}
	return "the run is waiting on " + a.Kind + " approval " + a.ID
}

// unconfirmedDetail is rule 13's refusal: the session and the turn not confirmed stopped.
func unconfirmedDetail(ctx context.Context, q store.Querier, runID string) string {
	turns, err := store.UnconfirmedTurns(ctx, q, runID)
	if err != nil || len(turns) == 0 {
		return "the run's last turn is not confirmed stopped yet; the platform checks it again every minute"
	}
	n := turns[len(turns)-1]
	return fmt.Sprintf("turn %s of session %s is not confirmed stopped yet; the platform checks it again every minute",
		strValue(n.OpenClawRunID), strValue(n.SessionKey))
}

// stopState is the cancel_state of a run that ended: unconfirmed while a visit's turn is not
// confirmed stopped, confirmed for a cancelled run, "" (as it was) otherwise.
func (e *Engine) stopState(ctx context.Context, q store.Querier, run *store.Run) string {
	unconfirmed, err := store.HasUnconfirmedTurn(ctx, q, run.ID)
	switch {
	case err == nil && unconfirmed:
		return store.CancelUnconfirmed
	case run.Status == store.RunCancelled:
		return store.CancelConfirmed
	}
	return ""
}

// ---- cancel (rules 10 to 12) ---------------------------------------------------------------

// Cancel cancels a run (DESIGN 5.2 rule 2, W2 rules 10 and 11). A queued run, and one no
// worker holds (paused, or waiting on a decision with no live visit), is cancelled at once
// and the command applied; a held run gets the cancel flag and its worker, woken now,
// acknowledges the command and stops the turn, and the command is applied when the stop is
// confirmed. A finished run is answered with run_finished. The cancel supersedes the
// pause, resume and steer commands still pending before it (rule 6).
func (e *Engine) Cancel(ctx context.Context, runID, by string) (*store.Run, error) {
	if by == "" {
		by = "user"
	}
	return e.receive(ctx, runID, store.CommandCancel, by, "", nil, func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error) {
		return e.decideCancel(ctx, tx, run, c, by)
	})
}

func (e *Engine) decideCancel(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand, by string) (verdict, error) {
	if store.IsTerminal(run.Status) {
		return reject(CodeRunFinished, endedDetail(run.Status)), nil
	}
	if err := store.RequestCancel(ctx, tx, run.ID, by); err != nil {
		return verdict{}, err
	}
	superseded, err := store.SupersedeBefore(ctx, tx, c)
	if err != nil {
		return verdict{}, err
	}
	announce := func(ctx context.Context) {
		for i := range superseded {
			e.emitCommand(ctx, &superseded[i])
		}
	}
	if e.held(run) {
		return verdict{outcome: store.CommandPending, after: func(ctx context.Context) {
			announce(ctx)
			for _, v := range e.activeVisits(run.ID) {
				v.cmdWake.Store(true) // acknowledged at once (rule 4)
				v.requestCancel()
			}
			if fresh, err := store.GetRun(ctx, e.deps.Pool, run.ID); err == nil {
				e.emit(ctx, run.ID, "run.status", map[string]any{"run": fresh})
			}
		}}, nil
	}
	reason := "cancelled by " + by
	done, err := store.TransitionRun(ctx, tx, run.ID, []string{run.Status}, store.RunCancelled, &reason, e.clock.Now())
	if err != nil {
		return verdict{}, err
	}
	if state := e.stopState(ctx, tx, done); state != "" {
		if done, err = store.SetRunCancelState(ctx, tx, run.ID, state); err != nil {
			return verdict{}, err
		}
	}
	return verdict{outcome: store.CommandApplied, run: done, after: func(ctx context.Context) {
		announce(ctx)
		e.log.Info("run cancelled with no worker holding it", "run_id", run.ID, "was", run.Status, "by", by)
		// A run re-queued after a release (DESIGN 5.26 rule 8), paused or waiting with no
		// worker still has its fork row, its parked visits and their questions open: closed as
		// a waiting run's are.
		e.closeWaitingRun(ctx, done, reason, "run cancelled")
	}}, nil
}

// ---- pause and resume (rules 14 and 15) ---------------------------------------------------

// Pause pauses a run (rule 14): a queued run at once; a live one through its worker, which
// stops the turn, saves the checkout and parks the visit. A run waiting on a decision is
// rejected, as is a paused or finished one.
func (e *Engine) Pause(ctx context.Context, runID, by string) (*store.Run, error) {
	if by == "" {
		by = "user"
	}
	return e.receive(ctx, runID, store.CommandPause, by, "", nil, func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error) {
		return e.decidePause(ctx, tx, run, by)
	})
}

func (e *Engine) decidePause(ctx context.Context, tx pgx.Tx, run *store.Run, by string) (verdict, error) {
	switch {
	case store.IsTerminal(run.Status):
		return reject(CodeRunFinished, endedDetail(run.Status)), nil
	case run.Status == store.RunPaused:
		return reject(CodeCommandRejected, "the run is already paused"), nil
	case run.CancelRequested:
		return reject(CodeCommandRejected, "the run is being cancelled"), nil
	case run.Status == store.RunWaitingApproval:
		return reject(CodeCommandRejected, waitingDetail(ctx, tx, run.ID)), nil
	case run.Status == store.RunQueued:
		done, err := store.PauseQueuedRun(ctx, tx, run.ID, e.clock.Now())
		if err != nil {
			return verdict{}, err
		}
		return verdict{outcome: store.CommandApplied, run: done, after: func(ctx context.Context) {
			e.emit(ctx, run.ID, "run.status", map[string]any{"run": done})
			e.logEvent(ctx, run.ID, "info", "paused by "+by+" before it started")
		}}, nil
	}
	if live := e.liveSteps(run.ID); len(live) > 1 {
		return reject(CodeCommandRejected, "the run has parallel branches running ("+strings.Join(live, ", ")+"); pause it outside its fork"), nil
	}
	return verdict{outcome: store.CommandPending, after: func(context.Context) { e.wakeCommands(run.ID) }}, nil
}

// Resume re-queues a paused run (rule 15): the step it was paused in starts a continuation
// visit from the paused visit's checkout, whose task carries the notice and the note (at most
// 2000 characters, redacted). Any other status is rejected, and so is a run whose last turn
// is not confirmed stopped (rule 13).
func (e *Engine) Resume(ctx context.Context, runID, by, note string) (*store.Run, error) {
	if by == "" {
		by = "user"
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxResumeNoteChars || !plainText(note) {
		return nil, &ValidationError{Messages: []string{fmt.Sprintf("note: at most %d characters", MaxResumeNoteChars)}}
	}
	note = obs.Redact(note)
	var payload map[string]any
	if note != "" {
		payload = map[string]any{"note": note}
	}
	return e.receive(ctx, runID, store.CommandResume, by, "", payload, func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error) {
		return e.decideResume(ctx, tx, run, by, note)
	})
}

func (e *Engine) decideResume(ctx context.Context, tx pgx.Tx, run *store.Run, by, note string) (verdict, error) {
	switch {
	case run.Status != store.RunPaused:
		return reject(CodeCommandRejected, "the run is not paused"), nil
	case run.CancelRequested:
		return reject(CodeCommandRejected, "the run is being cancelled"), nil
	case run.CancelState == store.CancelUnconfirmed:
		return reject(CodeCancelUnconfirmed, unconfirmedDetail(ctx, tx, run.ID)), nil
	}
	parked := strValue(run.ResumeNodeVisitID)
	done, err := store.ResumePausedRun(ctx, tx, run.ID, note, e.clock.Now())
	if err != nil {
		return verdict{}, err
	}
	return verdict{outcome: store.CommandApplied, run: done, after: func(ctx context.Context) {
		if parked != "" {
			if rn, err := store.GetRunNode(ctx, e.deps.Pool, parked); err == nil && rn.ResumedAt != nil {
				e.emit(ctx, run.ID, "node.status", map[string]any{"node": rn})
			}
		}
		e.emit(ctx, run.ID, "run.status", map[string]any{"run": done})
		e.logEvent(ctx, run.ID, "info", "resumed by "+by)
	}}, nil
}

// resumeParagraph is what the task of a step the operator paused and resumed gains (rule
// 15): the notice, and each resume's note.
func resumeParagraph(notes []string) string {
	var sb strings.Builder
	sb.WriteString(resumedNotice)
	for _, n := range notes {
		if n = strings.TrimSpace(n); n != "" {
			sb.WriteString("\nThe operator's note: " + n)
		}
	}
	return sb.String()
}

// ---- steering (rule 18) --------------------------------------------------------------------

// Steer sends the operator's message into the live step of a run (rule 18): node_id names
// the step when several are live. The message is 1 to 4000 characters, redacted.
func (e *Engine) Steer(ctx context.Context, runID, by, nodeID, message string) (*store.Run, error) {
	if by == "" {
		by = "user"
	}
	message = strings.TrimSpace(message)
	if n := utf8.RuneCountInString(message); n < 1 || n > MaxSteerChars || !plainText(message) {
		return nil, &ValidationError{Messages: []string{fmt.Sprintf("message: 1 to %d characters", MaxSteerChars)}}
	}
	nodeID = strings.TrimSpace(nodeID)
	payload := map[string]any{"message": obs.Redact(message)}
	return e.receive(ctx, runID, store.CommandSteer, by, nodeID, payload, func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error) {
		return e.decideSteer(run, nodeID), nil
	})
}

func (e *Engine) decideSteer(run *store.Run, nodeID string) verdict {
	if store.IsTerminal(run.Status) {
		return reject(CodeRunFinished, endedDetail(run.Status))
	}
	live := e.liveSteps(run.ID)
	switch {
	case !e.held(run) || len(live) == 0:
		return reject(CodeCommandRejected, nothingRunning)
	case nodeID == "" && len(live) > 1:
		return reject(CodeCommandRejected, "name the step: "+strings.Join(live, ", "))
	case nodeID != "" && !slices.Contains(live, nodeID):
		return reject(CodeCommandRejected, nodeID+" is not running; running: "+strings.Join(live, ", "))
	}
	return verdict{outcome: store.CommandPending, after: func(context.Context) { e.wakeCommands(run.ID) }}
}

// ---- commands no worker holds --------------------------------------------------------------

// settleAtEnd settles the pending commands of a run that ended: a cancel of a cancelled run
// is applied when the stop is confirmed (and stays acknowledged until the reconcile pass
// confirms it otherwise, rule 11); everything else is rejected, the run having ended first.
func (e *Engine) settleAtEnd(ctx context.Context, run *store.Run) {
	if !store.IsTerminal(run.Status) {
		return
	}
	list, err := store.PendingRunCommands(ctx, e.deps.Pool, run.ID)
	if err != nil {
		e.log.Warn("list pending commands failed", "run_id", run.ID, "error", err)
		return
	}
	for i := range list {
		c := &list[i]
		if c.Kind == store.CommandCancel && run.Status == store.RunCancelled {
			if run.CancelState == store.CancelUnconfirmed {
				if c.AckedAt == nil {
					e.ackCommand(store.WithLease(ctx, nil), c)
				}
				continue
			}
			e.settleCommand(ctx, c, store.CommandApplied, "")
			continue
		}
		e.settleCommand(ctx, c, store.CommandRejected, endedDetail(run.Status))
	}
}

// settleUnheld decides, as the api would, the commands still pending for a run its worker
// just let go (parked, paused, or waiting at an approval node): nothing holds the run any
// more to carry them out (rule 5).
func (e *Engine) settleUnheld(ctx context.Context, runID string) {
	ctx = store.WithLease(ctx, nil)
	list, err := store.PendingRunCommands(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("list pending commands failed", "run_id", runID, "error", err)
		return
	}
	for i := range list {
		c := &list[i]
		_, err := e.process(ctx, c, func(ctx context.Context, tx pgx.Tx, run *store.Run, c *store.RunCommand) (verdict, error) {
			if e.held(run) {
				return verdict{outcome: store.CommandPending}, nil // claimed again meanwhile: its worker takes it
			}
			by := commandBy(run, c)
			switch c.Kind {
			case store.CommandCancel:
				if run.Status == store.RunCancelled {
					return verdict{outcome: store.CommandApplied}, nil // the cancel ended it
				}
				return e.decideCancel(ctx, tx, run, c, by)
			case store.CommandPause:
				return e.decidePause(ctx, tx, run, by)
			case store.CommandResume:
				var p struct {
					Note string `json:"note"`
				}
				_ = json.Unmarshal(c.Payload, &p)
				return e.decideResume(ctx, tx, run, by, p.Note)
			case store.CommandSteer:
				return e.decideSteer(run, strValue(c.NodeID)), nil
			}
			return reject(CodeCommandRejected, "the run has not ended"), nil
		})
		var ce *CommandError
		if err != nil && !errors.As(err, &ce) {
			e.log.Warn("settle a pending command failed", "run_id", runID, "command_id", c.ID, "error", err)
		}
	}
}

// commandBy is who a command came from, for the reason lines: the run's cancel_by for a
// cancel, else the actor's name, else "user".
func commandBy(run *store.Run, c *store.RunCommand) string {
	if c.Kind == store.CommandCancel && run.CancelBy != nil && *run.CancelBy != "" {
		return *run.CancelBy
	}
	var a struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(c.Actor, &a) == nil {
		switch {
		case a.Name != "":
			return a.Name
		case a.Kind == string(actor.Operator):
			return "operator"
		}
	}
	return "user"
}

// ---- the reconcile pass (rules 11 and 12) ----------------------------------------------------

// reconcileStops repeats chat.abort and the session check for every run whose last turn is
// not confirmed stopped, on the sweeper's tick (rule 11): a turn the gateway shows no active
// run for is confirmed, and a run whose turns all are gets cancel_state confirmed and its
// acknowledged cancels applied. A gateway that is down leaves everything as it is.
func (e *Engine) reconcileStops(ctx context.Context) {
	gw := e.deps.Gateway
	if gw == nil {
		return
	}
	if _, up := gw.Info(); !up {
		return
	}
	runs, err := store.ListUnconfirmedRuns(ctx, e.deps.Pool, reconcileBatch)
	if err != nil {
		e.log.Warn("list unconfirmed stops failed", "error", err)
		return
	}
	for i := range runs {
		run := &runs[i]
		turns, err := store.UnconfirmedTurns(ctx, e.deps.Pool, run.ID)
		if err != nil {
			e.log.Warn("list unconfirmed turns failed", "run_id", run.ID, "error", err)
			continue
		}
		for _, n := range turns {
			key, turn := strValue(n.SessionKey), strValue(n.OpenClawRunID)
			if key != "" && turn != "" {
				actx, cancel := context.WithTimeout(ctx, abortRPCTimeout)
				_, aerr := openclaw.ChatAbort(actx, gw, key, turn)
				cancel()
				if aerr != nil {
					e.log.Warn("chat.abort of an unconfirmed turn failed", "run_id", run.ID, "openclaw_run_id", turn, "error", aerr)
				}
				sctx, cancel := context.WithTimeout(ctx, abortRPCTimeout)
				active, serr := openclaw.RunActive(sctx, gw, key, turn)
				cancel()
				if serr != nil || active {
					continue
				}
			}
			if err := store.ConfirmTurn(ctx, e.deps.Pool, n.ID); err != nil {
				e.log.Warn("confirm a stopped turn failed", "run_id", run.ID, "error", err)
			}
		}
		done, err := store.ConfirmRunStop(ctx, e.deps.Pool, run.ID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				e.log.Warn("confirm a run's stop failed", "run_id", run.ID, "error", err)
			}
			continue
		}
		if done.Status == store.RunCancelled {
			applied, err := store.ApplyAckedCommands(ctx, e.deps.Pool, run.ID, store.CommandCancel)
			if err != nil {
				e.log.Warn("apply confirmed cancels failed", "run_id", run.ID, "error", err)
			}
			for i := range applied {
				e.emitCommand(ctx, &applied[i])
			}
		}
		e.emit(ctx, run.ID, "run.status", map[string]any{"run": done})
		e.logEvent(ctx, run.ID, "info", "the stopped turn is confirmed ended")
	}
}

// leaseLost is what a worker does once it found its lease gone (rule 9): it says so in the
// run's log, through the event recorder, and writes nothing more to the run.
func (e *Engine) leaseLost(ctx context.Context, runID string) {
	e.logEvent(ctx, runID, "warn", "worker lost its lease; stopped")
}

// ---- the worker's side ----------------------------------------------------------------------

// steerMsg is an operator's message a visit took (rule 18): its command, the text the turn
// gets ("Operator: <message>"), the idempotency key of its steered chat.send, and whether
// that send reached the gateway.
type steerMsg struct {
	cmd  *store.RunCommand
	text string
	key  string
	sent bool
}

// commandsDue reports whether the visit reads its run's pending commands now: woken by the
// api, or commandReadEvery since the last read (rule 5).
func (v *visit) commandsDue() bool {
	return v.cmdWake.Swap(false) || time.Since(v.cmdReadAt) >= commandReadEvery
}

// commands carries out, in the order they were received (rule 6), the run's pending
// commands that are this visit's to take. A cancel is acknowledged here and stopped by the
// cancel flag set with it (checkAndAbort); a pause and a steer are acknowledged and carried
// out (rules 14 and 18); a resume or a retry of a live run is rejected. A command the visit
// cannot take now (it is being stopped, or the command names another step) stays pending.
func (v *visit) commands(ctx context.Context) {
	v.cmdReadAt = time.Now()
	if v.result != nil || v.closing {
		return
	}
	list, err := store.PendingRunCommands(ctx, v.e.deps.Pool, v.run.ID)
	if err != nil {
		v.log.Warn("read pending commands failed", "error", err)
		return
	}
	for i := range list {
		c := &list[i]
		switch c.Kind {
		case store.CommandCancel:
			if c.AckedAt == nil {
				v.e.ackCommand(ctx, c)
			}
		case store.CommandPause:
			v.takePause(ctx, c)
		case store.CommandSteer:
			v.takeSteer(ctx, c)
		case store.CommandResume:
			v.e.settleCommand(ctx, c, store.CommandRejected, "the run is not paused")
		case store.CommandRetry:
			v.e.settleCommand(ctx, c, store.CommandRejected, "the run has not ended")
		}
		if v.result != nil {
			return
		}
	}
}

// takePause starts a pause (rule 14): the turn is aborted, and once it is confirmed stopped
// the walk saves the checkout and parks the visit. A pause another visit took, or one that
// comes while the visit is being stopped or has its result, waits for the run's next step or
// its end; a cancel received meanwhile wins.
func (v *visit) takePause(ctx context.Context, c *store.RunCommand) {
	if c.AckedAt != nil || v.aborting != nil || v.pausing != nil {
		return
	}
	if _, done := v.finished(); done {
		return
	}
	v.refreshRun(ctx)
	switch {
	case v.run.CancelRequested:
		return
	case v.branch != nil || len(v.e.activeVisits(v.run.ID)) > 1:
		v.e.settleCommand(ctx, c, store.CommandRejected, "the run has parallel branches running ("+
			strings.Join(v.e.liveSteps(v.run.ID), ", ")+"); pause it outside its fork")
		return
	case v.rn.Status == store.NodeWaitingApproval:
		v.e.settleCommand(ctx, c, store.CommandRejected, waitingDetail(ctx, v.e.deps.Pool, v.run.ID))
		return
	}
	acked := v.e.ackCommand(ctx, c)
	if acked == nil {
		return
	}
	v.pausing = acked
	v.startAbort(ctx, outcome{paused: true, operatorPause: true})
}

// pauseUnconfirmed is a pause whose turn was not confirmed stopped in time (rule 14): the
// command fails and the step goes on.
func (v *visit) pauseUnconfirmed(ctx context.Context) {
	v.e.settleCommand(ctx, v.pausing, store.CommandFailed, "could not confirm the turn stopped")
	v.e.logEvent(ctx, v.run.ID, "warn", "the pause did not take: the turn was not confirmed stopped; the step goes on")
	v.pausing, v.aborting = nil, nil
}

// takeSteer takes an operator's message for this visit (rule 18) and sends it into the live
// turn with queueMode steer; with no live turn it waits for the next one, whose message it
// opens. A message for another step, or one already taken, is left to that visit.
func (v *visit) takeSteer(ctx context.Context, c *store.RunCommand) {
	if c.AckedAt != nil || (c.NodeID != nil && *c.NodeID != v.node.ID) {
		return
	}
	if c.NodeID == nil {
		if live := v.e.liveSteps(v.run.ID); len(live) > 1 {
			v.e.settleCommand(ctx, c, store.CommandRejected, "name the step: "+strings.Join(live, ", "))
			return
		}
	}
	if _, done := v.finished(); done {
		v.e.settleCommand(ctx, c, store.CommandRejected, "the step finished before the message reached it")
		return
	}
	if v.aborting != nil {
		return // the step is being stopped: the run's end, or its next step, settles it
	}
	acked := v.e.ackCommand(ctx, c)
	if acked == nil {
		return
	}
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(acked.Payload, &p)
	s := &steerMsg{cmd: acked, text: steerPrefix + p.Message, key: v.rn.ID + ":steer:" + acked.ID}
	v.steers = append(v.steers, s)
	if !v.turnLive || v.disconnected {
		v.e.logEvent(ctx, v.run.ID, "info", "operator message for "+v.node.ID+" waits for the step's next turn", "node_id", v.node.ID)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, v.e.sendTimeout)
	_, err := openclaw.ChatSend(sctx, v.e.deps.Gateway, openclaw.ChatSendParams{
		SessionKey: v.key, AgentID: v.ocID, Message: s.text, IdempotencyKey: s.key, QueueMode: openclaw.QueueSteer,
	})
	cancel()
	if err != nil {
		v.e.logEvent(ctx, v.run.ID, "warn", fmt.Sprintf("operator message not sent into the turn (%v); it opens the step's next turn", err),
			"node_id", v.node.ID)
		return
	}
	s.sent = true
	v.e.logEvent(ctx, v.run.ID, "info", "operator message sent into the turn of "+v.node.ID, "node_id", v.node.ID)
}

// steerSeen applies the operator messages a user transcript entry carries (rule 18): the
// entry of the steered send (its idempotency key plus ":user"), or one whose text holds the
// message.
func (v *visit) steerSeen(ctx context.Context, h openclaw.HistoryEntry) {
	if len(v.steers) == 0 {
		return
	}
	key, text := strings.TrimSuffix(h.IdempotencyKey, ":user"), h.Text()
	kept := v.steers[:0]
	for _, s := range v.steers {
		if key == s.key || strings.Contains(text, s.text) {
			v.e.settleCommand(ctx, s.cmd, store.CommandApplied, "")
			continue
		}
		kept = append(kept, s)
	}
	v.steers = kept
}

// steerText is the text the operator messages not yet delivered add to a turn's message.
func (v *visit) steerText() string {
	parts := make([]string, 0, len(v.steers))
	for _, s := range v.steers {
		parts = append(parts, s.text)
	}
	return strings.Join(parts, "\n\n")
}

// steersOpened applies the operator messages a turn just started with (rule 18: applied
// when the turn starts).
func (v *visit) steersOpened(ctx context.Context, carried []*steerMsg) {
	for _, s := range carried {
		v.e.settleCommand(ctx, s.cmd, store.CommandApplied, "")
	}
	v.steers = slices.DeleteFunc(v.steers, func(s *steerMsg) bool { return slices.Contains(carried, s) })
}

// steersDropped rejects the operator messages the visit ends without delivering.
func (v *visit) steersDropped(ctx context.Context) {
	for _, s := range v.steers {
		v.e.settleCommand(ctx, s.cmd, store.CommandRejected, "the step ended before the message reached it")
	}
	v.steers = nil
}

// turnStopped reports whether the gateway shows the visit's turn no longer running (rule
// 11): no turn, or the session shows no active run for it. A gateway that is away or does not
// answer confirms nothing.
func (v *visit) turnStopped(ctx context.Context) bool {
	if !v.turnLive || v.runID == "" {
		return true
	}
	gw := v.e.deps.Gateway
	if gw == nil {
		return false
	}
	if _, up := gw.Info(); !up {
		return false
	}
	sctx, cancel := context.WithTimeout(ctx, abortRPCTimeout)
	defer cancel()
	active, err := openclaw.RunActive(sctx, gw, v.key, v.runID)
	if err != nil {
		v.log.Warn("session check after the abort failed", "error", err)
		return false
	}
	return !active
}

// selfFence stops a visit whose worker lost its lease (rule 9): its turn is aborted and
// nothing more is written to the run.
func (v *visit) selfFence(ctx context.Context) outcome {
	v.abortLiveTurn(context.WithoutCancel(ctx))
	return outcome{leaseLost: true}
}
