package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/store"
)

// saveTestNote saves a project note as an agent (nil: no agent) at now, expiring in ttl
// days (nil: the 90-day default).
func saveTestNote(t *testing.T, p *store.Project, agent *store.Agent, run, key, value string, now time.Time, ttl *int) *store.ProjectFact {
	t.Helper()
	exp := memory.ProjectFactExpiresAt(ttl, now)
	f := &store.ProjectFact{ProjectID: p.ID, Key: key, Value: value, ExpiresAt: &exp}
	if agent != nil {
		f.AgentID = &agent.ID
	}
	if run != "" {
		f.SourceRunID = &run
	}
	mustDo(t, memory.SaveProjectFact(context.Background(), testPool, f, now))
	return f
}

func noteAgent(t *testing.T, name string) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: name + "-" + uniqueSuffix(), Model: "claude-opus-5-5"}
	mustDo(t, store.CreateAgent(context.Background(), testPool, a))
	return a
}

// T5.18.10: the facts API lists a project's live notes newest first with the saving
// agent's name (absent once the agent is gone) and the source run; deletes one with 204;
// answers 404 project not found for a missing or deleted project and 404 memory fact
// not found for a missing note; an expired note is neither listed nor needed to be swept.
func TestProjectFactsAPI(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "notes-"+sfx, acceptedTokenPrefix+sfx)
	other := seedProject(t, testCipher(t), "notes-other-"+sfx, acceptedTokenPrefix+sfx)
	coder, gone := noteAgent(t, "Coder"), noteAgent(t, "Gone")
	now := time.Now().UTC()
	first := saveTestNote(t, p, coder, "run-1", "test command", "go test ./...", now.Add(-2*time.Hour), nil)
	second := saveTestNote(t, p, gone, "run-2", "layout", "cmd/ holds the binaries", now.Add(-time.Hour), nil)
	saveTestNote(t, p, coder, "run-3", "stale", "an expired note", now.Add(-48*time.Hour), noteTTL(1))
	foreign := saveTestNote(t, other, coder, "run-4", "elsewhere", "another project", now, nil)
	_, err := pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, gone.ID)
	mustDo(t, err)

	code, body := do(t, ts, http.MethodGet, "/api/projects/"+p.ID+"/facts", "")
	var list []map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil || len(list) != 2 {
		t.Fatalf("list: %d %s", code, body)
	}
	if list[0]["id"] != second.ID || list[1]["id"] != first.ID {
		t.Fatalf("not newest first, or the expired note is listed: %s", body)
	}
	if list[1]["agent_name"] != coder.Name || list[1]["agent_id"] != coder.ID || list[1]["source_run_id"] != "run-1" ||
		list[1]["key"] != "test command" || list[1]["value"] != "go test ./..." || list[1]["project_id"] != p.ID {
		t.Fatalf("note with its agent = %v", list[1])
	}
	if _, has := list[0]["agent_name"]; has || list[0]["source_run_id"] != "run-2" {
		t.Fatalf("note whose agent is gone = %v", list[0])
	}
	if code, body := do(t, ts, http.MethodGet, "/api/projects/"+other.ID+"/facts", ""); code != http.StatusOK || strings.Contains(string(body), first.ID) {
		t.Fatalf("another project's list: %d %s", code, body)
	}

	// A project without notes lists [].
	empty := seedProject(t, testCipher(t), "notes-empty-"+sfx, acceptedTokenPrefix+sfx)
	if code, body := do(t, ts, http.MethodGet, "/api/projects/"+empty.ID+"/facts", ""); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("empty list: %d %s", code, body)
	}

	notFound := func(method, path, message string) {
		t.Helper()
		code, body := do(t, ts, method, path, "")
		if e := decodeError(t, body); code != http.StatusNotFound || e.Error.Code != "not_found" || e.Error.Message != message {
			t.Fatalf("%s %s: %d %s, want 404 %q", method, path, code, body, message)
		}
	}
	// A note of another project cannot be deleted through this one.
	notFound(http.MethodDelete, "/api/projects/"+p.ID+"/facts/"+foreign.ID, "memory fact not found")
	if code, body := do(t, ts, http.MethodDelete, "/api/projects/"+p.ID+"/facts/"+first.ID, ""); code != http.StatusNoContent || len(body) != 0 {
		t.Fatalf("delete: %d %s", code, body)
	}
	notFound(http.MethodDelete, "/api/projects/"+p.ID+"/facts/"+first.ID, "memory fact not found")
	notFound(http.MethodDelete, "/api/projects/"+p.ID+"/facts/no-such-fact", "memory fact not found")
	notFound(http.MethodGet, "/api/projects/no-such-project/facts", "project not found")
	notFound(http.MethodDelete, "/api/projects/no-such-project/facts/"+second.ID, "project not found")
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID+"/facts", "")
	if strings.Contains(string(body), first.ID) || !strings.Contains(string(body), second.ID) {
		t.Fatalf("after the delete: %s", body)
	}

	// A deleted project: 404 on both, though its notes still exist (rule 2).
	mustDo(t, store.DeleteProject(ctx, pool, p.ID))
	notFound(http.MethodGet, "/api/projects/"+p.ID+"/facts", "project not found")
	notFound(http.MethodDelete, "/api/projects/"+p.ID+"/facts/"+second.ID, "project not found")
	var kept int
	mustDo(t, pool.QueryRow(ctx, `SELECT count(*) FROM project_facts WHERE id=$1`, second.ID).Scan(&kept))
	if kept != 1 {
		t.Fatal("a deleted project lost a note through the API")
	}
}

func noteTTL(v int) *int { return &v }

// projectWave8ConformanceSteps exercises the wave 8 project paths and fields against the
// contract (DESIGN 5.18, 5.19): the two facts paths, and a project with instructions, both
// caps and its spend. The coordinator appends the call to TestOpenAPIConformance.
func projectWave8ConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	sfx := uniqueSuffix()
	p := seedProject(t, testCipher(t), "conf8-"+sfx, acceptedTokenPrefix+sfx)
	coder := noteAgent(t, "Conf")
	note := saveTestNote(t, p, coder, "run-conf", "build", "make", time.Now().UTC(), nil)
	gone := seedProject(t, testCipher(t), "conf8-gone-"+sfx, acceptedTokenPrefix+sfx)
	repo := `"repo_url":"https://gitlab.test/group/conf8-` + sfx + `.git"`
	return []conformanceStep{
		{http.MethodGet, "/api/projects/" + p.ID + "/facts", "", http.StatusOK},
		{http.MethodGet, "/api/projects/no-such-project/facts", "", http.StatusNotFound},
		{http.MethodDelete, "/api/projects/" + p.ID + "/facts/" + note.ID, "", http.StatusNoContent},
		{http.MethodDelete, "/api/projects/" + p.ID + "/facts/" + note.ID, "", http.StatusNotFound},
		{http.MethodDelete, "/api/projects/no-such-project/facts/" + note.ID, "", http.StatusNotFound},
		{http.MethodGet, "/api/projects/" + p.ID + "/facts", "", http.StatusOK},
		{http.MethodDelete, "/api/projects/" + gone.ID, "", http.StatusNoContent},
		{http.MethodGet, "/api/projects/" + gone.ID + "/facts", "", http.StatusNotFound},
		{http.MethodDelete, "/api/projects/" + gone.ID + "/facts/" + note.ID, "", http.StatusNotFound},
		{http.MethodPost, "/api/projects", `{"name":"conf8-new-` + sfx + `",` + repo + `,"token":"` + acceptedTokenPrefix + sfx + `",
			"instructions":"Use tabs.\nNever edit vendor/.","max_cost_usd_per_day":5.5,"max_cost_usd_per_month":100}`, http.StatusCreated},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf8-` + sfx + `",` + repo + `,"provider":"gitlab","instructions":"Run make test.",
			"max_cost_usd_per_day":2,"max_cost_usd_per_month":null}`, http.StatusOK},
		{http.MethodGet, "/api/projects/" + p.ID, "", http.StatusOK},
		{http.MethodGet, "/api/projects", "", http.StatusOK},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf8-` + sfx + `",` + repo + `,"provider":"gitlab","max_cost_usd_per_day":0}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf8-` + sfx + `",` + repo + `,"provider":"gitlab","max_cost_usd_per_month":1000001}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/projects/" + p.ID, `{"name":"conf8-` + sfx + `",` + repo + `,"provider":"gitlab","instructions":"bell\u0007"}`, http.StatusUnprocessableEntity},
	}
}
