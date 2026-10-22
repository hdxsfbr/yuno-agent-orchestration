package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	"yuno/api"
	"yuno/internal/store"
)

// diagnosisServer serves the API with MODEL_STALL_AFTER and ENGINE_STALE_AFTER at their
// defaults, which the diagnosis reports as M and S.
func diagnosisServer(t *testing.T) string {
	t.Helper()
	cfg := testConfig()
	cfg.ModelStallAfter, cfg.EngineStaleAfter = 10*time.Minute, 5*time.Minute
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: needDB(t)}, &fakeEngine{pool: testPool})
	return ts.URL
}

// setRun moves a run to status, as last written at updated.
func setRun(t *testing.T, id, status string, updated time.Time, reason *string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET status=$2::text, updated_at=$3::timestamptz, failure_reason=$4,
		finished_at=CASE WHEN $2::text IN ('succeeded', 'failed', 'cancelled', 'timed_out') THEN $3::timestamptz END WHERE id=$1`,
		id, status, updated, reason)
	mustDo(t, err)
}

// liveVisit stores a running agent visit, started at started, optionally in a branch.
func liveVisit(t *testing.T, runID, nodeID, agentID string, branchID *string, started time.Time) *store.RunNode {
	t.Helper()
	n := &store.RunNode{RunID: runID, NodeID: nodeID, AgentID: &agentID, Visit: 1, Status: store.NodeRunning,
		StartedAt: &started, BranchID: branchID}
	mustDo(t, store.CreateRunNode(context.Background(), testPool, n))
	return n
}

func getDiagnosis(t *testing.T, router routers.Router, base, runID string) runDiagnosis {
	t.Helper()
	code, body := checkConformance(t, router, base, http.MethodGet, "/api/runs/"+runID+"/diagnosis", "", true)
	if code != http.StatusOK {
		t.Fatalf("diagnosis of %s: %d %s", runID, code, body)
	}
	var d runDiagnosis
	mustDo(t, json.Unmarshal(body, &d))
	return d
}

// DESIGN 5.12 outputs: GET /api/runs/{id}/diagnosis and GET /api/runs/diagnosis classify
// from the stored evidence: a step on a tool, a fork's branch visits each classified, a
// parked visit, a queued run blocked by its project's budget and paused waves or merely
// queued, a running run between steps, and a finished run that reports its status.
func TestRunDiagnosis(t *testing.T) {
	base := diagnosisServer(t)
	ctx := context.Background()
	router := contract(t)
	now := time.Now().UTC()
	agentID := testAgent(t, "diagnosis-"+uniqueSuffix())
	p := seedProject(t, nil, "diag-"+uniqueSuffix(), "")

	// A running run: one step running a command for 40 s, and a fork whose left branch is
	// streaming a reply and whose right branch's worker went silent 301 s ago.
	running := projectRun(t, p)
	setRun(t, running.ID, store.RunRunning, now, nil)
	coder := liveVisit(t, running.ID, "coder", agentID, nil, now.Add(-3*time.Minute))
	mustDo(t, store.TouchRunNode(ctx, testPool, running.ID, coder.ID, now.Add(-10*time.Second)))
	progress := now.Add(-20 * time.Second)
	mustDo(t, store.SetRunNodeEvidence(ctx, testPool, coder.ID, store.NodeEvidence{WaitingOn: store.WaitingOnTool, ProgressAt: &progress,
		OpenCalls: json.RawMessage(fmt.Sprintf(`[{"id":"c1","name":"exec","started_at":%q,"summary":"make test"}]`,
			now.Add(-40*time.Second).Format(time.RFC3339Nano)))}))
	seq, err := store.AppendRunEvent(ctx, testPool, running.ID, "node.status", json.RawMessage(`{"node":{"id":"`+coder.ID+`"}}`))
	mustDo(t, err)
	_, err = store.AppendRunEvent(ctx, testPool, running.ID, "log", json.RawMessage(`{"message":"not about a step"}`))
	mustDo(t, err)
	fork := &store.RunNode{RunID: running.ID, NodeID: "fork", Visit: 1, Status: store.NodeRunning, StartedAt: &now}
	left, right := &store.RunBranch{RunID: running.ID, ForkID: "fork", Head: "left"}, &store.RunBranch{RunID: running.ID, ForkID: "fork", Head: "right"}
	mustDo(t, store.CreateForkVisit(ctx, testPool, fork, []*store.RunBranch{left, right}))
	lv := liveVisit(t, running.ID, "left", agentID, &left.ID, now.Add(-2*time.Minute))
	mustDo(t, store.TouchRunNode(ctx, testPool, running.ID, lv.ID, now.Add(-5*time.Second)))
	since := now.Add(-30 * time.Second)
	mustDo(t, store.SetRunNodeEvidence(ctx, testPool, lv.ID, store.NodeEvidence{WaitingOn: store.WaitingOnModel, ModelWaitSince: &since}))
	rv := liveVisit(t, running.ID, "right", agentID, &right.ID, now.Add(-time.Minute))
	mustDo(t, store.StartRunNodeEvidence(ctx, testPool, rv.ID, now.Add(-301*time.Second)))

	d := getDiagnosis(t, router, base, running.ID)
	if d.Status != store.RunRunning || d.Liveness != "stalled" || d.Reason != "right: worker silent 5m1s; the sweeper re-queues after 5m0s" {
		t.Fatalf("running run: %s %s %q", d.Status, d.Liveness, d.Reason)
	}
	if d.Thresholds != (diagnosisThresholds{HeartbeatFreshS: 90, ProgressFreshS: 120, ModelStallS: 600, WorkerStaleS: 300, ToolOverdueS: 1860}) {
		t.Fatalf("thresholds = %+v", d.Thresholds)
	}
	if len(d.Nodes) != 3 || len(d.Pending) != 0 {
		t.Fatalf("nodes = %+v pending = %+v", d.Nodes, d.Pending)
	}
	n := d.Nodes[0]
	if n.NodeID != "coder" || n.Visit != 1 || n.BranchID != nil || !strings.HasPrefix(n.Agent, "diagnosis-") || n.Status != store.NodeRunning ||
		n.Liveness != "executing" || n.Reason != "running exec make test for 40s" || n.WaitingOn != store.WaitingOnTool ||
		n.HeartbeatAt == nil || n.ProgressAt == nil || n.ModelWaitSince != nil ||
		len(n.OpenCalls) != 1 || n.OpenCalls[0].Name != "exec" || n.OpenCalls[0].Summary != "make test" || n.OpenCalls[0].AgeS != 40 ||
		n.LastEvent == nil || n.LastEvent.Seq != seq || n.LastEvent.Event != "node.status" {
		t.Fatalf("coder = %+v", n)
	}
	if n := d.Nodes[1]; n.NodeID != "left" || n.BranchID == nil || *n.BranchID != left.ID || n.Liveness != "executing" ||
		n.Reason != "model replying" || n.ModelWaitSince == nil || n.LastEvent != nil {
		t.Fatalf("left = %+v", n)
	}
	if n := d.Nodes[2]; n.NodeID != "right" || n.BranchID == nil || *n.BranchID != right.ID || n.Liveness != "stalled" ||
		n.WaitingOn != store.WaitingOnSetup {
		t.Fatalf("right = %+v", n)
	}

	// A visit parked on its question: the run waits on it, as does the visit.
	waiting := projectRun(t, p)
	setRun(t, waiting.ID, store.RunRunning, now, nil)
	parked := liveVisit(t, waiting.ID, "planner", agentID, nil, now.Add(-2*time.Minute))
	_, err = store.ParkVisit(ctx, testPool, waiting.ID, parked.ID, now.Add(-time.Minute))
	mustDo(t, err)
	q := &store.Approval{RunID: waiting.ID, RunNodeID: parked.ID, Kind: store.ApprovalKindQuestion, NodeID: "planner",
		Message: "Which branch?", RequestedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	mustDo(t, store.InsertApproval(ctx, testPool, q))
	d = getDiagnosis(t, router, base, waiting.ID)
	want := "waiting on question " + q.ID + " for 1m0s"
	if d.Status != store.RunWaitingApproval || d.Liveness != "waiting" || d.Reason != want ||
		len(d.Nodes) != 1 || d.Nodes[0].Liveness != "waiting" || d.Nodes[0].Reason != "parked, "+want ||
		len(d.Pending) != 1 || d.Pending[0].ApprovalID != q.ID || d.Pending[0].Kind != store.ApprovalKindQuestion || d.Pending[0].AgeS != 60 {
		t.Fatalf("parked: %+v", d)
	}

	// Queued: merely waiting, or blocked by its project's budget cap and paused waves.
	queued := projectRun(t, p)
	if d = getDiagnosis(t, router, base, queued.ID); d.Liveness != "waiting" || !strings.HasPrefix(d.Reason, "queued ") || len(d.Nodes) != 0 {
		t.Fatalf("queued: %+v", d)
	}
	capped := seedProject(t, nil, "diag-capped-"+uniqueSuffix(), "")
	_, err = testPool.Exec(ctx, `UPDATE projects SET budget_day_usd_micros=100 WHERE id=$1`, capped.ID)
	mustDo(t, err)
	mustDo(t, store.SetWavesPaused(ctx, testPool, capped.ID, true, "release freeze"))
	blocked := projectRun(t, capped)
	mustDo(t, store.AddRunUsage(ctx, testPool, blocked.ID, 1, 0, 10, 10, 0, 0, 200))
	d = getDiagnosis(t, router, base, blocked.ID)
	if d.Liveness != "blocked" || !strings.Contains(d.Reason, "project "+capped.Name+" has reached its daily budget ($0.0001)") ||
		!strings.HasSuffix(d.Reason, "; the project's waves are paused: release freeze") {
		t.Fatalf("blocked: %s %q", d.Liveness, d.Reason)
	}

	// Running between steps with a row last written 301 s ago; a finished run.
	between := projectRun(t, p)
	setRun(t, between.ID, store.RunRunning, now.Add(-301*time.Second), nil)
	if d = getDiagnosis(t, router, base, between.ID); d.Liveness != "stalled" || d.Reason != "no step running and the run silent for 5m1s" {
		t.Fatalf("between steps: %s %q", d.Liveness, d.Reason)
	}
	reason := "model error: overloaded"
	finished := projectRun(t, p)
	setRun(t, finished.ID, store.RunFailed, now, &reason)
	if d = getDiagnosis(t, router, base, finished.ID); d.Liveness != store.RunFailed || d.Reason != reason || len(d.Nodes) != 0 {
		t.Fatalf("finished: %+v", d)
	}

	// The list: every run of the project that has not ended, newest first, at one instant.
	code, body := checkConformance(t, router, base, http.MethodGet, "/api/runs/diagnosis?project_id="+p.ID, "", true)
	var list []runDiagnosis
	mustDo(t, json.Unmarshal(body, &list))
	var ids []string
	for _, x := range list {
		ids = append(ids, x.RunID)
		if !x.CheckedAt.Equal(list[0].CheckedAt) {
			t.Fatalf("checked at %v and %v", x.CheckedAt, list[0].CheckedAt)
		}
	}
	if code != http.StatusOK || !slices.Equal(ids, []string{between.ID, queued.ID, waiting.ID, running.ID}) {
		t.Fatalf("list: %d %v, want %v", code, ids, []string{between.ID, queued.ID, waiting.ID, running.ID})
	}
	if _, body = checkConformance(t, router, base, http.MethodGet, "/api/runs/diagnosis?project_id="+capped.ID, "", true); !strings.Contains(string(body), `"liveness":"blocked"`) {
		t.Fatalf("capped list: %s", body)
	}
	if code, body = checkConformance(t, router, base, http.MethodGet, "/api/runs/diagnosis?project_id=no-such-project", "", true); code != http.StatusOK || string(body) != "[]\n" {
		t.Fatalf("unknown project: %d %s", code, body)
	}
	if code, _ = checkConformance(t, router, base, http.MethodGet, "/api/runs/diagnosis", "", true); code != http.StatusOK {
		t.Fatalf("all runs: %d", code)
	}
	if code, _ = checkConformance(t, router, base, http.MethodGet, "/api/runs/no-such-run/diagnosis", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", code)
	}
}

// DESIGN 5.12 outputs: GET /api/runs/{id}/events/page pages a run's events in ascending
// order, filters by type, redacts each event's data again, and takes at most 500 a page.
func TestRunEventsPage(t *testing.T) {
	base := diagnosisServer(t)
	ctx := context.Background()
	router := contract(t)
	run, err := createTestRun(ctx, testPool, "", nil, store.Limits{})
	mustDo(t, err)
	other, err := createTestRun(ctx, testPool, "", nil, store.Limits{})
	mustDo(t, err)
	secret := "sk-ant-" + strings.Repeat("z", 12) // a key-shaped value, not a credential
	var seqs []int64
	for i, ev := range []struct{ typ, data string }{
		{"run.status", `{"run":{"status":"running"}}`},
		{"log", `{"level":"info","message":"turn 1 started"}`},
		{"tool_call", `{"tool_call":{"tool_name":"exec","input":{"command":"echo ` + secret + `"},"api_key":"plain"}}`},
		{"log", `{"level":"warn","message":"stored before redaction: ` + secret + `"}`},
		{"node.status", `{"node":{"status":"succeeded"}}`},
	} {
		seq, err := store.AppendRunEvent(ctx, testPool, run.ID, ev.typ, json.RawMessage(ev.data))
		mustDo(t, err)
		seqs = append(seqs, seq)
		if i == 2 {
			_, err = store.AppendRunEvent(ctx, testPool, other.ID, "log", json.RawMessage(`{"message":"another run"}`))
			mustDo(t, err)
		}
	}
	page := func(query string) runEventsPage {
		t.Helper()
		code, body := checkConformance(t, router, base, http.MethodGet, "/api/runs/"+run.ID+"/events/page"+query, "", true)
		if code != http.StatusOK {
			t.Fatalf("page %s: %d %s", query, code, body)
		}
		if strings.Contains(string(body), secret) || strings.Contains(string(body), `"plain"`) {
			t.Fatalf("page %s is not redacted: %s", query, body)
		}
		var p runEventsPage
		mustDo(t, json.Unmarshal(body, &p))
		return p
	}
	seqsOf := func(p runEventsPage) []int64 {
		var out []int64
		for _, e := range p.Events {
			out = append(out, e.Seq)
		}
		return out
	}

	// Paging: two at a time, ascending, next_after carried over; past the end, empty.
	p1 := page("?limit=2")
	p2 := page(fmt.Sprintf("?limit=2&after=%d", p1.NextAfter))
	p3 := page(fmt.Sprintf("?limit=2&after=%d", p2.NextAfter))
	p4 := page(fmt.Sprintf("?limit=2&after=%d", p3.NextAfter))
	if !slices.Equal(seqsOf(p1), seqs[:2]) || p1.NextAfter != seqs[1] || !slices.Equal(seqsOf(p2), seqs[2:4]) ||
		!slices.Equal(seqsOf(p3), seqs[4:]) || p3.NextAfter != seqs[4] || len(p4.Events) != 0 || p4.NextAfter != seqs[4] {
		t.Fatalf("pages %v %v %v %v, want %v", seqsOf(p1), seqsOf(p2), seqsOf(p3), seqsOf(p4), seqs)
	}
	if all := page(""); !slices.Equal(seqsOf(all), seqs) || all.Events[1].Event != "log" || all.Events[1].At.IsZero() {
		t.Fatalf("default page = %+v", all)
	}

	// Redaction: the stored secret and the secret key's value come back redacted.
	tool := page("?event=tool_call")
	if len(tool.Events) != 1 || !strings.Contains(string(tool.Events[0].Data), `echo [redacted]`) ||
		!strings.Contains(string(tool.Events[0].Data), `"api_key":"[redacted]"`) {
		t.Fatalf("tool_call = %s", tool.Events[0].Data)
	}

	// The filter: one type, several, and blanks ignored.
	if got := seqsOf(page("?event=log")); !slices.Equal(got, []int64{seqs[1], seqs[3]}) {
		t.Fatalf("logs = %v", got)
	}
	if got := seqsOf(page("?event=log,%20tool_call,,")); !slices.Equal(got, seqs[1:4]) {
		t.Fatalf("logs and tool calls = %v", got)
	}
	if got := page("?event=branch.status"); len(got.Events) != 0 || got.NextAfter != 0 {
		t.Fatalf("no such events = %+v", got)
	}

	// The cap: 500 is the most a page takes; 501, 0, a negative or a non-number after is 422.
	if got := page("?limit=500"); len(got.Events) != 5 {
		t.Fatalf("limit 500 = %d events", len(got.Events))
	}
	for query, path := range map[string]string{"?limit=501": "limit", "?limit=0": "limit", "?after=-1": "after", "?after=x": "after"} {
		code, body := checkConformance(t, router, base, http.MethodGet, "/api/runs/"+run.ID+"/events/page"+query, "", false)
		if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{path}) {
			t.Fatalf("%s: %d %s", query, code, body)
		}
	}
	if code, _ := checkConformance(t, router, base, http.MethodGet, "/api/runs/no-such-run/events/page", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", code)
	}
}

// The three operations are in the contract with operationId equal to their handler and
// x-yuno-risk read (DESIGN 5.12 outputs, platform rule A1).
func TestDiagnosisOperations(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	mustDo(t, err)
	for path, id := range map[string]string{
		"/api/runs/diagnosis":        "listRunDiagnoses",
		"/api/runs/{id}/diagnosis":   "getRunDiagnosis",
		"/api/runs/{id}/events/page": "listRunEventsPage",
	} {
		item := doc.Paths.Value(path)
		if item == nil || item.Get == nil {
			t.Fatalf("%s: no GET in the contract", path)
		}
		if item.Get.OperationID != id || item.Get.Extensions["x-yuno-risk"] != "read" {
			t.Fatalf("%s: operationId %q, x-yuno-risk %v", path, item.Get.OperationID, item.Get.Extensions["x-yuno-risk"])
		}
	}
}
