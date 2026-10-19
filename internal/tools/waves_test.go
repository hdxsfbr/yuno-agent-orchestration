package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// T5.29.10, tool side: advance_wave resolves the run by its token, calls the platform's
// function, and answers the model with the result or the refusal.

func TestAdvanceWaveTool(t *testing.T) {
	var gotRun string
	calls := 0
	deps := AdvanceWaveDeps{
		AdvanceWave: func(_ context.Context, runID string) (AdvanceResult, error) {
			calls++
			gotRun = runID
			return AdvanceResult{PlanID: "plan-1", Wave: 2, Closed: 3, Released: 2, Last: false}, nil
		},
		RunIDForToken: func(token string) (string, bool) {
			if token != "r_ok" {
				return "", false
			}
			return "run-9", true
		},
	}
	tool := NewAdvanceWave(deps)
	if tool.Name != "advance_wave" || tool.Risk != RiskSideEffecting || tool.Chat {
		t.Fatalf("tool = %+v", tool)
	}
	if got := EffectivePolicy(tool, nil); got != PolicyRequireApproval {
		t.Fatalf("policy = %q", got)
	}
	run := func(tl *Tool, input string) (Result, error) {
		return tl.Exec(context.Background(), Call{RunID: "run-caller", Input: json.RawMessage(input)})
	}

	res, err := run(tool, `{"run_token":"r_ok"}`)
	if err != nil || calls != 1 || gotRun != "run-9" {
		t.Fatalf("call: %v %d %q", err, calls, gotRun)
	}
	var out AdvanceResult
	if json.Unmarshal(res.Output, &out) != nil || out != (AdvanceResult{PlanID: "plan-1", Wave: 2, Closed: 3, Released: 2}) {
		t.Fatalf("result = %s", res.Output)
	}
	var raw map[string]any
	_ = json.Unmarshal(res.Output, &raw)
	for _, k := range []string{"plan_id", "wave", "closed", "released", "last"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("result lacks %q: %s", k, res.Output)
		}
	}

	// Input the model got wrong never reaches the platform.
	for name, input := range map[string]string{
		"missing token": `{}`,
		"blank token":   `{"run_token":"  "}`,
		"unknown token": `{"run_token":"r_other"}`,
		"malformed":     `{`,
	} {
		_, err := run(tool, input)
		var me *ModelError
		if !errors.As(err, &me) {
			t.Errorf("%s: err = %v, want a ModelError", name, err)
		}
	}
	if calls != 1 {
		t.Fatalf("a refused input reached the platform: %d calls", calls)
	}

	// A refusal is the model's error with its reason; any other failure is internal.
	refuse := deps
	refuse.AdvanceWave = func(context.Context, string) (AdvanceResult, error) {
		return AdvanceResult{}, &AdvanceRefused{Reason: "the project's waves are paused"}
	}
	_, err = run(NewAdvanceWave(refuse), `{"run_token":"r_ok"}`)
	var me *ModelError
	if !errors.As(err, &me) || me.Message != "the project's waves are paused" {
		t.Fatalf("refusal = %v", err)
	}
	broken := deps
	broken.AdvanceWave = func(context.Context, string) (AdvanceResult, error) { return AdvanceResult{}, errors.New("db down") }
	if _, err = run(NewAdvanceWave(broken), `{"run_token":"r_ok"}`); err == nil || errors.As(err, &me) {
		t.Fatalf("internal failure = %v", err)
	}

	// No function bound: not available.
	_, err = run(NewAdvanceWave(AdvanceWaveDeps{RunIDForToken: deps.RunIDForToken}), `{"run_token":"r_ok"}`)
	if !errors.As(err, &me) || me.Message != "advance_wave is not available" {
		t.Fatalf("nil function = %v", err)
	}
	// A chat has no run.
	if _, err = tool.Exec(context.Background(), Call{Chat: true, Input: json.RawMessage(`{"run_token":"r_ok"}`)}); !errors.As(err, &me) {
		t.Errorf("chat call = %v", err)
	}
}
