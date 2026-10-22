package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// A Retry starts the failed step afresh (DESIGN 5.21 rule 4, 5.24 rule 7, D-093). Found live
// on run 01a160c8: the documentation audit asked the operator, who answered "Stop this
// ticket"; the agent escalated and the run failed. Once the cause was fixed the operator
// retried, and the retried visit's task still carried that answer (the step's answer
// paragraph listed every decided question of the step in the run), so the agent escalated
// again at once. The answers and tool outcomes a failed attempt was given are not replayed
// after a Retry; a continuation (5.24 rule 5, 5.3 rule 5) and the visit that replaces one a
// restart interrupted still carry theirs.

const (
	checkoutQuestion = "The checkout is missing from my workspace. How should I go on?"
	stopAnswer       = "Stop this ticket"
	repoQuestion     = "Which repository should I audit?"
	repoAnswer       = "Use the run repository"
)

// taskWith is a task message's text before the protocol block: the task, then each block.
func taskWith(task string, blocks ...string) string {
	return strings.Join(append([]string{task}, blocks...), "\n\n") + "\n\n--- platform protocol ---\n"
}

// escalateFrom escalates the run of the visit whose first message is p, as the escalate tool
// does.
func (env *testEnv) escalateFrom(p openclaw.ChatSendParams, reason string) {
	rc, ok := env.e.ResolveRunToken(tokenIn(p.Message))
	if !ok {
		env.t.Error("token not live")
		return
	}
	if err := env.e.EscalateRun(context.Background(), rc.RunID, "other", reason); err != nil {
		env.t.Errorf("escalate: %v", err)
	}
}

// answerWhileItWaits answers the run's one pending question while its call still waits.
func (env *testEnv) answerWhileItWaits(runID, answer string) *store.Approval {
	env.t.Helper()
	eventually(env.t, "the question", func() bool { return runStatus(env.t, env, runID) == store.RunWaitingApproval })
	q := pendingApproval(env.t, env, runID)
	if _, err := env.e.AnswerQuestion(context.Background(), q.ID, answer, "api"); err != nil {
		env.t.Fatal(err)
	}
	return q
}

// runStoppable claims the queued run and executes it on a worker that stop ends, as a
// shutdown of the api does.
func (env *testEnv) runStoppable(runID string) (stop context.CancelFunc, done <-chan struct{}) {
	env.t.Helper()
	claimed, err := store.ClaimQueuedRun(context.Background(), env.pool)
	if err != nil || claimed.ID != runID {
		env.t.Fatalf("claim %s: %+v %v", runID, claimed, err)
	}
	wctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		env.e.runRun(wctx, claimed)
	}()
	return cancel, ch
}

// restart stops the worker the way a deploy does, lets the stale sweeper re-queue the run
// (attempt 2) and moves the test to a new engine on the same database.
func (env *testEnv) restart(runID string, stop context.CancelFunc, done <-chan struct{}) *Engine {
	env.t.Helper()
	eventually(env.t, "the run back to running", func() bool { return runStatus(env.t, env, runID) == store.RunRunning })
	stop()
	recv(env.t, done, "the worker to stop")
	if n, _, err := store.RequeueStaleRuns(context.Background(), env.pool, -time.Hour); err != nil || n != 1 {
		env.t.Fatalf("requeue stale: %d %v", n, err)
	}
	e2 := New(env.e.deps)
	e2.tickInterval = env.e.tickInterval
	env.e = e2
	return e2
}

// The live case: the failed attempt's question was answered within the wait and the agent
// escalated. The retried visit carries no trace of the answer, says the step was retried,
// continues nothing and may ask again; the question it asks and parks on is continued with
// that answer only, and the continuation still says the step was retried.
func TestRetryDropsTheAnswersOfTheFailedAttempt(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	var snt sends
	asked := make(chan askOutcome, 2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			switch i {
			case 1:
				out := env.askAs(p, checkoutQuestion, nil, 10*time.Second)
				asked <- out
				env.escalateFrom(p, `The operator answered "`+stopAnswer+`".`)
			case 2:
				asked <- env.askAs(p, repoQuestion, nil, 40*time.Millisecond)
				env.endTurn(p, "Saved my progress; waiting for the operator's answer.")
			default:
				finishThenFinal(t, env, p, `{"summary":"audited"}`)
			}
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "audit"})
	q1 := env.answerWhileItWaits(run.ID, stopAnswer)
	if out := recv(t, asked, "the first call"); out.decided == nil || deref(out.decided.Answer) != stopAnswer {
		t.Fatalf("the first call = %+v", out)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != `escalated (other): The operator answered "Stop this ticket".` {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}

	if _, err := env.e.Retry(ctx, run.ID, "andre", nil); err != nil {
		t.Fatal(err)
	}
	// The step's question count starts afresh with the retry (5.24 rule 2).
	if n, err := store.CountStepQuestions(ctx, env.pool, run.ID, "agent"); err != nil || n != 0 {
		t.Fatalf("the step's questions after the retry = %d %v", n, err)
	}
	r = env.resume(env.e, run.ID)
	if r.Status != store.RunWaitingApproval {
		t.Fatalf("retried run = %s %q; the retried visit parks on its own question", r.Status, deref(r.FailureReason))
	}
	if out := recv(t, asked, "the retried visit's call"); out.askErr != nil || out.parkErr != nil {
		t.Fatalf("the retried visit could not ask: %+v", out)
	}
	retried := snt.at(1).Message
	if !strings.HasPrefix(retried, taskWith("audit", retriedNotice)) || strings.Contains(retried, stopAnswer) || strings.Contains(retried, "Earlier in this step") {
		t.Fatalf("the retried visit's task:\n%s", retried)
	}
	q2 := pendingApproval(t, env, run.ID)
	if _, err := env.e.AnswerQuestion(ctx, q2.ID, repoAnswer, "api"); err != nil {
		t.Fatal(err)
	}
	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("continued run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:failed", "agent#2:succeeded", "agent#3:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if nodes[1].QuestionID != nil {
		t.Fatalf("the retried visit names %s; a retry continues nothing", *nodes[1].QuestionID)
	}
	if nodes[2].QuestionID == nil || *nodes[2].QuestionID != q2.ID {
		t.Fatalf("the continuation names %v, want %s", nodes[2].QuestionID, q2.ID)
	}
	continued := snt.at(2).Message
	if !strings.HasPrefix(continued, taskWith("audit", retriedNotice, answeredBlock(repoQuestion, repoAnswer))) || strings.Contains(continued, stopAnswer) {
		t.Fatalf("the continuation's task:\n%s", continued)
	}
	// The failed attempt's question stays on record as it was decided.
	if q := env.approvalRow(q1.ID); q.Status != store.ApprovalApproved || deref(q.Answer) != stopAnswer {
		t.Fatalf("the first question = %+v", q)
	}
}

// The tool approval twin: a parked call rejected before the step failed. The continuation of
// the park still carries the rejection (5.3 rule 5); the retried visit carries none of it,
// also when the rejection landed as the run ended, before its continuation started.
func TestRetryDropsTheToolOutcomesOfTheFailedAttempt(t *testing.T) {
	const rejected = "It was rejected: the branch is not ready."
	setup := func(t *testing.T, failContinuation bool) (*testEnv, *toolParkRig, *store.Run, gatedOutcome) {
		env := newEnv(t)
		env.e.deps.Effects = &mergeRequestEffects{pool: env.pool}
		ag := env.agent(nil)
		rig := &toolParkRig{env: env, calls: make(chan gatedOutcome, 1)}
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			i := rig.snt.add(p)
			go func() {
				switch {
				case i == 1:
					rig.calls <- env.mergeRequestCall(p, 40*time.Millisecond)
					env.endTurn(p, "The merge request waits for the operator's approval; ending my turn.")
				case i == 2 && failContinuation:
					env.endTurn(p, "The operator rejected the merge request; stopping here.")
				default:
					finishThenFinal(t, env, p, resultFromTask(p.Message))
				}
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
		g := rig.parked(t, done, run.ID)
		if _, err := env.e.DecideApproval(context.Background(), g.approval.ID, DecisionReject, "andre", "the branch is not ready"); err != nil {
			t.Fatal(err)
		}
		env.e.bg.Wait()
		if s := runStatus(t, env, run.ID); s != store.RunQueued {
			t.Fatalf("run after the rejection = %s", s)
		}
		return env, rig, run, g
	}
	// summary is what the step's agent finished with: it acts on what its task says.
	summary := func(t *testing.T, r *store.Run) any {
		agent, _ := stateOf(t, r)["agent"].(map[string]any)
		return agent["summary"]
	}

	t.Run("continued, then failed", func(t *testing.T) {
		env, rig, run, g := setup(t, true)
		ctx := context.Background()
		r := env.resume(env.e, run.ID)
		if r.Status != store.RunFailed || deref(r.FailureReason) != "agent did not call finish" {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if msg := rig.snt.at(1).Message; !strings.HasPrefix(msg, taskWith("ship ticket 7", outcomeBlock(g.approval.ID, rejected))) {
			t.Fatalf("the continuation's task:\n%s\nwant the rejection block and no retry sentence", msg)
		}
		if _, err := env.e.Retry(ctx, run.ID, "andre", nil); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, run.ID)
		if r.Status != store.RunSucceeded {
			t.Fatalf("retried run = %s %q", r.Status, deref(r.FailureReason))
		}
		nodes := runNodes(t, env, run.ID)
		if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:failed", "agent#3:succeeded"}) {
			t.Fatalf("visits = %v", got)
		}
		if nodes[2].QuestionID != nil {
			t.Fatalf("the retried visit names %s", *nodes[2].QuestionID)
		}
		if msg := rig.snt.at(2).Message; !strings.HasPrefix(msg, taskWith("ship ticket 7", retriedNotice)) || strings.Contains(msg, "Earlier in this step") {
			t.Fatalf("the retried visit's task:\n%s", msg)
		}
		if s := summary(t, r); s != "nothing to report" {
			t.Fatalf("the retried step finished with %v; it acted on the old rejection", s)
		}
	})

	t.Run("decided as the run ended", func(t *testing.T) {
		env, rig, run, g := setup(t, false)
		ctx := context.Background()
		// The rejection re-queued the run; it is cancelled before a worker starts the
		// continuation, so the decision is still unconsumed when the operator retries.
		if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
			t.Fatal(err)
		}
		if a, err := store.ContinuationQuestion(ctx, env.pool, run.ID, "agent"); err != nil || a.ID != g.approval.ID {
			t.Fatalf("before the retry the next visit would continue %v (%v)", a, err)
		}
		if _, err := env.e.Retry(ctx, run.ID, "andre", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ContinuationQuestion(ctx, env.pool, run.ID, "agent"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after the retry the next visit continues something: %v", err)
		}
		r := env.resume(env.e, run.ID)
		if r.Status != store.RunSucceeded {
			t.Fatalf("retried run = %s %q", r.Status, deref(r.FailureReason))
		}
		nodes := runNodes(t, env, run.ID)
		if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:succeeded"}) {
			t.Fatalf("visits = %v", got)
		}
		if nodes[1].QuestionID != nil {
			t.Fatalf("the retried visit names %s; it is no continuation", *nodes[1].QuestionID)
		}
		if msg := rig.snt.at(1).Message; !strings.HasPrefix(msg, taskWith("ship ticket 7", retriedNotice)) || strings.Contains(msg, "Earlier in this step") {
			t.Fatalf("the retried visit's task:\n%s", msg)
		}
		if s := summary(t, r); s != "nothing to report" {
			t.Fatalf("the retried step finished with %v; it acted on the old rejection", s)
		}
		// A fresh visit counts toward max_loop_visits like any other.
		if loop, _ := store.CountNodeLoopVisits(ctx, env.pool, run.ID, "agent"); loop != 2 {
			t.Fatalf("loop visits = %d, want 2", loop)
		}
	})
}

// Without a retry nothing changes: a parked question, answered, is continued with its
// answer and nothing about a retry.
func TestParkedAnswerIsContinuedWithoutARetry(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, repoQuestion, nil, `{"summary":"audited"}`)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "audit"})
	recv(t, rig.asked, "the call")
	if r := env.wait(done, run.ID); r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s", r.Status)
	}
	q := pendingApproval(t, env, run.ID)
	if _, err := env.e.AnswerQuestion(ctx, q.ID, repoAnswer, "api"); err != nil {
		t.Fatal(err)
	}
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if msg := rig.snt.at(1).Message; !strings.HasPrefix(msg, taskWith("audit", answeredBlock(repoQuestion, repoAnswer))) {
		t.Fatalf("the continuation's task:\n%s", msg)
	}
	if n := env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND retried_at IS NOT NULL`, run.ID); n != 0 {
		t.Fatalf("%d visits marked retried without a retry", n)
	}
}

// A visit that replaces one an api restart interrupted (attempt 2) is the same attempt, not a
// retry: it carries the answer given before the restart and says nothing about a retry. After
// a retry, the replacement carries the answers of the retried attempt only.
func TestRestartReplacementKeepsTheAnswersOfItsAttempt(t *testing.T) {
	t.Run("no retry", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		var snt sends
		asked := make(chan askOutcome, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			i := snt.add(p)
			go func() {
				if i == 1 {
					asked <- env.askAs(p, repoQuestion, nil, 10*time.Second) // the turn then goes on until the restart
					return
				}
				finishThenFinal(t, env, p, `{"summary":"audited"}`)
			}()
			return startedTurn(p)
		}
		run, err := env.e.Trigger(context.Background(), TriggerRequest{AgentID: ag.ID, Task: "audit"})
		if err != nil {
			t.Fatal(err)
		}
		stop, done := env.runStoppable(run.ID)
		env.answerWhileItWaits(run.ID, repoAnswer)
		if out := recv(t, asked, "the call"); out.decided == nil {
			t.Fatalf("the call = %+v", out)
		}
		e2 := env.restart(run.ID, stop, done)
		if r := env.resume(e2, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		nodes := runNodes(t, env, run.ID)
		if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:failed", "agent#2:succeeded"}) {
			t.Fatalf("visits = %v", got)
		}
		if deref(nodes[0].FailureReason) != "worker lost" || nodes[1].QuestionID != nil {
			t.Fatalf("visits = %+v", nodes)
		}
		if msg := snt.at(1).Message; !strings.HasPrefix(msg, taskWith("audit", answeredBlock(repoQuestion, repoAnswer))) {
			t.Fatalf("the replacement visit's task:\n%s", msg)
		}
	})

	t.Run("after a retry", func(t *testing.T) {
		env := newEnv(t)
		ctx := context.Background()
		ag := env.questionAgent()
		var snt sends
		asked := make(chan askOutcome, 2)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			i := snt.add(p)
			go func() {
				switch i {
				case 1:
					asked <- env.askAs(p, checkoutQuestion, nil, 10*time.Second)
					env.escalateFrom(p, "told to stop")
				case 2:
					asked <- env.askAs(p, repoQuestion, nil, 10*time.Second) // the turn then goes on until the restart
				default:
					finishThenFinal(t, env, p, `{"summary":"audited"}`)
				}
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "audit"})
		env.answerWhileItWaits(run.ID, stopAnswer)
		recv(t, asked, "the first call")
		if r := env.wait(done, run.ID); r.Status != store.RunFailed {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if _, err := env.e.Retry(ctx, run.ID, "andre", nil); err != nil {
			t.Fatal(err)
		}
		stop, worker := env.runStoppable(run.ID)
		env.answerWhileItWaits(run.ID, repoAnswer)
		if out := recv(t, asked, "the retried visit's call"); out.decided == nil {
			t.Fatalf("the retried visit's call = %+v", out)
		}
		e2 := env.restart(run.ID, stop, worker)
		if r := env.resume(e2, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		nodes := runNodes(t, env, run.ID)
		if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:failed", "agent#2:failed", "agent#3:succeeded"}) {
			t.Fatalf("visits = %v", got)
		}
		msg := snt.at(2).Message
		if !strings.HasPrefix(msg, taskWith("audit", retriedNotice, answeredBlock(repoQuestion, repoAnswer))) || strings.Contains(msg, stopAnswer) {
			t.Fatalf("the replacement visit's task:\n%s", msg)
		}
	})
}
