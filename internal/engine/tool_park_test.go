package engine

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// A tool call told pending_approval parks the step (DESIGN 5.3 rule 5 amended, D-085). The
// tests play the MCP gate as the questions' tests play ask_operator: the call records its
// awaiting_approval row, RequestApproval, WaitApproval bounded by a short wait standing for
// MCP_TOOL_WAIT; the agent then ends its turn without finish, as the pending result tells it.

// mergeRequestURL is what the late grant's merge request answers.
const mergeRequestURL = "https://gitlab.example.com/acme/app/-/merge_requests/7"

var mergeRequestURLPattern = regexp.MustCompile(`https://gitlab\.example\.com/[^"\s]+`)

// mergeRequestEffects is the engine's executor of a late grant, as the tools package runs
// it: it opens the merge request and writes the outcome onto the gated row (finishRow).
type mergeRequestEffects struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	ids  []string
}

func (m *mergeRequestEffects) ExecuteApproved(ctx context.Context, toolCallID string) (json.RawMessage, bool, error) {
	m.mu.Lock()
	m.ids = append(m.ids, toolCallID)
	m.mu.Unlock()
	out := json.RawMessage(`{"number":7,"url":"` + mergeRequestURL + `","branch":"yuno/ticket-7"}`)
	now := time.Now().UTC()
	row := &store.ToolCall{ID: toolCallID, Output: out, Status: store.ToolExecuted, Attempt: 1, FinishedAt: &now}
	if _, err := store.CompleteGatedToolCall(ctx, m.pool, row); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

func (m *mergeRequestEffects) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ids)
}

// gatedOutcome is what the MCP gate saw for one open_merge_request call.
type gatedOutcome struct {
	call     *store.ToolCall
	approval *store.Approval
	decided  *store.Approval // the decision within the wait; nil when the wait ended first
	waitErr  error
}

// mergeRequestCall plays the gate for an open_merge_request call of the visit whose first
// message is p, waiting up to wait for the decision.
func (env *testEnv) mergeRequestCall(p openclaw.ChatSendParams, wait time.Duration) gatedOutcome {
	ctx := context.Background()
	var out gatedOutcome
	token := tokenIn(p.Message)
	rc, ok := env.e.ResolveRunToken(token)
	if !ok {
		env.t.Error("token not live")
		return out
	}
	key := "key-" + rc.RunNodeID
	out.call = &store.ToolCall{RunID: rc.RunID, RunNodeID: rc.RunNodeID, ToolUseID: "mcp-" + store.NewID(), ToolName: "open_merge_request",
		Input: json.RawMessage(`{"title":"Ticket 7","draft":true}`), Status: store.ToolAwaitingApproval, IdempotencyKey: &key}
	if err := store.InsertToolCall(ctx, env.pool, out.call); err != nil {
		env.t.Error(err)
		return out
	}
	a, err := env.e.RequestApproval(ctx, ApprovalRequest{Token: token, ToolName: "open_merge_request", ToolCallID: out.call.ID,
		Message: `Open merge request "Ticket 7" on the run's project`, Input: out.call.Input})
	if err != nil {
		env.t.Errorf("RequestApproval: %v", err)
		return out
	}
	out.approval = a
	wctx, cancel := context.WithTimeout(ctx, wait)
	out.decided, out.waitErr = env.e.WaitApproval(wctx, a.ID)
	cancel()
	return out
}

// toolParkRig scripts a visit that calls open_merge_request, is told pending_approval and
// ends its turn without finish; every later visit finishes at once with what its task says
// about the merge request.
type toolParkRig struct {
	env   *testEnv
	snt   sends
	calls chan gatedOutcome
}

func newToolParkRig(t *testing.T, env *testEnv) *toolParkRig {
	r := &toolParkRig{env: env, calls: make(chan gatedOutcome, 1)}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := r.snt.add(p)
		go func() {
			if i == 1 {
				r.calls <- env.mergeRequestCall(p, 40*time.Millisecond)
				env.endTurn(p, "The merge request waits for the operator's approval; ending my turn.")
				return
			}
			finishThenFinal(t, env, p, resultFromTask(p.Message))
		}()
		return startedTurn(p)
	}
	return r
}

// resultFromTask is the result a scripted agent finishes with after reading its task: the
// merge request URL when the task gives one, else what it says about the call.
func resultFromTask(message string) string {
	if url := mergeRequestURLPattern.FindString(message); url != "" {
		return `{"summary":"merge request opened","merge_request_url":"` + url + `"}`
	}
	switch {
	case strings.Contains(message, "It was rejected"):
		return `{"summary":"the operator rejected the merge request","merge_request_url":""}`
	case strings.Contains(message, "It expired"):
		return `{"summary":"nobody approved the merge request in time","merge_request_url":""}`
	}
	return `{"summary":"nothing to report"}`
}

// parked waits for the worker to let the run go and checks the park: the run and the visit
// waiting_approval with parked_at, the approval pending and its row awaiting_approval.
func (r *toolParkRig) parked(t *testing.T, done <-chan struct{}, runID string) gatedOutcome {
	t.Helper()
	env := r.env
	g := recv(t, r.calls, "the gated call")
	if g.approval == nil || g.decided != nil || !errors.Is(g.waitErr, context.DeadlineExceeded) {
		t.Fatalf("the gated call = %+v", g)
	}
	run := env.wait(done, runID)
	node := runNodes(t, env, runID)[0]
	if run.Status != store.RunWaitingApproval || run.ResumeNodeVisitID == nil || *run.ResumeNodeVisitID != node.ID ||
		node.Status != store.NodeWaitingApproval || node.ParkedAt == nil || node.FinishedAt != nil {
		t.Fatalf("after the turn: run %s resumes at %v; visit %+v", run.Status, run.ResumeNodeVisitID, node)
	}
	if a := env.approvalRow(g.approval.ID); a.Status != store.ApprovalPending || a.Kind != store.ApprovalKindTool {
		t.Fatalf("approval after the park = %+v", a)
	}
	if row, _ := store.GetToolCall(context.Background(), env.pool, g.call.ID); row.Status != store.ToolAwaitingApproval {
		t.Fatalf("tool call after the park = %s", row.Status)
	}
	if env.activeVisit(runID) {
		t.Fatal("the engine still holds the parked visit")
	}
	eventually(t, "the park log line", func() bool {
		return env.logged(runID, "waits for the decision on approval "+g.approval.ID+"; the step continues when it arrives") == 1
	})
	return g
}

// outcomeBlock is the paragraph that tells the agent how its open_merge_request call ended.
func outcomeBlock(approvalID, ending string) string {
	return "Earlier in this step you called yuno__open_merge_request, which needed a human approval (approval " + approvalID + "). " + ending
}

// The approval of a call told pending_approval comes after the agent ended its turn: the step
// parks (run waiting_approval, worker free); the grant runs the effect, re-queues the run
// and closes the parked visit; the continuation visit names the approval, is free of the
// loop limit the parked visit used up, and its task carries the call's result, which the
// agent finishes with: the merge request URL reaches the step's output.
func TestToolApprovalParksAndTheGrantContinuesTheStep(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	rig := newToolParkRig(t, env)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7", Limits: store.Limits{MaxLoopVisits: 1}})
	g := rig.parked(t, done, run.ID)
	if len(eff.calls()) != 0 {
		t.Fatal("executed before the approval")
	}

	env.clock.Advance(2 * time.Hour) // the park does not count toward the time limit
	if _, err := env.e.DecideApproval(ctx, g.approval.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	env.e.bg.Wait()
	if got := eff.calls(); !slices.Equal(got, []string{g.call.ID}) {
		t.Fatalf("executor calls = %v", got)
	}
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunQueued || r.PausedAt != nil || r.PausedTotalMs < 2*3_600_000 {
		t.Fatalf("run after the grant = %s paused_at %v paused_total=%d", r.Status, r.PausedAt, r.PausedTotalMs)
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded"}) {
		t.Fatalf("visits after the grant = %v; the parked visit is closed succeeded", got)
	}
	eventually(t, "the continue log line", func() bool { return env.logged(run.ID, "tool approval approved; continuing agent") == 1 })

	r = env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if nodes[1].QuestionID == nil || *nodes[1].QuestionID != g.approval.ID {
		t.Fatalf("the continuation names %v, want the approval %s", nodes[1].QuestionID, g.approval.ID)
	}
	if loop, _ := store.CountNodeLoopVisits(ctx, env.pool, run.ID, "agent"); loop != 1 {
		t.Fatalf("loop visits = %d, want 1", loop)
	}
	second := rig.snt.at(1)
	block := outcomeBlock(g.approval.ID, "It was approved and ran; its result: {")
	if !strings.HasPrefix(second.Message, "ship ticket 7\n\n"+block) || !strings.Contains(second.Message, `"url":"`+mergeRequestURL+`"`) ||
		!strings.Contains(second.Message, "}\n\n--- platform protocol ---\n") {
		t.Fatalf("the continuation's task is not the task, the call's result, then the protocol block:\n%s", second.Message)
	}
	if second.SessionKey == rig.snt.at(0).SessionKey {
		t.Fatal("the continuation reused the parked visit's session")
	}
	agent, _ := stateOf(t, r)["agent"].(map[string]any)
	if agent["merge_request_url"] != mergeRequestURL {
		t.Fatalf("the step's output = %v; want the merge request URL", agent)
	}
	if row, _ := store.GetToolCall(ctx, env.pool, g.call.ID); row.Status != store.ToolExecuted {
		t.Fatalf("tool call = %s", row.Status)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// A rejection of the parked call continues the step with "rejected" and the operator's
// reason; nothing is executed.
func TestToolApprovalRejectedWhileParked(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	rig := newToolParkRig(t, env)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
	g := rig.parked(t, done, run.ID)
	if _, err := env.e.DecideApproval(ctx, g.approval.ID, DecisionReject, "andre", "the branch is not ready"); err != nil {
		t.Fatal(err)
	}
	env.e.bg.Wait()
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("run after the rejection = %s", s)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := outcomeBlock(g.approval.ID, "It was rejected: the branch is not ready.")
	if msg := rig.snt.at(1).Message; !strings.HasPrefix(msg, "ship ticket 7\n\n"+want+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the continuation's task:\n%s\nwant the rejection block:\n%s", msg, want)
	}
	if len(eff.calls()) != 0 {
		t.Fatal("a rejected call was executed")
	}
	if row, _ := store.GetToolCall(ctx, env.pool, g.call.ID); row.Status != store.ToolRejected {
		t.Fatalf("tool call = %s", row.Status)
	}
	agent, _ := stateOf(t, r)["agent"].(map[string]any)
	if agent["merge_request_url"] != "" {
		t.Fatalf("the step's output = %v", agent)
	}
}

// The parked call's approval expires: the sweep does not fail the run; the step continues
// with "expired" and the agent decides (here: an empty URL).
func TestToolApprovalExpiresWhileParked(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	rig := newToolParkRig(t, env)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7", Limits: store.Limits{ApprovalTimeoutSeconds: 60}})
	g := rig.parked(t, done, run.ID)
	env.clock.Advance(61 * time.Second)
	env.e.sweepApprovals(ctx)
	env.e.bg.Wait()
	if a := env.approvalRow(g.approval.ID); a.Status != store.ApprovalExpired {
		t.Fatalf("approval = %s", a.Status)
	}
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("run after the expiry = %s; a parked call's expiry continues the step", s)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	want := outcomeBlock(g.approval.ID, "It expired: nobody decided in time.")
	if msg := rig.snt.at(1).Message; !strings.HasPrefix(msg, "ship ticket 7\n\n"+want+"\n\n--- platform protocol ---\n") {
		t.Fatalf("the continuation's task:\n%s\nwant the expiry block:\n%s", msg, want)
	}
	if len(eff.calls()) != 0 {
		t.Fatal("an expired call was executed")
	}
	if row, _ := store.GetToolCall(ctx, env.pool, g.call.ID); row.Status != store.ToolExpired {
		t.Fatalf("tool call = %s", row.Status)
	}
}

// A decision within the wait is unchanged: the call gets it in the same turn and nothing
// parks.
func TestToolApprovalWithinTheWaitDoesNotPark(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	var snt sends
	calls := make(chan gatedOutcome, 1)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		snt.add(p)
		go func() {
			g := env.mergeRequestCall(p, 10*time.Second)
			calls <- g
			finishThenFinal(t, env, p, `{"summary":"merge request opened"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
	eventually(t, "the run to wait for the approval", func() bool { return runStatus(t, env, run.ID) == store.RunWaitingApproval })
	a := pendingApproval(t, env, run.ID)
	if _, err := env.e.DecideApproval(ctx, a.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	g := recv(t, calls, "the gated call")
	if g.waitErr != nil || g.decided == nil || g.decided.Status != store.ApprovalApproved {
		t.Fatalf("the call got %+v (%v)", g.decided, g.waitErr)
	}
	r := env.wait(done, run.ID)
	env.e.bg.Wait()
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:succeeded"}) || nodes[0].ParkedAt != nil {
		t.Fatalf("visits = %v parked_at %v", got, nodes[0].ParkedAt)
	}
	if snt.len() != 1 || len(eff.calls()) != 0 {
		t.Fatalf("chat.send calls = %d, engine executions %v; the call itself acts on the decision", snt.len(), eff.calls())
	}
}

// The grant lands after the wait and before the turn ends: the live visit takes the outcome,
// and its turn's end sends it as the next turn in the same session. Nothing parks.
func TestToolApprovalDecidedAfterTheWaitBeforeTheTurnEnds(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	var snt sends
	var token string
	calls := make(chan gatedOutcome, 1)
	release := make(chan struct{})
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		i := snt.add(p)
		go func() {
			if i == 1 {
				token = tokenIn(p.Message)
				calls <- env.mergeRequestCall(p, 40*time.Millisecond)
				<-release
				env.endTurn(p, "Waiting for the operator.")
				return
			}
			finishTurn(t, env, p, token, resultFromTask(p.Message))
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
	g := recv(t, calls, "the gated call")
	if g.decided != nil || !errors.Is(g.waitErr, context.DeadlineExceeded) {
		t.Fatalf("the call = %+v", g)
	}
	if s := runStatus(t, env, run.ID); s != store.RunRunning {
		t.Fatalf("run after the wait = %s", s)
	}
	if _, err := env.e.DecideApproval(ctx, g.approval.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	env.e.bg.Wait()
	close(release)
	r := env.wait(done, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if snt.len() != 2 {
		t.Fatalf("chat.send calls = %d, want the task and one next turn", snt.len())
	}
	first, next := snt.at(0), snt.at(1)
	if !strings.HasPrefix(next.Message, outcomeBlock(g.approval.ID, "It was approved and ran; its result: {")) ||
		!strings.Contains(next.Message, mergeRequestURL) || !strings.HasSuffix(next.Message, "}\n\nContinue your task and call yuno__finish when done.") {
		t.Fatalf("next turn's message:\n%s", next.Message)
	}
	if next.SessionKey != first.SessionKey {
		t.Fatalf("next turn in session %q, the first in %q", next.SessionKey, first.SessionKey)
	}
	nodes := runNodes(t, env, run.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"agent#1:succeeded"}) || nodes[0].ParkedAt != nil {
		t.Fatalf("visits = %v parked_at %v; nothing parked", got, nodes[0].ParkedAt)
	}
	agent, _ := stateOf(t, r)["agent"].(map[string]any)
	if agent["merge_request_url"] != mergeRequestURL {
		t.Fatalf("the step's output = %v", agent)
	}
}

// The api restarts while the step is parked: the park is in the rows, so the grant made on
// the new process runs the effect and continues the step.
func TestToolParkSurvivesARestart(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	rig := newToolParkRig(t, env)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
	g := rig.parked(t, done, run.ID)

	e2 := New(env.e.deps)
	e2.tickInterval = env.e.tickInterval
	env.e = e2
	if _, err := e2.DecideApproval(ctx, g.approval.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	e2.bg.Wait()
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("run after the grant = %s", s)
	}
	r := env.resume(e2, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if got := visitsOf(runNodes(t, env, run.ID)); !slices.Equal(got, []string{"agent#1:succeeded", "agent#2:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	agent, _ := stateOf(t, r)["agent"].(map[string]any)
	if agent["merge_request_url"] != mergeRequestURL || len(eff.calls()) != 1 {
		t.Fatalf("the step's output = %v, executor calls %v", agent, eff.calls())
	}
}

// The process that ran a parked call's effect is gone before it recorded the outcome (the
// grant is recorded, the row still awaiting_approval): the approval sweep settles it again
// and continues the step.
func TestSweepSettlesAnInterruptedGrantOfAParkedCall(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	eff := &mergeRequestEffects{pool: env.pool}
	env.e.deps.Effects = eff
	ag := env.agent(nil)
	rig := newToolParkRig(t, env)
	run, done := env.start(TriggerRequest{AgentID: ag.ID, Task: "ship ticket 7"})
	g := rig.parked(t, done, run.ID)
	if _, err := store.DecideApprovalAt(ctx, env.pool, g.approval.ID, store.ApprovalApproved, "andre", "", env.clock.Now()); err != nil {
		t.Fatal(err)
	}
	env.e.sweepApprovals(ctx)
	env.e.bg.Wait()
	if got := eff.calls(); !slices.Equal(got, []string{g.call.ID}) {
		t.Fatalf("executor calls = %v", got)
	}
	if s := runStatus(t, env, run.ID); s != store.RunQueued {
		t.Fatalf("run after the sweep = %s", s)
	}
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}
