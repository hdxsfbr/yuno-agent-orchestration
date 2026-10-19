package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Parallel steps (DESIGN 5.26, T5.26.2 to T5.26.10), engine side. The fake gateway starts
// every chat.send as a turn; the tests hold branch turns open until every branch has
// started, so "at once" is observed, and end them the way the gateway does.

// forkAgents creates n distinct agents (D-062: one agent, one branch).
func forkAgents(env *testEnv, n int) []*store.Agent {
	out := make([]*store.Agent, n)
	for i := range out {
		name := "Agent " + string(rune('A'+i))
		out[i] = env.agent(func(a *store.Agent) { a.Name = name })
	}
	return out
}

// forkDef builds start → coder → panel(fork, join merge) → heads… → merge → lead → end, the
// shape of the review panel (DESIGN 5.26 rule 14) with the given agents: agents[0] is the
// coder, agents[1:len-1] the heads a, b, c…, the last the lead. Every head's output is
// anything; the lead's input_from lists them.
func forkDef(agents []*store.Agent, checkout string) workflow.Definition {
	heads := agents[1 : len(agents)-1]
	d := workflow.Definition{
		Nodes: []workflow.Node{
			{ID: "start", Kind: workflow.KindStart},
			{ID: "coder", Kind: workflow.KindAgent, AgentID: agents[0].ID, Task: "write", OutputSchema: anyObject, InputFrom: []string{"start"}},
			{ID: "panel", Kind: workflow.KindFork, Join: "merge"},
		},
		Edges: []workflow.Edge{{From: "start", To: "coder"}, {From: "coder", To: "panel"}},
	}
	var names []string
	for i, a := range heads {
		id := string(rune('a' + i))
		names = append(names, id)
		d.Nodes = append(d.Nodes, workflow.Node{ID: id, Kind: workflow.KindAgent, AgentID: a.ID, Task: "review " + id, OutputSchema: anyObject, InputFrom: []string{"coder"}})
		d.Edges = append(d.Edges, workflow.Edge{From: "panel", To: id})
	}
	for _, id := range names {
		d.Edges = append(d.Edges, workflow.Edge{From: id, To: "merge"})
	}
	d.Nodes = append(d.Nodes,
		workflow.Node{ID: "merge", Kind: workflow.KindJoin, Checkout: checkout},
		workflow.Node{ID: "lead", Kind: workflow.KindAgent, AgentID: agents[len(agents)-1].ID, Task: "merge the reviews", OutputSchema: anyObject, InputFrom: names},
		workflow.Node{ID: "end", Kind: workflow.KindEnd})
	d.Edges = append(d.Edges, workflow.Edge{From: "merge", To: "lead"}, workflow.Edge{From: "lead", To: "end"})
	return d
}

// holder keeps branch turns open until n of them have started, then runs release on each.
type holder struct {
	mu   sync.Mutex
	n    int
	held []openclaw.ChatSendParams
	all  chan struct{}
}

func newHolder(n int) *holder { return &holder{n: n, all: make(chan struct{})} }

// add records a started branch turn; it reports whether this was the nth.
func (h *holder) add(p openclaw.ChatSendParams) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, p)
	if len(h.held) == h.n {
		close(h.all)
		return true
	}
	return false
}

func (h *holder) sends() []openclaw.ChatSendParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.held)
}

// branchRows reads the run's branch rows by head.
func branchRows(t *testing.T, env *testEnv, runID string) map[string]store.RunBranch {
	t.Helper()
	rows, err := store.ListRunBranches(context.Background(), env.pool, runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.RunBranch{}
	for _, b := range rows {
		out[b.Head] = b
	}
	return out
}

func nodeByID(nodes []store.RunNode, id string, visit int) *store.RunNode {
	for i := range nodes {
		if nodes[i].NodeID == id && nodes[i].Visit == visit {
			return &nodes[i]
		}
	}
	return nil
}

func sortedVisits(nodes []store.RunNode) []string {
	v := visitsOf(nodes)
	sort.Strings(v)
	return v
}

// T5.26.2: two branches run at once (both turns started before either ended), the state
// holds both outputs, the join's successor reads them, the rows and events are as rules 3
// to 5 say, and current_node stays on the fork until the join passes.
func TestForkRunsBranchesAtOnceAndJoins(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	h := newHolder(2)
	var mu sync.Mutex
	currents := map[string]string{}
	messages := map[string]string{}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		r, _ := store.GetRun(ctx, env.pool, run.ID)
		mu.Lock()
		currents[node] = deref(r.CurrentNode)
		messages[node] = p.Message
		mu.Unlock()
		switch node {
		case "a", "b":
			if h.add(p) {
				for _, q := range h.sends() {
					id, _ := visitOf(q.SessionKey)
					go finishThenFinal(t, env, q, `{"verdict":"`+id+`"}`)
				}
			}
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	mu.Lock()
	defer mu.Unlock()
	if currents["a"] != "panel" || currents["b"] != "panel" || currents["lead"] != "lead" {
		t.Fatalf("current_node when the turns started = %v; the fork is the position while branches run", currents)
	}
	if deref(r.CurrentNode) != "end" {
		t.Fatalf("current_node at the end = %q", deref(r.CurrentNode))
	}
	nodes := runNodes(t, env, run.ID)
	want := []string{"a#1:succeeded", "b#1:succeeded", "coder#1:succeeded", "lead#1:succeeded", "merge#1:succeeded", "panel#1:succeeded"}
	if got := sortedVisits(nodes); !slices.Equal(got, want) {
		t.Fatalf("visits = %v, want %v", got, want)
	}
	fork, join := nodeByID(nodes, "panel", 1), nodeByID(nodes, "merge", 1)
	headsOf := func(raw json.RawMessage) []string {
		var out struct {
			Branches []string `json:"branches"`
		}
		_ = json.Unmarshal(raw, &out)
		return out.Branches
	}
	if !slices.Equal(headsOf(fork.Output), []string{"a", "b"}) || !slices.Equal(headsOf(join.Output), []string{"a", "b"}) || fork.AgentID != nil {
		t.Fatalf("fork output %s, join output %s, fork agent %v", fork.Output, join.Output, fork.AgentID)
	}
	if join.StartedAt == nil || join.FinishedAt == nil || !join.StartedAt.Equal(*join.FinishedAt) {
		t.Fatalf("join row = %+v; it is created passed", join)
	}
	branches := branchRows(t, env, run.ID)
	if len(branches) != 2 {
		t.Fatalf("branches = %v", branches)
	}
	for _, head := range []string{"a", "b"} {
		b := branches[head]
		if b.Status != store.BranchSucceeded || b.CurrentNode != "merge" || b.ForkVisitID != fork.ID || b.ForkID != "panel" || b.FinishedAt == nil {
			t.Fatalf("branch %s = %+v", head, b)
		}
		n := nodeByID(nodes, head, 1)
		if n.BranchID == nil || *n.BranchID != b.ID {
			t.Fatalf("visit of %s names branch %v, want %s", head, n.BranchID, b.ID)
		}
	}
	if list, _ := store.ListRunBranches(ctx, env.pool, run.ID); list[0].Head != "a" || list[1].Head != "b" {
		t.Fatalf("branch order = %s, %s; want edge order", list[0].Head, list[1].Head)
	}
	for _, id := range []string{"coder", "lead", "panel", "merge"} {
		if n := nodeByID(nodes, id, 1); n.BranchID != nil {
			t.Fatalf("%s carries branch_id %s", id, *n.BranchID)
		}
	}
	st := stateOf(t, r)
	if st["a"] == nil || st["b"] == nil || st["lead"] == nil || st["panel"] != nil || st["merge"] != nil {
		t.Fatalf("state = %v; both branches write their keys, the fork and join write none", st)
	}
	if msg := messages["lead"]; !strings.Contains(msg, `a: {"verdict":"a"}`) || !strings.Contains(msg, `b: {"verdict":"b"}`) {
		t.Fatalf("the lead's task lacks the branches' outputs:\n%s", msg)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='branch.status'`, run.ID); n < 4 {
		t.Fatalf("branch.status events = %d, want at least the two starts and the two ends", n)
	}
	if n := env.count(`SELECT count(*) FROM agent_messages WHERE run_id=$1 AND to_node='merge'`, run.ID); n != 2 {
		t.Fatalf("agent messages into the join = %d, want 2", n)
	}
	// The branch visits' spans hang under the fork's span.
	if n := env.count(`SELECT count(*) FROM spans s JOIN spans f ON f.id = s.parent_id WHERE s.run_id=$1 AND s.kind='node' AND s.name IN ('a','b') AND f.name='panel'`, run.ID); n != 2 {
		t.Fatalf("branch node spans under the fork span = %d, want 2", n)
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	if env.logged(run.ID, "fork panel: 2 branches started: a, b") != 1 || env.logged(run.ID, "fork panel: joined; continuing lead") != 1 {
		t.Fatal("the fork's log lines are missing")
	}
}

// T5.26.3: a fork takes every true edge: a condition that leaves one edge true runs one
// branch; none true is `no matching edge from <fork>`.
func TestForkEdgeConditions(t *testing.T) {
	env := newEnv(t)
	agents := forkAgents(env, 4)
	def := forkDef(agents, "")
	for i := range def.Edges {
		if def.Edges[i].From == "panel" {
			def.Edges[i].When = cond(t, `{"path":"/coder/pick","op":"in","value":["`+def.Edges[i].To+`","both"]}`)
		}
	}
	wf := env.createWorkflow(def)
	for _, tc := range []struct {
		pick   string
		status string
		heads  []string
	}{
		{"both", store.RunSucceeded, []string{"a", "b"}},
		{"b", store.RunSucceeded, []string{"b"}},
		{"none", store.RunFailed, nil},
	} {
		t.Run(tc.pick, func(t *testing.T) {
			env.script(map[string][]string{"coder": {`{"pick":"` + tc.pick + `"}`}, "a": {`{}`}, "b": {`{}`}, "lead": {`{}`}})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
			if r.Status != tc.status {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			branches := branchRows(t, env, r.ID)
			var heads []string
			for h := range branches {
				heads = append(heads, h)
			}
			sort.Strings(heads)
			if !slices.Equal(heads, tc.heads) {
				t.Fatalf("branches = %v, want %v", heads, tc.heads)
			}
			if tc.status == store.RunFailed && deref(r.FailureReason) != "no matching edge from panel" {
				t.Fatalf("reason = %q", deref(r.FailureReason))
			}
			if tc.pick == "b" && nodeByID(runNodes(t, env, r.ID), "a", 1) != nil {
				t.Fatal("branch a ran although its edge was false")
			}
		})
	}
}

// T5.26.4: the first branch to fail stops its siblings and fails the run with
// `branch <head> failed: <reason>`; the sibling's visit and branch rows read the stop.
func TestBranchFailureStopsSiblings(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	h := newHolder(2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		switch node {
		case "a", "b":
			if h.add(p) {
				for _, q := range h.sends() {
					if id, _ := visitOf(q.SessionKey); id == "a" {
						// a's provider refuses the request: a model error that is not retried.
						env.fc.push("chat", chatEvent(q.SessionKey, q.IdempotencyKey, "error", map[string]any{
							"errorKind": "invalid_request", "errorMessage": "bad request", "errorDetail": map[string]any{"httpStatus": 400}}))
					}
				}
			}
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "branch a failed: model error: invalid_request" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	nodes := runNodes(t, env, run.ID)
	want := []string{"a#1:failed", "b#1:failed", "coder#1:succeeded", "panel#1:failed"}
	if got := sortedVisits(nodes); !slices.Equal(got, want) {
		t.Fatalf("visits = %v, want %v", got, want)
	}
	if b := nodeByID(nodes, "b", 1); deref(b.FailureReason) != "stopped: branch a failed" {
		t.Fatalf("b's visit reason = %q", deref(b.FailureReason))
	}
	if f := nodeByID(nodes, "panel", 1); deref(f.FailureReason) != "branch a failed: model error: invalid_request" {
		t.Fatalf("fork row reason = %q", deref(f.FailureReason))
	}
	branches := branchRows(t, env, run.ID)
	if a := branches["a"]; a.Status != store.BranchFailed || deref(a.FailureReason) != "model error: invalid_request" {
		t.Fatalf("branch a = %+v", a)
	}
	if b := branches["b"]; b.Status != store.BranchCancelled || deref(b.FailureReason) != "stopped: branch a failed" || b.FinishedAt == nil {
		t.Fatalf("branch b = %+v", b)
	}
	var bKey string
	for _, p := range h.sends() {
		if id, _ := visitOf(p.SessionKey); id == "b" {
			bKey = p.SessionKey
		}
	}
	aborted := false
	for _, raw := range env.fc.callsFor("chat.abort") {
		aborted = aborted || strings.Contains(string(raw), bKey)
	}
	if !aborted {
		t.Fatal("b's turn was not aborted")
	}
	if env.logged(run.ID, "branch a failed: model error: invalid_request; stopping 1 branch(es)") != 1 {
		t.Fatal("the stop log line is missing")
	}
	if n := env.openSpans(run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// T5.26.5: the counters sum across branches (the step limit reached by two branches
// together ends the run and stops the other); a cancel stops every branch.
func TestLimitsAndCancelAcrossBranches(t *testing.T) {
	t.Run("step limit", func(t *testing.T) {
		env := newEnv(t)
		ctx := context.Background()
		agents := forkAgents(env, 4)
		wf := env.createWorkflow(forkDef(agents, ""))
		// The coder's finish takes 2 steps; the limit falls on the branches' third step.
		run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Limits: store.Limits{MaxSteps: 5}})
		if err != nil {
			t.Fatal(err)
		}
		h := newHolder(2)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			node, _ := visitOf(p.SessionKey)
			switch node {
			case "a", "b":
				if h.add(p) {
					for i, q := range h.sends() {
						env.fc.push("session.message", assistantMsg(q.SessionKey, q.IdempotencyKey, []any{textPart("thinking")}, "toolUse", 100, 5, 0))
						if i == 1 {
							env.fc.push("session.message", assistantMsg(q.SessionKey, q.IdempotencyKey, []any{textPart("more")}, "toolUse", 100, 5, 0))
						}
					}
				}
			default:
				go finishThenFinal(t, env, p, `{"ok":true}`)
			}
			return startedTurn(p)
		}
		r := env.resume(env.e, run.ID)
		if r.Status != store.RunFailed || deref(r.FailureReason) != "step limit 5 exceeded" || r.StepCount < 5 {
			t.Fatalf("run = %s %q steps %d", r.Status, deref(r.FailureReason), r.StepCount)
		}
		branches := branchRows(t, env, run.ID)
		statuses := []string{branches["a"].Status, branches["b"].Status}
		sort.Strings(statuses)
		if !slices.Equal(statuses, []string{store.BranchCancelled, store.BranchFailed}) {
			t.Fatalf("branch statuses = %v", statuses)
		}
		for _, b := range branches {
			if b.Status == store.BranchCancelled && deref(b.FailureReason) != "stopped: step limit 5 exceeded" {
				t.Fatalf("stopped branch = %+v", b)
			}
		}
		if env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND status IN ('running','waiting_approval','pending')`, run.ID) != 0 {
			t.Fatal("a visit was left open")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		env := newEnv(t)
		ctx := context.Background()
		agents := forkAgents(env, 4)
		wf := env.createWorkflow(forkDef(agents, ""))
		run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
		if err != nil {
			t.Fatal(err)
		}
		h := newHolder(2)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			if node, _ := visitOf(p.SessionKey); node == "a" || node == "b" {
				h.add(p)
			} else {
				go finishThenFinal(t, env, p, `{"ok":true}`)
			}
			return startedTurn(p)
		}
		claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
		done := make(chan struct{})
		go func() { defer close(done); env.e.runRun(ctx, claimed) }()
		<-h.all
		if _, err := env.e.Cancel(ctx, run.ID, "tester"); err != nil {
			t.Fatal(err)
		}
		r := env.wait(done, run.ID)
		if r.Status != store.RunCancelled {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		for head, b := range branchRows(t, env, run.ID) {
			if b.Status != store.BranchCancelled || b.FinishedAt == nil {
				t.Fatalf("branch %s = %+v", head, b)
			}
		}
		if env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND status IN ('running','waiting_approval','pending')`, run.ID) != 0 {
			t.Fatal("a visit was left open")
		}
		if n := env.openSpans(run.ID); n != 0 {
			t.Fatalf("%d spans left open", n)
		}
	})
}

// T5.26.6: a gated tool call inside a branch pauses the node and not the run (D-063): the
// run stays running with no paused_at, the decision reaches the waiting call, and the
// time limit keeps counting through the wait.
func TestPauseInsideBranchKeepsTheRunRunning(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.deps.Cfg.MCPToolWait = 540 * time.Second
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Limits: store.Limits{MaxDurationSeconds: 600}})
	if err != nil {
		t.Fatal(err)
	}
	gates := make(chan *gate, 1)
	h := newHolder(2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		switch node {
		case "a":
			h.add(p)
			go func() {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				g := env.gatedCall(p, wctx)
				gates <- g
			}()
		case "b":
			h.add(p)
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	done := make(chan struct{})
	go func() { defer close(done); env.e.runRun(ctx, claimed) }()
	<-h.all
	eventually(t, "a's node waiting", func() bool {
		n := nodeByID(runNodes(t, env, run.ID), "a", 1)
		return n != nil && n.Status == store.NodeWaitingApproval
	})
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	if r.Status != store.RunRunning || r.PausedAt != nil {
		t.Fatalf("run while a branch waits = %s paused_at %v; the run keeps running", r.Status, r.PausedAt)
	}
	if b := nodeByID(runNodes(t, env, run.ID), "b", 1); b.Status != store.NodeRunning {
		t.Fatalf("b's node = %s", b.Status)
	}
	// The clock keeps counting through the wait: 200 s in, 400 s remain.
	env.clock.Advance(200 * time.Second)
	pending, _ := store.ListPendingApprovals(ctx, env.pool, run.ID)
	if len(pending) != 1 || pending[0].Kind != store.ApprovalKindTool {
		t.Fatalf("pending approvals = %+v", pending)
	}
	if _, err := env.e.DecideApproval(ctx, pending[0].ID, DecisionApprove, "tester", ""); err != nil {
		t.Fatal(err)
	}
	g := recv(t, gates, "the gated call")
	if err := recv(t, g.waitErr, "the wait"); err != nil {
		t.Fatalf("WaitApproval: %v", err)
	}
	eventually(t, "a's node running again", func() bool {
		n := nodeByID(runNodes(t, env, run.ID), "a", 1)
		return n != nil && n.Status == store.NodeRunning
	})
	for _, p := range h.sends() {
		go finishThenFinal(t, env, p, `{"ok":true}`)
	}
	r = env.wait(done, run.ID)
	if r.Status != store.RunSucceeded || r.PausedTotalMs != 0 {
		t.Fatalf("run = %s %q paused_total %d", r.Status, deref(r.FailureReason), r.PausedTotalMs)
	}
}

// T5.26.6 (time): the time limit runs through a branch's wait, so a long wait times the
// run out while a sequential run's wait would not count.
func TestTimeLimitCountsThroughABranchWait(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.deps.Cfg.MCPToolWait = 540 * time.Second
	agents := forkAgents(env, 4)
	wf := env.createWorkflow(forkDef(agents, ""))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, Limits: store.Limits{MaxDurationSeconds: 300}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHolder(2)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		switch node {
		case "a":
			h.add(p)
			go func() {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				env.gatedCall(p, wctx)
			}()
		case "b":
			h.add(p)
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	done := make(chan struct{})
	go func() { defer close(done); env.e.runRun(ctx, claimed) }()
	<-h.all
	eventually(t, "a's node waiting", func() bool {
		n := nodeByID(runNodes(t, env, run.ID), "a", 1)
		return n != nil && n.Status == store.NodeWaitingApproval
	})
	env.clock.Advance(400 * time.Second)
	r := env.wait(done, run.ID)
	if r.Status != store.RunTimedOut || deref(r.FailureReason) != "time limit 300s exceeded" {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	for head, b := range branchRows(t, env, run.ID) {
		if b.FinishedAt == nil || (b.Status != store.BranchFailed && b.Status != store.BranchCancelled) {
			t.Fatalf("branch %s = %+v", head, b)
		}
	}
	if pending, _ := store.ListPendingApprovals(ctx, env.pool, run.ID); len(pending) != 0 {
		t.Fatalf("approvals left pending: %+v", pending)
	}
}

// parkInBranch is a fork run whose branch a asks a question, gets no answer within the
// wait, and ends its turn without finish (the park of DESIGN 5.26 rule 8); branch b's
// turn is held until the test releases it. Agents hold ask_operator.
type parkRun struct {
	run   *store.Run
	done  chan struct{}
	asked chan askOutcome
	h     *holder
	bSend chan openclaw.ChatSendParams
	sends sends
}

func startParkInBranch(t *testing.T, env *testEnv) *parkRun {
	t.Helper()
	ctx := context.Background()
	agents := forkAgents(env, 4)
	for _, a := range agents {
		a.ToolAllowlist = append(a.ToolAllowlist, "ask_operator")
		if err := store.UpdateAgent(ctx, env.pool, a); err != nil {
			t.Fatal(err)
		}
	}
	wf := env.createWorkflow(forkDef(agents, ""))
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	pr := &parkRun{run: run, done: make(chan struct{}), asked: make(chan askOutcome, 2), h: newHolder(2), bSend: make(chan openclaw.ChatSendParams, 2)}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		pr.sends.add(p)
		node, visit := visitOf(p.SessionKey)
		switch {
		case node == "a" && visit == 1:
			pr.h.add(p)
			go func() {
				out := env.askAs(p, "Which language?", []string{"English", "Spanish"}, 50*time.Millisecond)
				pr.asked <- out
				env.endTurn(p, "saved my progress")
			}()
		case node == "b":
			pr.h.add(p)
			pr.bSend <- p
		default:
			go finishThenFinal(t, env, p, `{"ok":true}`)
		}
		return startedTurn(p)
	}
	claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
	go func() { defer close(pr.done); env.e.runRun(ctx, claimed) }()
	return pr
}

// T5.26.7: a parked branch leaves the run running while its sibling works; when the
// sibling ends the fork releases the worker on the fork's visit; the answer re-queues the
// run through the fork visit and the continuation visit carries question_id and the
// paragraph; the fork then joins.
func TestParkedBranchReleasesWhenSiblingsEnd(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	pr := startParkInBranch(t, env)
	out := recv(t, pr.asked, "the ask")
	if out.askErr != nil || out.decided != nil || !errors.Is(out.waitErr, context.DeadlineExceeded) || out.parkErr != nil {
		t.Fatalf("the ask = %+v", out)
	}
	eventually(t, "branch a parked", func() bool { return branchRows(t, env, pr.run.ID)["a"].Status == store.BranchWaitingApproval })
	r, _ := store.GetRun(ctx, env.pool, pr.run.ID)
	if r.Status != store.RunRunning || r.PausedAt != nil {
		t.Fatalf("run with a parked branch and a live sibling = %s paused_at %v", r.Status, r.PausedAt)
	}
	a1 := nodeByID(runNodes(t, env, pr.run.ID), "a", 1)
	if a1.Status != store.NodeWaitingApproval || a1.ParkedAt == nil {
		t.Fatalf("parked visit = %+v", a1)
	}
	if env.logged(pr.run.ID, "waits for the answer to question "+out.row.ID+"; branch a continues when it arrives") != 1 {
		t.Fatal("the branch park log line is missing")
	}
	// The sibling ends: nothing runs, one branch is parked, the fork releases the worker.
	bp := recv(t, pr.bSend, "b's send")
	go finishThenFinal(t, env, bp, `{"ok":true}`)
	r = env.wait(pr.done, pr.run.ID)
	fork := nodeByID(runNodes(t, env, pr.run.ID), "panel", 1)
	if r.Status != store.RunWaitingApproval || r.PausedAt == nil || r.ResumeNodeVisitID == nil || *r.ResumeNodeVisitID != fork.ID || fork.Status != store.NodeRunning {
		t.Fatalf("run after the release = %s paused_at %v resume %v; fork row %s", r.Status, r.PausedAt, r.ResumeNodeVisitID, fork.Status)
	}
	if env.logged(pr.run.ID, "fork panel: waiting for the answer to 1 question(s); 1 branch(es) done") != 1 {
		t.Fatal("the release log line is missing")
	}
	if parked, _ := store.IsParkedOn(ctx, env.pool, pr.run.ID, a1.ID); !parked {
		t.Fatal("IsParkedOn does not see the park through the fork visit")
	}

	env.clock.Advance(time.Hour) // the release does not count toward the time limit
	answered, err := env.e.AnswerQuestion(ctx, out.row.ID, "Spanish", "api")
	if err != nil || answered.Status != store.ApprovalApproved {
		t.Fatalf("answer: %+v %v", answered, err)
	}
	r, _ = store.GetRun(ctx, env.pool, pr.run.ID)
	if r.Status != store.RunQueued || r.PausedAt != nil || r.PausedTotalMs < 3_600_000 {
		t.Fatalf("run after the answer = %s paused_at %v paused_total %d", r.Status, r.PausedAt, r.PausedTotalMs)
	}
	nodes := runNodes(t, env, pr.run.ID)
	if a1 = nodeByID(nodes, "a", 1); a1.Status != store.NodeSucceeded || a1.FinishedAt == nil {
		t.Fatalf("parked visit after the answer = %+v; it is closed succeeded", a1)
	}
	if b := branchRows(t, env, pr.run.ID)["a"]; b.Status != store.BranchRunning {
		t.Fatalf("branch a after the answer = %s", b.Status)
	}
	// The claim re-enters the fork: the fork row is not a lost worker's visit.
	r = env.resume(env.e, pr.run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if env.logged(pr.run.ID, "unfinished node visit(s) marked failed") != 0 {
		t.Fatal("the released fork visit was treated as a lost worker's")
	}
	nodes = runNodes(t, env, pr.run.ID)
	want := []string{"a#1:succeeded", "a#2:succeeded", "b#1:succeeded", "coder#1:succeeded", "lead#1:succeeded", "merge#1:succeeded", "panel#1:succeeded"}
	if got := sortedVisits(nodes); !slices.Equal(got, want) {
		t.Fatalf("visits = %v, want %v", got, want)
	}
	a2 := nodeByID(nodes, "a", 2)
	if a2.QuestionID == nil || *a2.QuestionID != out.row.ID || a2.BranchID == nil || *a2.BranchID != *a1.BranchID {
		t.Fatalf("continuation visit = %+v", a2)
	}
	var cont string
	for i := 0; i < pr.sends.len(); i++ {
		if p := pr.sends.at(i); strings.Contains(p.SessionKey, "-a-2") {
			cont = p.Message
		}
	}
	if !strings.Contains(cont, answeredBlock("Which language?", "Spanish")) {
		t.Fatalf("the continuation's task lacks the answer paragraph:\n%s", cont)
	}
	if env.logged(pr.run.ID, "fork panel: resumed") != 1 || env.logged(pr.run.ID, "fork panel: joined; continuing lead") != 1 {
		t.Fatal("the re-entry log lines are missing")
	}
	if n := env.openSpans(pr.run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// T5.26.7 (a decision while the sibling runs): the answer lands while b is live; when b
// ends the fork continues a at once, and the run never waits.
func TestParkedBranchContinuesWithoutReleaseWhenAnsweredEarly(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	pr := startParkInBranch(t, env)
	out := recv(t, pr.asked, "the ask")
	eventually(t, "branch a parked", func() bool { return branchRows(t, env, pr.run.ID)["a"].Status == store.BranchWaitingApproval })
	if _, err := env.e.AnswerQuestion(ctx, out.row.ID, "English", "api"); err != nil {
		t.Fatal(err)
	}
	r, _ := store.GetRun(ctx, env.pool, pr.run.ID)
	if r.Status != store.RunRunning {
		t.Fatalf("run after an answer with a live sibling = %s", r.Status)
	}
	bp := recv(t, pr.bSend, "b's send")
	go finishThenFinal(t, env, bp, `{"ok":true}`)
	r = env.wait(pr.done, pr.run.ID)
	if r.Status != store.RunSucceeded || r.PausedTotalMs != 0 {
		t.Fatalf("run = %s %q paused_total %d", r.Status, deref(r.FailureReason), r.PausedTotalMs)
	}
	if env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='run.status' AND data->'run'->>'status'='waiting_approval'`, pr.run.ID) != 0 {
		t.Fatal("the run waited although the answer had landed")
	}
	nodes := runNodes(t, env, pr.run.ID)
	if a2 := nodeByID(nodes, "a", 2); a2 == nil || a2.QuestionID == nil || a2.Status != store.NodeSucceeded {
		t.Fatalf("continuation = %+v", a2)
	}
}

// T5.26.7 (expiry): a parked branch's question that expires fails the run with
// `question unanswered`, and the branch rows close.
func TestParkedBranchQuestionExpiresAndFailsTheRun(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	pr := startParkInBranch(t, env)
	out := recv(t, pr.asked, "the ask")
	eventually(t, "branch a parked", func() bool { return branchRows(t, env, pr.run.ID)["a"].Status == store.BranchWaitingApproval })
	bp := recv(t, pr.bSend, "b's send")
	go finishThenFinal(t, env, bp, `{"ok":true}`)
	r := env.wait(pr.done, pr.run.ID)
	if r.Status != store.RunWaitingApproval {
		t.Fatalf("run after the release = %s", r.Status)
	}
	env.clock.Advance(25 * time.Hour) // past the default approval timeout
	env.e.sweepApprovals(ctx)
	r, _ = store.GetRun(ctx, env.pool, pr.run.ID)
	if r.Status != store.RunFailed || deref(r.FailureReason) != "question unanswered" {
		t.Fatalf("run after the expiry = %s %q", r.Status, deref(r.FailureReason))
	}
	if q := env.approvalRow(out.row.ID); q.Status != store.ApprovalExpired {
		t.Fatalf("question = %s", q.Status)
	}
	for head, b := range branchRows(t, env, pr.run.ID) {
		if b.FinishedAt == nil || (head == "a" && (b.Status != store.BranchFailed || deref(b.FailureReason) != "question unanswered")) {
			t.Fatalf("branch %s = %+v", head, b)
		}
	}
	if env.count(`SELECT count(*) FROM run_nodes WHERE run_id=$1 AND status IN ('running','waiting_approval','pending')`, pr.run.ID) != 0 {
		t.Fatal("a visit was left open")
	}
}

// T5.26.8: re-entry after a crash runs every unfinished branch again from where it was;
// a retry after a branch failure runs the failed and stopped branches, not the one that
// had succeeded.
func TestForkReentryAfterCrashAndRetry(t *testing.T) {
	t.Run("crash", func(t *testing.T) {
		env := newEnv(t)
		ctx := context.Background()
		agents := forkAgents(env, 4)
		wf := env.createWorkflow(forkDef(agents, ""))
		run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
		if err != nil {
			t.Fatal(err)
		}
		h := newHolder(2)
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			node, visit := visitOf(p.SessionKey)
			switch {
			case (node == "a" || node == "b") && visit == 1:
				h.add(p) // held until the worker dies
			default:
				go finishThenFinal(t, env, p, `{"ok":true}`)
			}
			return startedTurn(p)
		}
		claimed, _ := store.ClaimQueuedRun(ctx, env.pool)
		wctx, kill := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); env.e.runRun(wctx, claimed) }()
		<-h.all
		kill() // the worker dies with both branch turns open
		<-done
		r, _ := store.GetRun(ctx, env.pool, run.ID)
		if r.Status != store.RunRunning || deref(r.CurrentNode) != "panel" {
			t.Fatalf("run after the crash = %s at %q", r.Status, deref(r.CurrentNode))
		}
		// The stale-run sweep re-queues it (DESIGN 5.1 rule 8); here directly.
		if _, err := store.TransitionRun(ctx, env.pool, run.ID, []string{store.RunRunning}, store.RunQueued, nil, env.clock.Now()); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, run.ID)
		if r.Status != store.RunSucceeded {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		want := []string{"a#1:failed", "a#2:succeeded", "b#1:failed", "b#2:succeeded", "coder#1:succeeded", "lead#1:succeeded", "merge#1:succeeded", "panel#1:succeeded"}
		if got := sortedVisits(runNodes(t, env, run.ID)); !slices.Equal(got, want) {
			t.Fatalf("visits = %v, want %v", got, want)
		}
		if n := env.count(`SELECT count(*) FROM run_branches WHERE run_id=$1`, run.ID); n != 2 {
			t.Fatalf("branch rows = %d; the re-entry reuses them", n)
		}
		if env.logged(run.ID, "fork panel: resumed") != 1 {
			t.Fatal("the re-entry log line is missing")
		}
	})
	t.Run("retry", func(t *testing.T) {
		env := newEnv(t)
		ctx := context.Background()
		agents := forkAgents(env, 4)
		wf := env.createWorkflow(forkDef(agents, ""))
		run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
		if err != nil {
			t.Fatal(err)
		}
		bDone := make(chan struct{})
		env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
			node, visit := visitOf(p.SessionKey)
			switch {
			case node == "b":
				go func() {
					finishThenFinal(t, env, p, `{"ok":true}`)
					close(bDone)
				}()
			case node == "a" && visit == 1:
				go func() {
					<-bDone
					eventually(t, "b succeeded", func() bool { return branchRows(t, env, run.ID)["b"].Status == store.BranchSucceeded })
					env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
						"errorKind": "invalid_request", "errorMessage": "bad request", "errorDetail": map[string]any{"httpStatus": 400}}))
				}()
			default:
				go finishThenFinal(t, env, p, `{"ok":true}`)
			}
			return startedTurn(p)
		}
		r := env.resume(env.e, run.ID)
		if r.Status != store.RunFailed || deref(r.FailureReason) != "branch a failed: model error: invalid_request" {
			t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
		}
		if _, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil {
			t.Fatal(err)
		}
		r = env.resume(env.e, run.ID)
		if r.Status != store.RunSucceeded {
			t.Fatalf("retried run = %s %q", r.Status, deref(r.FailureReason))
		}
		want := []string{"a#1:failed", "a#2:succeeded", "b#1:succeeded", "coder#1:succeeded", "lead#1:succeeded", "merge#1:succeeded", "panel#1:succeeded"}
		if got := sortedVisits(runNodes(t, env, run.ID)); !slices.Equal(got, want) {
			t.Fatalf("visits = %v, want %v", got, want)
		}
		if b := branchRows(t, env, run.ID)["b"]; b.Status != store.BranchSucceeded {
			t.Fatalf("branch b after the retry = %+v; it ran once", b)
		}
	})
}

// recordingCollector is an artifact collector that records what each visit's end hands it.
type recordingCollector struct {
	mu     sync.Mutex
	inputs []ArtifactInput
}

func (c *recordingCollector) Collect(_ context.Context, in ArtifactInput) {
	c.mu.Lock()
	c.inputs = append(c.inputs, in)
	c.mu.Unlock()
}

// T5.26.9 and T5.26.10: a branch visit receives the files of the visits before the fork
// and nothing from its siblings; after the join every branch's files flow on, the checkout
// only from the branch the join names; the generated diff is skipped for the other
// branch. A project run against a local bare repository (the harness of 5.16's tests).
func TestBranchFilesCheckoutAndDiff(t *testing.T) {
	for _, named := range []bool{true, false} {
		name := "checkout a"
		if !named {
			name = "no checkout"
		}
		t.Run(name, func(t *testing.T) {
			env, _, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, false)
			ctx := context.Background()
			rc := &recordingCollector{}
			env.e.deps.Artifacts = rc
			agents := forkAgents(env, 5) // coder, a, b, c, lead
			checkout := ""
			if named {
				checkout = "a"
			}
			def := forkDef(agents, checkout)
			def.Project = true
			// Branch a has a second node, a2, so a's later visit starts after b may have ended.
			def.Nodes = append(def.Nodes, workflow.Node{ID: "a2", Kind: workflow.KindAgent, AgentID: agents[3].ID, Task: "second look", OutputSchema: anyObject, InputFrom: []string{"a"}})
			for i := range def.Edges {
				if def.Edges[i].From == "a" && def.Edges[i].To == "merge" {
					def.Edges[i].To = "a2"
				}
				if def.Edges[i].From == "c" {
					def.Edges[i].From = "a2" // c's edge to merge becomes a2's
				}
			}
			def.Nodes = slices.DeleteFunc(def.Nodes, func(n workflow.Node) bool { return n.ID == "c" })
			def.Edges = slices.DeleteFunc(def.Edges, func(e workflow.Edge) bool { return e.To == "c" })
			for i := range def.Nodes {
				if def.Nodes[i].ID == "lead" {
					def.Nodes[i].InputFrom = []string{"a2", "b"}
				}
			}
			wf := env.createWorkflow(def)
			run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
			if err != nil {
				t.Fatal(err)
			}
			path := func(p openclaw.ChatSendParams, rel string) string {
				return filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "runs", run.ID, rel)
			}
			read := func(p openclaw.ChatSendParams, rel string) string {
				b, err := os.ReadFile(path(p, rel))
				if err != nil {
					return "missing"
				}
				return string(b)
			}
			var mu sync.Mutex
			seen := map[string]string{}
			bDone := make(chan struct{})
			env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
				node, _ := visitOf(p.SessionKey)
				mu.Lock()
				seen[node] = read(p, "notes.md") + " | " + read(p, "a.txt") + " | " + read(p, "b.txt") + " | " + read(p, "repo/README.md")
				mu.Unlock()
				switch node {
				case "coder":
					writeFile(t, path(p, "notes.md"), "by coder")
					writeFile(t, path(p, "repo/README.md"), "coder's tree")
					go finishThenFinal(t, env, p, `{"ok":true}`)
				case "a":
					writeFile(t, path(p, "a.txt"), "by a")
					writeFile(t, path(p, "repo/README.md"), "a's tree")
					go finishThenFinal(t, env, p, `{"ok":true}`)
				case "b":
					writeFile(t, path(p, "b.txt"), "by b")
					writeFile(t, path(p, "repo/README.md"), "b's tree")
					go func() {
						finishThenFinal(t, env, p, `{"ok":true}`)
						close(bDone)
					}()
				case "a2":
					go func() {
						<-bDone // b's files exist by now and must not reach a2
						eventually(t, "b succeeded", func() bool { return branchRows(t, env, run.ID)["b"].Status == store.BranchSucceeded })
						mu.Lock()
						seen["a2"] = read(p, "notes.md") + " | " + read(p, "a.txt") + " | " + read(p, "b.txt") + " | " + read(p, "repo/README.md")
						mu.Unlock()
						finishThenFinal(t, env, p, `{"ok":true}`)
					}()
				default:
					go finishThenFinal(t, env, p, `{"ok":true}`)
				}
				return startedTurn(p)
			}
			r := env.resume(env.e, run.ID)
			if r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			mu.Lock()
			defer mu.Unlock()
			if seen["a"] != "by coder | missing | missing | coder's tree" || seen["b"] != "by coder | missing | missing | coder's tree" {
				t.Fatalf("branch heads started with a %q, b %q; want the coder's files only", seen["a"], seen["b"])
			}
			if seen["a2"] != "by coder | by a | missing | a's tree" {
				t.Fatalf("a2 started with %q; its own branch's files, nothing of b's", seen["a2"])
			}
			wantLead := "by coder | by a | by b | coder's tree"
			if named {
				wantLead = "by coder | by a | by b | a's tree"
			}
			if seen["lead"] != wantLead {
				t.Fatalf("lead started with %q, want %q", seen["lead"], wantLead)
			}
			rc.mu.Lock()
			defer rc.mu.Unlock()
			skips := map[string]string{}
			for _, in := range rc.inputs {
				skips[in.Node.NodeID] = map[bool]string{true: "skip:" + in.BranchHead, false: "diff"}[in.SkipDiff]
			}
			wantSkips := map[string]string{"coder": "diff", "a": "skip:a", "a2": "skip:a", "b": "skip:b", "lead": "diff"}
			if named {
				wantSkips["a"], wantSkips["a2"] = "diff", "diff"
			}
			for node, w := range wantSkips {
				if skips[node] != w {
					t.Errorf("artifact input of %s = %q, want %q", node, skips[node], w)
				}
			}
		})
	}
}

// The sweep's repeat of a parked decision finds a run waiting on a fork visit
// (ListParkedQuestions through the branch), and a fork visit's row is listed under the
// trace's branches.
func TestParkedQuestionsListBranchParks(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	pr := startParkInBranch(t, env)
	out := recv(t, pr.asked, "the ask")
	eventually(t, "branch a parked", func() bool { return branchRows(t, env, pr.run.ID)["a"].Status == store.BranchWaitingApproval })
	bp := recv(t, pr.bSend, "b's send")
	go finishThenFinal(t, env, bp, `{"ok":true}`)
	r := env.wait(pr.done, pr.run.ID)
	if r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s", r.Status)
	}
	// Decide the row without delivering (as a decision from a process that then died).
	if _, err := store.AnswerApprovalAt(ctx, env.pool, out.row.ID, "English", "api", env.clock.Now()); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListParkedQuestions(ctx, env.pool)
	if err != nil || len(listed) != 1 || listed[0].ID != out.row.ID {
		t.Fatalf("ListParkedQuestions = %+v %v", listed, err)
	}
	env.e.requeueParkedDecided(ctx)
	r, _ = store.GetRun(ctx, env.pool, pr.run.ID)
	if r.Status != store.RunQueued {
		t.Fatalf("run after the sweep = %s", r.Status)
	}
	tr, err := store.GetTrace(ctx, env.pool, pr.run.ID)
	if err != nil || len(tr.Branches) != 2 {
		t.Fatalf("trace branches = %+v %v", tr.Branches, err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(mustJSON(tr), &raw)
	if len(raw["branches"]) == 0 {
		t.Fatal("the trace JSON has no branches")
	}
}
