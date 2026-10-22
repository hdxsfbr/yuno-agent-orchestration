package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// DESIGN wave 3 rules 1 and 2 (TestDependencyEdges): the Planner's depends_on is stored
// ticket to ticket; a prerequisite in a later wave, the ticket itself, an unknown ticket and
// a cycle are refused; edges between merge requests are unique, and a self-edge and an edge
// that would close a cycle are refused with the cycle's path.
func TestDependencyEdges(t *testing.T) {
	ctx := context.Background()
	const state = `{"slicer": {"tickets": [
		{"id":"T1","title":"a","slice":"s","files":["a"],"tests":["x"]},
		{"id":"T2","title":"b","slice":"s","files":["b"],"tests":["x"],"depends_on":["T1"]},
		{"id":"T3","title":"c","slice":"s","files":["c"],"tests":["x"],"depends_on":["T1","T2","T1"]}]},
		"waves": {"waves": [{"n":1,"tickets":["T1","T2"]},{"n":2,"tickets":["T3"]}]}}`
	e := newTicketEnv(t, project.ProviderGitLab, state)
	res, err := e.call(e.run)
	if err != nil || res.IsError {
		t.Fatalf("publish = %+v %v", res, err)
	}
	var out struct {
		PlanID string `json:"plan_id"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	prereqs := func(ticket string) []string {
		t.Helper()
		list, err := store.DependenciesOf(ctx, testPool, store.DependencyTicket, store.TicketNode(out.PlanID, ticket))
		must(t, err)
		var ids []string
		for _, d := range list {
			if d.Source != store.DependencyPlanner || d.PrerequisiteKind != store.DependencyTicket || d.ProjectID != e.project.ID {
				t.Fatalf("edge = %+v", d)
			}
			_, id, _ := store.SplitTicketNode(d.PrerequisiteID)
			ids = append(ids, id)
		}
		return ids
	}
	if got := prereqs("T2"); len(got) != 1 || got[0] != "T1" {
		t.Fatalf("T2 depends on %v", got)
	}
	if got := prereqs("T3"); len(got) != 2 || got[0] != "T1" || got[1] != "T2" {
		t.Fatalf("T3 depends on %v", got)
	}
	if got := prereqs("T1"); len(got) != 0 {
		t.Fatalf("T1 depends on %v", got)
	}

	// The Planner's refusals, before anything is published.
	for name, c := range map[string]struct{ tickets, want string }{
		"later wave": {`{"id":"T1","title":"a","depends_on":["T3"]},{"id":"T2","title":"b"},{"id":"T3","title":"c"}`, "T1 depends on T3, which is in a later wave"},
		"self":       {`{"id":"T1","title":"a","depends_on":["T1"]},{"id":"T2","title":"b"},{"id":"T3","title":"c"}`, "T1 depends on itself"},
		"unknown":    {`{"id":"T1","title":"a","depends_on":["T9"]},{"id":"T2","title":"b"},{"id":"T3","title":"c"}`, "T1 depends on T9, which is not a ticket of the plan"},
		"cycle":      {`{"id":"T1","title":"a","depends_on":["T2"]},{"id":"T2","title":"b","depends_on":["T1"]},{"id":"T3","title":"c"}`, "would make a cycle: T1 -> T2 -> T1"},
	} {
		run := &store.Run{Input: json.RawMessage(`{"feature":"checkout"}`), State: json.RawMessage(`{"slicer":{"tickets":[` + c.tickets +
			`]},"waves":{"waves":[{"n":1,"tickets":["T1","T2"]},{"n":2,"tickets":["T3"]}]}}`)}
		if _, err := readPublishPlan(run); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	// Edges between merge requests (rule 1).
	mr := func(number int64) *store.MergeRequest {
		t.Helper()
		r := newRunOf(t, e, `{"feature":"x"}`, `{}`)
		rec := &store.MergeRequest{ProjectID: e.project.ID, RunID: r.ID, Provider: "gitlab", Number: number, URL: "u", SourceBranch: "b", TargetBranch: "main"}
		ok, err := store.InsertMergeRequest(ctx, testPool, rec)
		must(t, err)
		if !ok {
			t.Fatal("record not inserted")
		}
		return rec
	}
	m17, m24, m31 := mr(17), mr(24), mr(31)
	edge := func(dep, pre *store.MergeRequest) (bool, error) {
		return store.AddDependency(ctx, testPool, &store.Dependency{ProjectID: e.project.ID, DependentKind: store.DependencyMergeRequest,
			DependentID: dep.ID, PrerequisiteKind: store.DependencyMergeRequest, PrerequisiteID: pre.ID, Source: store.DependencyOperator})
	}
	if ok, err := edge(m24, m17); !ok || err != nil {
		t.Fatalf("first edge = %v %v", ok, err)
	}
	if ok, err := edge(m24, m17); ok || err != nil {
		t.Fatalf("the same edge again = %v %v, want not inserted", ok, err)
	}
	if _, err := edge(m17, m17); !errors.Is(err, store.ErrSelfDependency) {
		t.Fatalf("self-edge = %v", err)
	}
	if ok, err := edge(m17, m31); !ok || err != nil {
		t.Fatalf("second edge = %v %v", ok, err)
	}
	var cycle *store.DependencyCycleError
	if _, err := edge(m31, m24); !errors.As(err, &cycle) || err.Error() != "would make a cycle: !31 -> !24 -> !17 -> !31" {
		t.Fatalf("cycle = %v", err)
	}
	// The operator's set replaces the operator's edges only.
	changed, err := store.ReplaceMergeRequestPrerequisites(ctx, testPool, e.project.ID, m24.ID, []string{m31.ID}, store.DependencyChief, nil)
	if err != nil || !changed {
		t.Fatalf("replace = %v %v", changed, err)
	}
	list, err := store.DependenciesOf(ctx, testPool, store.DependencyMergeRequest, m24.ID)
	must(t, err)
	if len(list) != 1 || list[0].PrerequisiteID != m31.ID || list[0].Source != store.DependencyChief {
		t.Fatalf("after the replace = %+v", list)
	}
	if changed, err := store.ReplaceMergeRequestPrerequisites(ctx, testPool, e.project.ID, m24.ID, []string{m31.ID}, store.DependencyOperator, nil); err != nil || changed {
		t.Fatalf("the same set again = %v %v", changed, err)
	}
}
