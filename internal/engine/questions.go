package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/obs"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Questions to the operator (DESIGN 5.24, decisions D-046, D-056, D-058, D-059). A
// question is an approval of kind question: the row, the waiter, the pause and the
// sweeper are the approval gate's (approvals.go). What is new is where a decision goes:
// to the call that waits, to the live visit through its own loop, or to the run parked on
// it (rule 5), and what a visit does when its turn ends with a question still pending
// (questionTurnEnd).

const (
	// maxStepQuestions is rule 2's cap: a step asks at most 3 questions in a run.
	maxStepQuestions = 3
	// maxAnswerBytes is rule 3's cap on an answer, after trimming.
	maxAnswerBytes = 4096
	// askLogBytes is how much of the question the run log line shows (rule 2).
	askLogBytes = 200
	// continueMessage ends the message that sends a decision to a live visit (rule 7).
	continueMessage = "Continue your task and call yuno__finish when done."
	// unansweredReason is the failure reason of a run whose parked question expired
	// (rule 8).
	unansweredReason = "question unanswered"
	// stepFinishedReason closes the question of a visit that ended with it pending
	// (rule 5).
	stepFinishedReason = "step finished"
)

// listStepQuestions reads a step's decided questions; a variable so a test can make the read
// fail (review L8).
var listStepQuestions = store.ListStepQuestions

// handoffTimeout bounds the handoff of a decision to the live visit through its loop (rule 5
// (b)): a visit whose loop is busy does not hold the decider for longer. When it passes the
// decision stays in its row, which the visit reads at its turn's end (review L6). A variable
// so a test can lower it.
var handoffTimeout = 5 * time.Second

// The engine is the second interface ask_operator calls: QuestionPending, when its wait
// ends with no decision (rule 4).
var _ tools.QuestionParker = (*Engine)(nil)

// questionTestHook lets a test act at the two points of a turn's end where a decision can
// land: "read" before the pending question is read, and "park" after it was read pending
// and before the park. It runs on the visit goroutine; nil in production.
var questionTestHook func(stage string)

// ErrNotQuestion is AnswerQuestion's refusal of an approval of another kind (422 at the
// API, DESIGN 5.24 rule 3).
var ErrNotQuestion = errors.New("approval is not a question")

// AskOperator records a question of the visit that holds the run token (DESIGN 5.24
// rule 2) and pauses the run and the node on it. Everything happens on the visit's own
// loop, so the step's question count is read under the visit's lock and two calls of one
// turn cannot both pass. The caller then blocks on WaitQuestion.
func (e *Engine) AskOperator(ctx context.Context, token, question string, options []string) (*store.Approval, error) {
	t, ok := e.tokens.get(token)
	if !ok {
		return nil, ErrInvalidRunToken
	}
	v := t.v
	// The tool checked these; the engine does too, since a NUL or invalid UTF-8 would fail
	// the insert with a database error nobody can act on (review L4).
	if !plainText(question) {
		return nil, &tools.ModelError{Message: "question: 1 to 1024 characters"}
	}
	var opts json.RawMessage
	if len(options) > 0 {
		// Each option is redacted as the question is (review L3).
		redacted := make([]string, len(options))
		for i, o := range options {
			if !plainText(o) {
				return nil, &tools.ModelError{Message: "options: 2 to 5 distinct options of at most 100 characters"}
			}
			redacted[i] = obs.Redact(o)
		}
		b, err := json.Marshal(redacted)
		if err != nil {
			return nil, fmt.Errorf("encode options: %w", err)
		}
		opts = b
	}
	var (
		a      *store.Approval
		run    *store.Run
		askErr error
	)
	if err := v.do(ctx, func() { a, run, askErr = v.askQuestion(ctx, question, opts) }); err != nil {
		// The visit ended before it took the call: nothing was recorded.
		return nil, err
	}
	if askErr != nil {
		return nil, askErr
	}
	e.logEvent(ctx, a.RunID, "info", fmt.Sprintf("%s asks: %s", v.agent.Name, firstBytes(a.Message, askLogBytes)),
		"node_id", a.NodeID, "approval_id", a.ID)
	e.announceQuestion(ctx, run, a) // DESIGN 5.24 rule 6
	return a, nil
}

// askQuestion is AskOperator on the visit goroutine.
func (v *visit) askQuestion(ctx context.Context, question string, options json.RawMessage) (*store.Approval, *store.Run, error) {
	e := v.e
	// A question after the step's result was accepted, or while the turn is being stopped,
	// would be a card and a notice that close at once (review L12).
	if _, accepted := v.finished(); accepted || v.aborting != nil {
		return nil, nil, &tools.ModelError{Message: "the step has finished"}
	}
	wctx := context.WithoutCancel(ctx)
	n, err := store.CountStepQuestions(wctx, e.deps.Pool, v.run.ID, v.node.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("count the step's questions: %w", err)
	}
	if n >= maxStepQuestions {
		return nil, nil, &tools.ModelError{Message: fmt.Sprintf("ask fewer questions: this step already asked %d", maxStepQuestions)}
	}
	if id := v.pendingQuestion; id != "" {
		// The waiter clears this flag through a message posted to this loop, so a decision
		// that already reached the waiting call may not have cleared it yet when the agent
		// asks again in the same turn: the row is the truth (DESIGN 5.24 rule 2).
		row, rerr := store.GetApproval(wctx, e.deps.Pool, id)
		if rerr != nil || row.Status == store.ApprovalPending {
			return nil, nil, &tools.ModelError{Message: "one question at a time: question " + id + " is still pending"}
		}
		v.pendingQuestion = ""
		delete(v.toolBlocking, id)
	}
	now := e.clock.Now()
	timeout := v.limits.ApprovalTimeoutSeconds
	if timeout <= 0 {
		timeout = store.DefaultLimits.ApprovalTimeoutSeconds
	}
	a := &store.Approval{ID: store.NewID(), RunID: v.run.ID, RunNodeID: v.rn.ID, Kind: store.ApprovalKindQuestion,
		NodeID: v.node.ID, Message: obs.Redact(question), Options: options, Status: store.ApprovalPending,
		RequestedAt: now, ExpiresAt: now.Add(secondsDuration(timeout))}
	// The waiter exists before the row does, so a decision made the moment the row is
	// visible reaches the call that is about to wait (as RequestApproval does).
	e.waitMu.Lock()
	e.waiters[a.ID] = &approvalWaiter{ch: make(chan *store.Approval, 1), v: v}
	e.waitMu.Unlock()
	if err := store.InsertApproval(wctx, e.deps.Pool, a); err != nil {
		e.dropWaiter(a.ID)
		return nil, nil, fmt.Errorf("record question: %w", err)
	}
	span := e.startSpan(wctx, a.RunID, &v.nodeSpan, "approval_wait", "question", now, nil,
		map[string]any{"approval_id": a.ID, "kind": store.ApprovalKindQuestion})
	e.waitMu.Lock()
	_, waiting := e.waiters[a.ID] // not decided in the moments since the insert
	e.waitMu.Unlock()
	if waiting {
		v.pendingQuestion = a.ID
		v.toolBlocking[a.ID] = true
		v.pause(wctx)
	} else {
		e.endSpan(wctx, span, e.clock.Now(), map[string]any{"status": "decided"})
	}
	e.emit(wctx, a.RunID, "approval.requested", map[string]any{"approval": a})
	return a, v.run, nil
}

// handoff runs fn on the visit's loop for a decision (rule 5 (b)), waiting for the loop no
// longer than handoffTimeout. ErrInvalidRunToken says the loop is gone; any other error
// says it stayed busy (or ctx ended) and nothing ran.
func (e *Engine) handoff(ctx context.Context, v *visit, fn func()) error {
	hctx, cancel := context.WithTimeout(ctx, handoffTimeout)
	defer cancel()
	return v.do(hctx, fn)
}

func (e *Engine) dropWaiter(id string) {
	e.waitMu.Lock()
	delete(e.waiters, id)
	e.waitMu.Unlock()
}

// WaitQuestion is WaitApproval for a question (DESIGN 5.24 rules 2 and 4): it blocks
// until the question is decided or ctx ends. A decision made while it waits goes to it
// first (rule 5 (a)).
func (e *Engine) WaitQuestion(ctx context.Context, id string) (*store.Approval, error) {
	return e.WaitApproval(ctx, id)
}

// QuestionPending records that the wait for a question ended with no decision (rule 4):
// the visit is marked parked-on the question, and the run and the node return to
// running, so the agent's turn goes on. The question stays pending. The tools package
// calls it through its QuestionParker interface; it works even when the request that
// waited is gone, since the run would stay paused on a call nobody awaits.
func (e *Engine) QuestionPending(ctx context.Context, token, id string) error {
	t, ok := e.tokens.get(token)
	if !ok {
		return ErrInvalidRunToken
	}
	v := t.v
	wctx := context.WithoutCancel(ctx)
	// The work runs without the caller's cancellation, but the handoff to the visit's loop
	// keeps its deadline: a busy loop does not hold the caller beyond it (review L5).
	cctx := wctx
	if d, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		cctx, cancel = context.WithDeadline(wctx, d)
		defer cancel()
	}
	return v.do(cctx, func() {
		delete(v.toolBlocking, id)
		// A decision that reached the visit meanwhile (questionDecided) already cleared
		// the question: the turn's end then sends it, there is nothing to park on.
		if v.pendingQuestion == id {
			v.parkedOn = id
		}
		v.resumeIfClear(wctx)
	})
}

// questionDecided takes a decision handed to the live visit through its own loop (rule
// 5 (b)): the visit keeps it, and its turn's end sends it as the next turn. A decision
// for a question the visit no longer waits on (taken at the turn's end already) is
// dropped.
func (v *visit) questionDecided(a *store.Approval) {
	if v.pendingQuestion != a.ID {
		return
	}
	v.pendingQuestion = ""
	if v.parkedOn == a.ID {
		v.parkedOn = ""
	}
	v.answered = a
}

// AnswerQuestion answers a pending question as the principal by (DESIGN 5.24 rule 3):
// the row becomes approved with the answer, redacted, then the decision is delivered.
// An approval that is not a question is ErrNotQuestion, one that is not pending
// ErrNotPending, an unknown one wraps store.ErrNotFound.
func (e *Engine) AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error) {
	answer = strings.TrimSpace(answer)
	if n := len(answer); n < 1 || n > maxAnswerBytes || !plainText(answer) {
		return nil, &ValidationError{Messages: []string{"answer: 1 to 4096 characters"}}
	}
	if strings.TrimSpace(by) == "" {
		by = "user"
	}
	a, err := store.GetApproval(ctx, e.deps.Pool, id)
	if err != nil {
		return nil, fmt.Errorf("approval %s: %w", id, err)
	}
	if a.Kind != store.ApprovalKindQuestion {
		return nil, ErrNotQuestion
	}
	if a.Status != store.ApprovalPending {
		return nil, ErrNotPending
	}
	decided, err := store.AnswerApprovalAt(ctx, e.deps.Pool, id, obs.Redact(answer), by, e.clock.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotPending
	}
	if err != nil {
		return nil, fmt.Errorf("answer question %s: %w", id, err)
	}
	e.announceDecision(ctx, decided)
	e.deliverQuestion(ctx, decided)
	return decided, nil
}

// decideQuestion declines a question (DESIGN 5.24 rule 3: reject on a question is a
// decline, D-059) and delivers the decision.
func (e *Engine) decideQuestion(ctx context.Context, a *store.Approval, by, reason string) (*store.Approval, error) {
	decided, err := e.recordDecision(ctx, a, store.ApprovalRejected, by, reason)
	if err != nil {
		return nil, err
	}
	e.deliverQuestion(ctx, decided)
	return decided, nil
}

// deliverQuestion hands a decided question (answered or declined) to the first of these
// that applies (DESIGN 5.24 rule 5 and the Consistency paragraph), in this order:
//
//	(i)   the run parked on it: re-queued, the parked visit closed (RequeueParkedRun);
//	(ii)  the call that waits for it (a waiter);
//	(iii) the live visit that asked, through its own loop, which sends it as the next turn;
//	      a visit that is gone is the park landing meanwhile, so (i) once more;
//	(iv)  a run left waiting with no worker, which is re-queued as the unattended
//	      re-queue of 5.3 does.
//
// The row is recorded before this runs, and every step is idempotent: a failure in (i)
// is logged and the next approval sweep repeats it (requeueParkedDecided).
func (e *Engine) deliverQuestion(ctx context.Context, a *store.Approval) {
	wctx := context.WithoutCancel(ctx)
	if e.requeueParked(wctx, a) {
		return
	}
	if e.deliver(a) {
		return
	}
	if v := e.visitOf(a); v != nil {
		if err := e.handoff(wctx, v, func() { v.questionDecided(a) }); err == nil {
			return
		}
		// The loop is gone: the visit may have parked after step (i) looked. If it ended
		// any other way, the run goes on with the walk and nothing waits for the answer.
		// A loop that stayed busy past the bound is still there, and its turn's end reads
		// the decided row (review L6); trying the re-queue is harmless then.
		e.requeueParked(wctx, a)
		return
	}
	// No visit in this process: the park may have landed since step (i), else a run left
	// waiting by a restart is re-queued.
	if e.requeueParked(wctx, a) {
		return
	}
	e.requeueIfUnattended(wctx, a.RunID)
}

// requeueParked is step (i): it re-queues the run parked on the decided question's visit
// and reports whether it did. The parked visit's row is closed succeeded without output by
// the same statement (store.RequeueParkedRun), which holds only once the visit waits on
// nothing any more: a settled tool approval of a parked visit (DESIGN 5.3 rule 5) comes
// here too, and the last decision of a visit that waits on several re-queues it.
func (e *Engine) requeueParked(ctx context.Context, a *store.Approval) bool {
	if a.RunID == "" || a.RunNodeID == "" {
		return false
	}
	run, err := store.RequeueParkedRun(ctx, e.deps.Pool, a.RunID, a.RunNodeID, e.clock.Now())
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		e.log.Warn("re-queue after a question failed; the next approval sweep repeats it", "run_id", a.RunID, "approval_id", a.ID, "error", err)
		return false
	}
	if rn, err := store.GetRunNode(ctx, e.deps.Pool, a.RunNodeID); err == nil {
		e.emit(ctx, a.RunID, "node.status", map[string]any{"node": rn})
		if rn.BranchID != nil {
			// The statement set the parked branch back to running (DESIGN 5.26 rule 8).
			if rows, err := store.ListRunBranches(ctx, e.deps.Pool, a.RunID); err == nil {
				for i := range rows {
					if rows[i].ID == *rn.BranchID {
						e.emit(ctx, a.RunID, "branch.status", map[string]any{"branch": &rows[i]})
					}
				}
			}
		}
	}
	e.emit(ctx, a.RunID, "run.status", map[string]any{"run": run})
	line := fmt.Sprintf("tool approval %s; continuing %s", a.Status, a.NodeID)
	if a.Kind == store.ApprovalKindQuestion {
		what := "answered"
		if a.Status == store.ApprovalRejected {
			what = "declined"
		}
		line = fmt.Sprintf("question %s; continuing %s", what, a.NodeID)
	}
	e.logEvent(ctx, a.RunID, "info", line, "node_id", a.NodeID, "approval_id", a.ID)
	return true
}

// requeueParkedDecided repeats what a decision could not finish: every run that waits on
// a parked visit whose newest question is decided is re-queued, and one whose question
// expired fails with question unanswered (DESIGN 5.24 Failure handling). It runs with
// each approval sweep. The tool approvals of parked visits are repeated the same way
// (requeueParkedTools).
func (e *Engine) requeueParkedDecided(ctx context.Context) {
	parked, err := store.ListParkedQuestions(ctx, e.deps.Pool)
	if err != nil {
		e.log.Warn("list parked questions failed", "error", err)
		return
	}
	for i := range parked {
		a := &parked[i]
		switch a.Status {
		case store.ApprovalApproved, store.ApprovalRejected:
			e.requeueParked(ctx, a)
		case store.ApprovalExpired:
			e.failRun(ctx, a.RunID, unansweredReason)
		}
	}
	e.requeueParkedTools(ctx)
}

// sweepQuestion expires a question past its expires_at (DESIGN 5.24 rule 8). A call that
// waits gets the expiry (the agent goes on, `unanswered`); a live visit keeps it for its
// turn's end (the "did not answer in time" paragraph); a run left waiting on the question
// with no visit to tell, parked or after a restart, fails with question unanswered.
func (e *Engine) sweepQuestion(ctx context.Context, a *store.Approval) {
	decided, delivered, ok := e.expire(ctx, a, "approval expired")
	if !ok || delivered {
		return
	}
	wctx := context.WithoutCancel(ctx)
	if v := e.visitOf(decided); v != nil {
		err := e.handoff(wctx, v, func() { v.questionDecided(decided) })
		if err == nil || !errors.Is(err, ErrInvalidRunToken) {
			// Handed over, or the visit's loop stayed busy past the bound: the visit is
			// alive and its turn's end reads the expired row (review L6). Failing the run
			// here would fail one whose visit goes on.
			return
		}
	}
	if e.waitsOn(wctx, decided) {
		e.failRun(wctx, decided.RunID, unansweredReason)
	}
}

// waitsOn reports whether the run waits on the question's visit with nobody to tell: the
// park of rule 5, or a run left behind by a restart that killed the call (rule 5 (d)).
func (e *Engine) waitsOn(ctx context.Context, a *store.Approval) bool {
	if a.RunID == "" || a.RunNodeID == "" {
		return false
	}
	if parked, err := store.IsParkedOn(ctx, e.deps.Pool, a.RunID, a.RunNodeID); err == nil && parked {
		return true
	}
	run, err := store.GetRun(ctx, e.deps.Pool, a.RunID)
	return err == nil && run.Status == store.RunWaitingApproval && run.ResumeNodeVisitID != nil && *run.ResumeNodeVisitID == a.RunNodeID
}

// expireStepQuestions closes the questions that visits a lost worker left behind still
// had pending, before the replacement visit renders its task.
func (e *Engine) expireStepQuestions(ctx context.Context, runID string, visits []store.RunNode) {
	if len(visits) == 0 {
		return
	}
	pending, err := store.ListPendingApprovals(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("list pending approvals failed", "run_id", runID, "error", err)
		return
	}
	for i := range pending {
		a := &pending[i]
		if a.Kind != store.ApprovalKindQuestion {
			continue
		}
		for _, n := range visits {
			if n.ID == a.RunNodeID {
				e.expireApproval(ctx, a, stepFinishedReason)
			}
		}
	}
}

// ---- the turn's end ----------------------------------------------------------------

// questionTurnEnd is what a turn that ended without finish does when the visit waits on
// the operator: for a question it asked (DESIGN 5.24 rule 5), or for the approval of a
// tool call it was told is pending_approval (5.3 rule 5). It reports whether it dealt with
// the turn's end:
//
//   - every decision reached the visit (the live handoff, or the rows read now): the next
//     turn starts in the same session with the answer paragraph and the calls' outcomes,
//     so the agent keeps its context;
//   - something is still undecided (the question, an approval, or an approved call whose
//     effect still runs): the visit parks (ParkVisit, one transaction), logs it, and ends
//     with a paused outcome that lets the worker go, as an approval node does.
//
// It returns false for a visit that waits on nothing, which goes on to the usual endings.
func (v *visit) questionTurnEnd(ctx context.Context) bool {
	asked := v.answered != nil || v.pendingQuestion != "" || v.parkedOn != ""
	if !asked && len(v.parkedTools) == 0 {
		return false
	}
	e := v.e
	if h := questionTestHook; h != nil {
		h("read")
	}
	pending := v.pendingQuestion
	if pending == "" {
		pending = v.parkedOn
	}
	// A decision recorded but not yet handed over (its delivery waits for this loop) is
	// found in the row; the handoff then finds the question taken and drops it.
	waitsOn, taken := "", ""
	if asked && v.answered == nil {
		if a, err := store.GetApproval(ctx, e.deps.Pool, pending); err != nil {
			v.log.Warn("read the pending question failed", "approval_id", pending, "error", err)
			waitsOn = pending
		} else if a.Status != store.ApprovalPending {
			taken = pending
		} else {
			waitsOn = pending
		}
	}
	settled, toolWaits := v.settledTools(ctx)
	if waitsOn == "" && toolWaits == "" {
		return v.continueWithAnswer(ctx, asked, taken, settled)
	}
	if h := questionTestHook; h != nil {
		h("park")
	}
	// What the log line names: the question, else an approval still undecided.
	what := "the answer to question " + waitsOn
	if waitsOn == "" {
		what, waitsOn = "the decision on approval "+toolWaits, toolWaits
	}
	now := e.clock.Now()
	if v.branch != nil {
		// Inside a parallel branch the park marks the visit and its branch and leaves the
		// run running; the fork releases the worker once every branch has ended or parked
		// (DESIGN 5.26 rule 8).
		if err := store.ParkBranchVisit(ctx, e.deps.Pool, v.run.ID, v.rn.ID, v.branch.ID, now); err != nil {
			v.end(ctx, failed(store.RunFailed, "park: "+err.Error()))
			return true
		}
		v.branch.Status = store.BranchWaitingApproval
		v.rn.Status = store.NodeWaitingApproval
		v.rn.ParkedAt = &now
		v.saveNode(ctx)
		e.emit(ctx, v.run.ID, "node.status", map[string]any{"node": v.rn})
		e.emit(ctx, v.run.ID, "branch.status", map[string]any{"branch": v.branch})
		e.logEvent(ctx, v.run.ID, "info", fmt.Sprintf("%s waits for %s; branch %s continues when it arrives", v.agent.Name, what, v.branch.Head),
			"node_id", v.node.ID, "approval_id", waitsOn)
		v.end(ctx, outcome{paused: true})
		return true
	}
	run, err := store.ParkVisit(ctx, e.deps.Pool, v.run.ID, v.rn.ID, now)
	if err != nil {
		v.end(ctx, failed(store.RunFailed, "park: "+err.Error()))
		return true
	}
	v.run = run
	v.rn.Status = store.NodeWaitingApproval
	v.rn.ParkedAt = &now
	v.saveNode(ctx)
	e.emit(ctx, run.ID, "node.status", map[string]any{"node": v.rn})
	e.emit(ctx, run.ID, "run.status", map[string]any{"run": run})
	e.logEvent(ctx, run.ID, "info", fmt.Sprintf("%s waits for %s; the step continues when it arrives", v.agent.Name, what),
		"node_id", v.node.ID, "approval_id", waitsOn)
	v.end(ctx, outcome{paused: true})
	return true
}

// continueWithAnswer starts the next turn of a visit whose question, or whose tool calls
// told pending_approval, were decided while its turn was open: the message is the answer
// paragraph of rule 7 when the visit asked, then one block per settled call (5.3 rule 5),
// and ends with the continue sentence. taken is the pending question found decided in its
// row ("" when the decision came through the live handoff); a question the visit asked
// since stays pending.
func (v *visit) continueWithAnswer(ctx context.Context, asked bool, taken string, settled []toolOutcome) bool {
	e := v.e
	v.answered = nil
	if taken != "" {
		if v.pendingQuestion == taken {
			v.pendingQuestion = ""
		}
		if v.parkedOn == taken {
			v.parkedOn = ""
		}
	}
	// A visit never runs on a silently incomplete prompt (DESIGN 5.18): when the step's
	// questions cannot be read, the visit fails with the read's error (review L8).
	var parts []string
	if asked {
		text, err := e.stepAnswers(ctx, v.run.ID, v.node.ID)
		if err != nil {
			v.end(ctx, failed(store.RunFailed, "read the step's answers: "+err.Error()))
			return true
		}
		if text != "" {
			parts = append(parts, text)
		}
		e.logEvent(ctx, v.run.ID, "info", "question answered; continuing "+v.node.ID, "node_id", v.node.ID)
	}
	for _, o := range settled {
		delete(v.parkedTools, o.approval.ID)
		parts = append(parts, o.block())
		e.logEvent(ctx, v.run.ID, "info", fmt.Sprintf("tool approval %s; continuing %s", o.approval.Status, v.node.ID),
			"node_id", v.node.ID, "approval_id", o.approval.ID)
	}
	message := strings.Join(append(parts, continueMessage), "\n\n")
	if res := v.startTurn(ctx, message); res != nil {
		v.end(ctx, *res)
	}
	return true
}

// ---- the answer paragraph (rule 7) -----------------------------------------------------

// stepAnswers is the paragraph of DESIGN 5.24 rule 7 for a step: one block per decided
// question of the step in this run since the operator last retried it (D-093), oldest
// first, separated by blank lines; empty when the step asked none. A failed read is the
// error: the callers fail the visit.
func (e *Engine) stepAnswers(ctx context.Context, runID, nodeID string) (string, error) {
	qs, err := listStepQuestions(ctx, e.deps.Pool, runID, nodeID)
	if err != nil {
		return "", err
	}
	blocks := make([]string, 0, len(qs))
	for i := range qs {
		blocks = append(blocks, answerBlock(&qs[i]))
	}
	return strings.Join(blocks, "\n\n"), nil
}

// answerBlock is one block of the paragraph. The answer is the operator's own text,
// redacted when it was recorded and not wrapped as untrusted content.
func answerBlock(q *store.Approval) string {
	const proceed = " Proceed without the answer and state what you assumed."
	var sb strings.Builder
	sb.WriteString("Earlier in this step you asked the operator: \"" + q.Message + "\"\n")
	switch q.Status {
	case store.ApprovalApproved:
		sb.WriteString("The operator answered: " + strValue(q.Answer))
	case store.ApprovalRejected:
		declined := "The operator declined to answer"
		if r := strings.TrimSpace(strValue(q.Reason)); r != "" {
			declined += ": " + r
		}
		sb.WriteString(sentence(declined) + proceed)
	default:
		sb.WriteString("The operator did not answer in time." + proceed)
	}
	return sb.String()
}

// firstBytes is the first n bytes of s, cut at a character boundary.
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// plainText reports whether s can be stored as the text of a question, an option or an
// answer: valid UTF-8 without a NUL, which the database refuses (review L4).
func plainText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
