package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Retry from the failed step (DESIGN 5.21). The rules' tests are named after the rule they
// pin: TestRetryRule1..., and carry the T5.21.n of the design.

// endedRun stores a run the way the engine leaves one that ended in status, stopped at
// node ("" for none): limits 40 steps, 60 tool calls, $2, 600 s; set is extra SQL
// assignments for its columns ("step_count=40, retries=4").
func (env *testEnv) endedRun(status, node, set string) *store.Run {
	env.t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "ended", Definition: json.RawMessage(`{}`)}
	if err := store.CreateWorkflow(ctx, env.pool, wf); err != nil {
		env.t.Fatal(err)
	}
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: json.RawMessage(`{}`), Trigger: store.TriggerManual,
		Limits: store.Limits{MaxSteps: 40, MaxToolCalls: 60, MaxCostUSD: 2, MaxDurationSeconds: 600}}
	if err := store.CreateRun(ctx, env.pool, run); err != nil {
		env.t.Fatal(err)
	}
	if set != "" {
		set = ", " + set
	}
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET status=$2, current_node=NULLIF($3,''), failure_reason='boom',
		finished_at=now(), started_at=now() `+set+` WHERE id=$1`, run.ID, status, node); err != nil {
		env.t.Fatal(err)
	}
	got, err := store.GetRun(ctx, env.pool, run.ID)
	if err != nil {
		env.t.Fatal(err)
	}
	return got
}

// reend puts a run that was retried back into a failed state, as if the retry had failed.
func (env *testEnv) reend(runID, status string) {
	env.t.Helper()
	if _, err := env.pool.Exec(context.Background(), `UPDATE runs SET status=$2, finished_at=now(), failure_reason='boom again'
		WHERE id=$1`, runID, status); err != nil {
		env.t.Fatal(err)
	}
}

func (env *testEnv) run(id string) *store.Run {
	env.t.Helper()
	r, err := store.GetRun(context.Background(), env.pool, id)
	if err != nil {
		env.t.Fatal(err)
	}
	return r
}

// wantRetryError fails unless err is a *RetryError with the code, the message and the
// details.
func wantRetryError(t *testing.T, err error, code, msg string, details ...string) {
	t.Helper()
	var re *RetryError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a *RetryError %s %q", err, code, msg)
	}
	if re.Code != code || re.Message != msg || !slices.Equal(re.Details, details) {
		t.Fatalf("refusal = %s %q %q, want %s %q %q", re.Code, re.Message, re.Details, code, msg, details)
	}
}

// runEvents are the payloads of a run's durable events of one type, in order.
func (env *testEnv) runEvents(runID, typ string) []map[string]any {
	env.t.Helper()
	rows, err := env.pool.Query(context.Background(), `SELECT data FROM run_events WHERE run_id=$1 AND event=$2 ORDER BY seq`, runID, typ)
	if err != nil {
		env.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			env.t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			env.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// unchanged fails when a refused retry changed the run.
func unchanged(t *testing.T, before, after *store.Run) {
	t.Helper()
	if after.Status != before.Status || after.Retries != before.Retries || after.Attempt != before.Attempt ||
		after.Limits != before.Limits || deref(after.FailureReason) != deref(before.FailureReason) ||
		(before.FinishedAt == nil) != (after.FinishedAt == nil) {
		t.Fatalf("a refused retry changed the run:\nbefore %+v\nafter  %+v", before, after)
	}
}

// near reports whether two instants agree to the precision the database keeps.
func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Millisecond && d < time.Millisecond
}

// ---- rule 1 -----------------------------------------------------------------------------

// T5.21.1: each status that is not failed, timed_out or cancelled, and a run with no
// current node, answer run_not_retryable with the texts of rule 1, and change nothing.
func TestRetryRule1RefusesWhatCannotBeRetried(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, status, node, msg string
	}{
		{"queued", store.RunQueued, "build", "run is queued"},
		{"running", store.RunRunning, "build", "run is running"},
		{"waiting for an approval", store.RunWaitingApproval, "gate", "run is waiting_approval"},
		{"succeeded", store.RunSucceeded, "end", "run is succeeded"},
		{"succeeded without a node", store.RunSucceeded, "", "run is succeeded"},
		{"failed before any node", store.RunFailed, "", "run has no step to retry from"},
		{"timed out before any node", store.RunTimedOut, "", "run has no step to retry from"},
		{"cancelled before any node", store.RunCancelled, "", "run has no step to retry from"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			before := env.endedRun(tc.status, tc.node, "")
			run, err := env.e.Retry(ctx, before.ID, "tester", nil)
			if run != nil {
				t.Fatalf("a refused retry returned %+v", run)
			}
			wantRetryError(t, err, "run_not_retryable", tc.msg)
			unchanged(t, before, env.run(before.ID))
			if n := len(env.runEvents(before.ID, "run.retry")); n != 0 {
				t.Fatalf("%d retry events for a refused retry", n)
			}
		})
	}

	t.Run("an empty current node is no step", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "", "")
		if _, err := env.pool.Exec(ctx, `UPDATE runs SET current_node='' WHERE id=$1`, before.ID); err != nil {
			t.Fatal(err)
		}
		_, err := env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "run_not_retryable", "run has no step to retry from")
	})

	t.Run("an unknown run", func(t *testing.T) {
		env := newEnv(t)
		if _, err := env.e.Retry(ctx, "no-such-run", "tester", nil); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want store.ErrNotFound", err)
		}
	})

	// The three statuses with a step to retry from are re-queued.
	for _, status := range []string{store.RunFailed, store.RunTimedOut, store.RunCancelled} {
		t.Run(status+" is retried", func(t *testing.T) {
			env := newEnv(t)
			before := env.endedRun(status, "build", "cancel_requested=true, cancel_by='operator'")
			run, err := env.e.Retry(ctx, before.ID, "tester", nil)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != store.RunQueued || run.Retries != 1 || run.Attempt != 1 || deref(run.CurrentNode) != "build" ||
				run.FailureReason != nil || run.FinishedAt != nil || run.CancelRequested || run.CancelBy != nil {
				t.Fatalf("retried run = %+v", run)
			}
			if got := env.run(before.ID); got.Status != store.RunQueued || got.Retries != 1 {
				t.Fatalf("stored run = %s retries %d", got.Status, got.Retries)
			}
		})
	}
}

// T5.21.2: the fifth retry passes, the sixth answers retry_limit.
func TestRetryRule1AtMostFiveRetries(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	run := env.endedRun(store.RunFailed, "build", "")
	for n := 1; n <= 5; n++ {
		got, err := env.e.Retry(ctx, run.ID, "tester", nil)
		if err != nil || got.Retries != n {
			t.Fatalf("retry %d: %+v, %v", n, got, err)
		}
		env.reend(run.ID, store.RunTimedOut)
	}
	before := env.run(run.ID)
	_, err := env.e.Retry(ctx, run.ID, "tester", nil)
	wantRetryError(t, err, "retry_limit", "run was retried 5 times")
	unchanged(t, before, env.run(run.ID))
	if before.Retries != 5 {
		t.Fatalf("retries = %d", before.Retries)
	}

	// A run that was never retried and one at 4 sit either side of the limit.
	four := env.endedRun(store.RunCancelled, "build", "retries=4")
	if got, err := env.e.Retry(ctx, four.ID, "tester", nil); err != nil || got.Retries != 5 {
		t.Fatalf("retry at 4: %+v, %v", got, err)
	}
	five := env.endedRun(store.RunCancelled, "build", "retries=5")
	_, err = env.e.Retry(ctx, five.ID, "tester", nil)
	wantRetryError(t, err, "retry_limit", "run was retried 5 times")
	// The status is checked before the count.
	ok := env.endedRun(store.RunSucceeded, "end", "retries=5")
	_, err = env.e.Retry(ctx, ok.ID, "tester", nil)
	wantRetryError(t, err, "run_not_retryable", "run is succeeded")
}

// ---- rule 2 -----------------------------------------------------------------------------

// T5.21.3: a counter at its cap refuses with one detail per exhausted limit; a raised
// limit passes; the raise is validated and clamped as at trigger time. Comparisons are
// >=, and the counters stay the run's totals.
func TestRetryRule2ExhaustedLimits(t *testing.T) {
	ctx := context.Background()

	t.Run("one exhausted limit", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=40, tool_call_count=3, cost_usd_micros=100")
		_, err := env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps", "max_steps: 40 of 40 used")
		unchanged(t, before, env.run(before.ID))
	})

	t.Run("every exhausted limit, in order", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=41, tool_call_count=60, cost_usd_micros=2000000")
		_, err := env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps, max_tool_calls, max_cost_usd",
			"max_steps: 41 of 40 used", "max_tool_calls: 60 of 60 used", "max_cost_usd: $2.00 of $2.00 used")
		unchanged(t, before, env.run(before.ID))
	})

	t.Run("a counter one under its cap passes", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=39, tool_call_count=59, cost_usd_micros=1999999")
		got, err := env.e.Retry(ctx, before.ID, "tester", nil)
		if err != nil || got.Status != store.RunQueued || got.StepCount != 39 {
			t.Fatalf("retry: %+v, %v", got, err)
		}
	})

	t.Run("a cost cap in fractions of a cent", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "cost_usd_micros=628, limits='{\"max_cost_usd\":0.0005}'")
		_, err := env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_cost_usd", "max_cost_usd: $0.000628 of $0.0005 used")
	})

	t.Run("a raised limit passes and replaces the run's limits", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=40, tool_call_count=3")
		got, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 80})
		if err != nil {
			t.Fatal(err)
		}
		// The limits a retry stores are resolved as at trigger time: what neither the
		// body nor the run set is filled from the defaults.
		want := before.Limits
		want.MaxSteps = 80
		want.MaxNodeIterations = store.DefaultLimits.MaxNodeIterations
		want.MaxLoopVisits = store.DefaultLimits.MaxLoopVisits
		want.ApprovalTimeoutSeconds = store.DefaultLimits.ApprovalTimeoutSeconds
		if got.Limits != want || env.run(before.ID).Limits != want {
			t.Fatalf("limits = %+v, want %+v", got.Limits, want)
		}
		if got.StepCount != 40 || got.ToolCallCount != 3 {
			t.Fatalf("counters = %d/%d: they stay the run's totals", got.StepCount, got.ToolCallCount)
		}
		ev := env.runEvents(before.ID, "run.retry")
		if len(ev) != 1 || ev[0]["limits"].(map[string]any)["max_steps"] != float64(80) {
			t.Fatalf("retry event = %v", ev)
		}
	})

	t.Run("a raise to the counter is still exhausted", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=40")
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 40})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps", "max_steps: 40 of 40 used")
		unchanged(t, before, env.run(before.ID))
	})

	t.Run("raising one limit does not excuse another", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=40, cost_usd_micros=2500000")
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 80})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_cost_usd", "max_cost_usd: $2.50 of $2.00 used")
		unchanged(t, before, env.run(before.ID))
		got, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 80, MaxCostUSD: 5})
		if err != nil || got.Limits.MaxSteps != 80 || got.Limits.MaxCostUSD != 5 {
			t.Fatalf("both raised: %+v, %v", got, err)
		}
	})

	t.Run("a lowered limit below the counter refuses", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunTimedOut, "build", "step_count=10")
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 5})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps", "max_steps: 10 of 5 used")
	})

	t.Run("the raise is clamped to the platform maximum", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=40")
		got, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 5000, MaxCostUSD: 100, MaxToolCalls: 100000,
			MaxDurationSeconds: 999999})
		if err != nil {
			t.Fatal(err)
		}
		cfg := testConfig()
		if got.Limits.MaxSteps != cfg.MaxSteps || got.Limits.MaxCostUSD != cfg.MaxCostUSD ||
			got.Limits.MaxToolCalls != cfg.MaxToolCalls || got.Limits.MaxDurationSeconds != cfg.MaxDurationSeconds {
			t.Fatalf("limits = %+v, want the platform maximums %d/%v/%d/%d", got.Limits, cfg.MaxSteps, cfg.MaxCostUSD,
				cfg.MaxToolCalls, cfg.MaxDurationSeconds)
		}
	})

	t.Run("a clamped raise can still be exhausted", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "step_count=200")
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: 5000})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps", "max_steps: 200 of 200 used")
	})

	t.Run("a negative limit is a validation error", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", "")
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxSteps: -1, MaxCostUSD: -0.5})
		var ve *ValidationError
		if !errors.As(err, &ve) || !slices.Equal(ve.Messages, []string{"limits.max_steps: must not be negative", "limits.max_cost_usd: must not be negative"}) {
			t.Fatalf("err = %v", err)
		}
		unchanged(t, before, env.run(before.ID))
	})

	t.Run("limits left out keep the run's, including an earlier retry's raise", func(t *testing.T) {
		env := newEnv(t)
		run := env.endedRun(store.RunFailed, "build", "step_count=40")
		if _, err := env.e.Retry(ctx, run.ID, "tester", &store.Limits{MaxSteps: 80, MaxDurationSeconds: 900}); err != nil {
			t.Fatal(err)
		}
		env.reend(run.ID, store.RunFailed)
		got, err := env.e.Retry(ctx, run.ID, "tester", &store.Limits{MaxCostUSD: 5})
		if err != nil {
			t.Fatal(err)
		}
		want := store.Limits{MaxSteps: 80, MaxToolCalls: 60, MaxCostUSD: 5, MaxDurationSeconds: 900,
			MaxNodeIterations: store.DefaultLimits.MaxNodeIterations, MaxLoopVisits: store.DefaultLimits.MaxLoopVisits,
			ApprovalTimeoutSeconds: store.DefaultLimits.ApprovalTimeoutSeconds}
		if got.Limits != want {
			t.Fatalf("limits = %+v, want %+v", got.Limits, want)
		}
	})

	t.Run("a participating agent that was deleted does not stop a retry with limits", func(t *testing.T) {
		env := newEnv(t)
		gone := env.agent(nil)
		def := workflow.SingleAgent(gone.ID, "work", nil, store.Limits{})
		snap, _ := json.Marshal(runSnapshot{Definition: *def})
		run := env.endedRun(store.RunFailed, "agent", "step_count=40")
		if _, err := env.pool.Exec(ctx, `UPDATE runs SET workflow_snapshot=$2 WHERE id=$1`, run.ID, snap); err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, gone.ID); err != nil {
			t.Fatal(err)
		}
		got, err := env.e.Retry(ctx, run.ID, "tester", &store.Limits{MaxSteps: 80})
		if err != nil || got.Limits.MaxSteps != 80 {
			t.Fatalf("retry: %+v, %v", got, err)
		}
	})

	t.Run("a participating agent's per-run cost cap still lowers the cost limit", func(t *testing.T) {
		env := newEnv(t)
		capped := env.agent(func(a *store.Agent) { a.Guardrails.MaxCostUSDPerRun = 1.5 })
		def := workflow.SingleAgent(capped.ID, "work", nil, store.Limits{})
		snap, _ := json.Marshal(runSnapshot{Definition: *def})
		run := env.endedRun(store.RunFailed, "agent", "cost_usd_micros=2000000")
		if _, err := env.pool.Exec(ctx, `UPDATE runs SET workflow_snapshot=$2 WHERE id=$1`, run.ID, snap); err != nil {
			t.Fatal(err)
		}
		got, err := env.e.Retry(ctx, run.ID, "tester", &store.Limits{MaxCostUSD: 10})
		// 10 is lowered to the agent's 1.5, which the 2.00 already spent exceeds.
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_cost_usd", "max_cost_usd: $2.00 of $1.50 used")
		if got != nil {
			t.Fatalf("returned %+v", got)
		}
	})
}

// T5.21.9: a retry is a trigger for the project's budget (DESIGN 5.19 rule 3): a run on a
// project at its cap is refused with the typed error and left as it was; one micro under
// the cap passes; a soft-deleted project cannot be over budget, and the run still serves
// its snapshot.
func TestRetryRule2ProjectBudgetRefusesTheRetry(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	p := env.project("https://gitlab.test/g/budget.git", false)
	// The run has cost $1.50 of its own $2.00, so its limits are not what refuses.
	run := env.endedRun(store.RunFailed, "build", fmt.Sprintf("project_id='%s', cost_usd_micros=1500000", p.ID))
	caps := func(day, month int64) {
		t.Helper()
		setProjectCaps(t, env, p, day, month)
	}

	caps(1_000_000, 0) // the day cap, below the run's spend
	_, err := env.e.Retry(ctx, run.ID, "tester", nil)
	var be *project.BudgetError
	_, dayEnd := store.DayBounds(env.clock.Now())
	if !errors.As(err, &be) || be.ProjectID != p.ID || be.ProjectName != p.Name || be.Window != project.WindowDay ||
		be.CapMicros != 1_000_000 || be.SpendMicros != 1_500_000 || !be.WindowEnd.Equal(dayEnd) {
		t.Fatalf("retry over the day cap: %v (%+v), want the day's *project.BudgetError", err, be)
	}
	var re *RetryError
	if errors.As(err, &re) {
		t.Fatalf("the budget refusal is a *RetryError: %+v", re)
	}
	unchanged(t, run, env.run(run.ID))
	if n := len(env.runEvents(run.ID, "run.retry")); n != 0 {
		t.Fatalf("a refused retry wrote %d retry events", n)
	}

	caps(1_500_000, 0) // spend exactly at the cap refuses
	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); !errors.As(err, &be) || be.SpendMicros != 1_500_000 {
		t.Fatalf("retry at the cap: %v", err)
	}
	caps(0, 1_000_000) // the month cap refuses alike
	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); !errors.As(err, &be) || be.Window != project.WindowMonth {
		t.Fatalf("retry over the month cap: %v", err)
	}
	unchanged(t, run, env.run(run.ID))

	caps(1_500_001, 0) // one micro under passes
	if got, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil || got.Status != store.RunQueued || got.Retries != 1 {
		t.Fatalf("retry one micro under the cap: %+v, %v", got, err)
	}

	// A deleted project is not checked, though the same spend is over its cap.
	caps(1_000_000, 0)
	env.reend(run.ID, store.RunFailed)
	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); !errors.As(err, &be) {
		t.Fatalf("retry again over the cap: %v", err)
	}
	if err := store.DeleteProject(ctx, env.pool, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil || got.Status != store.RunQueued || got.Retries != 2 {
		t.Fatalf("retry on a deleted project: %+v, %v", got, err)
	}
}

// visit stores a finished visit of a node on a run, as the walk leaves one.
func (env *testEnv) visit(runID, node string, n int, status string) {
	env.t.Helper()
	now := time.Now()
	rn := &store.RunNode{RunID: runID, NodeID: node, Visit: n, Status: status, StartedAt: &now, FinishedAt: &now}
	if err := store.CreateRunNode(context.Background(), env.pool, rn); err != nil {
		env.t.Fatal(err)
	}
}

// T5.21.3 and rule 2: the visits of current_node are compared with max_loop_visits as the
// node's entry check counts them (>=), under the limits that will apply, and the detail
// comes last, in the shape of the others.
func TestRetryRule2LoopVisitsOfTheResumedNode(t *testing.T) {
	ctx := context.Background()
	// endedRun's limits plus a loop cap of 3 visits, as a trigger resolves the default.
	const capped = `limits='{"max_steps":40,"max_tool_calls":60,"max_cost_usd":2,"max_duration_seconds":600,"max_loop_visits":3}'`

	t.Run("the cap is used up, alone and after the other limits", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", capped)
		for v := 1; v <= 3; v++ {
			env.visit(before.ID, "build", v, store.NodeFailed)
		}
		_, err := env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_loop_visits", "max_loop_visits: 3 of 3 used")
		unchanged(t, before, env.run(before.ID))

		if _, err := env.pool.Exec(ctx, `UPDATE runs SET step_count=40, cost_usd_micros=2000000 WHERE id=$1`, before.ID); err != nil {
			t.Fatal(err)
		}
		before = env.run(before.ID)
		_, err = env.e.Retry(ctx, before.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps, max_cost_usd, max_loop_visits",
			"max_steps: 40 of 40 used", "max_cost_usd: $2.00 of $2.00 used", "max_loop_visits: 3 of 3 used")
		unchanged(t, before, env.run(before.ID))
	})

	t.Run("under the cap, other nodes' visits, and no cap pass", func(t *testing.T) {
		env := newEnv(t)
		two := env.endedRun(store.RunFailed, "build", capped)
		env.visit(two.ID, "build", 1, store.NodeFailed)
		env.visit(two.ID, "build", 2, store.NodeFailed)
		for v := 1; v <= 5; v++ {
			env.visit(two.ID, "elsewhere", v, store.NodeSucceeded) // another node's visits do not count
		}
		if got, err := env.e.Retry(ctx, two.ID, "tester", nil); err != nil || got.Status != store.RunQueued {
			t.Fatalf("two visits of three: %+v, %v", got, err)
		}
		uncapped := env.endedRun(store.RunFailed, "build", "")
		for v := 1; v <= 4; v++ {
			env.visit(uncapped.ID, "build", v, store.NodeFailed)
		}
		if got, err := env.e.Retry(ctx, uncapped.ID, "tester", nil); err != nil || got.Status != store.RunQueued {
			t.Fatalf("a run with no loop cap: %+v, %v", got, err)
		}
	})

	t.Run("the body's limits are what the visits are compared with", func(t *testing.T) {
		env := newEnv(t)
		before := env.endedRun(store.RunFailed, "build", capped)
		for v := 1; v <= 3; v++ {
			env.visit(before.ID, "build", v, store.NodeFailed)
		}
		_, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxLoopVisits: 3})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_loop_visits", "max_loop_visits: 3 of 3 used")
		_, err = env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxLoopVisits: 2})
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_loop_visits", "max_loop_visits: 3 of 2 used")
		unchanged(t, before, env.run(before.ID))
		got, err := env.e.Retry(ctx, before.ID, "tester", &store.Limits{MaxLoopVisits: 4})
		if err != nil || got.Limits.MaxLoopVisits != 4 {
			t.Fatalf("raised to 4: %+v, %v", got, err)
		}
	})

	// The run the node's entry check would have failed at once now gets the cap in the
	// dialog and, raised, a fourth visit.
	t.Run("a node failed on its third visit, retried with the default cap and raised", func(t *testing.T) {
		f := newRetryReviewFlow(t, store.Limits{}) // the default cap of 3 visits
		env := f.env
		failedRun := env.resume(env.e, f.run.ID)
		for retry := 1; retry <= 2; retry++ { // visits 2 and 3 of "two" fail as well
			if _, err := env.e.Retry(ctx, f.run.ID, "tester", nil); err != nil {
				t.Fatalf("retry %d: %v", retry, err)
			}
			failedRun = env.resume(env.e, f.run.ID)
		}
		if failedRun.Status != store.RunFailed || deref(failedRun.CurrentNode) != "two" || failedRun.Limits.MaxLoopVisits != 3 {
			t.Fatalf("run = %s at %q, limits %+v", failedRun.Status, deref(failedRun.CurrentNode), failedRun.Limits)
		}
		if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got,
			[]string{"one#1:succeeded", "two#1:failed", "two#2:failed", "two#3:failed"}) {
			t.Fatalf("visits = %v", got)
		}

		_, err := env.e.Retry(ctx, f.run.ID, "tester", nil)
		wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_loop_visits", "max_loop_visits: 3 of 3 used")
		unchanged(t, failedRun, env.run(f.run.ID))

		f.failTwo.Store(false)
		if _, err := env.e.Retry(ctx, f.run.ID, "tester", &store.Limits{MaxLoopVisits: 4}); err != nil {
			t.Fatal(err)
		}
		done := env.resume(env.e, f.run.ID)
		if done.Status != store.RunSucceeded || done.Limits.MaxLoopVisits != 4 {
			t.Fatalf("retried run = %s %q limits %+v", done.Status, deref(done.FailureReason), done.Limits)
		}
		if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got,
			[]string{"one#1:succeeded", "two#1:failed", "two#2:failed", "two#3:failed", "two#4:succeeded", "three#1:succeeded"}) {
			t.Fatalf("visits after the raise = %v", got)
		}
	})
}

// ---- rule 3 -----------------------------------------------------------------------------

// T5.21.7: two retries at once, one is queued and the other is refused with
// run_not_retryable; one event follows.
func TestRetryRule3TwoConcurrentRetriesOneWins(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{2, 12} {
		t.Run(fmt.Sprintf("%d goroutines", n), func(t *testing.T) {
			env := newEnv(t)
			run := env.endedRun(store.RunTimedOut, "build", "")
			var start, done sync.WaitGroup
			start.Add(1)
			errs := make([]error, n)
			runs := make([]*store.Run, n)
			for i := range errs {
				done.Add(1)
				go func() {
					defer done.Done()
					start.Wait()
					runs[i], errs[i] = env.e.Retry(ctx, run.ID, "tester", nil)
				}()
			}
			start.Done()
			done.Wait()
			won := 0
			for i, err := range errs {
				if err == nil {
					won++
					continue
				}
				if runs[i] != nil {
					t.Fatalf("a refusal returned a run: %+v", runs[i])
				}
				var re *RetryError
				if !errors.As(err, &re) || re.Code != "run_not_retryable" || re.Message != "run is queued" {
					t.Fatalf("loser got %v", err)
				}
			}
			if won != 1 {
				t.Fatalf("%d retries were accepted", won)
			}
			if got := env.run(run.ID); got.Retries != 1 || got.Status != store.RunQueued {
				t.Fatalf("run = %s retries %d", got.Status, got.Retries)
			}
			if ev := env.runEvents(run.ID, "run.retry"); len(ev) != 1 {
				t.Fatalf("%d retry events", len(ev))
			}
		})
	}
}

// Rule 3: the event, the status broadcast and the log line follow the update.
func TestRetryRule3EventsFollowTheUpdate(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	run := env.endedRun(store.RunCancelled, "build", "")
	env.clock.Advance(90 * time.Minute)
	got, err := env.e.Retry(ctx, run.ID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := env.runEvents(run.ID, "run.retry")
	if len(ev) != 1 {
		t.Fatalf("retry events = %v", ev)
	}
	e := ev[0]
	at, perr := time.Parse(time.RFC3339Nano, fmt.Sprint(e["at"]))
	if e["retries"] != float64(1) || e["from_node"] != "build" || e["by"] != "operator" || e["limits"] != nil ||
		perr != nil || !at.Equal(env.clock.Now()) {
		t.Fatalf("retry event = %v (clock %v)", e, env.clock.Now())
	}
	statuses := env.runEvents(run.ID, "run.status")
	if len(statuses) == 0 {
		t.Fatal("no run.status event")
	}
	if last := statuses[len(statuses)-1]["run"].(map[string]any); last["status"] != "queued" || last["retries"] != float64(1) {
		t.Fatalf("run.status events = %v", statuses)
	}
	logs := env.runEvents(run.ID, "log")
	if len(logs) == 0 || logs[len(logs)-1]["message"] != "retry 1: continuing from build" || logs[len(logs)-1]["level"] != "info" {
		t.Fatalf("log events = %v", logs)
	}
	if got.Retries != 1 {
		t.Fatalf("returned run = %+v", got)
	}
	// The transition table of 5.1 rule 2 is not widened for any other path.
	if err := ValidTransition(store.RunFailed, store.RunQueued); err == nil {
		t.Fatal("failed → queued is valid outside a retry")
	}
}

// ---- rule 4 -----------------------------------------------------------------------------

var spanRetry = regexp.MustCompile(`"retry":\s*(\d+)`)

// retryReviewFlow is a three-node line: one (coder) → two (reviewer) → three (deployer).
// Visit 1 of "two" writes a partial file in its agent's workspace and fails the turn.
type retryReviewFlow struct {
	env           *testEnv
	run           *store.Run
	coder, review *store.Agent
	mu            sync.Mutex
	messages      map[string][]string // node → task message per send
	seen          map[string]string   // "<node>#<visit>" → the files the turn found when it started
	failTwo       atomic.Bool         // visit of "two" fails while set
}

func newRetryReviewFlow(t *testing.T, limits store.Limits) *retryReviewFlow {
	t.Helper()
	env := newEnv(t)
	f := &retryReviewFlow{env: env, messages: map[string][]string{}, seen: map[string]string{}}
	f.coder = env.agent(nil)
	f.review = env.agent(func(a *store.Agent) { a.Name = "Reviewer" })
	deployer := env.agent(func(a *store.Agent) { a.Name = "Deployer" })
	wf := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: f.coder.ID, Task: "write {{.input.what}}"},
		workflow.Node{ID: "two", AgentID: f.review.ID, Task: "review it", InputFrom: []string{"one"}},
		workflow.Node{ID: "three", AgentID: deployer.ID, Task: "ship it"}))
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"what":"fizzbuzz"}`), Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	f.run = run
	f.failTwo.Store(true)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, visit := visitOf(p.SessionKey)
		read := func(rel string) string {
			b, err := os.ReadFile(f.path(p, rel))
			if err != nil {
				return "missing"
			}
			return string(b)
		}
		f.mu.Lock()
		f.messages[node] = append(f.messages[node], p.Message)
		f.seen[fmt.Sprintf("%s#%d", node, visit)] = read("notes.md") + " | " + read("partial.txt")
		f.mu.Unlock()
		switch {
		case node == "one":
			writeFile(t, f.path(p, "notes.md"), "draft by one")
			go finishThenFinal(t, env, p, `{"summary":"drafted"}`)
		case node == "two" && f.failTwo.Load():
			writeFile(t, f.path(p, "partial.txt"), "half a review")
			go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
				"errorKind": "invalid_request", "errorMessage": "messages: bad shape",
				"errorDetail": map[string]any{"provider": "scripted", "model": "scripted-1", "httpStatus": 400}}))
		default:
			go finishThenFinal(t, env, p, fmt.Sprintf(`{"node":%q,"visit":%d}`, node, visit))
		}
		return startedTurn(p)
	}
	return f
}

// path is a file in the run directory of the workspace the turn p works in.
func (f *retryReviewFlow) path(p openclaw.ChatSendParams, rel string) string {
	return filepath.Join(turnWS(f.env, p), "runs", f.run.ID, rel)
}

func (f *retryReviewFlow) found(visit string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[visit]
}

// maskToken blanks the run token a task message carries, which differs per visit.
func maskToken(msg string) string {
	if tok := tokenIn(msg); tok != "" {
		return strings.ReplaceAll(msg, tok, "TOKEN")
	}
	return msg
}

// T5.21.4: a run that failed in its second node resumes there with the scripted
// provider: a new visit of the node, the failed one kept, state kept, counters
// continued, the failed visit's file present, the same task, a new root span marked
// with the retry.
func TestRetryRule4ResumesAtTheFailedNode(t *testing.T) {
	ctx := context.Background()
	f := newRetryReviewFlow(t, store.Limits{})
	env := f.env
	failedRun := env.resume(env.e, f.run.ID)
	if failedRun.Status != store.RunFailed || deref(failedRun.FailureReason) != "model error: invalid_request" ||
		deref(failedRun.CurrentNode) != "two" {
		t.Fatalf("first attempt = %s %q at %q", failedRun.Status, deref(failedRun.FailureReason), deref(failedRun.CurrentNode))
	}
	if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got, []string{"one#1:succeeded", "two#1:failed"}) {
		t.Fatalf("visits before the retry = %v", got)
	}
	if failedRun.StepCount == 0 {
		t.Fatalf("no steps counted before the retry: %+v", failedRun)
	}
	stateBefore := stateOf(t, failedRun)

	f.failTwo.Store(false)
	env.clock.Advance(3 * time.Hour)
	queued, err := env.e.Retry(ctx, f.run.ID, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != store.RunQueued || queued.StepCount != failedRun.StepCount || queued.CostUSDMicros != failedRun.CostUSDMicros {
		t.Fatalf("queued run = %s steps %d cost %d", queued.Status, queued.StepCount, queued.CostUSDMicros)
	}
	done := env.resume(env.e, f.run.ID)
	if done.Status != store.RunSucceeded {
		t.Fatalf("retried run = %s %q", done.Status, deref(done.FailureReason))
	}

	// A new visit of the node, the failed one stays in the trace, the walk goes on.
	if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got,
		[]string{"one#1:succeeded", "two#1:failed", "two#2:succeeded", "three#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	nodes := runNodes(t, env, f.run.ID)
	if deref(nodes[1].FailureReason) != "model error: invalid_request" || nodes[1].FinishedAt == nil {
		t.Fatalf("the failed visit was changed: %+v", nodes[1])
	}
	if nodes[1].SessionKey == nil || nodes[2].SessionKey == nil || *nodes[1].SessionKey == *nodes[2].SessionKey {
		t.Fatalf("the visits share a session: %v / %v", nodes[1].SessionKey, nodes[2].SessionKey)
	}
	// State is kept (one's output survives and is what two's task is built from).
	st := stateOf(t, done)
	for k, v := range stateBefore {
		if fmt.Sprint(st[k]) != fmt.Sprint(v) {
			t.Fatalf("state[%s] = %v, was %v", k, st[k], v)
		}
	}
	if st["two"].(map[string]any)["visit"] != float64(2) || st["three"] == nil {
		t.Fatalf("state = %v", st)
	}
	// Counters continue from the run's totals: visits two#2 and three#1 add two steps
	// each (the finish call and the closing reply).
	if done.StepCount != failedRun.StepCount+4 || done.ToolCallCount < failedRun.ToolCallCount ||
		done.CostUSDMicros < failedRun.CostUSDMicros {
		t.Fatalf("counters %d/%d/%d after %d/%d/%d", done.StepCount, done.ToolCallCount, done.CostUSDMicros,
			failedRun.StepCount, failedRun.ToolCallCount, failedRun.CostUSDMicros)
	}
	// The failed visit's partial edit is in the workspace the new visit starts from; so is
	// the file node one wrote (carried to two's agent at visit 1).
	if got := f.found("two#1"); got != "draft by one | missing" {
		t.Fatalf("visit 1 of two started with %q", got)
	}
	if got := f.found("two#2"); got != "draft by one | half a review" {
		t.Fatalf("visit 2 of two started with %q", got)
	}
	// The task text is the same.
	f.mu.Lock()
	two := f.messages["two"]
	f.mu.Unlock()
	if len(two) != 2 || maskToken(two[0]) != maskToken(two[1]) || !strings.Contains(two[1], `Context from earlier nodes:`) {
		t.Fatalf("task messages of two differ:\n%q", two)
	}
	// Two root spans: the first attempt's ended with its failure, the retry's carries the
	// number; nothing stays open.
	spans, _ := store.ListSpans(ctx, env.pool, f.run.ID)
	var roots []store.Span
	for _, s := range spans {
		if s.Kind == "run" {
			roots = append(roots, s)
		}
	}
	if len(roots) != 2 || roots[0].EndedAt == nil || roots[1].EndedAt == nil || !roots[1].StartedAt.After(roots[0].StartedAt) {
		t.Fatalf("root spans = %+v", roots)
	}
	if spanRetry.MatchString(string(roots[0].Attrs)) || strings.Contains(string(roots[0].Attrs), `"status": "succeeded"`) {
		t.Fatalf("first root span = %s", roots[0].Attrs)
	}
	if m := spanRetry.FindStringSubmatch(string(roots[1].Attrs)); m == nil || m[1] != "1" ||
		!strings.Contains(string(roots[1].Attrs), `"attempt": 1`) || !strings.Contains(string(roots[1].Attrs), `"status": "succeeded"`) {
		t.Fatalf("retry root span = %s", roots[1].Attrs)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, f.run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
	// The run reached its terminal states twice, and the retry marked the boundary.
	if ev := env.runEvents(f.run.ID, "run.retry"); len(ev) != 1 || ev[0]["from_node"] != "two" {
		t.Fatalf("retry events = %v", ev)
	}
}

// T5.21.4 (a run that stopped before a node): a limit that stops the walk as it enters
// the next node leaves current_node on it, with no visit. The retry refuses until the
// limit is raised, then the node's first visit starts from the files the earlier nodes
// left, brought forward as in 5.6 rule 13.
func TestRetryRule4ResumesBeforeANodeThatNeverStarted(t *testing.T) {
	ctx := context.Background()
	f := newRetryReviewFlow(t, store.Limits{MaxSteps: 2})
	env := f.env
	f.failTwo.Store(false)
	stopped := env.resume(env.e, f.run.ID)
	if stopped.Status != store.RunFailed || deref(stopped.FailureReason) != "step limit 2 exceeded" || deref(stopped.CurrentNode) != "two" {
		t.Fatalf("first attempt = %s %q at %q", stopped.Status, deref(stopped.FailureReason), deref(stopped.CurrentNode))
	}
	if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got, []string{"one#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	_, err := env.e.Retry(ctx, f.run.ID, "tester", nil)
	wantRetryError(t, err, "limits_exhausted", "limits exhausted: max_steps", "max_steps: 2 of 2 used")

	if _, err := env.e.Retry(ctx, f.run.ID, "tester", &store.Limits{MaxSteps: 10}); err != nil {
		t.Fatal(err)
	}
	done := env.resume(env.e, f.run.ID)
	if done.Status != store.RunSucceeded || done.Limits.MaxSteps != 10 {
		t.Fatalf("retried run = %s %q limits %+v", done.Status, deref(done.FailureReason), done.Limits)
	}
	if got := visitsOf(runNodes(t, env, f.run.ID)); !slices.Equal(got, []string{"one#1:succeeded", "two#1:succeeded", "three#1:succeeded"}) {
		t.Fatalf("visits = %v", got)
	}
	if got := f.found("two#1"); got != "draft by one | missing" {
		t.Fatalf("the first visit of two started with %q: the file of the earlier node did not come forward", got)
	}
	if done.StepCount != 2+4 {
		t.Fatalf("steps = %d: the counter continues from the run's total", done.StepCount)
	}
}

// Rule 4, edge case: a node that failed and followed its on_failure edge moved
// current_node to the target, so the retry resumes there, not at the node.
func TestRetryRule4ResumesAtTheOnFailureTarget(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	coder, fixer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Fixer" })
	wf := env.createWorkflow(line(workflow.Node{ID: "try", AgentID: coder.ID, Task: "try", OnFailure: "fix"},
		workflow.Node{ID: "fix", AgentID: fixer.ID, Task: "fix it"}))
	var fixFails atomic.Bool
	fixFails.Store(true)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		if node == "try" || (node == "fix" && fixFails.Load()) {
			go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
				"errorKind": "invalid_request", "errorMessage": "bad", "errorDetail": map[string]any{"httpStatus": 400}}))
			return startedTurn(p)
		}
		go finishThenFinal(t, env, p, `{"fixed":true}`)
		return startedTurn(p)
	}
	failedRun := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if failedRun.Status != store.RunFailed || deref(failedRun.CurrentNode) != "fix" {
		t.Fatalf("first attempt = %s at %q", failedRun.Status, deref(failedRun.CurrentNode))
	}
	fixFails.Store(false)
	if _, err := env.e.Retry(ctx, failedRun.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	done := env.resume(env.e, failedRun.ID)
	if got := visitsOf(runNodes(t, env, failedRun.ID)); done.Status != store.RunSucceeded ||
		!slices.Equal(got, []string{"try#1:failed", "fix#1:failed", "fix#2:succeeded"}) {
		t.Fatalf("run = %s, visits = %v", done.Status, got)
	}
}

// ---- rule 2: the time limit restarts ------------------------------------------------------

// T5.21.5: a run that timed out is retried and runs its full time limit again, each time:
// the time before the retry, the wait before it and the paused time of the earlier
// attempt do not count, and the limit counts from the injected clock's instant at the
// claim.
func TestRetryRule2TimeLimitRestarts(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 60 })
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "wait"})
	eventually(t, "first turn", func() bool { return len(env.fc.sendParams()) == 1 })
	env.clock.Advance(60 * time.Second)
	timedOut := env.wait(done, run.ID)
	if timedOut.Status != store.RunTimedOut || deref(timedOut.FailureReason) != "time limit 60s exceeded" || deref(timedOut.CurrentNode) != "agent" {
		t.Fatalf("first attempt = %s %q at %q", timedOut.Status, deref(timedOut.FailureReason), deref(timedOut.CurrentNode))
	}
	firstStart := *timedOut.StartedAt
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET paused_total_ms=120000 WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}

	// retry queues the retry and starts the claimed run in the background; the returned
	// channel closes when the worker lets it go. It waits for the attempt's turn n.
	retry := func(turn int) (claimedAt time.Time, finished chan struct{}) {
		t.Helper()
		queued, err := env.e.Retry(ctx, run.ID, "tester", nil)
		if err != nil {
			t.Fatal(err)
		}
		if queued.StartedAt != nil || queued.PausedTotalMs != 0 || queued.PausedAt != nil {
			t.Fatalf("queued retry: started_at=%v paused=%d: nothing of the earlier attempt's clock may survive", queued.StartedAt, queued.PausedTotalMs)
		}
		claimedAt = env.clock.Now()
		claimed, err := store.ClaimQueuedRun(ctx, env.pool)
		if err != nil || claimed.ID != run.ID {
			t.Fatalf("claim: %v", err)
		}
		finished = make(chan struct{})
		go func() {
			defer close(finished)
			env.e.runRun(ctx, claimed)
		}()
		eventually(t, fmt.Sprintf("turn %d", turn), func() bool { return len(env.fc.sendParams()) == turn })
		if got := env.run(run.ID); got.StartedAt == nil || !near(*got.StartedAt, claimedAt) {
			t.Fatalf("started_at = %v, want the claim's instant %v", got.StartedAt, claimedAt)
		}
		return claimedAt, finished
	}
	running := func(what string) {
		t.Helper()
		time.Sleep(150 * time.Millisecond) // several limit ticks
		if got := env.run(run.ID); got.Status != store.RunRunning {
			t.Fatalf("%s: %s %q", what, got.Status, deref(got.FailureReason))
		}
	}

	// Retry 1, after the operator took two hours: the limit is 60 s from the claim, not
	// from the first attempt's start (hours ago) and not shortened by its paused time.
	env.clock.Advance(2 * time.Hour)
	claimedAt, finished := retry(2)
	if near(claimedAt, firstStart) {
		t.Fatal("the clock did not move")
	}
	env.clock.Advance(60*time.Second - time.Millisecond)
	running("one ms before the restarted limit")
	env.clock.Advance(time.Millisecond)
	second := env.wait(finished, run.ID)
	if second.Status != store.RunTimedOut || deref(second.FailureReason) != "time limit 60s exceeded" || second.Retries != 1 {
		t.Fatalf("second attempt = %s %q retries %d", second.Status, deref(second.FailureReason), second.Retries)
	}

	// Retry 2 gets its full minute again, and a run that finishes inside it succeeds.
	env.clock.Advance(time.Hour)
	_, finished = retry(3)
	env.clock.Advance(59 * time.Second)
	running("59 s into the third attempt")
	finishThenFinal(t, env, env.fc.sendParams()[2], `{"summary":"made it"}`)
	third := env.wait(finished, run.ID)
	if third.Status != store.RunSucceeded || third.Retries != 2 {
		t.Fatalf("third attempt = %s %q retries %d", third.Status, deref(third.FailureReason), third.Retries)
	}
	// Only the first two attempts' turns were aborted by the limit.
	if n := len(env.fc.callsFor("chat.abort")); n != 2 {
		t.Fatalf("%d aborts", n)
	}
	// The trace keeps every attempt's root span with its own start.
	spans, _ := store.ListSpans(ctx, env.pool, run.ID)
	var starts []time.Time
	for _, s := range spans {
		if s.Kind == "run" {
			starts = append(starts, s.StartedAt)
		}
	}
	if len(starts) != 3 || !starts[1].After(starts[0]) || !starts[2].After(starts[1]) {
		t.Fatalf("root span starts = %v", starts)
	}
}

// refuseStartedAt makes the database refuse to stamp started_at on one run, as a failed
// write does; the retry's own clearing of it (to null) is not refused.
func refuseStartedAt(t *testing.T, env *testEnv, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION refuse_started_at() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'started_at refused by the test'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER refuse_started_at BEFORE UPDATE ON runs FOR EACH ROW
		WHEN (OLD.id = '%s' AND OLD.started_at IS NULL AND NEW.started_at IS NOT NULL) EXECUTE FUNCTION refuse_started_at()`, runID)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS refuse_started_at ON runs`)
		_, _ = env.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS refuse_started_at()`)
	})
}

// Rule 2, Time: when the write of started_at fails on the first claim after a retry (the
// row keeps null), the attempt still has its time limit, counted from the claim's instant
// in memory; a run with no start would never time out.
func TestRetryRule2TimeLimitSurvivesAFailedStartedAtWrite(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	a := env.agent(func(a *store.Agent) { a.Limits.MaxDurationSeconds = 60 })
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "wait"})
	eventually(t, "first turn", func() bool { return len(env.fc.sendParams()) == 1 })
	env.clock.Advance(60 * time.Second)
	if first := env.wait(done, run.ID); first.Status != store.RunTimedOut {
		t.Fatalf("first attempt = %s %q", first.Status, deref(first.FailureReason))
	}

	refuseStartedAt(t, env, run.ID)
	env.clock.Advance(time.Hour)
	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || claimed.ID != run.ID || claimed.StartedAt != nil {
		t.Fatalf("claim: %+v, %v", claimed, err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		env.e.runRun(ctx, claimed)
	}()
	eventually(t, "second turn", func() bool { return len(env.fc.sendParams()) == 2 })
	if got := env.run(run.ID); got.StartedAt != nil || got.Status != store.RunRunning {
		t.Fatalf("the refused write reached the row: %s, started_at %v", got.Status, got.StartedAt)
	}
	time.Sleep(150 * time.Millisecond) // several limit ticks: the minute has not passed
	if got := env.run(run.ID); got.Status != store.RunRunning {
		t.Fatalf("one minute early: %s %q", got.Status, deref(got.FailureReason))
	}
	env.clock.Advance(60 * time.Second)
	second := env.wait(finished, run.ID)
	if second.Status != store.RunTimedOut || deref(second.FailureReason) != "time limit 60s exceeded" || second.Retries != 1 {
		t.Fatalf("second attempt = %s %q retries %d: a run whose start was not written has no time limit",
			second.Status, deref(second.FailureReason), second.Retries)
	}
	if _, ok := env.e.unsavedStarts.Load(run.ID); ok {
		t.Fatal("the in-memory stamp outlived the attempt")
	}
}

// Rule 4: a run the stale-run sweeper failed ("worker lost") still has its root span
// open; the retry ends it, marked retried, and the claimed retry opens a root span of
// its own with the retry attribute instead of continuing it.
func TestRetryRule4EndsTheRootSpanAWorkerLostLeftOpen(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "work"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim: %v", err)
	}
	// The worker that held the run was lost on its second attempt, with the visit open.
	lostSpan := env.e.startSpan(ctx, run.ID, nil, "run", "run", env.clock.Now(), nil, map[string]any{"attempt": 2, "trigger": store.TriggerManual})
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET attempt=2, current_node='agent' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if requeued, failed, err := store.RequeueStaleRuns(ctx, env.pool, -time.Hour); err != nil || requeued != 0 || failed != 1 {
		t.Fatalf("sweep: requeued %d, failed %d, %v", requeued, failed, err)
	}
	lost := env.run(run.ID)
	if lost.Status != store.RunFailed || deref(lost.FailureReason) != "worker lost" {
		t.Fatalf("run = %s %q", lost.Status, deref(lost.FailureReason))
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, run.ID); n != 1 {
		t.Fatalf("%d spans open after the sweep, want the root span", n)
	}

	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, run.ID); n != 0 {
		t.Fatalf("%d spans open after the retry", n)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"recovered"}`)
		return startedTurn(p)
	}
	if done := env.resume(env.e, run.ID); done.Status != store.RunSucceeded {
		t.Fatalf("retried run = %s %q", done.Status, deref(done.FailureReason))
	}

	spans, _ := store.ListSpans(ctx, env.pool, run.ID)
	var roots []store.Span
	for _, sp := range spans {
		if sp.Kind == "run" {
			roots = append(roots, sp)
		}
	}
	if len(roots) != 2 || roots[0].ID != lostSpan || roots[0].EndedAt == nil || roots[1].EndedAt == nil {
		t.Fatalf("root spans = %+v", roots)
	}
	attrs := func(sp store.Span) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(sp.Attrs, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if first := attrs(roots[0]); first["retried"] != true || first["status"] != store.RunFailed || first["retry"] != nil {
		t.Fatalf("the lost attempt's span = %v", first)
	}
	if second := attrs(roots[1]); second["retry"] != float64(1) || second["status"] != store.RunSucceeded || second["retried"] != nil {
		t.Fatalf("the retry's span = %v: it must carry the retry attribute", second)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, run.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// ---- rule 4: approval nodes ---------------------------------------------------------------

// T5.21.6: a run cancelled while waiting at an approval node retries by asking again: a
// new approval is created on resume, the old one stays expired, and its decision still
// carries the run on.
func TestRetryRule4CancelledAtAnApprovalNodeAsksAgain(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	ag := env.agent(nil)
	wf := env.createWorkflow(line(workflow.Node{ID: "build", AgentID: ag.ID, Task: "build"},
		workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "go?"},
		workflow.Node{ID: "ship", AgentID: ag.ID, Task: "ship"}))
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	paused := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
	if paused.Status != store.RunWaitingApproval || deref(paused.CurrentNode) != "gate" {
		t.Fatalf("run = %s at %q", paused.Status, deref(paused.CurrentNode))
	}
	first := pendingApproval(t, env, paused.ID)
	cancelled, err := env.e.Cancel(ctx, paused.ID, "tester")
	if err != nil || cancelled.Status != store.RunCancelled {
		t.Fatalf("cancel: %+v, %v", cancelled, err)
	}
	if got, _ := store.GetApproval(ctx, env.pool, first.ID); got.Status != store.ApprovalExpired {
		t.Fatalf("the first approval is %s", got.Status)
	}

	queued, err := env.e.Retry(ctx, paused.ID, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}
	if queued.CancelRequested || queued.ResumeNodeVisitID != nil || queued.Status != store.RunQueued {
		t.Fatalf("retried run = %+v", queued)
	}
	again := env.resume(env.e, paused.ID)
	if again.Status != store.RunWaitingApproval || deref(again.CurrentNode) != "gate" {
		t.Fatalf("after the retry the run is %s at %q: a new approval was not asked", again.Status, deref(again.CurrentNode))
	}
	second := pendingApproval(t, env, paused.ID)
	if second.ID == first.ID {
		t.Fatal("the cancelled approval was reused")
	}
	if got := visitsOf(runNodes(t, env, paused.ID)); !slices.Equal(got, []string{"build#1:succeeded", "gate#1:failed", "gate#2:waiting_approval"}) {
		t.Fatalf("visits = %v", got)
	}
	if n := env.count(`SELECT count(*) FROM approvals WHERE run_id=$1`, paused.ID); n != 2 {
		t.Fatalf("%d approvals", n)
	}
	if _, err := env.e.DecideApproval(ctx, second.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	if done := env.resume(env.e, paused.ID); done.Status != store.RunSucceeded {
		t.Fatalf("after the decision: %s %q", done.Status, deref(done.FailureReason))
	}
	// The root span the cancel closed is not continued: the retry opened its own, and the
	// run that waited and resumed kept it.
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='run'`, paused.ID); n != 2 {
		t.Fatalf("%d root spans", n)
	}
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND ended_at IS NULL`, paused.ID); n != 0 {
		t.Fatalf("%d spans left open", n)
	}
}

// Rule 4: a retried run that waits at an approval node and resumes keeps the start the
// retry's claim gave it (the time limit does not restart at every decision).
func TestRetryRule4ApprovalAfterARetryKeepsTheRestartedStart(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	ag := env.agent(nil)
	wf := env.createWorkflow(line(workflow.Node{ID: "gate", Kind: workflow.KindApproval, Message: "go?"},
		workflow.Node{ID: "ship", AgentID: ag.ID, Task: "ship"}))
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	paused := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Limits: store.Limits{MaxDurationSeconds: 600}})
	if _, err := env.e.Cancel(ctx, paused.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Retry(ctx, paused.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	retryStart := env.clock.Now()
	again := env.resume(env.e, paused.ID)
	if again.Status != store.RunWaitingApproval || again.StartedAt == nil || !near(*again.StartedAt, retryStart) {
		t.Fatalf("run = %s started %v, want %v", again.Status, again.StartedAt, retryStart)
	}
	env.clock.Advance(5 * time.Minute)
	if _, err := env.e.DecideApproval(ctx, pendingApproval(t, env, paused.ID).ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	done := env.resume(env.e, paused.ID)
	if done.Status != store.RunSucceeded || done.StartedAt == nil || !near(*done.StartedAt, retryStart) {
		t.Fatalf("after the decision: %s started %v, want %v", done.Status, done.StartedAt, retryStart)
	}
}

// ---- rule 6: report back ------------------------------------------------------------------

// T5.21.8: a channel run reports its new outcome once more when the retry ends, under
// report-<run id>-terminal-r<n>, with " (retry <n>)" after the status; the first outcome
// keeps its key; nothing is sent twice.
func TestRetryRule6ReportsTheNewOutcome(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	env.e.deps.Cfg.PublicBaseURL = "https://yuno.example"
	conv := env.chiefConversation(chiefConvKey)
	a := env.agent(nil)
	var fail atomic.Bool
	fail.Store(true)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		switch {
		case strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix):
		case fail.Load():
			go env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "error", map[string]any{
				"errorKind": "invalid_request", "errorMessage": "bad", "errorDetail": map[string]any{"httpStatus": 400}}))
		default:
			go finishThenFinal(t, env, p, `{"summary":"it works now"}`)
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "say hello", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if r := env.wait(done, run.ID); r.Status != store.RunFailed {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	env.e.reports.Wait()
	if sends := env.reportSends(); len(sends) != 1 || sends[0].IdempotencyKey != "report-"+run.ID+"-end" ||
		!strings.HasPrefix(sends[0].Message, "[platform] Run "+run.ID+" failed after ") {
		t.Fatalf("first report = %+v", sends)
	}

	retryAndRun := func() *store.Run {
		t.Helper()
		if _, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil {
			t.Fatal(err)
		}
		if env.reportedAt("runs", run.ID) != nil {
			t.Fatal("the retry did not clear reported_at")
		}
		r := env.resume(env.e, run.ID)
		env.e.reports.Wait()
		return r
	}
	// Retry 1 fails again; retry 2 succeeds.
	if r := retryAndRun(); r.Status != store.RunFailed || r.Retries != 1 {
		t.Fatalf("retry 1 = %s retries %d", r.Status, r.Retries)
	}
	fail.Store(false)
	if r := retryAndRun(); r.Status != store.RunSucceeded || r.Retries != 2 {
		t.Fatalf("retry 2 = %s %q retries %d", r.Status, deref(r.FailureReason), r.Retries)
	}

	sends := env.reportSends()
	if len(sends) != 3 {
		t.Fatalf("reports = %+v", sends)
	}
	for i, want := range []struct{ key, status string }{
		{"report-" + run.ID + "-end", "failed"},
		{"report-" + run.ID + "-terminal-r1", "failed (retry 1)"},
		{"report-" + run.ID + "-terminal-r2", "succeeded (retry 2)"},
	} {
		p := sends[i]
		if p.IdempotencyKey != want.key || p.SessionKey != chiefConvKey || !p.Deliver ||
			!strings.HasPrefix(p.Message, "[platform] Run "+run.ID+" "+want.status+" after ") ||
			!strings.HasSuffix(p.Message, "https://yuno.example/runs/"+run.ID) {
			t.Fatalf("report %d = %+v, want key %s and status %q", i, p, want.key, want.status)
		}
		if id, ok := reportRunID(p.IdempotencyKey); !ok || id != run.ID {
			t.Fatalf("reportRunID(%q) = %q, %v", p.IdempotencyKey, id, ok)
		}
		if id, ok := reportRunID(p.IdempotencyKey + ":user"); !ok || id != run.ID {
			t.Fatalf("reportRunID(%q) = %q, %v: the conversation mirror cannot link the entry", p.IdempotencyKey+":user", id, ok)
		}
	}
	if !strings.Contains(sends[2].Message, `{"summary":"it works now"}`) {
		t.Fatalf("report = %q", sends[2].Message)
	}
	if env.reportedAt("runs", run.ID) == nil {
		t.Fatal("reported_at not set after the last report")
	}
	// Every later path finds it reported.
	env.e.reportRun(ctx, run.ID)
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if n := len(env.reportSends()); n != 3 {
		t.Fatalf("%d reports after the sweeps", n)
	}
}

// T5.21.8: the sweep picks up a retried run whose hook missed its end, since the retry
// cleared reported_at; a retried run that is not terminal is left alone.
func TestRetryRule6SweepReportsARetriedRunTheHookMissed(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t)
	conv := env.chiefConversation(chiefConvKey)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) { return startedTurn(p) }
	run := env.endedRun(store.RunFailed, "agent", "")
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET trigger='channel', trigger_ref=$2, reported_at=now() WHERE id=$1`,
		run.ID, store.ConversationRef(conv.ID)); err != nil {
		t.Fatal(err)
	}
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if n := len(env.reportSends()); n != 0 {
		t.Fatalf("a reported run was reported again: %d", n)
	}
	if _, err := env.e.Retry(ctx, run.ID, "tester", nil); err != nil {
		t.Fatal(err)
	}
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if n := len(env.reportSends()); n != 0 {
		t.Fatalf("a queued run was reported: %d", n)
	}
	// It ends without the hook (the stale-run sweep does that).
	reason := "worker lost"
	if _, err := store.TransitionRun(ctx, env.pool, run.ID, []string{store.RunQueued}, store.RunFailed, &reason, env.clock.Now()); err != nil {
		t.Fatal(err)
	}
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	sends := env.reportSends()
	if len(sends) != 1 || sends[0].IdempotencyKey != "report-"+run.ID+"-terminal-r1" ||
		!strings.HasPrefix(sends[0].Message, "[platform] Run "+run.ID+" failed (retry 1) after ") ||
		!strings.Contains(sends[0].Message, "worker lost") {
		t.Fatalf("reports = %+v", sends)
	}
}
