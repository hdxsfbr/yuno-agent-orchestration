package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"yuno/internal/store"
)

// versionsOf reads GET /api/workflows/{id}/versions and returns the version numbers,
// checking that the list carries no definitions and names who wrote each version.
func versionsOf(t *testing.T, ts *httptest.Server, id string) []int {
	t.Helper()
	code, body := do(t, ts, http.MethodGet, "/api/workflows/"+id+"/versions", "")
	var list []map[string]json.RawMessage
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("versions of %s: %d %s", id, code, body)
	}
	out := make([]int, len(list))
	for i, v := range list {
		if _, ok := v["definition"]; ok {
			t.Fatalf("the version list carries definitions: %s", body)
		}
		var by struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(v["created_by"], &by) != nil || by.Kind == "" {
			t.Fatalf("a version without its author: %s", body)
		}
		mustDo(t, json.Unmarshal(v["version"], &out[i]))
	}
	return out
}

// versionOf reads GET /api/workflows/{id}/versions/{v}.
func versionOf(t *testing.T, ts *httptest.Server, id string, v int) store.WorkflowVersion {
	t.Helper()
	code, body := do(t, ts, http.MethodGet, "/api/workflows/"+id+"/versions/"+strconv.Itoa(v), "")
	var out store.WorkflowVersion
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Version != v || out.WorkflowID != id {
		t.Fatalf("version %d of %s: %d %s", v, id, code, body)
	}
	return out
}

// sameDefinition reports whether two definitions are the same document.
func sameDefinition(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	mustDo(t, json.Unmarshal(a, &x))
	mustDo(t, json.Unmarshal(b, &y))
	return reflect.DeepEqual(x, y)
}

func decodeWorkflow(t *testing.T, b []byte) store.Workflow {
	t.Helper()
	var wf store.Workflow
	if err := json.Unmarshal(b, &wf); err != nil || wf.ID == "" {
		t.Fatalf("not a workflow: %s", b)
	}
	return wf
}

// The version reads, the restores and the archived-agent list answer as the contract says.
func TestOpenAPIConformanceW4Versions(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	router := contract(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	agentID := testAgent(t, "conformance-versions-"+uniqueSuffix())
	wf := &store.Workflow{Name: "conformance versions", Definition: json.RawMessage(validDefinition(agentID))}
	mustDo(t, store.CreateWorkflow(ctx, pool, wf))
	archived := testAgent(t, "conformance-archived-"+uniqueSuffix())
	mustDo(t, store.DeleteAgent(ctx, pool, archived))
	base := "/api/workflows/" + wf.ID
	for _, st := range []conformanceStep{
		{http.MethodPut, base, `{"name":"conformance versions 2","definition":` + validDefinition(agentID) + `}`, http.StatusOK},
		{http.MethodGet, base + "/versions", "", http.StatusOK},
		{http.MethodGet, base + "/versions/1", "", http.StatusOK},
		{http.MethodGet, base + "/versions/9", "", http.StatusNotFound},
		{http.MethodGet, "/api/workflows/no-such-workflow/versions", "", http.StatusNotFound},
		{http.MethodPost, base + "/versions/1/restore", `{"expected_version":2}`, http.StatusOK},
		{http.MethodPost, base + "/versions/1/restore", `{"expected_version":2}`, http.StatusPreconditionFailed},
		{http.MethodPost, base + "/versions/1/restore", `{}`, http.StatusUnprocessableEntity},
		{http.MethodPost, base + "/versions/9/restore", `{"expected_version":3}`, http.StatusNotFound},
		{http.MethodGet, "/api/agents?include_archived=true", "", http.StatusOK},
		{http.MethodGet, "/api/agents?include_archived=false", "", http.StatusOK},
		{http.MethodPost, "/api/agents/" + archived + "/restore", "", http.StatusOK},
		{http.MethodPost, "/api/agents/" + archived + "/restore", "", http.StatusConflict},
		{http.MethodPost, "/api/agents/no-such-agent/restore", "", http.StatusNotFound},
	} {
		if code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300); code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
	}
}

// DESIGN W4 rules 1 and 2: create, update, the template loader, instantiation and the
// seed's reset each write the version they make; the list is newest first without
// definitions and one version reads back with its definition.
func TestWorkflowVersionsWritten(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	sfx := uniqueSuffix()
	agentID := testAgent(t, "versions-"+sfx)
	key := "t_versions_" + sfx
	writeTemplate(t, dir, key, "Versioned template", "versions-"+sfx)
	s, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, TemplatesDir: dir}, &fakeEngine{pool: pool})
	mustDo(t, s.LoadTemplates(ctx))

	// The API's create and update.
	code, body := do(t, ts, http.MethodPost, "/api/workflows", `{"name":"Versioned","description":"first","definition":`+validDefinition(agentID)+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	created := decodeWorkflow(t, body)
	if got := versionsOf(t, ts, created.ID); !slices.Equal(got, []int{1}) {
		t.Fatalf("after create: %v", got)
	}
	edited := strings.Replace(validDefinition(agentID), `"max_steps":10`, `"max_steps":12`, 1)
	if code, body = do(t, ts, http.MethodPut, "/api/workflows/"+created.ID, `{"name":"Versioned 2","definition":`+edited+`}`); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	if got := versionsOf(t, ts, created.ID); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("after update: %v", got)
	}
	v1, v2 := versionOf(t, ts, created.ID, 1), versionOf(t, ts, created.ID, 2)
	if v1.Name != "Versioned" || v1.Description != "first" || !sameDefinition(t, v1.Definition, created.Definition) {
		t.Fatalf("version 1 = %+v %s", v1.WorkflowVersionInfo, v1.Definition)
	}
	if v2.Name != "Versioned 2" || v2.Description != "" || !sameDefinition(t, v2.Definition, json.RawMessage(edited)) {
		t.Fatalf("version 2 = %+v %s", v2.WorkflowVersionInfo, v2.Definition)
	}

	// The template loader: the template row on load and again when its file changes.
	tpl, err := store.GetWorkflowByTemplateKey(ctx, pool, key)
	mustDo(t, err)
	if got := versionsOf(t, ts, tpl.ID); !slices.Equal(got, []int{1}) {
		t.Fatalf("template after load: %v", got)
	}
	path := filepath.Join(dir, key+".json")
	raw, err := os.ReadFile(path)
	mustDo(t, err)
	mustDo(t, os.WriteFile(path, []byte(strings.Replace(string(raw), "test template", "changed template", 1)), 0o644))
	mustDo(t, s.LoadTemplates(ctx))
	if got := versionsOf(t, ts, tpl.ID); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("template after a change: %v", got)
	}

	// Instantiation, then the seed's reset of the copy to its template.
	code, body = do(t, ts, http.MethodPost, "/api/workflow-templates/"+key+"/instantiate", `{"name":"Copy `+sfx+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("instantiate: %d %s", code, body)
	}
	copied := decodeWorkflow(t, body)
	if got := versionsOf(t, ts, copied.ID); !slices.Equal(got, []int{1}) {
		t.Fatalf("copy: %v", got)
	}
	mustDo(t, NewShared(Deps{Pool: pool}).ResetFromTemplate(ctx, copied.ID))
	if got := versionsOf(t, ts, copied.ID); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("copy after a reset: %v", got)
	}

	// What is not there is 404.
	for _, p := range []string{
		"/api/workflows/no-such-workflow/versions",
		"/api/workflows/no-such-workflow/versions/1",
		"/api/workflows/" + created.ID + "/versions/3",
		"/api/workflows/" + created.ID + "/versions/0",
		"/api/workflows/" + created.ID + "/versions/latest",
	} {
		if code, body := do(t, ts, http.MethodGet, p, ""); code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
			t.Fatalf("GET %s: %d %s", p, code, body)
		}
	}
	if code, _ := do(t, ts, http.MethodDelete, "/api/workflows/"+created.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := do(t, ts, http.MethodGet, "/api/workflows/"+created.ID+"/versions", ""); code != http.StatusNotFound {
		t.Fatalf("versions of a deleted workflow: %d", code)
	}
}

// DESIGN W4 rule 3: a restore writes the version's name, description and definition as
// version current + 1; it answers 412 when expected_version is not current and 422 when
// the definition would not be accepted now; a run in flight keeps its version and
// snapshot.
func TestWorkflowRestore(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	first, second := testAgent(t, "restore-first-"+sfx), testAgent(t, "restore-second-"+sfx)
	restore := func(id string, v int, body string) (int, []byte) {
		return do(t, ts, http.MethodPost, "/api/workflows/"+id+"/versions/"+strconv.Itoa(v)+"/restore", body)
	}

	code, body := do(t, ts, http.MethodPost, "/api/workflows", `{"name":"Restore me","description":"one","definition":`+validDefinition(first)+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	wf := decodeWorkflow(t, body)
	if code, body = do(t, ts, http.MethodPut, "/api/workflows/"+wf.ID, `{"name":"Restore me 2","definition":`+validDefinition(second)+`}`); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	v2 := decodeWorkflow(t, body)

	// A run started on version 2 is in flight.
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: v2.Version, WorkflowSnapshot: v2.Definition, Trigger: store.TriggerManual, Status: store.RunRunning}
	mustDo(t, store.CreateRun(ctx, pool, run))

	// Valid: version 1 comes back as version 3.
	code, body = restore(wf.ID, 1, `{"expected_version":2}`)
	if code != http.StatusOK {
		t.Fatalf("restore: %d %s", code, body)
	}
	got := decodeWorkflow(t, body)
	if got.Version != 3 || got.Name != "Restore me" || got.Description != "one" || !sameDefinition(t, got.Definition, wf.Definition) {
		t.Fatalf("restored = %+v %s", got, got.Definition)
	}
	if v := versionsOf(t, ts, wf.ID); !slices.Equal(v, []int{3, 2, 1}) {
		t.Fatalf("versions after the restore: %v", v)
	}
	if v3 := versionOf(t, ts, wf.ID, 3); v3.Name != "Restore me" || !sameDefinition(t, v3.Definition, wf.Definition) {
		t.Fatalf("version 3 = %+v", v3.WorkflowVersionInfo)
	}
	// Read back.
	if code, body = do(t, ts, http.MethodGet, "/api/workflows/"+wf.ID, ""); code != http.StatusOK || decodeWorkflow(t, body).Version != 3 {
		t.Fatalf("read back: %d %s", code, body)
	}

	// The run in flight is pinned to version 2.
	pinned, err := store.GetRun(ctx, pool, run.ID)
	mustDo(t, err)
	if pinned.WorkflowVersion != 2 || !sameDefinition(t, pinned.WorkflowSnapshot, v2.Definition) {
		t.Fatalf("run in flight: version %d snapshot %s", pinned.WorkflowVersion, pinned.WorkflowSnapshot)
	}

	// Version conflict: the caller read version 2, the workflow is at 3.
	code, body = restore(wf.ID, 2, `{"expected_version":2}`)
	var conflict struct {
		CurrentVersion int `json:"current_version"`
	}
	if e := decodeError(t, body); code != http.StatusPreconditionFailed || e.Error.Code != "version_conflict" ||
		json.Unmarshal(e.Error.Details, &conflict) != nil || conflict.CurrentVersion != 3 {
		t.Fatalf("stale restore: %d %s", code, body)
	}

	// Invalid now: version 1 names an agent that has been deleted since.
	mustDo(t, store.DeleteAgent(ctx, pool, first))
	code, body = restore(wf.ID, 1, `{"expected_version":3}`)
	if code != http.StatusUnprocessableEntity || !slices.Contains(detailPaths(t, body), "nodes[1].agent_id") {
		t.Fatalf("restore of a version invalid now: %d %s", code, body)
	}
	if v := versionsOf(t, ts, wf.ID); !slices.Equal(v, []int{3, 2, 1}) {
		t.Fatalf("a refused restore wrote a version: %v", v)
	}
	// Version 2 is still valid, and restoring it moves on to version 4.
	if code, body = restore(wf.ID, 2, `{"expected_version":3}`); code != http.StatusOK || decodeWorkflow(t, body).Version != 4 ||
		decodeWorkflow(t, body).Name != "Restore me 2" {
		t.Fatalf("restore of version 2: %d %s", code, body)
	}

	// The request must name the version it read; unknown versions and managed rows are refused.
	if code, body = restore(wf.ID, 2, `{}`); code != http.StatusUnprocessableEntity || !slices.Contains(detailPaths(t, body), "expected_version") {
		t.Fatalf("no expected_version: %d %s", code, body)
	}
	if code, body = restore(wf.ID, 2, `{"expected_version":4,"force":true}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %d %s", code, body)
	}
	if code, _ = restore(wf.ID, 9, `{"expected_version":4}`); code != http.StatusNotFound {
		t.Fatalf("unknown version: %d", code)
	}
	if code, _ = restore("no-such-workflow", 1, `{"expected_version":1}`); code != http.StatusNotFound {
		t.Fatalf("unknown workflow: %d", code)
	}
	qk := "quickrun:" + sfx
	managed := &store.Workflow{Name: "Quick run", Definition: json.RawMessage(validDefinition(second)), TemplateKey: &qk}
	mustDo(t, store.CreateWorkflow(ctx, pool, managed))
	if code, body = restore(managed.ID, 1, `{"expected_version":1}`); code != http.StatusConflict || decodeError(t, body).Error.Code != "read_only" {
		t.Fatalf("restore of a managed workflow: %d %s", code, body)
	}
}
