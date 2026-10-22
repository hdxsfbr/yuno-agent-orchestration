package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/actor"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Chat acceptance (DESIGN W4 rule 7): the Chief's platform_call through the MCP server, the
// tools, the dispatcher, the api's whole chain (actor, audit, platform gate, idempotency,
// If-Match) and the real handlers, against the database. Every change is read back.

// pcallOK runs platform_call as the Chief and wants the given status; it returns the body
// (nil when it is not an object) and the etag.
func (f *chatFixture) pcallOK(t *testing.T, want int, args map[string]any) (map[string]any, string) {
	t.Helper()
	out, text, isErr := f.callJSON(t, "platform_call", args)
	if isErr || out["status"] != float64(want) {
		t.Fatalf("%v = %s, want status %d", args["operation_id"], text, want)
	}
	body, _ := out["body"].(map[string]any)
	etag, _ := out["etag"].(string)
	return body, etag
}

// read is a GET of one resource through platform_call: its body and etag.
func (f *chatFixture) read(t *testing.T, op, id string) (map[string]any, string) {
	t.Helper()
	return f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": op, "path_params": map[string]any{"id": id}})
}

// readGone wants a GET of one resource to answer 404.
func (f *chatFixture) readGone(t *testing.T, op, id string) {
	t.Helper()
	f.pcallOK(t, http.StatusNotFound, map[string]any{"operation_id": op, "path_params": map[string]any{"id": id}})
}

// atInstant reports whether a JSON time names the instant want (RFC 3339).
func atInstant(v any, want string) bool {
	s, _ := v.(string)
	got, err := time.Parse(time.RFC3339Nano, s)
	w, _ := time.Parse(time.RFC3339, want)
	return err == nil && got.Equal(w)
}

// approvedCall is a gated call end to end (rule A6): the first call waits for a human; the
// person sends the code and the Chief decides with it; the repeat runs once; a second
// repeat replays the stored answer. It returns the answer of the run.
func (f *chatFixture) approvedCall(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	first, text, isErr := f.callJSON(t, "platform_call", args)
	if isErr || first["status"] != tools.GatePending {
		t.Fatalf("%v, first call = %s", args["operation_id"], text)
	}
	f.say(t, "yes, go ahead: "+first["code"].(string), "", time.Now().UTC())
	if out, text, isErr := f.callJSON(t, "decide_approval", map[string]any{"approval_id": first["approval_id"], "code": first["code"],
		"decision": "approve"}); isErr || out["status"] != store.ApprovalApproved {
		t.Fatalf("decide_approval = %s", text)
	}
	done, text, isErr := f.callJSON(t, "platform_call", args)
	if isErr || done["status"] == tools.GatePending || done["idempotent_replayed"] != nil {
		t.Fatalf("%v, approved repeat = %s", args["operation_id"], text)
	}
	again, text, isErr := f.callJSON(t, "platform_call", args)
	if isErr || again["idempotent_replayed"] != true || again["status"] != done["status"] {
		t.Fatalf("%v, second repeat = %s", args["operation_id"], text)
	}
	return done
}

// outcomesOf counts the audit outcomes of a path's rows with one method, and checks that
// each names the Chief and the person it acts for.
func (f *chatFixture) outcomesOf(t *testing.T, method, path string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, e := range auditRows(t, path) {
		if e.Method != method {
			continue
		}
		out[e.Outcome]++
		if e.ActorKind != string(actor.Agent) || e.ActorID != f.agent.ID || e.Via != actor.ViaChief || e.OnBehalfOf != "telegram:"+f.conv.ExternalChatID {
			t.Errorf("%s %s row actor = %s %q %s %q", method, path, e.ActorKind, e.ActorID, e.Via, e.OnBehalfOf)
		}
	}
	return out
}

// An agent created, read, changed and deleted from chat. A grant of tools is not made
// without the person: it waits, the person rejects it, and its repeat is refused and audited
// denied for the Chief on the person's behalf. The delete goes through the approval.
func TestChiefAgentCRUD(t *testing.T) {
	f := newChatFixture(t, nil)
	sfx := uniqueSuffix()
	name := "Crud-" + sfx
	created, _ := f.pcallOK(t, http.StatusCreated, map[string]any{"operation_id": "createAgent", "idempotency_key": "create-" + sfx,
		"body": map[string]any{"name": name, "model": "claude-opus-5-5", "role": "writes reports", "system_prompt": "be brief"}})
	id, _ := created["id"].(string)
	path := "/api/agents/" + id
	got, etag := f.read(t, "getAgent", id)
	if got["name"] != name || got["role"] != "writes reports" || got["system_prompt"] != "be brief" {
		t.Fatalf("read back = %v", got)
	}

	// A new prompt, sent with the etag of the read.
	edit := maps.Clone(got)
	edit["system_prompt"] = "be thorough"
	f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": "updateAgent", "path_params": map[string]any{"id": id}, "body": edit,
		"idempotency_key": "update-" + sfx, "if_match": etag})
	if got, etag = f.read(t, "getAgent", id); got["system_prompt"] != "be thorough" {
		t.Fatalf("read back after the update = %v", got)
	}

	// A privileged change: refused without the person's approval.
	grant := maps.Clone(got)
	grant["tool_allowlist"] = []string{"exec"}
	args := map[string]any{"operation_id": "updateAgent", "path_params": map[string]any{"id": id}, "body": grant,
		"idempotency_key": "grant-" + sfx, "if_match": etag}
	pending, text, isErr := f.callJSON(t, "platform_call", args)
	if isErr || pending["status"] != tools.GatePending {
		t.Fatalf("grant = %s", text)
	}
	if code, body := do(t, f.ts, http.MethodPost, "/api/approvals/"+pending["approval_id"].(string)+"/reject", `{"reason":"not now"}`); code != http.StatusOK {
		t.Fatalf("reject: %d %s", code, body)
	}
	if _, text, isErr := f.callJSON(t, "platform_call", args); !isErr || text != "rejected by api" {
		t.Fatalf("grant after the rejection = %v %s", isErr, text)
	}
	if got, etag = f.read(t, "getAgent", id); got["tool_allowlist"] != nil && len(got["tool_allowlist"].([]any)) != 0 {
		t.Fatalf("the tool was granted: %v", got["tool_allowlist"])
	}
	rows := auditRows(t, path)
	last := rows[len(rows)-1]
	if last.Method != http.MethodPut || last.Outcome != store.AuditDenied || last.ErrorCode != "approval_rejected" ||
		last.ApprovalID != pending["approval_id"] || jsonField(t, last.Evidence, "risk") != "privileged" {
		t.Fatalf("refused grant row = %+v evidence %s", last, last.Evidence)
	}
	if o := f.outcomesOf(t, http.MethodPut, path); o[store.AuditOK] != 1 || o[store.AuditPendingApproval] != 1 || o[store.AuditDenied] != 1 {
		t.Fatalf("update outcomes = %v", o)
	}

	// The delete waits for the person, runs once and replays.
	done := f.approvedCall(t, map[string]any{"operation_id": "deleteAgent", "path_params": map[string]any{"id": id},
		"idempotency_key": "delete-" + sfx, "if_match": etag})
	if done["status"] != float64(http.StatusNoContent) {
		t.Fatalf("delete = %v", done)
	}
	f.readGone(t, "getAgent", id)
	f.fe.mu.Lock()
	removed := slices.Clone(f.fe.removed)
	f.fe.mu.Unlock()
	if n := len(slices.DeleteFunc(removed, func(r string) bool { return r != id })); n != 1 {
		t.Fatalf("removed from OpenClaw %d times", n)
	}
	if o := f.outcomesOf(t, http.MethodDelete, path); o[store.AuditOK] != 1 || o[store.AuditReplayed] != 1 || o[store.AuditPendingApproval] != 1 {
		t.Fatalf("delete outcomes = %v", o)
	}
}

// A workflow validated, created, read, changed, restored to an earlier version and deleted
// from chat; runs in flight keep the version and snapshot they started with.
func TestChiefWorkflowCRUD(t *testing.T) {
	f := newChatFixture(t, nil)
	ctx := context.Background()
	sfx := uniqueSuffix()
	worker := testAgent(t, "chief-worker-"+sfx)
	key := func(what string) string { return what + "-" + sfx }

	// Validation answers what is wrong, and changes nothing.
	invalid := strings.Replace(validDefinition(worker), worker, "no-such-agent-"+sfx, 1)
	out, text, isErr := f.callJSON(t, "platform_call", map[string]any{"operation_id": "validateWorkflow", "idempotency_key": key("invalid"),
		"body": map[string]any{"definition": json.RawMessage(invalid)}})
	if isErr || out["status"] != float64(http.StatusUnprocessableEntity) || !strings.Contains(text, "nodes[1].agent_id") {
		t.Fatalf("validate an invalid definition = %s", text)
	}
	if body, _ := f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": "validateWorkflow", "idempotency_key": key("valid"),
		"body": map[string]any{"definition": json.RawMessage(validDefinition(worker))}}); body["valid"] != true {
		t.Fatalf("validate = %v", body)
	}

	created, _ := f.pcallOK(t, http.StatusCreated, map[string]any{"operation_id": "createWorkflow", "idempotency_key": key("create"),
		"body": map[string]any{"name": "Chief flow " + sfx, "description": "first", "definition": json.RawMessage(validDefinition(worker))}})
	id, _ := created["id"].(string)
	got, etag := f.read(t, "getWorkflow", id)
	if got["name"] != "Chief flow "+sfx || got["version"] != float64(1) {
		t.Fatalf("read back = %v", got)
	}
	inFlight := func(version int) *store.Run {
		t.Helper()
		wf, err := store.GetWorkflow(ctx, testPool, id)
		mustDo(t, err)
		if wf.Version != version {
			t.Fatalf("workflow at version %d, want %d", wf.Version, version)
		}
		run := &store.Run{WorkflowID: id, WorkflowVersion: version, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual, Status: store.RunRunning}
		mustDo(t, store.CreateRun(ctx, testPool, run))
		return run
	}
	first := inFlight(1)

	// An update is version 2.
	edited := strings.Replace(validDefinition(worker), `"max_steps":10`, `"max_steps":12`, 1)
	f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": "updateWorkflow", "path_params": map[string]any{"id": id}, "idempotency_key": key("update"),
		"if_match": etag, "body": map[string]any{"name": "Chief flow 2 " + sfx, "definition": json.RawMessage(edited)}})
	if got, _ = f.read(t, "getWorkflow", id); got["version"] != float64(2) || got["name"] != "Chief flow 2 "+sfx {
		t.Fatalf("read back after the update = %v", got)
	}
	second := inFlight(2)

	// The versions, then a restore of version 1 as version 3.
	out, text, isErr = f.callJSON(t, "platform_call", map[string]any{"operation_id": "listWorkflowVersions", "path_params": map[string]any{"id": id}})
	if versions, _ := out["body"].([]any); isErr || len(versions) != 2 || versions[0].(map[string]any)["version"] != float64(2) {
		t.Fatalf("versions = %s", text)
	}
	f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": "restoreWorkflowVersion", "path_params": map[string]any{"id": id, "version": 1},
		"idempotency_key": key("restore"), "body": map[string]any{"expected_version": 2}})
	got, etag = f.read(t, "getWorkflow", id)
	v1, _ := json.Marshal(got["definition"])
	if got["version"] != float64(3) || got["name"] != "Chief flow "+sfx || !sameDefinition(t, v1, json.RawMessage(validDefinition(worker))) {
		t.Fatalf("read back after the restore = %v", got)
	}
	for _, run := range []*store.Run{first, second} {
		now, err := store.GetRun(ctx, testPool, run.ID)
		mustDo(t, err)
		if now.WorkflowVersion != run.WorkflowVersion || !sameDefinition(t, now.WorkflowSnapshot, run.WorkflowSnapshot) {
			t.Fatalf("run on version %d now on %d with %s", run.WorkflowVersion, now.WorkflowVersion, now.WorkflowSnapshot)
		}
	}

	// The delete waits for the person.
	done := f.approvedCall(t, map[string]any{"operation_id": "deleteWorkflow", "path_params": map[string]any{"id": id},
		"idempotency_key": key("delete"), "if_match": etag})
	if done["status"] != float64(http.StatusNoContent) {
		t.Fatalf("delete = %v", done)
	}
	f.readGone(t, "getWorkflow", id)
	if o := f.outcomesOf(t, http.MethodDelete, "/api/workflows/"+id); o[store.AuditOK] != 1 || o[store.AuditReplayed] != 1 {
		t.Fatalf("delete outcomes = %v", o)
	}
}

// A schedule created, read, moved to another zone, paused and resumed (DESIGN W4 rule 6:
// enabled, the zone and next_run_at through platform_call) and deleted from chat.
func TestChiefScheduleCRUD(t *testing.T) {
	f := newChatFixture(t, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	sfx := uniqueSuffix()
	wf := scheduleWorkflow(t)
	created, _ := f.pcallOK(t, http.StatusCreated, map[string]any{"operation_id": "createSchedule", "idempotency_key": "create-" + sfx,
		"body": scheduleBody(wf)})
	id, _ := created["id"].(string)
	got, etag := f.read(t, "getSchedule", id)
	if got["tz"] != "UTC" || got["enabled"] != true || !atInstant(got["next_run_at"], "2099-10-15T09:00:00Z") {
		t.Fatalf("read back = %v", got)
	}
	update := func(what string, body map[string]any) {
		t.Helper()
		f.pcallOK(t, http.StatusOK, map[string]any{"operation_id": "updateSchedule", "path_params": map[string]any{"id": id}, "body": body,
			"idempotency_key": what + "-" + sfx, "if_match": etag})
		got, etag = f.read(t, "getSchedule", id)
	}

	// 09:00 in New York in October is 13:00 UTC.
	update("zone", map[string]any{"tz": "America/New_York"})
	if got["tz"] != "America/New_York" || !atInstant(got["next_run_at"], "2099-10-15T13:00:00Z") {
		t.Fatalf("read back after the zone = %v", got)
	}
	update("pause", map[string]any{"enabled": false})
	if got["enabled"] != false || got["next_run_at"] != nil {
		t.Fatalf("read back after the pause = %v", got)
	}
	update("resume", map[string]any{"enabled": true})
	if got["enabled"] != true || !atInstant(got["next_run_at"], "2099-10-15T13:00:00Z") {
		t.Fatalf("read back after the resume = %v", got)
	}

	done := f.approvedCall(t, map[string]any{"operation_id": "deleteSchedule", "path_params": map[string]any{"id": id},
		"idempotency_key": "delete-" + sfx, "if_match": etag})
	if done["status"] != float64(http.StatusNoContent) {
		t.Fatalf("delete = %v", done)
	}
	f.readGone(t, "getSchedule", id)
}

// DESIGN W4 rule 7 and 5.31 rule 1: a request without the bearer is refused 401 and audited
// as anonymous and denied, its body unread and nothing read on its behalf, whatever it asks
// for; the rows are in the audit read.
func TestUnauthorizedAudited(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.APIToken = "unauthorized-audited-token-not-a-secret"
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, &fakeEngine{pool: pool})
	sfx := uniqueSuffix()
	since := time.Now().UTC().Add(-time.Second)
	agentID := testAgent(t, "unauthorized-"+sfx)
	send := func(method, path, body string, hdr map[string]string) int {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, ts.URL+path, rd)
		mustDo(t, err)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		mustDo(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	type refusal struct {
		method, path, body string
		hdr                map[string]string
		via                string
	}
	cases := []refusal{
		{http.MethodPut, "/api/agents/" + agentID, `{"name":"taken over","model":"claude-opus-5-5"}`, map[string]string{"Authorization": "Bearer wrong"}, actor.ViaAPI},
		{http.MethodDelete, "/api/agents/" + agentID, "", map[string]string{"X-Yuno-Client": "web"}, actor.ViaUI},
		{http.MethodPost, "/api/runs/r-" + sfx + "/retry", `{"limits":{"max_steps":3}}`, nil, actor.ViaAPI},
		{http.MethodPost, "/api/audit/x-" + sfx + "/revert", "", nil, actor.ViaAPI},
		{http.MethodGet, "/api/schedules/s-" + sfx, "", nil, actor.ViaAPI},
	}
	for _, c := range cases {
		if code := send(c.method, c.path, c.body, c.hdr); code != http.StatusUnauthorized {
			t.Fatalf("%s %s: %d", c.method, c.path, code)
		}
		rows := slices.DeleteFunc(auditRows(t, c.path), func(e store.AuditEvent) bool { return e.Method != c.method })
		if len(rows) != 1 {
			t.Fatalf("%s %s: %d rows", c.method, c.path, len(rows))
		}
		e := rows[0]
		if e.Method != c.method || e.ActorKind != string(actor.Anonymous) || e.Via != c.via || e.Outcome != store.AuditDenied ||
			e.Status == nil || *e.Status != http.StatusUnauthorized || e.ErrorCode != "unauthorized" || e.Request != nil || e.Before != nil {
			t.Errorf("%s %s row = %+v", c.method, c.path, e)
		}
	}
	if a, err := store.GetAgent(ctx, pool, agentID); err != nil || a.Name != "unauthorized-"+sfx {
		t.Fatalf("the agent changed: %+v %v", a, err)
	}
	// The operator finds them in the audit read.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/audit?actor=anonymous&limit=200&since="+since.Format(time.RFC3339Nano), nil)
	mustDo(t, err)
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	resp, err := ts.Client().Do(req)
	mustDo(t, err)
	defer resp.Body.Close()
	var page auditPage
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
		t.Fatalf("audit read: %d", resp.StatusCode)
	}
	found := 0
	for _, e := range page.Events {
		if slices.ContainsFunc(cases, func(c refusal) bool { return c.path == e.Path && c.method == e.Method }) {
			found++
		}
	}
	if found != len(cases) {
		t.Fatalf("the audit read lists %d of the %d refusals", found, len(cases))
	}
}
