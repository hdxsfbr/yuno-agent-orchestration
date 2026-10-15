package evals

import (
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// goodTrace is a complete, successful trace for a case with max_steps 10 and 120 s.
func goodTrace() store.Trace {
	return store.Trace{
		Run: &store.Run{
			ID:         "run1",
			Status:     store.RunSucceeded,
			StepCount:  4,
			Limits:     store.Limits{MaxSteps: 10, MaxDurationSeconds: 120},
			StartedAt:  ptr(t0),
			FinishedAt: ptr(t0.Add(10 * time.Second)),
		},
		ModelCalls: []store.ModelCall{
			{ID: "m1", Model: "scripted-1", InputTokens: 100, OutputTokens: 20},
			{ID: "m2", Model: "scripted-1", InputTokens: 150, OutputTokens: 30},
			{ID: "m3", Model: "scripted-1", InputTokens: 200, OutputTokens: 10},
		},
		ToolCalls: []store.ToolCall{
			{ID: "t1", ToolName: "exec", Status: store.ToolExecuted, FinishedAt: ptr(t0.Add(time.Second))},
			{ID: "t2", ToolName: "write", Status: store.ToolExecuted, FinishedAt: ptr(t0.Add(2 * time.Second))},
		},
		Spans: []store.Span{{ID: "s1", Kind: "run", Name: "run"}},
	}
}

func baseCase() Case {
	return Case{
		ID: "c1", Version: "v1", Split: SplitPublic,
		Agent:  AgentSpec{Limits: Limits{MaxSteps: 10, MaxDurationSeconds: 120}},
		Expect: Expect{Status: store.RunSucceeded},
	}
}

func TestEvalTermination(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Case, tr *store.Trace)
		pass   bool
		want   string // substring of Detail
	}{
		{name: "within limits", pass: true, want: "4 of 10 steps"},
		{name: "running is not terminal", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunRunning
			tr.Run.FinishedAt = nil
		}, want: `status "running" is not terminal`},
		{name: "waiting approval is not terminal", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunWaitingApproval
		}, want: "not terminal"},
		{name: "terminal without finished_at", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = nil
		}, want: "no finished_at"},
		{name: "step_count over the limit", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.StepCount = 11
		}, want: "step_count 11 exceeds max_steps 10"},
		{name: "step_count exactly at the limit", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.StepCount = 10
		}},
		{name: "one step over is the step that tripped the limit", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunFailed
			tr.Run.Limits.MaxSteps = 2
			tr.Run.StepCount = 3
			tr.Run.FailureReason = ptr("step limit 2 exceeded")
		}},
		{name: "two steps over fails even with the step limit reason", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunFailed
			tr.Run.Limits.MaxSteps = 2
			tr.Run.StepCount = 4
			tr.Run.FailureReason = ptr("step limit 2 exceeded")
		}, want: "step_count 4 exceeds max_steps 2"},
		{name: "one step over without the step limit reason fails", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Limits.MaxSteps = 2
			tr.Run.StepCount = 3
		}, want: "step_count 3 exceeds max_steps 2"},
		{name: "duration over the limit", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = ptr(t0.Add(121 * time.Second))
		}, want: "exceeds max_duration_seconds 120"},
		{name: "duration at the limit", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = ptr(t0.Add(120 * time.Second))
		}},
		{name: "a timed-out run may finish within the abort window", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunTimedOut
			tr.Run.FailureReason = ptr("time limit 120s exceeded")
			tr.Run.FinishedAt = ptr(t0.Add(120*time.Second + timeoutEnforcementWindow))
		}},
		{name: "a timed-out run past the abort window fails", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunTimedOut
			tr.Run.FinishedAt = ptr(t0.Add(120*time.Second + timeoutEnforcementWindow + time.Millisecond))
		}, want: "exceeds max_duration_seconds 120"},
		{name: "the abort window is only for timed-out runs", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = ptr(t0.Add(125 * time.Second))
		}, want: "exceeds max_duration_seconds 120"},
		{name: "paused time does not count", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = ptr(t0.Add(150 * time.Second))
			tr.Run.PausedTotalMs = 40000
		}},
		{name: "paused time cannot hide an overrun", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FinishedAt = ptr(t0.Add(200 * time.Second))
			tr.Run.PausedTotalMs = 40000
		}, want: "exceeds max_duration_seconds"},
		{name: "never started (cancelled while queued)", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Status = store.RunCancelled
			tr.Run.StartedAt = nil
			tr.Run.StepCount = 0
		}},
		{name: "falls back to the case limits", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Limits = store.Limits{}
			c.Agent.Limits.MaxSteps = 3
		}, want: "step_count 4 exceeds max_steps 3"},
		{name: "falls back to platform defaults", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Limits = store.Limits{}
			c.Agent.Limits = Limits{}
			tr.Run.StepCount = store.DefaultLimits.MaxSteps + 1
		}, want: "exceeds max_steps 40"},
		{name: "platform defaults accept a normal run", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.Limits = store.Limits{}
			c.Agent.Limits = Limits{}
		}},
		{name: "no run in the trace", mutate: func(c *Case, tr *store.Trace) { tr.Run = nil }, want: "no run"},
		{name: "several problems are all reported", mutate: func(c *Case, tr *store.Trace) {
			tr.Run.StepCount = 99
			tr.Run.FinishedAt = ptr(t0.Add(999 * time.Second))
		}, want: "step_count 99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, tr := baseCase(), goodTrace()
			if tt.mutate != nil {
				tt.mutate(&c, &tr)
			}
			got := evalTermination(c, tr)
			if got.Name != EvalTermination {
				t.Fatalf("name = %q", got.Name)
			}
			if got.Passed != tt.pass {
				t.Fatalf("passed = %v, want %v (detail: %s)", got.Passed, tt.pass, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.want) {
				t.Fatalf("detail %q does not contain %q", got.Detail, tt.want)
			}
		})
	}
}

func TestEvalTerminationReportsEveryProblem(t *testing.T) {
	c, tr := baseCase(), goodTrace()
	tr.Run.StepCount = 99
	tr.Run.FinishedAt = ptr(t0.Add(999 * time.Second))
	got := evalTermination(c, tr)
	if !strings.Contains(got.Detail, "step_count 99") || !strings.Contains(got.Detail, "max_duration_seconds") {
		t.Fatalf("detail = %q, want both problems", got.Detail)
	}
}

func TestEvalTraceIntegrity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "complete trace", pass: true, want: "3 model calls, 2 tool calls, 1 spans"},
		{name: "no calls at all but a span", pass: true, mutate: func(tr *store.Trace) {
			tr.ModelCalls, tr.ToolCalls = nil, nil
		}},
		{name: "tool call still awaiting approval", mutate: func(tr *store.Trace) {
			tr.ToolCalls[0].Status = store.ToolAwaitingApproval
		}, want: "non-terminal status"},
		{name: "tool call approved but not executed", mutate: func(tr *store.Trace) {
			tr.ToolCalls[0].Status = store.ToolApproved
		}, want: `non-terminal status "approved"`},
		{name: "tool call with empty status", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].Status = ""
		}, want: "t2"},
		{name: "tool call without finished_at", mutate: func(tr *store.Trace) {
			tr.ToolCalls[1].FinishedAt = nil
		}, want: "t2 (write) has no finished_at"},
		{name: "failed tool call is still terminal", pass: true, mutate: func(tr *store.Trace) {
			tr.ToolCalls[0].Status = store.ToolFailed
		}},
		{name: "rejected, blocked and expired are terminal", pass: true, mutate: func(tr *store.Trace) {
			tr.ToolCalls[0].Status = store.ToolRejected
			tr.ToolCalls[1].Status = store.ToolBlocked
			tr.ToolCalls = append(tr.ToolCalls, store.ToolCall{ID: "t3", Status: store.ToolExpired, FinishedAt: ptr(t0)})
		}},
		{name: "model call without a model", mutate: func(tr *store.Trace) {
			tr.ModelCalls[1].Model = "  "
		}, want: "model call m2 has no model"},
		{name: "negative input tokens", mutate: func(tr *store.Trace) {
			tr.ModelCalls[0].InputTokens = -1
		}, want: "m1 has negative tokens"},
		{name: "negative output tokens", mutate: func(tr *store.Trace) {
			tr.ModelCalls[0].OutputTokens = -5
		}, want: "negative tokens"},
		{name: "negative cache read tokens", mutate: func(tr *store.Trace) {
			tr.ModelCalls[0].CacheReadTokens = -1
		}, want: "negative tokens"},
		{name: "negative cache write tokens", mutate: func(tr *store.Trace) {
			tr.ModelCalls[0].CacheWriteTokens = -1
		}, want: "negative tokens"},
		{name: "zero tokens are fine", pass: true, mutate: func(tr *store.Trace) {
			tr.ModelCalls[0].InputTokens, tr.ModelCalls[0].OutputTokens = 0, 0
		}},
		{name: "no spans", mutate: func(tr *store.Trace) { tr.Spans = nil }, want: "no spans"},
		{name: "no run", mutate: func(tr *store.Trace) { tr.Run = nil }, want: "no run"},
		{name: "many violations are summarised", mutate: func(tr *store.Trace) {
			tr.Spans = nil
			for i := range tr.ToolCalls {
				tr.ToolCalls[i].FinishedAt = nil
				tr.ToolCalls[i].Status = ""
			}
		}, want: "and 2 more"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := goodTrace()
			if tt.mutate != nil {
				tt.mutate(&tr)
			}
			got := evalTraceIntegrity(baseCase(), tr)
			if got.Name != EvalTraceIntegrity {
				t.Fatalf("name = %q", got.Name)
			}
			if got.Passed != tt.pass {
				t.Fatalf("passed = %v, want %v (detail: %s)", got.Passed, tt.pass, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.want) {
				t.Fatalf("detail %q does not contain %q", got.Detail, tt.want)
			}
		})
	}
}

func TestEvalExpectation(t *testing.T) {
	failed := func(tr *store.Trace, reason string) {
		tr.Run.Status = store.RunFailed
		tr.Run.FailureReason = ptr(reason)
	}
	tests := []struct {
		name   string
		mutate func(c *Case, tr *store.Trace)
		pass   bool
		want   string
	}{
		{name: "status matches", pass: true, want: "succeeded as expected"},
		{name: "status differs", mutate: func(c *Case, tr *store.Trace) {
			failed(tr, "boom")
		}, want: `status "failed", want "succeeded"`},
		{name: "failure reason contains the text", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect = Expect{Status: store.RunFailed, FailureReasonContains: "step limit 2 exceeded"}
			failed(tr, "step limit 2 exceeded")
		}},
		{name: "failure reason match ignores case", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect = Expect{Status: store.RunFailed, FailureReasonContains: "STEP LIMIT"}
			failed(tr, "Step limit 2 exceeded while running")
		}},
		{name: "failure reason differs", mutate: func(c *Case, tr *store.Trace) {
			c.Expect = Expect{Status: store.RunFailed, FailureReasonContains: "step limit 2 exceeded"}
			failed(tr, "agent did not call finish")
		}, want: `does not contain "step limit 2 exceeded"`},
		{name: "failure reason expected but run has none", mutate: func(c *Case, tr *store.Trace) {
			c.Expect = Expect{Status: store.RunSucceeded, FailureReasonContains: "oops"}
		}, want: `failure reason "" does not contain "oops"`},
		{name: "no failure reason expectation ignores the reason", pass: true, mutate: func(c *Case, tr *store.Trace) {
			tr.Run.FailureReason = ptr("anything")
		}},
		{name: "enough tool calls", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.MinToolCalls = 2
		}},
		{name: "too few tool calls", mutate: func(c *Case, tr *store.Trace) {
			c.Expect.MinToolCalls = 3
		}, want: "2 tool calls, want at least 3"},
		{name: "enough model calls", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.MinModelCalls = 3
		}},
		{name: "too few model calls", mutate: func(c *Case, tr *store.Trace) {
			c.Expect.MinModelCalls = 4
		}, want: "3 model calls, want at least 4"},
		{name: "cancelled as expected", pass: true, mutate: func(c *Case, tr *store.Trace) {
			c.Expect.Status = store.RunCancelled
			tr.Run.Status = store.RunCancelled
		}},
		{name: "no run", mutate: func(c *Case, tr *store.Trace) { tr.Run = nil }, want: "no run"},
		{name: "all problems are reported", mutate: func(c *Case, tr *store.Trace) {
			c.Expect = Expect{Status: store.RunFailed, FailureReasonContains: "x", MinToolCalls: 9, MinModelCalls: 9}
		}, want: "tool calls, want at least 9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, tr := baseCase(), goodTrace()
			if tt.mutate != nil {
				tt.mutate(&c, &tr)
			}
			got := evalExpectation(c, tr)
			if got.Name != EvalExpectation {
				t.Fatalf("name = %q", got.Name)
			}
			if got.Passed != tt.pass {
				t.Fatalf("passed = %v, want %v (detail: %s)", got.Passed, tt.pass, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.want) {
				t.Fatalf("detail %q does not contain %q", got.Detail, tt.want)
			}
		})
	}
}

func TestEvaluateRunsAllInOrder(t *testing.T) {
	got := Evaluate(baseCase(), goodTrace())
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
		if !r.Passed {
			t.Errorf("%s failed on a good trace: %s", r.Name, r.Detail)
		}
	}
	want := []string{EvalTermination, EvalTraceIntegrity, EvalExpectation}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("evaluators = %v, want %v", names, want)
	}
	if strings.Join(EvaluatorNames(), ",") != strings.Join(want, ",") {
		t.Fatalf("EvaluatorNames = %v, want %v", EvaluatorNames(), want)
	}
}

func TestEvaluateOnAnEmptyTraceFailsWithoutPanicking(t *testing.T) {
	got := Evaluate(baseCase(), store.Trace{})
	if len(got) != 3 {
		t.Fatalf("got %d results", len(got))
	}
	for _, r := range got {
		if r.Passed {
			t.Errorf("%s passed on an empty trace", r.Name)
		}
	}
}

func TestSafeEvaluateTurnsAPanicIntoAFailure(t *testing.T) {
	boom := namedEvaluator{name: "boom", fn: func(Case, store.Trace) EvalResult { panic("kaput") }}
	got := safeEvaluate(boom, baseCase(), goodTrace())
	if got.Passed || got.Name != "boom" || !strings.Contains(got.Detail, "kaput") {
		t.Fatalf("got %+v", got)
	}
}

func TestFailedResults(t *testing.T) {
	got := failedResults("api down")
	if len(got) != 3 {
		t.Fatalf("got %d results", len(got))
	}
	for i, r := range got {
		if r.Passed || r.Detail != "api down" || r.Name != EvaluatorNames()[i] {
			t.Errorf("result %d = %+v", i, r)
		}
	}
}

func TestScoreRubric(t *testing.T) {
	if got := ScoreRubric(baseCase(), goodTrace()); got.Score != 1 || got.Criteria != 0 {
		t.Fatalf("empty rubric = %+v, want score 1", got)
	}
	c := baseCase()
	c.Rubric = []map[string]any{{"id": "a"}, {"id": "b"}}
	got := ScoreRubric(c, goodTrace())
	if got.Score != 0 || got.Criteria != 2 || got.Detail == "" {
		t.Fatalf("non-empty rubric = %+v, want unscored with a reason", got)
	}
}
