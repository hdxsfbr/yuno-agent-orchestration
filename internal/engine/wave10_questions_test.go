package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// Questions to the operator (DESIGN 5.24, T5.24.2 to T5.24.9), engine side. The tests play
// the ask_operator tool as the tools package does: AskOperator, WaitQuestion bounded by a
// short wait standing for MCP_TOOL_WAIT, and QuestionPending when the wait ends with no
// decision; and they end a turn the way the gateway does, with a final chat event.

// askOutcome is what the tool saw.
type askOutcome struct {
	row     *store.Approval // the pending row AskOperator returned
	askErr  error
	decided *store.Approval // what WaitQuestion returned; nil when the wait ended first
	waitErr error
	parkErr error // QuestionPending's answer when the wait ended first
}

// askWithToken plays one ask_operator call of the visit that holds token.
func (env *testEnv) askWithToken(token, question string, options []string, wait time.Duration) askOutcome {
	ctx := context.Background()
	var out askOutcome
	out.row, out.askErr = env.e.AskOperator(ctx, token, question, options)
	if out.askErr != nil {
		return out
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	out.decided, out.waitErr = env.e.WaitQuestion(wctx, out.row.ID)
	cancel()
	if out.waitErr != nil {
		out.parkErr = env.e.QuestionPending(ctx, token, out.row.ID)
	}
	return out
}

// askAs is askWithToken for the visit whose first message is p.
func (env *testEnv) askAs(p openclaw.ChatSendParams, question string, options []string, wait time.Duration) askOutcome {
	return env.askWithToken(tokenIn(p.Message), question, options, wait)
}

// endTurn ends the turn of p with a final reply that holds no result: the agent saved its
// progress and stopped without calling finish.
func (env *testEnv) endTurn(p openclaw.ChatSendParams, text string) {
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart(text)}, "stop", 100, 5, 0))
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
		"message": map[string]any{"role": "assistant", "content": []any{textPart(text)}}}))
}

// finishTurn is finishThenFinal for a turn whose message carries no token (a next turn in
// the same session): the token is the one of the visit's first message.
func finishTurn(t *testing.T, env *testEnv, p openclaw.ChatSendParams, token, result string) {
	t.Helper()
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
		[]any{toolCallPart("call_fin", "yuno__finish", map[string]any{"run_token": token, "result": json.RawMessage(result)})}, "toolUse", 100, 10, 0))
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_fin", "yuno__finish", "start", "running", ""))
	if err := env.e.AcceptFinish(context.Background(), token, json.RawMessage(result)); err != nil {
		t.Errorf("AcceptFinish: %v", err)
	}
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_fin", "yuno__finish", "end", "completed", ""))
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart("Done.")}, "stop", 120, 5, 0))
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
		"message": map[string]any{"role": "assistant", "content": []any{textPart("Done.")}}}))
}

// questionAgent is an agent that holds ask_operator.
func (env *testEnv) questionAgent() *store.Agent {
	return env.agent(func(a *store.Agent) {
		a.ToolAllowlist = []string{"exec", "finish", tools.AskOperatorName}
	})
}

// questionsOf lists a run's questions, oldest first.
func (env *testEnv) questionsOf(runID string) []store.Approval {
	env.t.Helper()
	all, err := store.ListApprovals(context.Background(), env.pool, "", runID, 100)
	if err != nil {
		env.t.Fatal(err)
	}
	var out []store.Approval
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Kind == store.ApprovalKindQuestion {
			out = append(out, all[i])
		}
	}
	return out
}

func (env *testEnv) approvalRow(id string) *store.Approval {
	env.t.Helper()
	a, err := store.GetApproval(context.Background(), env.pool, id)
	if err != nil {
		env.t.Fatal(err)
	}
	return a
}

func (env *testEnv) logged(runID, substr string) int {
	env.t.Helper()
	return env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data->>'message' LIKE $2`, runID, "%"+substr+"%")
}

func (env *testEnv) openSpans(runID string) int {
	return env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, runID)
}

// activeVisit reports whether the engine still holds a live visit of the run.
func (env *testEnv) activeVisit(runID string) bool {
	env.e.mu.Lock()
	defer env.e.mu.Unlock()
	return env.e.active[runID] != nil
}

// setQuestionHook installs the engine's test hook for the test's duration.
func setQuestionHook(t *testing.T, fn func(stage string)) {
	t.Helper()
	questionTestHook = fn
	t.Cleanup(func() { questionTestHook = nil })
}

// sends collects every chat.send of a run: message, session and idempotency key, in order.
type sends struct {
	mu   sync.Mutex
	list []openclaw.ChatSendParams
}

func (s *sends) add(p openclaw.ChatSendParams) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, p)
	return len(s.list)
}

func (s *sends) at(i int) openclaw.ChatSendParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.list) {
		return openclaw.ChatSendParams{}
	}
	return s.list[i]
}

func (s *sends) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

// answeredBlock is rule 7's block for an answered question.
func answeredBlock(question, answer string) string {
	return "Earlier in this step you asked the operator: \"" + question + "\"\nThe operator answered: " + answer
}

const proceedText = " Proceed without the answer and state what you assumed."

// ---- T5.24.2 ---------------------------------------------------------------------------

// T5.24.2: the agent asks; the run and the node pause; an answer within the wait returns
// to the call as the decided row, in the same turn; the row carries the answer; the paused
// time does not count toward the time limit; nothing is registered with the gateway.
func TestQuestionAnsweredWithinTheWait(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	asked := make(chan askOutcome, 1)
	var snt sends
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		snt.add(p)
		go func() {
			out := env.askAs(p, "Which greeting language?", []string{"English", "Portuguese"}, 10*time.Second)
			asked <- out
			finishThenFinal(t, env, p, `{"summary":"greeted"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "greet", Limits: store.Limits{MaxDurationSeconds: 600}})
	eventually(t, "the run to pause on the question", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	eventually(t, "the node to pause", func() bool { return runNodes(t, env, run.ID)[0].Status == store.NodeWaitingApproval })
	q := pendingApproval(t, env, run.ID)
	node := runNodes(t, env, run.ID)[0]
	if q.Kind != store.ApprovalKindQuestion || q.Message != "Which greeting language?" || string(q.Options) != `["English", "Portuguese"]` &&
		string(q.Options) != `["English","Portuguese"]` || q.NodeID != "agent" || q.RunNodeID != node.ID || q.Status != store.ApprovalPending ||
		q.ExternalID != nil || q.ToolCallID != nil || q.Answer != nil || !q.ExpiresAt.Equal(q.RequestedAt.Add(86400*time.Second)) {
		t.Fatalf("question = %+v", q)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='approval_wait' AND ended_at IS NULL AND attrs->>'kind'='question' AND attrs->>'approval_id'=$2`, run.ID, q.ID); n != 1 {
		t.Fatalf("open approval_wait spans of the question = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='approval.requested' AND data->'approval'->>'kind'='question'`, run.ID); n != 1 {
		t.Fatalf("approval.requested events = %d", n)
	}
	if n := env.logged(run.ID, "Test Agent asks: Which greeting language?"); n != 1 {
		t.Fatalf("log lines %q = %d", "<agent> asks: <question>", n)
	}
	if len(env.fc.callsFor("plugin.approval.request")) != 0 {
		t.Fatal("a question was registered with the gateway")
	}

	env.clock.Advance(2 * time.Hour) // far past the 600 s limit: the wait does not count
	time.Sleep(100 * time.Millisecond)
	if s := runStatus(t, env, run.ID); s != store.RunWaitingApproval {
		t.Fatalf("status during the wait = %s", s)
	}
	answered, err := env.e.AnswerQuestion(ctx, q.ID, "Portuguese", "api")
	if err != nil {
		t.Fatal(err)
	}
	if answered.Status != store.ApprovalApproved || answered.Answer == nil || *answered.Answer != "Portuguese" ||
		deref(answered.DecidedBy) != "api" || answered.DecidedAt == nil || answered.Reason != nil {
		t.Fatalf("answered question = %+v", answered)
	}
	out := recv(t, asked, "the call")
	if out.askErr != nil || out.waitErr != nil || out.decided == nil || out.decided.ID != q.ID || out.decided.Status != store.ApprovalApproved ||
		out.decided.Answer == nil || *out.decided.Answer != "Portuguese" {
		t.Fatalf("the call got %+v (wait error %v)", out.decided, out.waitErr)
	}

	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || r.PausedTotalMs < 7_200_000 {
		t.Fatalf("run = %s %q paused_total=%d", r.Status, deref(r.FailureReason), r.PausedTotalMs)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits = %v; the answer within the wait continues the same visit", got)
	}
	if node := runNodes(t, env, run.ID)[0]; node.ParkedAt != nil || node.QuestionID != nil {
		t.Fatalf("visit = %+v; it never parked and continues no question", node)
	}
	if snt.len() != 1 {
		t.Fatalf("chat.send calls = %d; the answer came in the tool result, not as a turn", snt.len())
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='approval_wait' AND ended_at IS NOT NULL`, run.ID); n != 1 {
		t.Fatalf("closed approval spans = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='approval.decided' AND data->'approval'->>'answer'='Portuguese'`, run.ID); n != 1 {
		t.Fatalf("approval.decided events with the answer = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status' AND data->'run'->>'status'='waiting_approval'`, run.ID); n == 0 {
		t.Fatal("no run.status event for the pause")
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if n := env.logged(run.ID, "question answered by api"); n != 1 {
		t.Fatalf("decision log lines = %d", n)
	}
	// A second answer is refused.
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "English", "api"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second answer: %v", err)
	}
}

// Two answers at once: the row takes one, the other is refused, and the call that waits
// gets the first.
func TestTwoAnswersAtOnceOneWins(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			asked <- env.askAs(p, "Which one?", []string{"a", "b"}, 10*time.Second)
			finishThenFinal(t, env, p, `{"summary":"ok"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	eventually(t, "the question", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	q := pendingApproval(t, env, run.ID)
	errs := make(chan error, 3)
	start := make(chan struct{})
	for _, who := range []string{"api", "chat:Chief of Staff"} {
		go func() {
			<-start
			_, err := env.e.AnswerQuestion(ctx, q.ID, "answer from "+who, who)
			errs <- err
		}()
	}
	go func() {
		<-start
		_, err := env.e.DecideApproval(ctx, q.ID, DecisionReject, "api", "no")
		errs <- err
	}()
	close(start)
	var won, lost int
	for range 3 {
		switch err := recv(t, errs, "a decision"); {
		case err == nil:
			won++
		case errors.Is(err, ErrNotPending):
			lost++
		default:
			t.Fatalf("decision: %v", err)
		}
	}
	if won != 1 || lost != 2 {
		t.Fatalf("%d decisions won, %d lost; want 1 and 2", won, lost)
	}
	out := recv(t, asked, "the call")
	cur := env.approvalRow(q.ID)
	if out.decided == nil || out.decided.Status != cur.Status || deref(out.decided.DecidedBy) != deref(cur.DecidedBy) {
		t.Fatalf("the call got %+v; the row is %+v", out.decided, cur)
	}
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}

// A run cancelled while its call waits for the answer ends with the question closed "step
// finished" (the visit ends with it pending, rule 5); the waiting call is released.
func TestCancelWhileTheCallWaits(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() { asked <- env.askAs(p, "Which one?", nil, 10*time.Second) }()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	eventually(t, "the question", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	q := pendingApproval(t, env, run.ID)
	if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
		t.Fatal(err)
	}
	if r := env.wait(done, run.ID); r.Status != store.RunCancelled {
		t.Fatalf("run = %s", r.Status)
	}
	out := recv(t, asked, "the call")
	if out.decided == nil || out.decided.Status != store.ApprovalExpired || out.decided.ID != q.ID {
		t.Fatalf("the waiting call got %+v (%v)", out.decided, out.waitErr)
	}
	if cur := env.approvalRow(q.ID); deref(cur.Reason) != "step finished" {
		t.Fatalf("question = %s %q", cur.Status, deref(cur.Reason))
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// Rule 3: the answer is redacted, trimmed and bounded; an approval of another kind is
// ErrNotQuestion; approving a question says nothing and is refused.
func TestAnswerQuestionValidation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			asked <- env.askAs(p, "The key?", nil, 10*time.Second)
			finishThenFinal(t, env, p, `{"summary":"ok"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	eventually(t, "the question", func() bool {
		p, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
		return len(p) == 1
	})
	q := pendingApproval(t, env, run.ID)
	if len(q.Options) != 0 {
		t.Fatalf("a free-text question has options %s", q.Options)
	}

	// A NUL or invalid UTF-8 is refused with the same text (review L4): the insert would
	// fail with a database error the operator cannot act on.
	for _, bad := range []string{"", "   \n", strings.Repeat("x", 4097), "a\x00b", "\xff\xfe"} {
		var ve *ValidationError
		if _, err := env.e.AnswerQuestion(ctx, q.ID, bad, "api"); !errors.As(err, &ve) || ve.Messages[0] != "answer: 1 to 4096 characters" {
			t.Fatalf("answer %.10q: %v", bad, err)
		}
	}
	if _, err := env.e.AnswerQuestion(ctx, "no-such-id", "x", "api"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	var ve *ValidationError
	if _, err := env.e.DecideApproval(ctx, q.ID, DecisionApprove, "api", ""); !errors.As(err, &ve) || ve.Messages[0] != "answer: a question needs an answer" {
		t.Fatalf("approve on a question: %v", err)
	}
	if _, err := env.e.DecideApproval(ctx, q.ID, DecisionApproveAlways, "api", ""); !errors.As(err, &ve) {
		t.Fatalf("approve_always on a question: %v", err)
	}
	if cur := env.approvalRow(q.ID); cur.Status != store.ApprovalPending {
		t.Fatalf("a refused answer changed the question: %+v", cur)
	}

	// A tool approval is not a question.
	tool := &store.Approval{RunID: run.ID, RunNodeID: q.RunNodeID, Kind: store.ApprovalKindTool, NodeID: "agent", Message: "deploy",
		RequestedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := store.InsertApproval(ctx, env.pool, tool); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.AnswerQuestion(ctx, tool.ID, "yes", "api"); !errors.Is(err, ErrNotQuestion) {
		t.Fatalf("answer to a tool approval: %v", err)
	}

	// The answer is redacted and trimmed, and the decider defaults.
	answered, err := env.e.AnswerQuestion(ctx, q.ID, "  use token sk-ant-"+strings.Repeat("z", 24)+"  ", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*answered.Answer, "zzzz") || !strings.HasPrefix(*answered.Answer, "use token ") || deref(answered.DecidedBy) != "user" {
		t.Fatalf("answer = %q by %q", *answered.Answer, deref(answered.DecidedBy))
	}
	out := recv(t, asked, "the call")
	if out.decided == nil || out.decided.Answer == nil || strings.Contains(*out.decided.Answer, "zzzz") {
		t.Fatalf("the call got %+v", out.decided)
	}
	env.wait(done, run.ID)
}

// ---- T5.24.3 ---------------------------------------------------------------------------

// T5.24.3: a decline within the wait returns the decision with the operator's reason, and
// an expiry within the wait (a timeout shorter than the wait) returns it expired: either
// way the agent goes on and the run is not failed (D-059, rule 8).
func TestQuestionDeclinedAndExpiredWithinTheWait(t *testing.T) {
	for _, tc := range []string{"declined", "expired"} {
		t.Run(tc, func(t *testing.T) {
			env := newEnv(t)
			ctx := context.Background()
			ag := env.questionAgent()
			asked := make(chan askOutcome, 1)
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				go func() {
					asked <- env.askAs(p, "Which database?", nil, 10*time.Second)
					finishThenFinal(t, env, p, `{"summary":"went on"}`)
				}()
				return startedTurn(p)
			}
			req := TriggerRequest{AgentID: ag.ID, Task: "go"}
			if tc == "expired" {
				req.Limits = store.Limits{ApprovalTimeoutSeconds: 1}
			}
			run, done := env.start(req)
			eventually(t, "the question", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
			q := pendingApproval(t, env, run.ID)
			if tc == "declined" {
				decided, err := env.e.DecideApproval(ctx, q.ID, DecisionReject, "api", "not now")
				if err != nil || decided.Status != store.ApprovalRejected || deref(decided.Reason) != "not now" || decided.Answer != nil {
					t.Fatalf("decline: %+v %v", decided, err)
				}
			} else {
				if !q.ExpiresAt.Equal(q.RequestedAt.Add(time.Second)) {
					t.Fatalf("expires_at = %v, requested at %v", q.ExpiresAt, q.RequestedAt)
				}
				env.clock.Advance(time.Second - time.Millisecond)
				env.e.sweepApprovals(ctx)
				if cur := env.approvalRow(q.ID); cur.Status != store.ApprovalPending {
					t.Fatalf("expired a millisecond early: %s", cur.Status)
				}
				env.clock.Advance(time.Millisecond)
				env.e.sweepApprovals(ctx)
			}
			out := recv(t, asked, "the call")
			want := store.ApprovalRejected
			if tc == "expired" {
				want = store.ApprovalExpired
			}
			if out.decided == nil || out.decided.Status != want {
				t.Fatalf("the call got %+v (wait error %v)", out.decided, out.waitErr)
			}
			if tc == "declined" && deref(out.decided.Reason) != "not now" {
				t.Fatalf("reason = %q", deref(out.decided.Reason))
			}
			r := env.wait(done, run.ID)
			if r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q; a question that nobody answers does not fail a run whose agent goes on", r.Status, deref(r.FailureReason))
			}
			cur := env.approvalRow(q.ID)
			if tc == "expired" && (deref(cur.Reason) != "approval expired" || deref(cur.DecidedBy) != "platform") {
				t.Fatalf("expired question = %+v", cur)
			}
			if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
				t.Fatalf("visits = %v", got)
			}
		})
	}
}

// ---- T5.24.4 ---------------------------------------------------------------------------

// parkRig scripts a visit that asks, is told pending, saves a file and ends its turn
// without finish; every later visit finishes at once. It records what each send carried.
type parkRig struct {
	env      *testEnv
	token    string
	snt      sends
	asked    chan askOutcome
	notes    sync.Map // send number → content of runs/<run>/notes.md when the turn started
	question string
	options  []string
	results  []string // results[k] is what the visit of send k+2 (the first after the park) finishes with
}

func newParkRig(t *testing.T, env *testEnv, question string, options []string, results ...string) *parkRig {
	r := &parkRig{env: env, asked: make(chan askOutcome, 4), question: question, options: options, results: results}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := r.snt.add(p)
		go r.turn(t, p, i)
		return startedTurn(p)
	}
	return r
}

func (r *parkRig) notesPath(p openclaw.ChatSendParams) string {
	rc, ok := r.env.e.ResolveRunToken(tokenIn(p.Message))
	if !ok {
		return ""
	}
	return filepath.Join(openclaw.WorkspaceDir(r.env.dir, p.AgentID), "runs", rc.RunID, "notes.md")
}

func (r *parkRig) turn(t *testing.T, p openclaw.ChatSendParams, i int) {
	if i == 1 {
		r.token = tokenIn(p.Message)
		if path := r.notesPath(p); path != "" {
			_ = os.WriteFile(path, []byte("progress so far"), 0o644)
		}
		out := r.env.askAs(p, r.question, r.options, 40*time.Millisecond)
		r.asked <- out
		r.env.endTurn(p, "Saved my progress; waiting for the operator's answer.")
		return
	}
	if path := r.notesPath(p); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			r.notes.Store(i, string(b))
		}
	}
	result := `{"summary":"done"}`
	if i-2 < len(r.results) {
		result = r.results[i-2]
	}
	finishThenFinal(t, r.env, p, result)
}

// T5.24.4: the wait ends (pending, the run back to running); the turn ends without finish:
// the visit parks (run and node waiting_approval, parked_at, the worker free, the root span
// open); the answer re-queues the run and closes the parked visit; the continuation visit
// names the question, carries the paragraph, finds the agent's files, and is free of the
// loop limit that the parked visit used up; the paused time does not count.
func TestQuestionParksAfterTheWaitAndContinues(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which greeting language?", []string{"English", "Portuguese"}, `{"summary":"greeted in Portuguese"}`)
	// max_loop_visits 1: the parked visit uses it up, so only a free continuation can run.
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "greet", Limits: store.Limits{MaxLoopVisits: 1}})
	out := recv(t, rig.asked, "the call")
	if out.askErr != nil || out.decided != nil || !errors.Is(out.waitErr, context.DeadlineExceeded) || out.parkErr != nil || out.row == nil {
		t.Fatalf("the call = %+v", out)
	}
	r := env.wait(done, run.ID) // the worker lets the run go
	if r.Status != store.RunWaitingApproval || r.PausedAt == nil || r.FinishedAt != nil {
		t.Fatalf("run after the park = %s paused_at %v finished_at %v", r.Status, r.PausedAt, r.FinishedAt)
	}
	node := runNodes(t, env, run.ID)[0]
	if node.Status != store.NodeWaitingApproval || node.ParkedAt == nil || node.FinishedAt != nil ||
		r.ResumeNodeVisitID == nil || *r.ResumeNodeVisitID != node.ID {
		t.Fatalf("parked visit = %+v; run resumes at %v", node, r.ResumeNodeVisitID)
	}
	q := env.approvalRow(out.row.ID)
	if q.Status != store.ApprovalPending {
		t.Fatalf("question after the park = %s", q.Status)
	}
	if env.activeVisit(run.ID) {
		t.Fatal("the engine still holds the parked visit")
	}
	if _, live := env.e.ResolveRunToken(rig.token); live {
		t.Fatal("the parked visit's run token is still valid")
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='run' AND parent_id IS NULL AND ended_at IS NULL`, run.ID); n != 1 {
		t.Fatalf("open root spans = %d; the worker that resumes the run continues it", n)
	}
	if n := env.openSpans(run.ID); n != 2 { // the root span and the question's approval_wait
		t.Fatalf("open spans = %d, want the root span and the question's wait", n)
	}
	if n := env.logged(run.ID, "waits for the answer to question "+q.ID+"; the step continues when it arrives"); n != 1 {
		t.Fatalf("park log lines = %d", n)
	}
	if keys := env.fc.callsFor("sessions.delete"); len(keys) != 1 || !strings.Contains(string(keys[0]), rig.snt.at(0).SessionKey) {
		t.Fatalf("sessions.delete calls = %s; the parked visit's session is deleted when the worker lets go", keys)
	}
	if rig.snt.len() != 1 {
		t.Fatalf("chat.send calls = %d", rig.snt.len())
	}

	env.clock.Advance(3 * time.Hour) // the park does not count toward the 600 s time limit
	answered, err := env.e.AnswerQuestion(ctx, q.ID, "Portuguese", "api")
	if err != nil || answered.Status != store.ApprovalApproved {
		t.Fatalf("answer: %+v %v", answered, err)
	}
	r, _ = store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunQueued || r.PausedAt != nil || r.PausedTotalMs < 3*3_600_000 {
		t.Fatalf("run after the answer = %s paused_at %v paused_total=%d", r.Status, r.PausedAt, r.PausedTotalMs)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits after the answer = %v; the parked visit is closed succeeded", got)
	}
	if node := runNodes(t, env, run.ID)[0]; node.FinishedAt == nil || node.FailureReason != nil || node.ParkedAt == nil {
		t.Fatalf("closed parked visit = %+v", node)
	}
	if n := env.logged(run.ID, "question answered; continuing agent"); n != 1 {
		t.Fatalf("continue log lines = %d", n)
	}

	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	nodes := runNodes(t, env, run.ID)
	if nodes[0].QuestionID != nil || nodes[1].QuestionID == nil || *nodes[1].QuestionID != q.ID {
		t.Fatalf("question_id: visit 1 %v, visit 2 %v; want only the continuation to name %s", nodes[0].QuestionID, nodes[1].QuestionID, q.ID)
	}
	if loop, _ := store.CountNodeLoopVisits(ctx, env.pool, run.ID, "agent"); loop != 1 {
		t.Fatalf("loop visits = %d, want 1", loop)
	}
	if all, _ := store.CountNodeVisits(ctx, env.pool, run.ID, "agent"); all != 2 {
		t.Fatalf("visits = %d, want 2", all)
	}
	first, second := rig.snt.at(0).Message, rig.snt.at(1).Message
	para := answeredBlock("Which greeting language?", "Portuguese")
	if strings.Contains(first, "Earlier in this step") {
		t.Fatalf("the first visit's task has the paragraph:\n%s", first)
	}
	if !strings.HasPrefix(second, "greet\n\n"+para+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the continuation's task is not the task, the paragraph, then the protocol block:\n%s", second)
	}
	if got, _ := rig.notes.Load(2); got != "progress so far" {
		t.Fatalf("notes.md at the continuation = %v; the agent's files must be there", got)
	}
	if rig.snt.at(1).SessionKey == rig.snt.at(0).SessionKey {
		t.Fatal("the continuation reused the parked visit's session")
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if st := stateOf(t, r); st["agent"] == nil {
		t.Fatalf("state = %v; the continuation's result is the step's", st)
	}
}

// A retry of a continuation counts the step's loop visits without the continuation
// (5.21 rule 2 amended): max_loop_visits 2, the parked visit and one retry.
func TestRetryCountsLoopVisitsWithoutTheContinuation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		switch n {
		case 1:
			go func() {
				env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				env.endTurn(p, "waiting")
			}()
		default:
			go env.endTurn(p, "no result, no finish") // the continuation fails: agent did not call finish
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go", Limits: store.Limits{MaxLoopVisits: 2}})
	env.wait(done, run.ID)
	q := env.questionsOf(run.ID)[0]
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "agent did not call finish" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:failed"}) {
		t.Fatalf("visits = %v", got)
	}
	// Loop visits: 1 (the parked one). Counting the continuation too (2 of 2) would refuse the retry.
	retried, err := env.e.Retry(ctx, run.ID, "andre", nil)
	if err != nil {
		t.Fatalf("retry: %v; the continuation visit must not count toward max_loop_visits", err)
	}
	if retried.Status != store.RunQueued {
		t.Fatalf("retried run = %s", retried.Status)
	}
}

// ---- T5.24.5 ---------------------------------------------------------------------------

// T5.24.5: the decision arrives after the wait and before the turn ends: the visit keeps
// it, and the turn's end sends it as the next turn in the same session, with the answer
// paragraph and the closing sentence. Nothing parks; the agent keeps its context.
func TestQuestionDecidedAfterTheWaitBeforeTheTurnEnds(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	var snt sends
	var token string
	asked := make(chan askOutcome, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				token = tokenIn(p.Message)
				asked <- env.askAs(p, "Which greeting language?", []string{"English", "Portuguese"}, 40*time.Millisecond)
				<-release
				env.endTurn(p, "Waiting for the operator.")
				return
			}
			finishTurn(t, env, p, token, `{"summary":"greeted"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "greet"})
	out := recv(t, asked, "the call")
	if out.decided != nil || out.parkErr != nil {
		t.Fatalf("the call = %+v", out)
	}
	// The wait ended: the run and the node are back to running, the question stays pending.
	if s := runStatus(t, env, run.ID); s != store.RunRunning {
		t.Fatalf("run after the wait = %s", s)
	}
	if n := runNodes(t, env, run.ID)[0]; n.Status != store.NodeRunning {
		t.Fatalf("node after the wait = %s", n.Status)
	}
	if cur := env.approvalRow(out.row.ID); cur.Status != store.ApprovalPending {
		t.Fatalf("question after the wait = %s", cur.Status)
	}
	if _, err := env.e.AnswerQuestion(ctx, out.row.ID, "Portuguese", "api"); err != nil {
		t.Fatal(err)
	}
	close(release)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if snt.len() != 2 {
		t.Fatalf("chat.send calls = %d, want the task and one next turn", snt.len())
	}
	first, next := snt.at(0), snt.at(1)
	want := answeredBlock("Which greeting language?", "Portuguese") + "\n\nContinue your task and call yuno__finish when done."
	if next.Message != want {
		t.Fatalf("next turn's message:\n%s\nwant:\n%s", next.Message, want)
	}
	if next.SessionKey != first.SessionKey || next.IdempotencyKey == first.IdempotencyKey {
		t.Fatalf("next turn: session %q key %q; first: %q %q; want the same session and a new key", next.SessionKey, next.IdempotencyKey, first.SessionKey, first.IdempotencyKey)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits = %v; nothing parked", got)
	}
	if n := runNodes(t, env, run.ID)[0]; n.ParkedAt != nil {
		t.Fatalf("visit parked at %v", n.ParkedAt)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// The same, when the question expires after the wait and before the turn ends: the next
// turn carries the unanswered line, and the run is not failed (rule 8: a live visit past the
// wait gets nothing more).
func TestQuestionExpiredAfterTheWaitBeforeTheTurnEnds(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	var snt sends
	var token string
	asked := make(chan askOutcome, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				token = tokenIn(p.Message)
				asked <- env.askAs(p, "Which greeting language?", nil, 40*time.Millisecond)
				<-release
				env.endTurn(p, "Waiting for the operator.")
				return
			}
			finishTurn(t, env, p, token, `{"summary":"assumed English"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "greet", Limits: store.Limits{ApprovalTimeoutSeconds: 30}})
	out := recv(t, asked, "the call")
	env.clock.Advance(31 * time.Second)
	env.e.sweepApprovals(ctx)
	if cur := env.approvalRow(out.row.ID); cur.Status != store.ApprovalExpired {
		t.Fatalf("question = %s", cur.Status)
	}
	if s := runStatus(t, env, run.ID); s != store.RunRunning {
		t.Fatalf("run after the expiry = %s; a live visit past the wait is left alone", s)
	}
	close(release)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := "Earlier in this step you asked the operator: \"Which greeting language?\"\nThe operator did not answer in time." + proceedText +
		"\n\nContinue your task and call yuno__finish when done."
	if snt.len() != 2 || snt.at(1).Message != want {
		t.Fatalf("sends = %d; next turn's message:\n%s\nwant:\n%s", snt.len(), snt.at(1).Message, want)
	}
}

// ---- the race between the turn's end and a decision --------------------------------------

// decideInGoroutine answers the run's pending question from another goroutine, as the API
// does, and returns a channel that holds the result of the call.
func decideInGoroutine(env *testEnv, id, answer string) <-chan error {
	res := make(chan error, 1)
	go func() {
		_, err := env.e.AnswerQuestion(context.Background(), id, answer, "api")
		res <- err
	}()
	return res
}

// awaitDecided waits until the question's row is decided, the moment before its delivery. It
// runs on the visit goroutine, so it reports with Errorf.
func awaitDecided(t *testing.T, env *testEnv, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for env.approvalRow(id).Status == store.ApprovalPending {
		if time.Now().After(deadline) {
			t.Errorf("question %s was not decided", id)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // the delivery reaches the live visit and blocks on its loop
}

// A decision recorded before the turn's end reads the question, but handed over only after:
// the visit finds it in the row, sends the next turn once, and drops the late handoff.
func TestDecisionRecordedBeforeTheTurnEndReadsTheQuestion(t *testing.T) {
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
	var answerResult <-chan error
	var once sync.Once
	setQuestionHook(t, func(stage string) {
		if stage != "read" {
			return
		}
		once.Do(func() {
			// On the visit goroutine, as its turn ends: the decision is recorded here, and its
			// delivery to the live visit waits for this very loop.
			q := env.questionsOf(env.runIDOfOnlyRun())[0]
			answerResult = decideInGoroutine(env, q.ID, "this one")
			awaitDecided(t, env, q.ID)
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	recv(t, asked, "the call")
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if err := recv(t, answerResult, "the answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if snt.len() != 2 {
		t.Fatalf("chat.send calls = %d; the decision must produce one next turn, not two", snt.len())
	}
	if want := answeredBlock("Which one?", "this one") + "\n\nContinue your task and call yuno__finish when done."; snt.at(1).Message != want {
		t.Fatalf("next turn's message:\n%s", snt.at(1).Message)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
}

// runIDOfOnlyRun is the id of the test's one run.
func (env *testEnv) runIDOfOnlyRun() string {
	env.t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(), `SELECT id FROM runs ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		env.t.Fatal(err)
	}
	return id
}

// The decision lands after the visit read its question pending and before the park commits
// (Consistency): the answer's delivery to the live visit waits for the visit's loop, the
// visit parks and ends, and the delivery then finds the park and re-queues the run. Nothing
// is lost: the run is queued, the parked visit closed, and a continuation visit finishes.
func TestDecisionBetweenTheReadAndThePark(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which one?", nil, `{"summary":"done with the answer"}`)
	var answerResult <-chan error
	var once sync.Once
	setQuestionHook(t, func(stage string) {
		if stage != "park" {
			return
		}
		once.Do(func() {
			q := env.questionsOf(env.runIDOfOnlyRun())[0]
			if q.Status != store.ApprovalPending {
				t.Errorf("the question was decided before the hook: %s", q.Status)
			}
			answerResult = decideInGoroutine(env, q.ID, "this one")
			awaitDecided(t, env, q.ID)
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	recv(t, rig.asked, "the call")
	env.wait(done, run.ID)
	if err := recv(t, answerResult, "the answer"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunQueued || r.PausedAt != nil {
		t.Fatalf("run = %s paused_at %v; the decision made while the visit parked must re-queue it", r.Status, r.PausedAt)
	}
	nodes := runNodes(t, env, run.ID)
	if len(nodes) != 1 || nodes[0].Status != store.NodeSucceeded || nodes[0].ParkedAt == nil || nodes[0].FinishedAt == nil {
		t.Fatalf("parked visit = %+v", nodes)
	}
	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes = runNodes(t, env, run.ID)
	if len(nodes) != 2 || nodes[1].QuestionID == nil || !strings.Contains(rig.snt.at(1).Message, answeredBlock("Which one?", "this one")) {
		t.Fatalf("continuation = %+v\n%s", nodes, rig.snt.at(1).Message)
	}
}

// The decision lands the moment after the park commits, while the visit is still handing
// the run back: it re-queues the run at once, and what the visit saves on its way out does
// not reopen the parked visit's row.
func TestDecisionRightAfterThePark(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which one?", nil, `{"summary":"done"}`)
	var answerErr error
	var once sync.Once
	env.onEvent(func(typ string, data json.RawMessage) {
		if typ != "log" || !strings.Contains(string(data), "waits for the answer to question") {
			return
		}
		once.Do(func() {
			// On the visit goroutine, after ParkVisit committed and before the visit ended.
			q := env.questionsOf(env.runIDOfOnlyRun())[0]
			_, answerErr = env.e.AnswerQuestion(ctx, q.ID, "this one", "api")
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	recv(t, rig.asked, "the call")
	env.wait(done, run.ID)
	if answerErr != nil {
		t.Fatalf("answer: %v", answerErr)
	}
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunQueued {
		t.Fatalf("run = %s; the decision right after the park must re-queue it", r.Status)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits = %v; the visit's last save reopened the closed row", got)
	}
	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if nodes := runNodes(t, env, run.ID); len(nodes) != 2 || nodes[1].QuestionID == nil {
		t.Fatalf("visits = %v", visitsOf(nodes))
	}
}

// ---- T5.24.6 ---------------------------------------------------------------------------

// T5.24.6: the agent is told pending, goes on and calls finish: the question expires with
// "step finished", nothing parks, and a later answer is refused.
func TestQuestionExpiresWhenTheStepFinishes(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	asked := make(chan askOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
			finishThenFinal(t, env, p, `{"summary":"decided myself"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	out := recv(t, asked, "the call")
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	q := env.approvalRow(out.row.ID)
	if q.Status != store.ApprovalExpired || deref(q.Reason) != "step finished" || deref(q.DecidedBy) != "platform" || q.Answer != nil {
		t.Fatalf("question = %+v", q)
	}
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "late", "api"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late answer: %v", err)
	}
	if _, err := env.e.DecideApproval(ctx, q.ID, DecisionReject, "api", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late decline: %v", err)
	}
	if n := runNodes(t, env, run.ID)[0]; n.ParkedAt != nil {
		t.Fatalf("a finished visit parked: %+v", n)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if got, _ := store.ListPendingApprovals(ctx, env.pool, run.ID); len(got) != 0 {
		t.Fatalf("pending approvals left: %+v", got)
	}
}

// Every other end of a visit with its question pending closes it the same way: a result
// taken from the reply, an error and a cancel (rule 5).
func TestQuestionExpiresWhenTheVisitEndsAnyOtherWay(t *testing.T) {
	ctx := context.Background()
	t.Run("a result in the reply", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		asked := make(chan askOutcome, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() {
				asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				env.endTurn(p, `Here is my result: {"summary":"from the reply"}`)
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, asked, "the call")
		if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q; a result in the reply wins over the park", r.Status, deref(r.FailureReason))
		}
		if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired || deref(q.Reason) != "step finished" {
			t.Fatalf("question = %+v", q)
		}
	})
	t.Run("a cancel", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		asked := make(chan askOutcome, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() { asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond) }()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, asked, "the call")
		if _, err := env.e.Cancel(ctx, run.ID, "andre"); err != nil {
			t.Fatal(err)
		}
		if r := env.wait(done, run.ID); r.Status != store.RunCancelled {
			t.Fatalf("run = %s", r.Status)
		}
		if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired || deref(q.Reason) != "step finished" {
			t.Fatalf("question = %+v", q)
		}
	})
	t.Run("a turn that fails", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		asked := make(chan askOutcome, 1)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			go func() {
				asked <- env.askAs(p, "Which one?", nil, 40*time.Millisecond)
				env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{"errorKind": "invalid_request"}))
			}()
			return startedTurn(p)
		}
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, asked, "the call")
		if r := env.wait(done, run.ID); r.Status != store.RunFailed {
			t.Fatalf("run = %s", r.Status)
		}
		if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired || deref(q.Reason) != "step finished" {
			t.Fatalf("question = %+v", q)
		}
	})
}

// ---- T5.24.7 ---------------------------------------------------------------------------

// T5.24.7: a parked question expires: the sweeper fails the run with "question unanswered"
// and the parked visit's row fails with the same reason; the run is retryable, and the
// retried visit (not a continuation) carries the paragraph with the unanswered line and may
// ask again.
func TestParkedQuestionExpiresAndTheRetryCarriesTheUnansweredLine(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which greeting language?", nil, `{"summary":"assumed English"}`)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "greet", Limits: store.Limits{ApprovalTimeoutSeconds: 60}})
	out := recv(t, rig.asked, "the call")
	env.wait(done, run.ID)

	env.clock.Advance(59 * time.Second)
	env.e.sweepApprovals(ctx)
	if r, _ := store.GetRun(ctx, env.pool, run.ID); r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s a second early", r.Status)
	}
	env.clock.Advance(2 * time.Second)
	env.e.sweepApprovals(ctx)
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "question unanswered" || r.FinishedAt == nil {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	q := env.approvalRow(out.row.ID)
	if q.Status != store.ApprovalExpired || deref(q.Reason) != "approval expired" {
		t.Fatalf("question = %s %q", q.Status, deref(q.Reason))
	}
	node := runNodes(t, env, run.ID)[0]
	if node.Status != store.NodeFailed || deref(node.FailureReason) != "question unanswered" || node.ParkedAt == nil {
		t.Fatalf("parked visit = %+v", node)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "late", "api"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late answer: %v", err)
	}
	// A second sweep changes nothing.
	env.e.sweepApprovals(ctx)

	retried, err := env.e.Retry(ctx, run.ID, "andre", nil)
	if err != nil || retried.Status != store.RunQueued {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("retried run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:failed", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if nodes[1].QuestionID != nil {
		t.Fatalf("the retried visit names question %s; a retry continues nothing", *nodes[1].QuestionID)
	}
	want := "Earlier in this step you asked the operator: \"Which greeting language?\"\nThe operator did not answer in time." + proceedText
	if m := rig.snt.at(1).Message; !strings.HasPrefix(m, "greet\n\n"+want+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the retry's task:\n%s", m)
	}
}

// ---- T5.24.8 ---------------------------------------------------------------------------

// T5.24.8: a run cancelled while parked expires the question with "run cancelled" and fails
// the parked visit.
func TestCancelWhileParked(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which one?", nil)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	out := recv(t, rig.asked, "the call")
	env.wait(done, run.ID)

	cancelled, err := env.e.Cancel(ctx, run.ID, "andre")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != store.RunCancelled || deref(cancelled.FailureReason) != "cancelled by andre" {
		t.Fatalf("run = %s %q", cancelled.Status, deref(cancelled.FailureReason))
	}
	q := env.approvalRow(out.row.ID)
	if q.Status != store.ApprovalExpired || deref(q.Reason) != "run cancelled" {
		t.Fatalf("question = %s %q", q.Status, deref(q.Reason))
	}
	node := runNodes(t, env, run.ID)[0]
	if node.Status != store.NodeFailed || deref(node.FailureReason) != "cancelled by andre" {
		t.Fatalf("parked visit = %+v", node)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if _, err := env.e.AnswerQuestion(ctx, q.ID, "late", "api"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("late answer: %v", err)
	}
}

// A cancel that came in while the visit parked is applied when the worker lets the run go:
// nothing else would see the flag.
func TestCancelRequestedWhileTheVisitParks(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	rig := newParkRig(t, env, "Which one?", nil)
	var once sync.Once
	env.onEvent(func(typ string, data json.RawMessage) {
		if typ != "log" || !strings.Contains(string(data), "waits for the answer to question") {
			return
		}
		once.Do(func() {
			// The parked run is waiting_approval and the visit still registered: Cancel only
			// flags it, as it does for any visit of the engine.
			if err := store.RequestCancel(ctx, env.pool, env.runIDOfOnlyRun(), "andre"); err != nil {
				t.Errorf("flag the cancel: %v", err)
			}
		})
	})
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	out := recv(t, rig.asked, "the call")
	r := env.wait(done, run.ID)
	if r.Status != store.RunCancelled {
		t.Fatalf("run = %s %q; the flag set while the visit parked must be applied", r.Status, deref(r.FailureReason))
	}
	if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired || deref(q.Reason) != "run cancelled" {
		t.Fatalf("question = %s %q", q.Status, deref(q.Reason))
	}
}

// ---- T5.24.9 ---------------------------------------------------------------------------

// T5.24.9: a second question while one of the visit is pending, and the fourth of the step,
// are refused with their texts; an unknown or finished token is invalid run token.
func TestQuestionCaps(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	type result struct {
		token  string
		second error
		fourth error
		first  *store.Approval
	}
	res := make(chan result, 2)
	answered := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			r := result{token: tokenIn(p.Message)}
			// The wait ends with the first pending: a second question is refused.
			one := env.askWithToken(r.token, "first?", nil, 40*time.Millisecond)
			r.first = one.row
			r.second = env.askWithToken(r.token, "second?", nil, time.Second).askErr
			res <- r
			<-answered
			for _, q := range []string{"second?", "third?"} {
				if out := env.askWithToken(r.token, q, nil, 10*time.Second); out.askErr != nil || out.decided == nil {
					t.Errorf("question %q: %+v", q, out)
					break
				}
			}
			r.fourth = env.askWithToken(r.token, "fourth?", nil, time.Second).askErr
			res <- r
			finishThenFinal(t, env, p, `{"summary":"enough"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	first := recv(t, res, "the first question")
	var me *tools.ModelError
	if !errors.As(first.second, &me) || me.Message != "one question at a time: question "+first.first.ID+" is still pending" {
		t.Fatalf("second question: %v", first.second)
	}
	if got := env.questionsOf(run.ID); len(got) != 1 {
		t.Fatalf("questions after the refused one = %d", len(got))
	}
	if _, err := env.e.AnswerQuestion(ctx, first.first.ID, "one", "api"); err != nil {
		t.Fatal(err)
	}
	close(answered)
	// The second and the third are asked and answered within their waits.
	for _, want := range []string{"second?", "third?"} {
		eventually(t, want, func() bool {
			p, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
			return len(p) == 1 && p[0].Message == want
		})
		if _, err := env.e.AnswerQuestion(ctx, pendingApproval(t, env, run.ID).ID, "answer", "api"); err != nil {
			t.Fatal(err)
		}
	}
	last := recv(t, res, "the fourth question")
	if !errors.As(last.fourth, &me) || me.Message != "ask fewer questions: this step already asked 3" {
		t.Fatalf("fourth question: %v", last.fourth)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := env.questionsOf(run.ID); len(got) != 3 {
		t.Fatalf("questions = %d, want 3 (the refused ones are not rows)", len(got))
	}
	if _, err := env.e.AskOperator(ctx, "r_not_a_token", "x?", nil); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := env.e.AskOperator(ctx, last.token, "x?", nil); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("finished token: %v", err)
	}
	if err := env.e.QuestionPending(ctx, last.token, last.first.ID); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("QuestionPending with a finished token: %v", err)
	}
	if err := env.e.QuestionPending(ctx, "r_not_a_token", last.first.ID); !errors.Is(err, ErrInvalidRunToken) {
		t.Fatalf("QuestionPending with an unknown token: %v", err)
	}
}

// Two ask_operator calls of one turn issued together: the visit's lock serializes them, one
// is recorded and the other is refused with the first's id; never two pending questions.
func TestTwoQuestionsAtOnceOnlyOneIsRecorded(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	outs := make(chan askOutcome, 8)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			token := tokenIn(p.Message)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					outs <- env.askWithToken(token, []string{"a?", "b?", "c?", "d?"}[i], nil, 60*time.Millisecond)
				}()
			}
			close(start)
			wg.Wait()
			finishThenFinal(t, env, p, `{"summary":"ok"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
	var recorded []*store.Approval
	var refused int
	for range 4 {
		out := recv(t, outs, "a call")
		var me *tools.ModelError
		switch {
		case out.askErr == nil:
			recorded = append(recorded, out.row)
		case errors.As(out.askErr, &me) && strings.HasPrefix(me.Message, "one question at a time: question "):
			refused++
		default:
			t.Fatalf("call: %+v", out)
		}
	}
	if len(recorded) != 1 || refused != 3 {
		t.Fatalf("recorded %d, refused %d; want 1 and 3", len(recorded), refused)
	}
	if got := env.questionsOf(run.ID); len(got) != 1 || got[0].ID != recorded[0].ID {
		t.Fatalf("questions = %+v", got)
	}
	env.wait(done, run.ID)
	_ = ctx
}

// ---- rule 5 (d): the api restarts ---------------------------------------------------------

// The api restarts while a question's call blocks: the call dies with the process, the run
// stays waiting_approval with the question pending; the answer, made on the new process with
// nothing to hand it to, re-queues the run, and the new visit's task lists the question and
// its answer.
func TestAnswerAfterRestartRequeues(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	callDone := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			defer close(callDone)
			token := tokenIn(p.Message)
			row, err := env.e.AskOperator(ctx, token, "Which one?", []string{"a", "b"})
			if err != nil {
				t.Errorf("ask: %v", err)
				return
			}
			_, _ = env.e.WaitQuestion(waitCtx, row.ID)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: ag.ID, Task: "go"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	wctx, shutdown := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	q := pendingApproval(t, env, run.ID)
	shutdown()
	recv(t, done, "the worker to stop")
	stopWaiting()
	recv(t, callDone, "the call to end")

	e2 := New(env.e.deps)
	e2.tickInterval = env.e.tickInterval
	env.e = e2
	answered, err := e2.AnswerQuestion(ctx, q.ID, "b", "api")
	if err != nil || answered.Status != store.ApprovalApproved {
		t.Fatalf("answer: %+v %v", answered, err)
	}
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("status after the answer = %s; nothing waits, so the run is re-queued", s)
	}
	var snt sends
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		snt.add(p)
		go finishThenFinal(t, env, p, `{"summary":"used b"}`)
		return startedTurn(p)
	}
	r := env.resume(e2, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:failed", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if nodes[1].QuestionID != nil {
		t.Fatalf("the visit after a restart names question %s; it never parked", *nodes[1].QuestionID)
	}
	if m := snt.at(0).Message; !strings.HasPrefix(m, "go\n\n"+answeredBlock("Which one?", "b")+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the new visit's task:\n%s", m)
	}
}

// The question of a run left waiting by a restart expires: the run fails with "question
// unanswered" (nobody can receive the answer).
func TestExpiryAfterRestartFailsTheRun(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	waitCtx, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	callDone := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go func() {
			defer close(callDone)
			row, err := env.e.AskOperator(ctx, tokenIn(p.Message), "Which one?", nil)
			if err != nil {
				t.Errorf("ask: %v", err)
				return
			}
			_, _ = env.e.WaitQuestion(waitCtx, row.ID)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: ag.ID, Task: "go", Limits: store.Limits{ApprovalTimeoutSeconds: 30}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	wctx, shutdown := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	eventually(t, "waiting", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	q := pendingApproval(t, env, run.ID)
	shutdown()
	recv(t, done, "the worker to stop")
	stopWaiting()
	recv(t, callDone, "the call to end")

	e2 := New(env.e.deps)
	env.e = e2
	env.clock.Advance(31 * time.Second)
	e2.sweepApprovals(ctx)
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "question unanswered" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if cur := env.approvalRow(q.ID); cur.Status != store.ApprovalExpired {
		t.Fatalf("question = %s", cur.Status)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:failed"}) {
		t.Fatalf("visits = %v", got)
	}
}

// A visit a lost worker left behind with its question pending is closed when the
// replacement visit starts, and its task lists the question as unanswered.
func TestReplacementVisitClosesTheLostVisitsQuestion(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	first := make(chan askOutcome, 1)
	var snt sends
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				first <- env.askAs(p, "Which one?", nil, 40*time.Millisecond) // pending; the turn then runs on
				return
			}
			finishThenFinal(t, env, p, `{"summary":"done"}`)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: ag.ID, Task: "go"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	wctx, shutdown := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(wctx, claimed)
	}()
	out := recv(t, first, "the call")
	shutdown()
	recv(t, done, "the worker to stop")
	// The worker was lost with the run running: the stale sweeper re-queues it.
	if n, _, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil || n != 1 {
		t.Fatalf("requeue stale: %d %v", n, err)
	}
	e2 := New(env.e.deps)
	e2.tickInterval = env.e.tickInterval
	env.e = e2
	r := env.resume(e2, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired || deref(q.Reason) != "step finished" {
		t.Fatalf("the lost visit's question = %s %q", q.Status, deref(q.Reason))
	}
	if m := snt.at(1).Message; !strings.Contains(m, "Earlier in this step you asked the operator: \"Which one?\"\nThe operator did not answer in time.") {
		t.Fatalf("the replacement visit's task:\n%s", m)
	}
}

// ---- the sweep repeats what a decision could not finish ------------------------------------

// Failure handling: a decision whose re-queue did not happen (here: recorded straight in the
// row) is repeated by the next approval sweep; so is the failure of a run whose parked
// question expired.
func TestSweepRepeatsTheRequeueOfAParkedDecision(t *testing.T) {
	ctx := context.Background()
	t.Run("answered", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		rig := newParkRig(t, env, "Which one?", nil, `{"summary":"done"}`)
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, rig.asked, "the call")
		env.wait(done, run.ID)
		if _, err := store.AnswerApprovalAt(ctx, env.pool, out.row.ID, "this one", "api", env.clock.Now()); err != nil {
			t.Fatal(err)
		}
		if s := runStatus(t, env, run.ID); s != store.RunWaitingApproval {
			t.Fatalf("run = %s", s)
		}
		env.e.sweepApprovals(ctx)
		if s := runStatus(t, env, run.ID); s != store.RunQueued {
			t.Fatalf("run after the sweep = %s", s)
		}
		env.e.sweepApprovals(ctx) // idempotent
		if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if nodes := runNodes(t, env, run.ID); len(nodes) != 2 || nodes[1].QuestionID == nil {
			t.Fatalf("visits = %v", visitsOf(nodes))
		}
	})
	t.Run("declined", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		rig := newParkRig(t, env, "Which one?", nil, `{"summary":"assumed"}`)
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, rig.asked, "the call")
		env.wait(done, run.ID)
		decided, err := env.e.DecideApproval(ctx, out.row.ID, DecisionReject, "api", "ask me later.")
		if err != nil || decided.Status != store.ApprovalRejected {
			t.Fatalf("decline: %+v %v", decided, err)
		}
		if s := runStatus(t, env, run.ID); s != store.RunQueued {
			t.Fatalf("run = %s; a decline continues the step (D-059)", s)
		}
		if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		want := "Earlier in this step you asked the operator: \"Which one?\"\nThe operator declined to answer: ask me later." + proceedText
		if m := rig.snt.at(1).Message; !strings.HasPrefix(m, "go\n\n"+want+"\n\n--- platform protocol ---\n") {
			t.Fatalf("the continuation's task:\n%s", m)
		}
		if nodes := runNodes(t, env, run.ID); len(nodes) != 2 || nodes[1].QuestionID == nil || *nodes[1].QuestionID != out.row.ID {
			t.Fatalf("visits = %v", visitsOf(nodes))
		}
	})
	t.Run("expired", func(t *testing.T) {
		env := newEnv(t)
		ag := env.questionAgent()
		rig := newParkRig(t, env, "Which one?", nil)
		run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "go"})
		out := recv(t, rig.asked, "the call")
		env.wait(done, run.ID)
		if _, err := store.DecideApprovalAt(ctx, env.pool, out.row.ID, store.ApprovalExpired, "platform", "approval expired", env.clock.Now()); err != nil {
			t.Fatal(err)
		}
		env.e.sweepApprovals(ctx)
		r, _ := store.GetRun(ctx, env.pool, run.ID)
		if r.Status != store.RunFailed || deref(r.FailureReason) != "question unanswered" {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
	})
}

// ---- the loop revisit and the paragraph -------------------------------------------------------

// A loop revisit of a step that asked earlier carries the paragraph (the answer is still
// true) and counts toward max_loop_visits as any visit does; it continues no question.
func TestLoopRevisitCarriesTheParagraph(t *testing.T) {
	env := newEnv(t)
	ag := env.questionAgent()
	wf := env.createWorkflow(workflow.Definition{
		Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart},
			{ID: "work", Kind: workflow.KindAgent, AgentID: ag.ID, Task: "work", OutputSchema: anyObject},
			{ID: "end", Kind: workflow.KindEnd}},
		Edges: []workflow.Edge{{From: "start", To: "work"},
			{From: "work", To: "work", When: cond(t, `{"path":"/work/again","op":"eq","value":true}`)},
			{From: "work", To: "end"}},
	})
	var snt sends
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				out := env.askAs(p, "Which one?", nil, 10*time.Second)
				if out.decided == nil {
					t.Errorf("no decision: %+v", out)
				}
				finishThenFinal(t, env, p, `{"again":true}`)
				return
			}
			finishThenFinal(t, env, p, `{"again":false}`)
		}()
		return startedTurn(p)
	}
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := store.ClaimQueuedRun(context.Background(), env.pool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(context.Background(), claimed)
	}()
	eventually(t, "the question", func() bool {
		p, _ := store.ListPendingApprovals(context.Background(), env.pool, run.ID)
		return len(p) == 1
	})
	q := pendingApproval(t, env, run.ID)
	if _, err := env.e.AnswerQuestion(context.Background(), q.ID, "this one", "api"); err != nil {
		t.Fatal(err)
	}
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"work#1:succeeded", "work#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if nodes[1].QuestionID != nil {
		t.Fatalf("a loop revisit names question %s", *nodes[1].QuestionID)
	}
	if first := snt.at(0).Message; strings.Contains(first, "Earlier in this step") {
		t.Fatalf("the first visit's task has the paragraph:\n%s", first)
	}
	if second := snt.at(1).Message; !strings.HasPrefix(second, "work\n\n"+answeredBlock("Which one?", "this one")+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the revisit's task:\n%s", second)
	}
	if loop, _ := store.CountNodeLoopVisits(context.Background(), env.pool, run.ID, "work"); loop != 2 {
		t.Fatalf("loop visits = %d, want 2 (a revisit counts)", loop)
	}
}

// paragraph is the step's answer paragraph, read the way a visit reads it.
func (env *testEnv) paragraph(run *store.Run, node *workflow.Node) string {
	env.t.Helper()
	text, err := env.e.stepAnswers(context.Background(), run.ID, node.ID)
	if err != nil {
		env.t.Fatal(err)
	}
	return text
}

// The paragraph's blocks: oldest first, blank-line separated, one for each decided question
// of the step in this run; none when the step asked nothing or only has a pending one.
func TestAnswerParagraphBlocks(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	ag := env.questionAgent()
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: ag.ID, Task: "go"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rn := &store.RunNode{RunID: run.ID, NodeID: "agent", Status: store.NodeSucceeded, StartedAt: &now}
	if err := store.CreateRunNode(ctx, env.pool, rn); err != nil {
		t.Fatal(err)
	}
	node := &workflow.Node{ID: "agent"}
	if got := env.paragraph(run, node); got != "" {
		t.Fatalf("paragraph of a step that asked nothing = %q", got)
	}
	add := func(i int, question, status string, answer, reason *string) {
		at := now.Add(time.Duration(i) * time.Second)
		a := &store.Approval{RunID: run.ID, RunNodeID: rn.ID, Kind: store.ApprovalKindQuestion, NodeID: "agent", Message: question,
			Status: status, Answer: answer, Reason: reason, RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
		if err := store.InsertApproval(ctx, env.pool, a); err != nil {
			t.Fatal(err)
		}
	}
	text, none := "Portuguese", ""
	reason := "later"
	add(3, "third?", store.ApprovalExpired, nil, nil)
	add(1, "first?", store.ApprovalApproved, &text, nil)
	add(2, "second?", store.ApprovalRejected, nil, &reason)
	add(4, "fourth?", store.ApprovalPending, nil, nil)
	add(5, "fifth?", store.ApprovalRejected, nil, &none)
	got := env.paragraph(run, node)
	want := strings.Join([]string{
		"Earlier in this step you asked the operator: \"first?\"\nThe operator answered: Portuguese",
		"Earlier in this step you asked the operator: \"second?\"\nThe operator declined to answer: later." + proceedText,
		"Earlier in this step you asked the operator: \"third?\"\nThe operator did not answer in time." + proceedText,
		"Earlier in this step you asked the operator: \"fifth?\"\nThe operator declined to answer." + proceedText,
	}, "\n\n")
	if got != want {
		t.Fatalf("paragraph:\n%s\nwant:\n%s", got, want)
	}
	if other := env.paragraph(run, &workflow.Node{ID: "reviewer"}); other != "" {
		t.Fatalf("paragraph of another step = %q", other)
	}
}
