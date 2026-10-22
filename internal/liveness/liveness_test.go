package liveness

import (
	"testing"
	"time"

	"yuno/internal/store"
)

var checkedAt = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// ago is a time s seconds before the check.
func ago(s int) *time.Time {
	t := checkedAt.Add(-time.Duration(s) * time.Second)
	return &t
}

// The configured M and S at their defaults (MODEL_STALL_AFTER 600 s, ENGINE_STALE_AFTER 300 s).
var th = NewThresholds(600*time.Second, 300*time.Second)

// DESIGN 5.12 liveness rules 1 to 7, one case per rule, with both sides of every threshold
// (H, P, M, S and T at value−1 and value+1 seconds).
func TestDiagnosisRules(t *testing.T) {
	if th.H != 90*time.Second || th.P != 120*time.Second || th.M != 600*time.Second || th.S != 300*time.Second || th.T != 1860*time.Second {
		t.Fatalf("thresholds = %+v", th)
	}
	fresh := ago(10)
	model := func(since *time.Time) Node {
		return Node{NodeID: "coder", HeartbeatAt: fresh, WaitingOn: WaitModel, ModelWaitSince: since}
	}
	tool := func(calls ...OpenCall) Node {
		return Node{NodeID: "coder", HeartbeatAt: fresh, WaitingOn: WaitTool, OpenCalls: calls}
	}
	call := func(id string, s int) OpenCall {
		return OpenCall{ID: id, Name: "exec", Summary: "make test", StartedAt: *ago(s)}
	}
	progressed := func(n Node, s int) Node {
		n.ProgressAt = ago(s)
		return n
	}
	cases := []struct {
		name         string
		th           Thresholds
		n            Node
		want, reason string
	}{
		{"1 no heartbeat", th, Node{WaitingOn: WaitModel, ModelWaitSince: ago(5), ProgressAt: ago(5)},
			Unknown, "no heartbeat recorded"},

		{"2 heartbeat S-1", th, Node{HeartbeatAt: ago(299), WaitingOn: WaitModel, ModelWaitSince: ago(5)},
			Unknown, "heartbeat 4m59s old"},
		{"2 heartbeat S+1", th, Node{HeartbeatAt: ago(301), WaitingOn: WaitModel, ModelWaitSince: ago(5)},
			Stalled, "worker silent 5m1s; the sweeper re-queues after 5m0s"},
		{"2 sweeper off", NewThresholds(600*time.Second, 0), Node{HeartbeatAt: ago(3600), WaitingOn: WaitModel, ModelWaitSince: ago(5)},
			Unknown, "heartbeat 1h0m0s old"},

		{"3 heartbeat H-1", th, Node{HeartbeatAt: ago(89), WaitingOn: WaitModel, ModelWaitSince: ago(5)},
			Executing, "model replying"},
		{"3 heartbeat H+1", th, Node{HeartbeatAt: ago(91), WaitingOn: WaitModel, ModelWaitSince: ago(5)},
			Unknown, "heartbeat 1m31s old"},

		{"4 question", th, Node{HeartbeatAt: fresh, WaitingOn: WaitHuman,
			Pending: []Pending{{ID: "q1", Kind: store.ApprovalKindQuestion, RequestedAt: *ago(125)}, {ID: "a2", Kind: "exec", RequestedAt: *ago(5)}}},
			Waiting, "waiting on question q1 for 2m5s"},
		{"4 exec approval", th, Node{HeartbeatAt: fresh, WaitingOn: WaitHuman, Pending: []Pending{{ID: "a1", Kind: store.ApprovalKindExec, RequestedAt: *ago(30)}}},
			Waiting, "waiting on exec approval a1 for 30s"},
		{"4 decision not read", th, Node{HeartbeatAt: fresh, WaitingOn: WaitHuman},
			Waiting, "waiting on a decision"},

		{"5 tool T-1", th, tool(call("c1", 1859)), Executing, "running exec make test for 30m59s"},
		{"5 tool T+1", th, tool(call("c1", 1861)), Stalled, "exec running 31m1s, past its timeout"},
		{"5 the oldest call decides", th, tool(call("c2", 5), call("c1", 1861)), Stalled, "exec running 31m1s, past its timeout"},

		{"6 model P-1", th, model(ago(119)), Executing, "model replying"},
		{"6 model P+1", th, model(ago(121)), Waiting, "waiting on the model 2m1s; restarted at 10m0s"},
		{"6 model M-1", th, model(ago(599)), Waiting, "waiting on the model 9m59s; restarted at 10m0s"},
		{"6 model M+1", th, model(ago(601)), Stalled, "waiting on the model 10m1s, past the 10m0s restart"},
		{"6 progress restarts the wait", th, progressed(model(ago(601)), 119), Executing, "model replying"},
		{"6 older progress does not", th, progressed(model(ago(601)), 900), Stalled, "waiting on the model 10m1s, past the 10m0s restart"},
		{"6 restart off", NewThresholds(0, 300*time.Second), model(ago(3600)), Waiting, "waiting on the model 1h0m0s; no restart is configured"},

		{"7 setup P-1", th, Node{HeartbeatAt: fresh, WaitingOn: WaitSetup, ProgressAt: ago(119)},
			Executing, "setting up the step; last activity 1m59s ago"},
		{"7 setup P+1", th, Node{HeartbeatAt: fresh, WaitingOn: WaitSetup, ProgressAt: ago(121)},
			Unknown, "no activity for 2m1s"},
		{"7 nothing named P-1", th, Node{HeartbeatAt: fresh, ProgressAt: ago(119)},
			Executing, "last activity 1m59s ago"},
		{"7 nothing named P+1", th, Node{HeartbeatAt: fresh, ProgressAt: ago(121)},
			Unknown, "no activity for 2m1s"},

		{"parked on a question", th, Node{Parked: true, WaitingOn: WaitHuman,
			Pending: []Pending{{ID: "q1", Kind: store.ApprovalKindQuestion, RequestedAt: *ago(3600)}}},
			Waiting, "parked, waiting on question q1 for 1h0m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyNode(tc.n, tc.th, checkedAt)
			if v.Liveness != tc.want || v.Reason != tc.reason {
				t.Fatalf("got %s %q, want %s %q", v.Liveness, v.Reason, tc.want, tc.reason)
			}
		})
	}
}

// The verdict carries the evidence's ages, in seconds.
func TestDiagnosisAges(t *testing.T) {
	v := ClassifyNode(Node{NodeID: "coder", HeartbeatAt: ago(12), ProgressAt: ago(40), WaitingOn: WaitTool,
		OpenCalls: []OpenCall{{ID: "c1", Name: "exec", StartedAt: *ago(75)}}}, th, checkedAt)
	if v.NodeID != "coder" || *v.HeartbeatAgeS != 12 || *v.ProgressAgeS != 40 || v.ModelWaitAgeS != nil ||
		len(v.OpenCalls) != 1 || v.OpenCalls[0].AgeS != 75 || v.OpenCalls[0].ID != "c1" {
		t.Fatalf("verdict = %+v", v)
	}
	v = ClassifyNode(Node{HeartbeatAt: ago(1), WaitingOn: WaitModel, ModelWaitSince: ago(30)}, th, checkedAt)
	if v.ProgressAgeS != nil || v.ModelWaitAgeS == nil || *v.ModelWaitAgeS != 30 {
		t.Fatalf("verdict = %+v", v)
	}
}

// DESIGN 5.12 run liveness: each run status, the worst of the live nodes, and a running
// run between steps on both sides of H and S.
func TestDiagnosisRun(t *testing.T) {
	verdict := func(node, liveness string) Verdict {
		return Verdict{NodeID: node, Liveness: liveness, Reason: liveness + " reason"}
	}
	cases := []struct {
		name         string
		r            Run
		want, reason string
	}{
		{"succeeded", Run{Status: store.RunSucceeded}, store.RunSucceeded, ""},
		{"failed", Run{Status: store.RunFailed, FailureReason: "model error: overloaded"}, store.RunFailed, "model error: overloaded"},
		{"cancelled", Run{Status: store.RunCancelled, FailureReason: "cancelled by andre"}, store.RunCancelled, "cancelled by andre"},
		{"timed out", Run{Status: store.RunTimedOut, FailureReason: "time limit 60s exceeded"}, store.RunTimedOut, "time limit 60s exceeded"},
		{"terminal ignores its nodes", Run{Status: store.RunSucceeded, Nodes: []Verdict{verdict("a", Stalled)}}, store.RunSucceeded, ""},

		{"queued", Run{Status: store.RunQueued, UpdatedAt: *ago(45)}, Waiting, "queued 45s"},
		{"queued and blocked", Run{Status: store.RunQueued, UpdatedAt: *ago(45), Blocked: "the project's waves are paused"},
			Blocked, "the project's waves are paused"},

		{"waiting on a question", Run{Status: store.RunWaitingApproval, PausedAt: ago(70),
			Pending: []Pending{{ID: "q1", Kind: store.ApprovalKindQuestion, RequestedAt: *ago(60)}}},
			Waiting, "waiting on question q1 for 1m0s"},
		{"waiting on an approval node", Run{Status: store.RunWaitingApproval, PausedAt: ago(70),
			Pending: []Pending{{ID: "n1", Kind: store.ApprovalKindNode, RequestedAt: *ago(60)}}},
			Waiting, "waiting on approval n1 for 1m0s"},
		{"waiting with no pending row", Run{Status: store.RunWaitingApproval, PausedAt: ago(90)},
			Waiting, "paused 1m30s, waiting on a decision"},

		{"running, executing", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Executing)}}, Executing, "a: executing reason"},
		{"running, waiting beats executing", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Executing), verdict("b", Waiting)}},
			Waiting, "b: waiting reason"},
		{"running, blocked beats waiting", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Waiting), verdict("b", Blocked)}},
			Blocked, "b: blocked reason"},
		{"running, unknown beats blocked", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Blocked), verdict("b", Unknown)}},
			Unknown, "b: unknown reason"},
		{"running, stalled beats unknown", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Unknown), verdict("b", Stalled), verdict("c", Executing)}},
			Stalled, "b: stalled reason"},
		{"running, the first of equals", Run{Status: store.RunRunning, Nodes: []Verdict{verdict("a", Waiting), verdict("b", Waiting)}},
			Waiting, "a: waiting reason"},

		{"between steps H-1", Run{Status: store.RunRunning, UpdatedAt: *ago(89)}, Executing, "between steps; the run was updated 1m29s ago"},
		{"between steps H+1", Run{Status: store.RunRunning, UpdatedAt: *ago(91)}, Unknown, "no step running; the run was updated 1m31s ago"},
		{"between steps S-1", Run{Status: store.RunRunning, UpdatedAt: *ago(299)}, Unknown, "no step running; the run was updated 4m59s ago"},
		{"between steps S+1", Run{Status: store.RunRunning, UpdatedAt: *ago(301)}, Stalled, "no step running and the run silent for 5m1s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ClassifyRun(tc.r, th, checkedAt)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("got %s %q, want %s %q", got, reason, tc.want, tc.reason)
			}
		})
	}
}

// DESIGN 5.12: missing telemetry never yields executing, whatever else looks fresh.
func TestDiagnosisMissingTelemetry(t *testing.T) {
	fresh := ago(1)
	var nodes []Node
	for _, w := range []string{WaitModel, WaitTool, WaitHuman, WaitSetup, "", "napping"} {
		calls := []OpenCall{{ID: "c1", Name: "exec", StartedAt: *fresh}}
		// No heartbeat, or one older than H, with every other piece fresh.
		nodes = append(nodes,
			Node{WaitingOn: w, ProgressAt: fresh, ModelWaitSince: fresh, OpenCalls: calls},
			Node{HeartbeatAt: ago(91), WaitingOn: w, ProgressAt: fresh, ModelWaitSince: fresh, OpenCalls: calls})
	}
	nodes = append(nodes,
		// Waiting on the model, but the wait's start was never recorded.
		Node{HeartbeatAt: fresh, WaitingOn: WaitModel, ProgressAt: fresh},
		// Waiting on a tool, with no open call, or one whose start was never recorded.
		Node{HeartbeatAt: fresh, WaitingOn: WaitTool, ProgressAt: fresh},
		Node{HeartbeatAt: fresh, WaitingOn: WaitTool, OpenCalls: []OpenCall{{ID: "c1", Name: "exec", StartedAt: *fresh}, {ID: "c2", Name: "read"}}},
		// Setup, or nothing named, with no progress recorded.
		Node{HeartbeatAt: fresh, WaitingOn: WaitSetup},
		Node{HeartbeatAt: fresh},
		// A value no rule names.
		Node{HeartbeatAt: fresh, WaitingOn: "napping", ProgressAt: fresh},
	)
	for i, n := range nodes {
		if v := ClassifyNode(n, th, checkedAt); v.Liveness == Executing {
			t.Errorf("node %d (%+v) reads executing: %q", i, n, v.Reason)
		}
	}
	// A running run with no live visit and a stale row is not executing either.
	if got, reason := ClassifyRun(Run{Status: store.RunRunning, UpdatedAt: *ago(91)}, th, checkedAt); got == Executing {
		t.Fatalf("between steps: %s %q", got, reason)
	}
}
