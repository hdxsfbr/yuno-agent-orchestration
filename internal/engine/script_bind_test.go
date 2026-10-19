package engine

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"yuno/internal/store"
)

type fakeBinder struct {
	mu    sync.Mutex
	ids   map[string][]string // by project id and trigger
	taken []string
}

func (b *fakeBinder) TakeBound(projectID, trigger string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := projectID + "/" + trigger
	q := b.ids[key]
	if len(q) == 0 {
		return "", false
	}
	b.ids[key] = q[1:]
	b.taken = append(b.taken, q[0])
	return q[0], true
}

// A run the platform starts for a project (an issue's run, a ship run) takes the script an
// eval bound to the project and trigger; any other run, and a run that names its own
// script, is started as it always was (DESIGN 5.29 rule 17).
func TestPlatformRunsTakeBoundScripts(t *testing.T) {
	env, p := goProject(t)
	ctx := context.Background()
	wf := projectWorkflow(env, env.agent(nil))
	b := &fakeBinder{ids: map[string][]string{p.ID + "/issue": {"s-first", "s-second"}, p.ID + "/plan": {"s-ship"}}}
	env.e.deps.Scripts = b

	overrides := func(run *store.Run) runOverrides {
		var snap runSnapshot
		if err := json.Unmarshal(run.WorkflowSnapshot, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.RunOverrides == nil {
			return runOverrides{}
		}
		return *snap.RunOverrides
	}
	trigger := func(req TriggerRequest) *store.Run {
		t.Helper()
		req.WorkflowID, req.ProjectID = wf.ID, p.ID
		run, err := env.e.Trigger(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}

	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerIssue, TriggerRef: p.ID + "#1"})); o.ScriptID != "s-first" || o.Model != ScriptedRunModel {
		t.Errorf("first issue run: %+v", o)
	}
	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerPlan, TriggerRef: "plan:x:wave:1"})); o.ScriptID != "s-ship" {
		t.Errorf("ship run: %+v", o)
	}
	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerManual})); o != (runOverrides{}) {
		t.Errorf("manual run: %+v", o)
	}
	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerEval, ScriptID: "own", Model: "scripted/scripted-1"})); o.ScriptID != "own" {
		t.Errorf("a run that names its script: %+v", o)
	}
	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerIssue, TriggerRef: p.ID + "#2"})); o.ScriptID != "s-second" {
		t.Errorf("second issue run: %+v", o)
	}
	if o := overrides(trigger(TriggerRequest{Trigger: store.TriggerIssue, TriggerRef: p.ID + "#3"})); o != (runOverrides{}) {
		t.Errorf("an issue run with nothing bound: %+v", o)
	}
	if len(b.taken) != 3 {
		t.Errorf("bindings taken: %v", b.taken)
	}
}
