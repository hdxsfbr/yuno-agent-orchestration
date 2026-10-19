package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// Wave 11 (DESIGN 5.26, T5.26.11): the trace of a run with a fork carries its branch rows
// and the visits inside them, as the contract says.

// branchConformanceSteps seeds a run at a fork with two branches, one parked, and reads
// its trace and a workflow whose definition holds a fork and a join.
func branchConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	ctx := context.Background()
	pool := needDB(t)
	run, err := createTestRun(ctx, pool, store.TriggerManual, json.RawMessage(`{"task":"review"}`), store.Limits{MaxSteps: 10})
	mustDo(t, err)
	now := time.Now().UTC()
	fork := &store.RunNode{RunID: run.ID, NodeID: "panel", Status: store.NodeRunning, StartedAt: &now, Output: json.RawMessage(`{"branches":["security","performance"]}`)}
	mustDo(t, store.CreateRunNode(ctx, pool, fork))
	a := &store.RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "security", Status: store.BranchSucceeded, CurrentNode: "merge", StartedAt: now}
	b := &store.RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "performance", Status: store.BranchWaitingApproval, CurrentNode: "performance", StartedAt: now.Add(time.Microsecond)}
	mustDo(t, store.InsertRunBranch(ctx, pool, a))
	mustDo(t, store.InsertRunBranch(ctx, pool, b))
	finished := now.Add(time.Minute)
	mustDo(t, store.CreateRunNode(ctx, pool, &store.RunNode{RunID: run.ID, NodeID: "security", Status: store.NodeSucceeded, StartedAt: &now, FinishedAt: &finished, BranchID: &a.ID, Output: json.RawMessage(`{"verdict":"approved"}`)}))
	mustDo(t, store.CreateRunNode(ctx, pool, &store.RunNode{RunID: run.ID, NodeID: "performance", Status: store.NodeWaitingApproval, StartedAt: &now, BranchID: &b.ID, ParkedAt: &finished}))
	return []conformanceStep{
		{http.MethodGet, "/api/runs/" + run.ID + "/trace", "", http.StatusOK},
		{http.MethodPost, "/api/workflows/validate", `{"definition":{"nodes":[{"id":"start","kind":"start"},{"id":"coder","kind":"agent","agent_id":"nope","task":"t"},
			{"id":"panel","kind":"fork","join":"merge"},{"id":"a","kind":"agent","agent_id":"nope","task":"t"},{"id":"b","kind":"agent","agent_id":"nope","task":"t"},
			{"id":"merge","kind":"join"},{"id":"end","kind":"end"}],
			"edges":[{"from":"start","to":"coder"},{"from":"coder","to":"panel"},{"from":"panel","to":"a"},{"from":"panel","to":"b"},{"from":"a","to":"merge"},{"from":"b","to":"merge"},{"from":"merge","to":"end"}]}}`,
			http.StatusUnprocessableEntity},
	}
}

// The trace's branches and the visits' branch_id are served as stored, and a fork
// region's violation reaches the client path-qualified (DESIGN 5.26 rules 2 and 12).
func TestTraceServesBranches(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, Gateway: adminGateway(), TemplatesDir: t.TempDir()}, &fakeEngine{pool: pool})
	steps := branchConformanceSteps(t)
	_, resp := do(t, ts, http.MethodGet, steps[0].path, "")
	var tr struct {
		Nodes    []store.RunNode   `json:"nodes"`
		Branches []store.RunBranch `json:"branches"`
	}
	mustDo(t, json.Unmarshal(resp, &tr))
	if len(tr.Branches) != 2 || tr.Branches[0].Head != "security" || tr.Branches[1].Head != "performance" || tr.Branches[1].Status != store.BranchWaitingApproval ||
		tr.Branches[0].CurrentNode != "merge" || tr.Branches[1].CurrentNode != "performance" || tr.Branches[0].ForkID != "panel" ||
		tr.Branches[0].ForkVisitID == "" || tr.Branches[0].ForkVisitID != tr.Branches[1].ForkVisitID || tr.Branches[0].StartedAt.IsZero() {
		t.Fatalf("branches = %+v", tr.Branches)
	}
	for _, n := range tr.Nodes {
		if n.NodeID == "panel" && (n.BranchID != nil || n.ID != tr.Branches[0].ForkVisitID) {
			t.Fatalf("fork row = %+v; the branches name it", n)
		}
	}
	var inBranch int
	for _, n := range tr.Nodes {
		if n.BranchID != nil {
			inBranch++
		}
	}
	if inBranch != 2 {
		t.Fatalf("visits with branch_id = %d, want 2", inBranch)
	}
	_, body := do(t, ts, http.MethodPost, "/api/workflows/validate", `{"definition":{"nodes":[{"id":"start","kind":"start"},{"id":"a","kind":"agent","agent_id":"nope","task":"t"},
		{"id":"panel","kind":"fork","join":"merge"},{"id":"b","kind":"agent","agent_id":"nope","task":"t"},{"id":"merge","kind":"join"},{"id":"end","kind":"end"}],
		"edges":[{"from":"start","to":"a"},{"from":"a","to":"panel"},{"from":"panel","to":"b"},{"from":"b","to":"merge"},{"from":"merge","to":"end"}]}}`)
	if !strings.Contains(string(body), "a fork needs at least two outgoing edges") {
		t.Fatalf("validate answered %s", body)
	}
}
