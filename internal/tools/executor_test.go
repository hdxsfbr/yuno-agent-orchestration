package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/store"
)

func newService(t *testing.T, f *fixture, ct *countingTarget) (*Service, *noSleep) {
	t.Helper()
	r := NewRegistry()
	r.Register(NewDeployRelease(DeployConfig{TargetURL: ct.srv.URL, MaxBytes: 1 << 20}))
	ns := &noSleep{}
	return &Service{Pool: testPool, Registry: r, MaxOutputBytes: 32768, Sleep: ns.sleep,
		Workspace: func(string) string { return f.workspace }}, ns
}

const deployInputJSON = `{"run_token":"r_tok","version":"1.0.0","notes":"n","paths":["app.py"]}`

func releaseID(t *testing.T, out json.RawMessage) (string, bool) {
	t.Helper()
	var o struct {
		ReleaseID string `json:"release_id"`
		Replayed  bool   `json:"replayed"`
	}
	if err := json.Unmarshal(out, &o); err != nil || o.ReleaseID == "" {
		t.Fatalf("not a release: %s", out)
	}
	return o.ReleaseID, o.Replayed
}

// T5.4.1: the same input twice in one visit makes one external call; the second
// result is the stored one, marked replayed.
func TestSideEffectReplayInVisit(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	svc, _ := newService(t, f, ct)
	tool, _ := svc.Registry.Get(DeployReleaseName)
	ctx := context.Background()

	first, err := svc.Execute(ctx, tool, f.call(t, f.visit, DeployReleaseName, deployInputJSON))
	if err != nil || first.Result.IsError || first.Replayed {
		t.Fatalf("first: %+v %v", first, err)
	}
	// Same input with keys reordered and another token: still the same effect.
	second, err := svc.Execute(ctx, tool, f.call(t, f.visit, DeployReleaseName,
		`{"paths":["app.py"],"notes":"n","version":"1.0.0","run_token":"r_other"}`))
	if err != nil || second.Result.IsError || !second.Replayed {
		t.Fatalf("second: %+v %v", second, err)
	}
	id1, _ := releaseID(t, first.Result.Output)
	id2, replayed := releaseID(t, second.Result.Output)
	if id1 != id2 || !replayed {
		t.Fatalf("second output %s, first %s", second.Result.Output, first.Result.Output)
	}
	if n := ct.posts.Load(); n != 1 {
		t.Fatalf("deploy target called %d times, want 1", n)
	}
	se, err := store.GetSideEffect(ctx, pool, f.call(t, f.visit, DeployReleaseName, deployInputJSON).IdempotencyKey)
	if err != nil || se.ToolName != DeployReleaseName || se.RunID != f.run.ID {
		t.Fatalf("side effect row: %+v %v", se, err)
	}
	list, err := store.ListSideEffects(ctx, pool, f.run.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSideEffects = %d rows, %v", len(list), err)
	}
}

// T5.4.2: the same input in a later visit (a loop) is a new effect.
func TestSideEffectNewVisit(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	svc, _ := newService(t, f, ct)
	tool, _ := svc.Registry.Get(DeployReleaseName)
	ctx := context.Background()
	a, err := svc.Execute(ctx, tool, f.call(t, f.visit, DeployReleaseName, deployInputJSON))
	if err != nil || a.Result.IsError {
		t.Fatalf("visit 1: %+v %v", a, err)
	}
	b, err := svc.Execute(ctx, tool, f.call(t, f.newVisit(t, pool), DeployReleaseName, deployInputJSON))
	if err != nil || b.Result.IsError || b.Replayed {
		t.Fatalf("visit 2: %+v %v", b, err)
	}
	ida, _ := releaseID(t, a.Result.Output)
	idb, _ := releaseID(t, b.Result.Output)
	if ida == idb || ct.created.Load() != 2 {
		t.Fatalf("want two releases, got %s and %s (created %d)", ida, idb, ct.created.Load())
	}
}

// T5.4.3: an external failure deletes the key and a retry succeeds with one effect.
// T5.5.7: a side effect is attempted at most twice on transient errors.
func TestSideEffectFailureReleasesKey(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	svc, ns := newService(t, f, ct)
	tool, _ := svc.Registry.Get(DeployReleaseName)
	ctx := context.Background()
	call := f.call(t, f.visit, DeployReleaseName, deployInputJSON)

	ct.failNext("busy", http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable)
	out, err := svc.Execute(ctx, tool, call)
	if err != nil || !out.Result.IsError || out.Attempts != 2 {
		t.Fatalf("failing call: %+v %v", out, err)
	}
	if n := ct.posts.Load(); n != 2 {
		t.Fatalf("side effect attempted %d times, want 2", n)
	}
	if len(ns.waits) != 1 || ns.waits[0] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s]", ns.waits)
	}
	if _, err := store.GetSideEffect(ctx, pool, call.IdempotencyKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("key kept after a failure: %v", err)
	}

	ct.failNext("") // the third queued failure is dropped; the target works again
	retry, err := svc.Execute(ctx, tool, call)
	if err != nil || retry.Result.IsError || retry.Replayed {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if ct.created.Load() != 1 {
		t.Fatalf("releases created = %d, want 1", ct.created.Load())
	}
	if _, err := store.GetSideEffect(ctx, pool, call.IdempotencyKey); err != nil {
		t.Fatalf("key not recorded after success: %v", err)
	}

	// A permanent failure is not retried: one attempt, key released.
	ct.failNext("bad request", http.StatusBadRequest)
	other := f.call(t, f.newVisit(t, pool), DeployReleaseName, deployInputJSON)
	before := ct.posts.Load()
	perm, err := svc.Execute(ctx, tool, other)
	if err != nil || !perm.Result.IsError || perm.Attempts != 1 || ct.posts.Load()-before != 1 {
		t.Fatalf("permanent failure: %+v %v (posts %d)", perm, err, ct.posts.Load()-before)
	}
}

// Concurrent calls with one key (OpenClaw runs a turn's tool calls at once): the
// second waits on the key's lock and replays.
func TestSideEffectConcurrentSameKey(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	ct.delay = 200 * time.Millisecond
	svc, _ := newService(t, f, ct)
	tool, _ := svc.Registry.Get(DeployReleaseName)
	var wg sync.WaitGroup
	outs := make([]Outcome, 2)
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := svc.Execute(context.Background(), tool, f.call(t, f.visit, DeployReleaseName, deployInputJSON))
			if err != nil {
				t.Error(err)
			}
			outs[i] = o
		}()
	}
	wg.Wait()
	if ct.posts.Load() != 1 || outs[0].Replayed == outs[1].Replayed {
		t.Fatalf("posts %d, replayed %v/%v", ct.posts.Load(), outs[0].Replayed, outs[1].Replayed)
	}
}

// T5.4.6: the engine's fallback execution of a gated call whose effect an earlier MCP
// call already produced replays it: one effect.
func TestExecuteApprovedReplays(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	svc, _ := newService(t, f, ct)
	tool, _ := svc.Registry.Get(DeployReleaseName)
	ctx := context.Background()
	call := f.call(t, f.visit, DeployReleaseName, deployInputJSON)
	if out, err := svc.Execute(ctx, tool, call); err != nil || out.Result.IsError {
		t.Fatalf("mcp call: %+v %v", out, err)
	}

	gated := gatedRow(t, f, call)
	out, isErr, err := svc.ExecuteApproved(ctx, gated.ID)
	if err != nil || isErr {
		t.Fatalf("ExecuteApproved: %s %v %v", out, isErr, err)
	}
	if _, replayed := releaseID(t, out); !replayed || ct.posts.Load() != 1 {
		t.Fatalf("fallback output %s, posts %d", out, ct.posts.Load())
	}
	row, _ := store.GetToolCall(ctx, pool, gated.ID)
	if row.Status != store.ToolExecuted || row.FinishedAt == nil {
		t.Fatalf("row after fallback: %+v", row)
	}
	// A second fallback finds the row finished and executes nothing.
	again, _, err := svc.ExecuteApproved(ctx, gated.ID)
	if err != nil || string(again) != string(row.Output) || ct.posts.Load() != 1 {
		t.Fatalf("second fallback: %s %v (posts %d)", again, err, ct.posts.Load())
	}
}

// T5.3.5 (executor side): a call approved after the MCP wait gave up runs once, from
// the stored row, under its key, with the workspace rebuilt from the visit's agent.
func TestExecuteApprovedRuns(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	f.writeRunFile(t, "app.py", "print(1)")
	ct := newCountingTarget(t)
	svc, _ := newService(t, f, ct)
	var gotWorkspaceFor string
	svc.Workspace = func(id string) string { gotWorkspaceFor = id; return f.workspace }
	ctx := context.Background()
	call := f.call(t, f.visit, DeployReleaseName, deployInputJSON)
	gated := gatedRow(t, f, call)

	out, isErr, err := svc.ExecuteApproved(ctx, gated.ID)
	if err != nil || isErr || ct.created.Load() != 1 || ct.keys[0] != call.IdempotencyKey {
		t.Fatalf("ExecuteApproved: %s %v %v (created %d)", out, isErr, err, ct.created.Load())
	}
	if gotWorkspaceFor != *f.agent.OpenClawAgentID {
		t.Fatalf("workspace resolved for %q", gotWorkspaceFor)
	}
	row, _ := store.GetToolCall(ctx, pool, gated.ID)
	stored, _ := CanonicalJSON(row.Output)
	returned, _ := CanonicalJSON(out)
	if row.Status != store.ToolExecuted || string(stored) != string(returned) {
		t.Fatalf("row %s output %s, returned %s", row.Status, row.Output, out)
	}
	if _, _, err := svc.ExecuteApproved(ctx, "no-such-call"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// gatedRow inserts the awaiting_approval row the MCP gate records for a call.
func gatedRow(t *testing.T, f *fixture, call Call) *store.ToolCall {
	t.Helper()
	key := call.IdempotencyKey
	now := time.Now().UTC()
	row := &store.ToolCall{RunID: f.run.ID, RunNodeID: call.NodeVisitID, ToolUseID: "mcp-" + store.NewID(),
		ToolName: DeployReleaseName, Input: call.Input, Status: store.ToolAwaitingApproval, IdempotencyKey: &key, StartedAt: &now}
	must(t, store.InsertToolCall(context.Background(), testPool, row))
	return row
}

// T5.5.6: read-only and computational tools are retried up to three times on
// transient errors with 1 s, 2 s backoff; each attempt of a retried call is a span.
func TestRetryTransient(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	tests := []struct {
		name         string
		script       []Result
		wantAttempts int
		wantErr      bool
		wantWaits    []time.Duration
	}{
		{"transient twice then success", []Result{{IsError: true, Transient: true, Text: "timed out"}, {IsError: true, Transient: true, Text: "HTTP 503"}, {Text: "ok"}}, 3, false, []time.Duration{time.Second, 2 * time.Second}},
		{"always transient gives up after three", []Result{{IsError: true, Transient: true, Text: "t"}, {IsError: true, Transient: true, Text: "t"}, {IsError: true, Transient: true, Text: "t"}, {Text: "never"}}, 3, true, []time.Duration{time.Second, 2 * time.Second}},
		{"permanent error is not retried", []Result{{IsError: true, Text: "HTTP 404"}, {Text: "never"}}, 1, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := NewRegistry()
			r.Register(&Tool{Name: "probe", Risk: RiskReadOnly, Exec: func(context.Context, Call) (Result, error) {
				res := tc.script[calls]
				calls++
				return res, nil
			}})
			tool, _ := r.Get("probe")
			ns := &noSleep{}
			svc := &Service{Pool: pool, Registry: r, Sleep: ns.sleep}
			call := Call{RunID: f.run.ID, ToolCallID: store.NewID()}
			out, err := svc.Execute(context.Background(), tool, call)
			if err != nil || out.Attempts != tc.wantAttempts || calls != tc.wantAttempts || out.Result.IsError != tc.wantErr {
				t.Fatalf("outcome %+v err %v calls %d", out, err, calls)
			}
			if len(ns.waits) != len(tc.wantWaits) {
				t.Fatalf("waits = %v, want %v", ns.waits, tc.wantWaits)
			}
			for i := range ns.waits {
				if ns.waits[i] != tc.wantWaits[i] {
					t.Fatalf("waits = %v, want %v", ns.waits, tc.wantWaits)
				}
			}
			spans, _ := store.ListSpans(context.Background(), pool, f.run.ID)
			n := 0
			for _, s := range spans {
				if strings.Contains(string(s.Attrs), call.ToolCallID) {
					n++
				}
			}
			if want := tc.wantAttempts; (want > 1 && n != want) || (want == 1 && n != 0) {
				t.Fatalf("attempt spans = %d for %d attempts", n, want)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := Backoff(i + 1); got != w*time.Second {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w*time.Second)
		}
	}
}
