package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// T5.29.5, tool side: escalate validates its input, calls the engine's function with the
// run, kind and trimmed reason, and answers the model.

func TestEscalateTool(t *testing.T) {
	var gotRun, gotKind, gotReason string
	calls := 0
	tool := NewEscalate(EscalateDeps{Escalate: func(_ context.Context, runID, kind, reason string) error {
		calls++
		gotRun, gotKind, gotReason = runID, kind, reason
		return nil
	}})
	if tool.Name != "escalate" || tool.Risk != RiskComputational || tool.Hits != HitsPostgres || tool.Chat {
		t.Fatalf("tool = %+v", tool)
	}
	run := func(input string) (Result, error) {
		return tool.Exec(context.Background(), Call{RunID: "run-1", Input: json.RawMessage(input)})
	}

	res, err := run(`{"run_token":"r_x","kind":"prd_gap","reason":"  the PRD never says who may export  "}`)
	if err != nil || calls != 1 || gotRun != "run-1" || gotKind != "prd_gap" || gotReason != "the PRD never says who may export" {
		t.Fatalf("call: %v %d %q %q %q", err, calls, gotRun, gotKind, gotReason)
	}
	var out map[string]string
	if json.Unmarshal(res.Output, &out) != nil || out["status"] != "escalated" || out["kind"] != "prd_gap" {
		t.Fatalf("result = %s", res.Output)
	}

	for name, input := range map[string]string{
		"empty reason":   `{"run_token":"r_x","kind":"other","reason":"  "}`,
		"long reason":    `{"run_token":"r_x","kind":"other","reason":"` + strings.Repeat("é", 2001) + `"}`,
		"unknown kind":   `{"run_token":"r_x","kind":"whim","reason":"r"}`,
		"missing kind":   `{"run_token":"r_x","reason":"r"}`,
		"malformed JSON": `{`,
	} {
		_, err := run(input)
		var me *ModelError
		if !errors.As(err, &me) {
			t.Errorf("%s: err = %v, want a ModelError", name, err)
		}
	}
	if calls != 1 {
		t.Fatalf("a refused input reached the engine: %d calls", calls)
	}
	// 2000 characters is allowed.
	if _, err := run(`{"run_token":"r_x","kind":"loop_limit","reason":"` + strings.Repeat("é", 2000) + `"}`); err != nil || calls != 2 {
		t.Fatalf("2000 characters: %v %d", err, calls)
	}

	// A chat call has no run.
	_, err = tool.Exec(context.Background(), Call{Chat: true, Input: json.RawMessage(`{"run_token":"r_x","kind":"other","reason":"r"}`)})
	var me *ModelError
	if !errors.As(err, &me) {
		t.Errorf("chat call = %v", err)
	}

	// The engine's answers: a run that ended is the model's error, anything else internal.
	ended := NewEscalate(EscalateDeps{Escalate: func(context.Context, string, string, string) error { return errors.New("the run has ended already") }})
	_, err = ended.Exec(context.Background(), Call{RunID: "r", Input: json.RawMessage(`{"run_token":"r_x","kind":"other","reason":"r"}`)})
	if !errors.As(err, &me) {
		t.Errorf("ended run = %v", err)
	}
	broken := NewEscalate(EscalateDeps{Escalate: func(context.Context, string, string, string) error { return errors.New("db down") }})
	_, err = broken.Exec(context.Background(), Call{RunID: "r", Input: json.RawMessage(`{"run_token":"r_x","kind":"other","reason":"r"}`)})
	if err == nil || errors.As(err, &me) {
		t.Errorf("internal failure = %v", err)
	}
	if _, err = NewEscalate(EscalateDeps{}).Exec(context.Background(), Call{RunID: "r", Input: json.RawMessage(`{"run_token":"r_x","kind":"other","reason":"r"}`)}); err == nil {
		t.Error("an unconfigured tool ran")
	}
}
