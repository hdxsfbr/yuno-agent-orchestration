package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"yuno/internal/store"
)

// Chief platform control, DESIGN 5.34 rules 1, 3 and 5 through the API: a merge
// request's prerequisites are set and read, a self-edge, an unknown request and a cycle are
// refused, the Planner's edges of its ticket are listed and not editable, and the record
// read says what holds the request.
func TestMergeRequestDependencyRoutes(t *testing.T) {
	e := newForgeEnv(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM dependencies WHERE project_id = $1`, e.project.ID)
		_, _ = e.pool.Exec(context.Background(), `DELETE FROM plans WHERE project_id = $1`, e.project.ID)
	})
	record := func(number int64) *store.MergeRequest {
		t.Helper()
		rec := &store.MergeRequest{ProjectID: e.project.ID, RunID: projectRun(t, e.project).ID, Provider: "gitlab", Number: number,
			URL: "https://gitlab.test/acme/app/-/merge_requests/1", SourceBranch: "b", TargetBranch: "main", Title: "t"}
		ok, err := store.InsertMergeRequest(ctx, e.pool, rec)
		mustDo(t, err)
		if !ok {
			t.Fatal("record not inserted")
		}
		return rec
	}
	m17, m24, m31 := record(17), record(24), record(31)
	path := func(id string) string { return "/api/merge-requests/" + id + "/dependencies" }
	read := func(body []byte) dependenciesOut {
		t.Helper()
		var out dependenciesOut
		mustDo(t, json.Unmarshal(body, &out))
		return out
	}

	code, body := e.call(http.MethodGet, path(m24.ID), "", true)
	if out := read(body); code != http.StatusOK || len(out.Prerequisites) != 0 || len(out.BlockedBy) != 0 {
		t.Fatalf("empty = %d %s", code, body)
	}

	code, body = e.call(http.MethodPut, path(m24.ID), `{"prerequisites":["`+m17.ID+`"]}`, true)
	out := read(body)
	if code != http.StatusOK || len(out.Prerequisites) != 1 || out.Prerequisites[0].Label != "!17" || out.Prerequisites[0].Source != "operator" ||
		!out.Prerequisites[0].Editable || out.Prerequisites[0].Satisfied || len(out.BlockedBy) != 1 || out.BlockedBy[0].Reason != "!17 has not been checked yet" {
		t.Fatalf("put = %d %s", code, body)
	}
	// The record read says what holds it.
	code, body = e.call(http.MethodGet, "/api/merge-requests/"+m24.ID, "", true)
	var rec struct {
		ID        string `json:"id"`
		BlockedBy []struct {
			Prerequisite string `json:"prerequisite"`
			Label        string `json:"label"`
			Reason       string `json:"reason"`
		} `json:"blocked_by"`
	}
	mustDo(t, json.Unmarshal(body, &rec))
	if code != http.StatusOK || rec.ID != m24.ID || len(rec.BlockedBy) != 1 || rec.BlockedBy[0].Prerequisite != m17.ID || rec.BlockedBy[0].Label != "!17" {
		t.Fatalf("record read = %d %s", code, body)
	}
	if code, body = e.call(http.MethodGet, "/api/merge-requests/"+m17.ID, "", true); code != http.StatusOK || strings.Contains(string(body), "blocked_by") {
		t.Fatalf("a request nothing holds = %d %s", code, body)
	}

	// Refusals: a cycle, a self-edge, a request of no project of this one, no list at all.
	code, body = e.call(http.MethodPut, path(m17.ID), `{"prerequisites":["`+m24.ID+`"]}`, true)
	e.expectError("cycle", code, body, http.StatusConflict, "dependency_cycle", "would make a cycle: !17 -> !24 -> !17")
	code, body = e.call(http.MethodPut, path(m24.ID), `{"prerequisites":["`+m24.ID+`"]}`, true)
	e.expectError("self", code, body, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
	code, body = e.call(http.MethodPut, path(m24.ID), `{"prerequisites":["nope"]}`, true)
	e.expectError("unknown", code, body, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
	code, body = e.call(http.MethodPut, path(m24.ID), `{}`, false)
	e.expectError("no list", code, body, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")

	// The Planner's edge of !31's ticket is listed, not editable, and outlives an empty PUT.
	plan := &store.Plan{ProjectID: e.project.ID, Feature: "search", PlanRunID: e.run.ID, Status: store.PlanBuilding}
	mustDo(t, store.CreatePlan(ctx, e.pool, plan))
	mustDo(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: "T1", Wave: 1, Title: "a"}))
	mustDo(t, store.UpsertPlanTicket(ctx, e.pool, &store.PlanTicket{PlanID: plan.ID, TicketID: "T2", Wave: 1, Title: "b", MergeRequestID: &m31.ID,
		Status: store.TicketInReview}))
	mustDo(t, store.ReplacePlannerDependencies(ctx, e.pool, e.project.ID, plan.ID, [][2]string{{"T2", "T1"}}))
	code, body = e.call(http.MethodPut, path(m31.ID), `{"prerequisites":[]}`, true)
	out = read(body)
	if code != http.StatusOK || len(out.Prerequisites) != 1 || out.Prerequisites[0].Label != "T1" || out.Prerequisites[0].Kind != "ticket" ||
		out.Prerequisites[0].Source != "planner" || out.Prerequisites[0].Editable {
		t.Fatalf("planner edge = %d %s", code, body)
	}
	// Removing the operator's edge lets !24 go of !17.
	code, body = e.call(http.MethodPut, path(m24.ID), `{"prerequisites":[]}`, true)
	if out := read(body); code != http.StatusOK || len(out.Prerequisites) != 0 || len(out.BlockedBy) != 0 {
		t.Fatalf("emptied = %d %s", code, body)
	}
}
