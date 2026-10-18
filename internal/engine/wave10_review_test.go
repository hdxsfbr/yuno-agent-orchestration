package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The fixes of the wave 10 review (docs/reviews/wave10-review.md), engine side: the review
// finding each test covers is named in its comment.

// retryableModelError pushes the turn's terminal event for a provider that is overloaded.
func (env *testEnv) retryableModelError(p openclaw.ChatSendParams) {
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
		"errorKind": "provider_error", "errorMessage": "overloaded", "errorDetail": map[string]any{"httpStatus": 529}}))
}

// visitOfToken is the live visit that holds the run token.
func (env *testEnv) visitOfToken(token string) *visit {
	env.t.Helper()
	tk, ok := env.e.tokens.get(token)
	if !ok {
		env.t.Fatal("no visit holds the token")
	}
	return tk.v
}

// L7: a turn that carries the operator's answer is not a retry. The answer turn and three
// turns that the provider's overload ends are four failures after the first turn; the
// budget of 3 retries is spent on the overload only, so the fifth turn runs and finishes.
func TestModelErrorRetriesAreCountedApartFromAnswerTurns(t *testing.T) {
	env := newEnv(t)
	ag := env.questionAgent()
	var snt sends
	var token string
	asked := make(chan askOutcome, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			switch {
			case i == 1:
				token = tokenIn(p.Message)
				asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				<-release
				env.endTurn(p, "Waiting for the operator.")
			case i <= 4: // the answer turn, then two retries: each fails with the overload
				env.retryableModelError(p)
			default:
				finishTurn(t, env, p, token, `{"summary":"done"}`)
			}
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	out := recv(t, asked, "the call")
	if _, err := env.e.AnswerQuestion(context.Background(), out.row.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	close(release)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q; the answer turn must not use the retry budget", r.Status, deref(r.FailureReason))
	}
	if snt.len() != 5 {
		t.Fatalf("turns = %d, want the task, the answer turn and three retries", snt.len())
	}
	want := answeredBlock("Which one?", "this one") + "\n\nContinue your task and call yuno__finish when done."
	if snt.at(1).Message != want || snt.at(2).Message != "Continue." || snt.at(3).Message != "Continue." || snt.at(4).Message != "Continue." {
		t.Fatalf("messages: %q, %q, %q, %q", snt.at(1).Message, snt.at(2).Message, snt.at(3).Message, snt.at(4).Message)
	}
	if n := env.logged(run.ID, "retry 3 of 3"); n != 1 {
		t.Fatalf("'retry 3 of 3' logged %d times", n)
	}

	// And the budget is still 3: a fourth failure after the answer turn fails the visit.
	env2 := newEnv(t)
	ag2 := env2.questionAgent()
	var snt2 sends
	asked2 := make(chan askOutcome, 1)
	release2 := make(chan struct{})
	env2.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt2.add(p)
		go func() {
			if i == 1 {
				asked2 <- env2.askAs(p, "Which one?", nil, 40*time.Millisecond)
				<-release2
				env2.endTurn(p, "Waiting for the operator.")
				return
			}
			env2.retryableModelError(p)
		}()
		return startedTurn(p)
	}
	run2, done2 := env2.start(TriggerRequest{AgentID: ag2.ID, Task: "go"})
	out2 := recv(t, asked2, "the call")
	if _, err := env2.e.AnswerQuestion(context.Background(), out2.row.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	close(release2)
	r2 := env2.wait(done2, run2.ID)
	if r2.Status != store.RunFailed || !strings.HasPrefix(deref(r2.FailureReason), "model error") || snt2.len() != 5 {
		t.Fatalf("run = %s %q after %d turns; want a model error after the answer turn and three retries", r2.Status, deref(r2.FailureReason), snt2.len())
	}
}

// L12: a question after the step's result was accepted, or while the turn is being stopped,
// is refused with the model error `the step has finished`: no card, no notice, no row.
func TestAskOperatorAfterTheStepFinished(t *testing.T) {
	ctx := context.Background()
	t.Run("after finish was accepted", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		asked := make(chan askOutcome, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() {
				token := tokenIn(p.Message)
				if err := env.e.AcceptFinish(ctx, token, []byte(`{"summary":"done"}`)); err != nil {
					t.Errorf("AcceptFinish: %v", err)
				}
				asked <- env.askWithToken(token, "One more thing?", []string{"yes", "no"}, 40*time.Millisecond)
				env.endTurn(p, "Done.")
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, asked, "the call")
		var me *tools.ModelError
		if !errors.As(out.askErr, &me) || me.Message != "the step has finished" || out.row != nil {
			t.Fatalf("ask after finish: row %+v, error %v", out.row, out.askErr)
		}
		if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if qs := env.questionsOf(run.ID); len(qs) != 0 {
			t.Fatalf("a refused question left rows: %+v", qs)
		}
		if n := env.logged(run.ID, " asks: "); n != 0 {
			t.Fatalf("a refused question was logged as asked %d times", n)
		}
	})
	t.Run("while the turn is being stopped", func(t *testing.T) {
		env := newEnv(t)
		env.fc.abortEmits = false // the gateway takes the abort and does not confirm it yet
		ag := env.questionAgent()
		tokens := make(chan string, 1)
		var first openclaw.ChatSendParams
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			first = p
			tokens <- tokenIn(p.Message)
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		token := recv(t, tokens, "the first turn")
		// The visit is aborting until the gateway confirms.
		if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the abort", func() bool { return len(env.fc.callsFor("chat.abort")) == 1 })
		out := env.askWithToken(token, "Too late?", nil, 40*time.Millisecond)
		var me *tools.ModelError
		if !errors.As(out.askErr, &me) || me.Message != "the step has finished" || out.row != nil {
			t.Fatalf("ask while aborting: row %+v, error %v", out.row, out.askErr)
		}
		env.fc.push("chat", chatEvent(first.SessionKey, first.IdempotencyKey, "aborted", map[string]any{"stopReason": "rpc"}))
		if r := env.wait(done, run.ID); r.Status != store.RunCancelled {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if qs := env.questionsOf(run.ID); len(qs) != 0 {
			t.Fatalf("a refused question left rows: %+v", qs)
		}
	})
}

// L3 and L4 (engine): each option is redacted as the question is; a NUL or invalid UTF-8 in
// the question or an option is refused with the field's text, since the insert would fail
// with an error the model cannot act on.
func TestAskOperatorRedactsOptionsAndRefusesUnstorableText(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	type result struct {
		row  *store.Approval
		errs []error
		err  error
	}
	res := make(chan result, 1)
	secret := "sk-ant-" + strings.Repeat("z", 24)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			var r result
			ask := func(q string, o []string) {
				_, err := env.e.AskOperator(ctx, token, q, o)
				r.errs = append(r.errs, err)
			}
			ask("a\x00b", nil)
			ask("\xff\xfe", nil)
			ask("Which?", []string{"ok", "a\x00b"})
			ask("Which?", []string{"ok", "\xff"})
			r.row, r.err = env.e.AskOperator(ctx, token, "Which key?", []string{"use " + secret, "plain"})
			res <- r
			finishThenFinal(t, env, p, `{"summary":"ok"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	r := recv(t, res, "the asks")
	env.wait(done, run.ID)
	for i, want := range []string{"question: 1 to 1024 characters", "question: 1 to 1024 characters",
		"options: 2 to 5 distinct options of at most 100 characters", "options: 2 to 5 distinct options of at most 100 characters"} {
		var me *tools.ModelError
		if !errors.As(r.errs[i], &me) || me.Message != want {
			t.Errorf("ask %d: %v, want the model error %q", i, r.errs[i], want)
		}
	}
	if r.err != nil || r.row == nil {
		t.Fatalf("the valid ask: %+v, %v", r.row, r.err)
	}
	for name, raw := range map[string]string{"the returned row": string(r.row.Options), "the stored row": string(env.approvalRow(r.row.ID).Options)} {
		if strings.Contains(raw, "zzzz") || !strings.Contains(raw, `"plain"`) || !strings.Contains(raw, `"use `) {
			t.Errorf("%s options = %s; the secret must be redacted and the rest kept", name, raw)
		}
	}
	if qs := env.questionsOf(run.ID); len(qs) != 1 {
		t.Fatalf("questions recorded = %d, want only the valid one", len(qs))
	}
}

// L5: QuestionPending keeps the caller's deadline for the handoff to the visit's loop, so a
// loop that stays busy does not hold the tool beyond the time it gave the call.
func TestQuestionPendingKeepsTheCallersDeadline(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	type asked struct {
		token string
		row   *store.Approval
	}
	ready := make(chan asked, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			row, err := env.e.AskOperator(ctx, token, "Which one?", nil)
			if err != nil {
				t.Errorf("AskOperator: %v", err)
			}
			ready <- asked{token, row}
			<-release
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	a := recv(t, ready, "the question")

	// Keep the visit's loop busy.
	entered, gate := make(chan struct{}), make(chan struct{})
	env.visitOfToken(a.token).post(func(context.Context) {
		close(entered)
		<-gate
	})
	recv(t, entered, "the busy loop")

	dctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	res := make(chan error, 1)
	go func() { res <- env.e.QuestionPending(dctx, a.token, a.row.ID) }()
	select {
	case err := <-res:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("QuestionPending = %v; want the caller's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("QuestionPending kept waiting for a busy visit past the caller's deadline")
	}
	close(gate)
	close(release)
	env.wait(done, run.ID)
}

// L6: the decision's handoff to a live visit waits for its loop for a bounded time. A visit
// whose loop is busy (here: at its turn's end, before it reads the question) does not hold
// the decider; the decision stays in the row, which the turn's end then reads, and the next
// turn carries the answer.
func TestDecisionHandoffToABusyVisitIsBounded(t *testing.T) {
	old := handoffTimeout
	handoffTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handoffTimeout = old })
	env := newEnv(t)
	ag := env.questionAgent()
	var snt sends
	var token string
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				token = tokenIn(p.Message)
				asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				env.endTurn(p, "Waiting.")
				return
			}
			finishTurn(t, env, p, token, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	var once sync.Once
	var answered error
	setQuestionHook(t, func(stage string) {
		if stage != "read" {
			return
		}
		once.Do(func() {
			// On the visit goroutine, as its turn ends: its loop is busy until this returns.
			q := env.questionsOf(env.runIDOfOnlyRun())[0]
			select {
			case answered = <-decideInGoroutine(env, q.ID, "this one"):
			case <-time.After(5 * time.Second):
				t.Errorf("the answer waited for the busy visit past the bound")
			}
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	recv(t, asked, "the call")
	r := env.wait(done, run.ID)
	if answered != nil {
		t.Fatalf("answer: %v", answered)
	}
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if want := answeredBlock("Which one?", "this one") + "\n\nContinue your task and call yuno__finish when done."; snt.len() != 2 || snt.at(1).Message != want {
		t.Fatalf("sends = %d; next turn's message:\n%s\nwant:\n%s", snt.len(), snt.at(1).Message, want)
	}
}

// L6, the sweeper's side: an expiry handed to a busy visit does not hold the sweep, and it
// does not fail the run either, since the visit is alive and reads the expired row at its
// turn's end (the unanswered line).
func TestExpiryHandoffToABusyVisitIsBounded(t *testing.T) {
	old := handoffTimeout
	handoffTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handoffTimeout = old })
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	var snt sends
	var token string
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				token = tokenIn(p.Message)
				asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				env.endTurn(p, "Waiting.")
				return
			}
			finishTurn(t, env, p, token, `{"summary":"assumed"}`)
		}()
		return startedTurn(p)
	}
	var once sync.Once
	setQuestionHook(t, func(stage string) {
		if stage != "read" {
			return
		}
		once.Do(func() {
			env.clock.Advance(31 * time.Second) // past the question's 30 s timeout
			swept := make(chan struct{})
			go func() {
				env.e.sweepApprovals(ctx)
				close(swept)
			}()
			select {
			case <-swept:
			case <-time.After(5 * time.Second):
				t.Errorf("the sweep waited for the busy visit past the bound")
			}
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go", Limits: store.Limits{ApprovalTimeoutSeconds: 30}})
	out := recv(t, asked, "the call")
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q; the visit was alive and goes on", r.Status, deref(r.FailureReason))
	}
	if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired {
		t.Fatalf("question = %s", q.Status)
	}
	want := "Earlier in this step you asked the operator: \"Which one?\"\nThe operator did not answer in time." + proceedText +
		"\n\nContinue your task and call yuno__finish when done."
	if snt.len() != 2 || snt.at(1).Message != want {
		t.Fatalf("sends = %d; next turn's message:\n%s\nwant:\n%s", snt.len(), snt.at(1).Message, want)
	}
}

// L8: when the step's questions cannot be read as a decision starts the next turn, the
// visit fails with the read's error, as a visit never runs on a silently incomplete prompt:
// no "Continue" without the paragraph, so the answer cannot be lost.
func TestUnreadableAnswersFailTheVisit(t *testing.T) {
	env := newEnv(t)
	ag := env.questionAgent()
	var calls int
	var mu sync.Mutex
	old := listStepQuestions
	listStepQuestions = func(ctx context.Context, q store.Querier, runID, nodeID string) ([]store.Approval, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n > 1 { // the first read renders the visit's task
			return nil, errors.New("the database went away")
		}
		return old(ctx, q, runID, nodeID)
	}
	t.Cleanup(func() { listStepQuestions = old })
	var snt sends
	asked := make(chan askOutcome, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		snt.add(p)
		go func() {
			asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
			<-release
			env.endTurn(p, "Waiting.")
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	out := recv(t, asked, "the call")
	if _, err := env.e.AnswerQuestion(context.Background(), out.row.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	close(release)
	r := env.wait(done, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "read the step's answers: the database went away" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if snt.len() != 1 {
		t.Fatalf("turns = %d; no turn may start without the paragraph", snt.len())
	}
}

// L13: a retry of a run whose answered question has no continuation visit yet does not
// count the step's loop visits: the walk lets that visit in whatever the count, so the
// retry must not refuse the run for a limit the visit is free of.
func TestRetryOfARunWithAnUnconsumedContinuationCountsNoLoopVisits(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which one?", nil)
	// max_loop_visits 1: the parked visit used it up.
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go", Limits: store.Limits{MaxLoopVisits: 1}})
	recv(t, rig.asked, "the call")
	if r := env.wait(done, run.ID); r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s", r.Status)
	}
	q := env.questionsOf(run.ID)[0]
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	// The answer re-queued the run; it ends before a worker starts the continuation.
	if r, _ := store.GetRun(ctx, env.pool, run.ID); r.Status != store.RunQueued {
		t.Fatalf("run after the answer = %s", r.Status)
	}
	if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
		t.Fatal(err)
	}
	if r, _ := store.GetRun(ctx, env.pool, run.ID); r.Status != store.RunCancelled {
		t.Fatalf("run after the cancel = %s", r.Status)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits = %v; no continuation started", got)
	}
	retried, err := env.e.Retry(ctx, run.ID, "andre", nil)
	if err != nil {
		t.Fatalf("retry: %v; the unconsumed continuation is free of max_loop_visits", err)
	}
	if retried.Status != store.RunQueued {
		t.Fatalf("retried run = %s", retried.Status)
	}
	// The retried walk really does start it: a continuation visit, past the limit.
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("resumed run = %s %q", r.Status, deref(r.FailureReason))
	}
	if nodes := runNodes(t, env, run.ID); len(nodes) != 2 || nodes[1].QuestionID == nil || *nodes[1].QuestionID != q.ID {
		t.Fatalf("visits = %v", visitsOf(nodes))
	}
}

// L6, the sweeper's side again: the wait for a question ended but the visit's loop is busy,
// so the run still waits on the question's visit when the question expires. The expiry
// cannot be handed to the visit within the bound; the run is not failed for it, since the
// visit is alive and reads the expired row at its turn's end.
func TestExpiryOfAQuestionWhoseBusyVisitStillHoldsTheRunDoesNotFailIt(t *testing.T) {
	old := handoffTimeout
	handoffTimeout = 100 * time.Millisecond
	t.Cleanup(func() { handoffTimeout = old })
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	type asked struct {
		token string
		row   *store.Approval
	}
	ready, waited, release := make(chan asked, 1), make(chan struct{}), make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			row, err := env.e.AskOperator(ctx, token, "Which one?", nil)
			if err != nil {
				t.Errorf("AskOperator: %v", err)
				return
			}
			ready <- asked{token, row}
			wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			_, _ = env.e.WaitQuestion(wctx, row.ID) // the wait ends with no decision
			cancel()
			close(waited)
			<-release
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go", Limits: store.Limits{ApprovalTimeoutSeconds: 30}})
	a := recv(t, ready, "the question")
	entered, gate := make(chan struct{}), make(chan struct{})
	env.visitOfToken(a.token).post(func(context.Context) {
		close(entered)
		<-gate
	})
	recv(t, entered, "the busy loop")
	recv(t, waited, "the end of the wait")
	// The visit has not yet been told that the call stopped waiting: the run waits on it.
	if s := runStatus(t, env, run.ID); s != store.RunWaitingApproval {
		t.Fatalf("run before the sweep = %s", s)
	}

	env.clock.Advance(31 * time.Second)
	swept := make(chan struct{})
	go func() {
		env.e.sweepApprovals(ctx)
		close(swept)
	}()
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Error("the sweep waited for the busy visit past the bound")
	}
	if cur := env.approvalRow(a.row.ID); cur.Status != store.ApprovalExpired {
		t.Fatalf("question = %s", cur.Status)
	}
	if s := runStatus(t, env, run.ID); s != store.RunWaitingApproval {
		t.Fatalf("run after the sweep = %s %q; a live visit's run is not failed for an expiry it could not be told", s, deref(mustRun(t, env, run.ID).FailureReason))
	}
	close(gate)
	close(release)
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

func mustRun(t *testing.T, env *testEnv, id string) *store.Run {
	t.Helper()
	r, err := store.GetRun(context.Background(), env.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
