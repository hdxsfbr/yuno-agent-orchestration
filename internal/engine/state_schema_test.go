package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// The run state is checked against the workflow's state_schema after every agent node
// (audit #16). The dev pipeline's reviewer is given a looser output schema than the
// state schema, so a verdict outside the enum passes finish and is caught here.
func TestStateSchemaFailsNode(t *testing.T) {
	env := newEnv(t)
	stored := devPipeline(t, env)
	var def workflow.Definition
	if err := json.Unmarshal(stored.Definition, &def); err != nil {
		t.Fatal(err)
	}
	for i := range def.Nodes {
		if def.Nodes[i].ID == "reviewer" {
			def.Nodes[i].OutputSchema = anyObject
		}
	}
	wf := env.createWorkflow(def)
	env.script(map[string][]string{
		"coder":    {`{"summary":"done","files":["fizz.py"]}`},
		"reviewer": {`{"verdict":"maybe","feedback":"Not sure."}`},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"task":"x"}`)})
	const prefix = "state schema: at /reviewer/verdict: "
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), prefix) {
		t.Fatalf("run = %s %q, want failed with %q…", r.Status, deref(r.FailureReason), prefix)
	}
	nodes := runNodes(t, env, r.ID)
	if got := visitsOf(nodes); !slices.Equal(got, []string{"coder#1:succeeded", "reviewer#1:failed"}) {
		t.Fatalf("visits = %v", got)
	}
	if deref(nodes[1].FailureReason) != deref(r.FailureReason) {
		t.Fatalf("node reason %q, run reason %q", deref(nodes[1].FailureReason), deref(r.FailureReason))
	}
	if st := stateOf(t, r); st["reviewer"] != nil || st["coder"] == nil {
		t.Fatalf("state = %s: the refused output must not be stored", r.State)
	}
}

// A state schema failure is the node's own failure: on_failure applies (DESIGN 5.6
// rule 12), and a valid output passes.
func TestStateSchemaFollowsOnFailure(t *testing.T) {
	tests := []struct {
		name, output string
		visits       []string
	}{
		{"invalid state follows on_failure", `{"ok":"yes"}`, []string{"a#1:failed", "fix#1:succeeded"}},
		{"valid state continues", `{"ok":true}`, []string{"a#1:succeeded"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			ag := env.agent(nil)
			def := line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "try", OnFailure: "fix"})
			def.Nodes = append(def.Nodes, workflow.Node{ID: "fix", Kind: workflow.KindAgent, AgentID: ag.ID, Task: "repair", OutputSchema: anyObject})
			def.Edges = append(def.Edges, workflow.Edge{From: "fix", To: "end"})
			def.StateSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}`)
			wf := env.createWorkflow(def)
			env.script(map[string][]string{"a": {tc.output}, "fix": {`{"fixed":true}`}})
			r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID})
			if r.Status != store.RunSucceeded {
				t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
			}
			if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, tc.visits) {
				t.Fatalf("visits = %v", got)
			}
		})
	}
}

// pending_effects is the engine's record, not node output: a strict state schema that
// does not name it is not broken by it.
func TestStateSchemaIgnoresPendingEffects(t *testing.T) {
	env := newEnv(t)
	ag := env.agent(nil)
	def := line(workflow.Node{ID: "a", AgentID: ag.ID, Task: "try"})
	def.StateSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":"object"}},"additionalProperties":false}`)
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPendingEffect(context.Background(), env.pool, run.ID, json.RawMessage(`{"tool":"deploy_release"}`)); err != nil {
		t.Fatal(err)
	}
	env.script(map[string][]string{"a": {`{}`}})
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
}
