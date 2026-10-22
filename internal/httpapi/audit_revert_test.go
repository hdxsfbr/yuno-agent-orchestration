package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// lastAuditRow is the newest audit row of a path.
func lastAuditRow(t *testing.T, path string) store.AuditEvent {
	t.Helper()
	rows := auditRows(t, path)
	if len(rows) == 0 {
		t.Fatalf("no audit row for %s", path)
	}
	return rows[len(rows)-1]
}

// putAudited sends a PUT that must succeed and returns its audit row.
func putAudited(t *testing.T, ts *httptest.Server, path, body string) store.AuditEvent {
	t.Helper()
	if code, b := do(t, ts, http.MethodPut, path, body); code != http.StatusOK {
		t.Fatalf("PUT %s: %d %s", path, code, b)
	}
	return lastAuditRow(t, path)
}

// decodeObject reads a JSON object.
func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	return m
}

// jsonFields are the JSON names of a struct's fields.
func jsonFields(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// DESIGN W4 rule 5: each mapper names its route's PUT operation, and only fields that the
// route's input takes and its read shows, never a token.
func TestRevertMappers(t *testing.T) {
	c, err := tools.Contract()
	mustDo(t, err)
	inputs := map[string]any{"/api/agents/{id}": agentInput{}, "/api/schedules/{id}": scheduleInput{}, "/api/skills/{id}": skillInput{},
		"/api/projects/{id}": projectInput{}}
	reads := map[string]any{"/api/agents/{id}": store.Agent{}, "/api/schedules/{id}": store.Schedule{}, "/api/skills/{id}": store.Skill{},
		"/api/projects/{id}": store.Project{}}
	if len(revertMappers) != len(inputs) {
		t.Fatalf("%d mappers", len(revertMappers))
	}
	for route, m := range revertMappers {
		if op, ok := c.Operation(m.op); !ok || op.Method != http.MethodPut || op.Path != route {
			t.Errorf("%s: operation %s", route, m.op)
		}
		in, read := jsonFields(inputs[route]), jsonFields(reads[route])
		for _, f := range append(slices.Clone(m.fields), m.clear...) {
			if !in[f] || !read[f] || obs.IsSecretKey(f) {
				t.Errorf("%s: field %s (input %v, read %v)", route, f, in[f], read[f])
			}
		}
	}
}

// DESIGN W4 rule 5: a PUT recorded in the audit log is undone by writing its before back
// through the resource's PUT, for an agent, a schedule, a skill and a project (its token
// left alone); a resource changed since answers 412 until the later change is reverted; a
// row with nothing a revert writes, or without its before, answers 422; anything but a PUT
// that succeeded on those resources is refused.
func TestAuditRevert(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	pinScheduleClock(t, "2099-10-15T08:00:00Z")
	sfx := uniqueSuffix()
	revert := func(id string) (int, []byte) {
		t.Helper()
		return do(t, ts, http.MethodPost, "/api/audit/"+id+"/revert", "")
	}
	refused := func(id string, status int, code, message string) {
		t.Helper()
		got, body := revert(id)
		if e := decodeError(t, body); got != status || e.Error.Code != code || (message != "" && e.Error.Message != message) {
			t.Fatalf("revert %s: %d %s, want %d %s", id, got, body, status, code)
		}
	}
	readField := func(path, field string) any {
		t.Helper()
		code, body := do(t, ts, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		return decodeObject(t, body)[field]
	}

	// An agent's prompt, changed three times; reverting the second needs the third
	// reverted first.
	agentID := testAgent(t, "revert-"+sfx)
	agentPath := "/api/agents/" + agentID
	agentBody := func(prompt string) string {
		return `{"name":"revert-` + sfx + `","model":"claude-opus-5-5","system_prompt":"` + prompt + `","tool_allowlist":["read"]}`
	}
	putAudited(t, ts, agentPath, agentBody("one"))
	second := putAudited(t, ts, agentPath, agentBody("two"))
	third := putAudited(t, ts, agentPath, agentBody("three"))
	refused(second.ID, http.StatusPreconditionFailed, "version_conflict", revertChangedSince)
	if got := readField(agentPath, "system_prompt"); got != "three" {
		t.Fatalf("a refused revert changed the prompt to %v", got)
	}
	code, body := revert(third.ID)
	if code != http.StatusOK || decodeObject(t, body)["system_prompt"] != "two" || readField(agentPath, "system_prompt") != "two" {
		t.Fatalf("revert of the third: %d %s", code, body)
	}
	// The revert's own row reads the agent before and after it.
	row := lastAuditRow(t, "/api/audit/"+third.ID+"/revert")
	if row.Route != revertRoute || row.Target != agentPath || row.Outcome != store.AuditOK || row.ActorKind != string(actor.Operator) ||
		jsonField(t, row.Before, "system_prompt") != "three" || jsonField(t, row.After, "system_prompt") != "two" {
		t.Fatalf("revert row = %+v before %s after %s", row, row.Before, row.After)
	}
	if code, body = revert(second.ID); code != http.StatusOK || readField(agentPath, "system_prompt") != "one" {
		t.Fatalf("revert of the second, after the third: %d %s", code, body)
	}
	refused(third.ID, http.StatusPreconditionFailed, "version_conflict", revertChangedSince)

	// A schedule: paused and moved to another zone, then reverted; next_run_at follows.
	wf := scheduleWorkflow(t)
	code, body = do(t, ts, http.MethodPost, "/api/schedules", `{"workflow_id":"`+wf.ID+`","kind":"cron","cron_expr":"0 9 * * *","tz":"UTC","input":{"task":"report"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create schedule: %d %s", code, body)
	}
	sc := decodeSchedule(t, body)
	schedPath := "/api/schedules/" + sc.ID
	paused := putAudited(t, ts, schedPath, `{"enabled":false,"tz":"Europe/Berlin"}`)
	if code, body = revert(paused.ID); code != http.StatusOK {
		t.Fatalf("revert of the schedule: %d %s", code, body)
	}
	if got := decodeSchedule(t, body); !got.Enabled || got.TZ != "UTC" || got.NextRunAt == nil || !got.NextRunAt.Equal(time.Date(2099, 10, 15, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("reverted schedule = %+v", got)
	}

	// A skill's description; a skill deleted since answers 412.
	skillID := func(name string) string {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/skills", `{"name":"`+name+`","description":"first","body":"# Skill"}`)
		if code != http.StatusCreated {
			t.Fatalf("create skill: %d %s", code, body)
		}
		return decodeObject(t, body)["id"].(string)
	}
	sk := skillID("revert-" + sfx)
	described := putAudited(t, ts, "/api/skills/"+sk, `{"name":"revert-`+sfx+`","description":"second","body":"# Skill"}`)
	if code, body = revert(described.ID); code != http.StatusOK || readField("/api/skills/"+sk, "description") != "first" {
		t.Fatalf("revert of the skill: %d %s", code, body)
	}
	gone := skillID("revert-gone-" + sfx)
	goneRow := putAudited(t, ts, "/api/skills/"+gone, `{"name":"revert-gone-`+sfx+`","description":"second","body":"# Skill"}`)
	if code, _ := do(t, ts, http.MethodDelete, "/api/skills/"+gone, ""); code != http.StatusNoContent {
		t.Fatalf("delete skill: %d", code)
	}
	refused(goneRow.ID, http.StatusPreconditionFailed, "version_conflict", revertChangedSince)

	// A project: its settings come back and its token stays the latest one; a change of the
	// token alone is nothing to revert.
	oldToken, newToken, lastToken := acceptedTokenPrefix+"old-"+sfx, acceptedTokenPrefix+"new-"+sfx, acceptedTokenPrefix+"last-"+sfx
	p := seedProject(t, testCipher(t), "revert-"+sfx, oldToken)
	projPath := "/api/projects/" + p.ID
	settings := `"name":"revert-` + sfx + `","repo_url":"` + p.RepoURL + `","provider":"gitlab","default_branch":"main"`
	edited := putAudited(t, ts, projPath, `{`+settings+`,"description":"edited","max_cost_usd_per_day":5,"token":"`+newToken+`"}`)
	if code, body = revert(edited.ID); code != http.StatusOK {
		t.Fatalf("revert of the project: %d %s", code, body)
	}
	if got := decodeObject(t, body); got["description"] != "" || got["max_cost_usd_per_day"] != nil || got["token_set"] != true {
		t.Fatalf("reverted project = %s", body)
	}
	if tok, err := s.projects.Token(ctx, p.ID); err != nil || tok != newToken {
		t.Fatalf("the token after the revert is not the latest one (%v)", err)
	}
	tokenOnly := putAudited(t, ts, projPath, `{`+settings+`,"token":"`+lastToken+`"}`)
	refused(tokenOnly.ID, http.StatusUnprocessableEntity, codeNothingToRevert, revertNothing)
	if tok, _ := s.projects.Token(ctx, p.ID); tok != lastToken {
		t.Fatal("the token changed")
	}
	for _, id := range []string{edited.ID, tokenOnly.ID} {
		var whole string
		mustDo(t, pool.QueryRow(ctx, `SELECT coalesce(string_agg(row_to_json(a)::text, ''), '') FROM audit_events a WHERE path = $1`,
			"/api/audit/"+id+"/revert").Scan(&whole))
		for _, secret := range []string{oldToken, newToken, lastToken} {
			if strings.Contains(whole, secret) {
				t.Fatalf("a revert row carries a token: %s", whole)
			}
		}
	}

	// A before that holds a redacted value is not written back.
	secretPath := "/api/agents/" + testAgent(t, "revert-secret-"+sfx)
	putAudited(t, ts, secretPath, `{"name":"revert-secret-`+sfx+`","model":"claude-opus-5-5","system_prompt":"push with ghp_`+strings.Repeat("a", 30)+`"}`)
	clean := putAudited(t, ts, secretPath, `{"name":"revert-secret-`+sfx+`","model":"claude-opus-5-5","system_prompt":"no secrets"}`)
	refused(clean.ID, http.StatusUnprocessableEntity, codeNotRevertible, revertRedacted)

	// What a revert does not undo.
	refused("no-such-audit-event", http.StatusNotFound, "not_found", "")
	created := lastAuditRow(t, "/api/schedules")
	if created.Method != http.MethodPost {
		t.Fatalf("schedule create row = %+v", created)
	}
	refused(created.ID, http.StatusUnprocessableEntity, codeNotRevertible, revertOnlyPut)
	if code, _ := do(t, ts, http.MethodPut, agentPath, `{"name":""}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid PUT: %d", code)
	}
	refused(lastAuditRow(t, agentPath).ID, http.StatusUnprocessableEntity, codeNotRevertible, revertOnlyPut)
	code, body = do(t, ts, http.MethodPost, "/api/workflows", `{"name":"revert-`+sfx+`","definition":`+validDefinition(agentID)+`}`)
	if code != http.StatusCreated {
		t.Fatalf("create workflow: %d %s", code, body)
	}
	wfPath := "/api/workflows/" + decodeWorkflow(t, body).ID
	wfRow := putAudited(t, ts, wfPath, `{"name":"revert-2-`+sfx+`","definition":`+validDefinition(agentID)+`}`)
	refused(wfRow.ID, http.StatusUnprocessableEntity, codeNotRevertible, revertWorkflow)
	noBefore := &store.AuditEvent{ActorKind: "operator", Via: "api", Method: http.MethodPut, Route: "/api/agents/{id}", Path: agentPath, Target: agentPath}
	mustDo(t, store.InsertAuditEvent(ctx, pool, noBefore))
	status := http.StatusOK
	noBefore.Status, noBefore.Outcome, noBefore.After = &status, store.AuditOK, json.RawMessage(`{"id":"`+agentID+`","system_prompt":"x"}`)
	mustDo(t, store.CompleteAuditEvent(ctx, pool, noBefore))
	refused(noBefore.ID, http.StatusUnprocessableEntity, codeNothingToRevert, revertNoBefore)

	// The answers match the contract.
	conf := putAudited(t, ts, agentPath, agentBody("conformance"))
	router := contract(t)
	for _, st := range []conformanceStep{
		{http.MethodPost, "/api/audit/" + conf.ID + "/revert", "", http.StatusOK},
		{http.MethodPost, "/api/audit/" + conf.ID + "/revert", "", http.StatusPreconditionFailed},
		{http.MethodPost, "/api/audit/" + tokenOnly.ID + "/revert", "", http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/audit/no-such-audit-event/revert", "", http.StatusNotFound},
	} {
		if code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300); code != st.want {
			t.Fatalf("%s %s: %d %s", st.method, st.path, code, body)
		}
	}
}

// DESIGN W4 rules 5 and 7 through the Chief: a revert restores an agent's prompt; it carries
// the risk of the change it writes back (rule A5), so a revert that gives tools back, changes
// the Chief itself or raises a budget cap waits for a human, and runs once approved.
func TestAuditRevertByChief(t *testing.T) {
	f := newChatFixture(t, nil)
	ctx := context.Background()
	sfx := uniqueSuffix()
	revert := func(auditID, key string) (map[string]any, string, bool) {
		t.Helper()
		return f.callJSON(t, "platform_call", map[string]any{"operation_id": "revertAudit", "path_params": map[string]any{"id": auditID},
			"idempotency_key": key})
	}
	other := &store.Agent{Name: "Reverted-" + sfx, Model: "claude-opus-5-5", MaxTokens: defaultMaxTokens, SystemPrompt: "be brief",
		ToolAllowlist: []string{"read", "finish"}}
	mustDo(t, store.CreateAgent(ctx, testPool, other))
	path := "/api/agents/" + other.ID
	agentBody := func(prompt string, tools ...string) string {
		b, _ := json.Marshal(map[string]any{"name": other.Name, "model": other.Model, "system_prompt": prompt, "tool_allowlist": tools})
		return string(b)
	}

	// The operator's prompt change, undone by the Chief: nothing the gate holds, so it runs.
	verbose := putAudited(t, f.ts, path, agentBody("be verbose", "read", "finish"))
	out, text, isErr := revert(verbose.ID, "revert-prompt-"+sfx)
	if isErr || out["status"] != float64(http.StatusOK) || out["body"].(map[string]any)["system_prompt"] != "be brief" {
		t.Fatalf("revert of the prompt = %s", text)
	}
	read, text, isErr := f.callJSON(t, "platform_call", map[string]any{"operation_id": "getAgent", "path_params": map[string]any{"id": other.ID}})
	if isErr || read["body"].(map[string]any)["system_prompt"] != "be brief" {
		t.Fatalf("read back = %s", text)
	}
	row := lastAuditRow(t, "/api/audit/"+verbose.ID+"/revert")
	if row.ActorKind != string(actor.Agent) || row.ActorID != f.agent.ID || row.OnBehalfOf != "telegram:"+f.conv.ExternalChatID ||
		row.Target != path || row.Outcome != store.AuditOK || jsonField(t, row.Evidence, "risk") != "routine" ||
		jsonField(t, row.Before, "system_prompt") != "be verbose" || jsonField(t, row.After, "system_prompt") != "be brief" {
		t.Fatalf("revert row = %+v evidence %s", row, row.Evidence)
	}

	// A revert that gives a tool back is privileged: the person approves it by code.
	shrink := putAudited(t, f.ts, path, agentBody("be brief", "read"))
	key := "revert-tools-" + sfx
	pending, text, isErr := revert(shrink.ID, key)
	if isErr || pending["status"] != tools.GatePending {
		t.Fatalf("revert of a tool removal = %s", text)
	}
	apID := pending["approval_id"].(string)
	a, err := store.GetApproval(ctx, testPool, apID)
	mustDo(t, err)
	for _, want := range []string{"revertAudit: POST /api/audit/" + shrink.ID + "/revert", "because it adds tools to the agent's allowlist",
		"It writes back to " + path + ": {", `"tool_allowlist":["read","finish"]`, "Now: {"} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("approval message lacks %q:\n%s", want, a.Message)
		}
	}
	if got, _ := store.GetAgent(ctx, testPool, other.ID); !slices.Equal(got.ToolAllowlist, []string{"read"}) {
		t.Fatalf("the revert ran before the approval: %v", got.ToolAllowlist)
	}
	f.say(t, "approve "+pending["code"].(string), "", time.Now().UTC())
	if dec, text, isErr := f.callJSON(t, "decide_approval", map[string]any{"approval_id": apID, "code": pending["code"], "decision": "approve"}); isErr ||
		dec["status"] != store.ApprovalApproved {
		t.Fatalf("decide_approval = %s", text)
	}
	if done, text, isErr := revert(shrink.ID, key); isErr || done["status"] != float64(http.StatusOK) || done["idempotent_replayed"] != nil {
		t.Fatalf("approved revert = %s", text)
	}
	if got, _ := store.GetAgent(ctx, testPool, other.ID); !slices.Equal(got.ToolAllowlist, []string{"read", "finish"}) {
		t.Fatalf("tools after the revert = %v", got.ToolAllowlist)
	}
	if again, text, isErr := revert(shrink.ID, key); isErr || again["idempotent_replayed"] != true {
		t.Fatalf("repeat = %s", text)
	}

	// A revert of a change to the Chief itself waits for a human.
	selfPath := "/api/agents/" + f.agent.ID
	selfBody, _ := json.Marshal(map[string]any{"name": f.agent.Name, "model": f.agent.Model, "system_prompt": "edited by the operator",
		"tool_allowlist": f.agent.ToolAllowlist, "memory_policy": map[string]any{"enabled": true}})
	self := putAudited(t, f.ts, selfPath, string(selfBody))
	if out, text, _ := revert(self.ID, "revert-self-"+sfx); out["status"] != tools.GatePending {
		t.Fatalf("revert of the Chief's own change = %s", text)
	}

	// Each side of rule A5, judged on what the revert writes back against the record now.
	op, _ := f.s.contract.Operation("revertAudit")
	chief := actor.Actor{Kind: actor.Agent, ID: f.agent.ID, Name: f.agent.Name, Via: actor.ViaChief}
	riskOf := func(auditID string) (tools.PlatformRisk, []string) {
		t.Helper()
		risk, reasons, err := f.s.platformRisk(ctx, op, httptest.NewRequest(http.MethodPost, "/api/audit/"+auditID+"/revert", nil), chief, nil)
		mustDo(t, err)
		return risk, reasons
	}
	grow := putAudited(t, f.ts, path, agentBody("be brief", "read", "finish", "exec"))
	if risk, reasons := riskOf(grow.ID); risk != tools.PlatformRoutine || len(reasons) != 0 {
		t.Errorf("a revert that removes a tool = %s %v", risk, reasons)
	}
	p := seedProject(t, testCipher(t), "revert-chief-"+sfx, acceptedTokenPrefix+sfx)
	capped := func(usd int) store.AuditEvent {
		b, _ := json.Marshal(map[string]any{"name": p.Name, "repo_url": p.RepoURL, "provider": "gitlab", "default_branch": "main", "max_cost_usd_per_day": usd})
		return putAudited(t, f.ts, "/api/projects/"+p.ID, string(b))
	}
	capped(5)
	lowered := capped(4)
	if risk, reasons := riskOf(lowered.ID); risk != tools.PlatformPrivileged || !slices.Contains(reasons, "it raises the project's budget cap") {
		t.Errorf("a revert that raises a cap = %s %v", risk, reasons)
	}
	raised := capped(6)
	if risk, reasons := riskOf(raised.ID); risk != tools.PlatformRoutine || len(reasons) != 0 {
		t.Errorf("a revert that lowers a cap = %s %v", risk, reasons)
	}
	if risk, reasons := riskOf("no-such-audit-event"); risk != tools.PlatformRoutine || len(reasons) != 0 {
		t.Errorf("a revert the handler refuses = %s %v", risk, reasons)
	}
}
