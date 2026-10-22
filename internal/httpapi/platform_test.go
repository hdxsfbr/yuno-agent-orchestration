package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/actor"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Chief platform control, wave 1 part A: platform_call through the api's own chain and the
// platform gate.

// platformFixture is the Chief's chat fixture with platform_call dispatched through DESIGN
// B6's chain, in which part B's idempotency and If-Match layers are played by fakes that
// keep to the headers of B3 and B4, and with a count of handler runs by route.
type platformFixture struct {
	*chatFixture
	tool *tools.Tool
	mu   sync.Mutex
	runs map[string]int
}

func newPlatformFixture(t *testing.T) *platformFixture {
	t.Helper()
	f := newChatFixture(t, nil)
	p := &platformFixture{chatFixture: f, runs: map[string]int{}}
	s := f.s
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		p.mu.Lock()
		p.runs[pattern]++
		p.mu.Unlock()
		s.mux.ServeHTTP(w, r)
	})
	chain := s.recoverer(s.requestID(s.logging(s.auth(s.platformGate(fakeIdempotency(fakeVersions(s, counted)))))))
	p.tool = tools.NewPlatformCall(tools.PlatformCallDeps{Pool: testPool, Dispatcher: platformDispatcher{s: s, handler: chain}})
	tools.NewRegistry().Register(p.tool)
	return p
}

// pc runs platform_call as the Chief in its conversation: the result's JSON, its text and
// whether it is a refusal.
func (p *platformFixture) pc(t *testing.T, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	args["chat_token"] = p.token
	in, err := json.Marshal(args)
	mustDo(t, err)
	res, err := p.tool.Run(context.Background(), tools.Call{AgentID: p.agent.ID, Chat: true, ConversationID: p.conv.ID, Input: in},
		p.s.cfg.ToolOutputMaxBytes)
	mustDo(t, err)
	var out map[string]any
	if !res.IsError {
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("not JSON: %s", res.Text)
		}
	}
	return out, res.Text, res.IsError
}

func (p *platformFixture) ran(pattern string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[pattern]
}

// fakeIdempotency plays DESIGN B3 by its headers: a mutating request's Idempotency-Key, per
// actor, method and path, replays the stored response with Idempotency-Replayed: true for
// the same body, refuses another body, and stores nothing of a 5xx.
func fakeIdempotency(next http.Handler) http.Handler {
	type stored struct {
		sha    string
		status int
		header http.Header
		body   []byte
	}
	var mu sync.Mutex
	keys := map[string]*stored{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(tools.HeaderIdempotencyKey)
		if key == "" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		who, _ := actor.From(r.Context())
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		k := who.ID + " " + r.Method + " " + r.URL.Path + " " + key
		mu.Lock()
		st := keys[k]
		mu.Unlock()
		if st != nil {
			if st.sha != tools.BodySHA256(body) {
				writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "the key was used with another body", nil)
				return
			}
			for h, v := range st.header {
				w.Header()[h] = v
			}
			w.Header().Set(tools.HeaderIdempotencyReplayed, "true")
			w.WriteHeader(st.status)
			_, _ = w.Write(st.body)
			return
		}
		rec := newResponseBuffer(platformResponseMax)
		next.ServeHTTP(rec, r)
		res := rec.response()
		if res.Status < 500 {
			mu.Lock()
			keys[k] = &stored{sha: tools.BodySHA256(body), status: res.Status, header: res.Header.Clone(), body: bytes.Clone(res.Body)}
			mu.Unlock()
		}
		for h, v := range res.Header {
			w.Header()[h] = v
		}
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
	})
}

// fakeETag is DESIGN B4's version of a JSON body.
func fakeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// fakeVersions plays DESIGN B4 by its headers: a GET 200 carries ETag, and an update or
// delete with If-Match runs only when it is the target's current one, else 412
// version_conflict.
func fakeVersions(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rec := newResponseBuffer(platformResponseMax)
			next.ServeHTTP(rec, r)
			res := rec.response()
			for h, v := range res.Header {
				w.Header()[h] = v
			}
			if res.Status == http.StatusOK && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
				w.Header().Set(tools.HeaderETag, fakeETag(res.Body))
			}
			w.WriteHeader(res.Status)
			_, _ = w.Write(res.Body)
		case http.MethodPut, http.MethodPatch, http.MethodDelete:
			if want := r.Header.Get(tools.HeaderIfMatch); want != "" {
				get := httptest.NewRequestWithContext(r.Context(), http.MethodGet, r.URL.EscapedPath(), nil)
				rec := newResponseBuffer(platformResponseMax)
				s.mux.ServeHTTP(rec, get)
				if cur := fakeETag(rec.response().Body); cur != want {
					writeError(w, http.StatusPreconditionFailed, "version_conflict", "the target changed since it was read", map[string]string{"current_etag": cur})
					return
				}
			}
			next.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// scheduleBody is a valid schedule of wf.
func scheduleBody(wf *store.Workflow) map[string]any {
	return map[string]any{"workflow_id": wf.ID, "kind": "cron", "cron_expr": "0 9 * * *", "tz": "UTC", "input": map[string]any{"task": "report"}}
}

// newSchedule creates a schedule through platform_call and reads it back: its id and etag.
func (p *platformFixture) newSchedule(t *testing.T) (id, etag string) {
	t.Helper()
	out, text, isErr := p.pc(t, map[string]any{"operation_id": "createSchedule", "body": scheduleBody(scheduleWorkflow(t)),
		"idempotency_key": "create-" + uniqueSuffix()})
	if isErr || out["status"] != float64(http.StatusCreated) {
		t.Fatalf("create schedule = %s", text)
	}
	id = out["body"].(map[string]any)["id"].(string)
	out, text, isErr = p.pc(t, map[string]any{"operation_id": "getSchedule", "path_params": map[string]any{"id": id}})
	if isErr || out["status"] != float64(http.StatusOK) || out["etag"] == nil {
		t.Fatalf("read schedule = %s", text)
	}
	return id, out["etag"].(string)
}

// Platform control A3 and A9: a read needs nothing, goes through the MCP gate and the api's
// own handler, and answers status, request id and the body; an error status is data.
func TestPlatformCallRead(t *testing.T) {
	f := newChatFixture(t, nil)
	other := testAgent(t, "Read-"+uniqueSuffix())
	text, isErr := f.call(t, "platform_call", map[string]any{"chat_token": f.token, "operation_id": "getAgent", "path_params": map[string]any{"id": other}})
	var out map[string]any
	if isErr || json.Unmarshal([]byte(text), &out) != nil {
		t.Fatalf("getAgent = %v %s", isErr, text)
	}
	if out["status"] != float64(http.StatusOK) || out["request_id"] == "" || out["body"].(map[string]any)["id"] != other {
		t.Fatalf("getAgent = %s", text)
	}
	text, isErr = f.call(t, "platform_call", map[string]any{"chat_token": f.token, "operation_id": "listRuns", "query": map[string]any{"limit": 1}})
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out["status"] != float64(http.StatusOK) || len(out["body"].([]any)) > 1 {
		t.Fatalf("listRuns = %v %s", isErr, text)
	}
	text, isErr = f.call(t, "platform_call", map[string]any{"chat_token": f.token, "operation_id": "getAgent", "path_params": map[string]any{"id": "no-such-agent"}})
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out["status"] != float64(http.StatusNotFound) ||
		out["body"].(map[string]any)["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("404 = %v %s", isErr, text)
	}
	text, isErr = f.call(t, "platform_operations", map[string]any{"chat_token": f.token})
	if isErr || !strings.Contains(text, `"tags"`) {
		t.Fatalf("platform_operations = %v %s", isErr, text)
	}
	// The calls are recorded like every chat call (DESIGN 5.15 rule 3).
	n := 0
	for _, r := range f.rows(t) {
		if r.ToolName == tools.PlatformCallName && r.Status == store.ToolExecuted {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d platform_call rows", n)
	}
}

// Platform control A3: everything but a read needs an idempotency key; with one, a routine
// call runs, and its repeat replays (B3) without running again.
func TestPlatformCallRoutine(t *testing.T) {
	p := newPlatformFixture(t)
	body := scheduleBody(scheduleWorkflow(t))
	_, text, isErr := p.pc(t, map[string]any{"operation_id": "createSchedule", "body": body})
	if !isErr || text != gateKeyRequired || p.ran("POST /api/schedules") != 0 {
		t.Fatalf("no key = %v %s", isErr, text)
	}
	key := "create-" + uniqueSuffix()
	out, text, isErr := p.pc(t, map[string]any{"operation_id": "createSchedule", "body": body, "idempotency_key": key})
	if isErr || out["status"] != float64(http.StatusCreated) || out["idempotent_replayed"] != nil || p.ran("POST /api/schedules") != 1 {
		t.Fatalf("with a key = %s", text)
	}
	id := out["body"].(map[string]any)["id"].(string)
	if _, err := store.GetSchedule(context.Background(), testPool, id); err != nil {
		t.Fatalf("not stored: %v", err)
	}
	out, text, isErr = p.pc(t, map[string]any{"operation_id": "createSchedule", "body": body, "idempotency_key": key})
	if isErr || out["idempotent_replayed"] != true || out["body"].(map[string]any)["id"] != id || p.ran("POST /api/schedules") != 1 {
		t.Fatalf("repeat = %s", text)
	}
	// A routine operation that changes nothing still wants the key (DESIGN A edge cases).
	valid := map[string]any{"operation_id": "validateWorkflow", "body": map[string]any{"definition": json.RawMessage(validDefinition(p.agent.ID))}}
	if _, text, isErr := p.pc(t, valid); !isErr || text != gateKeyRequired {
		t.Fatalf("validate without a key = %s", text)
	}
	valid["idempotency_key"] = "validate-" + uniqueSuffix()
	if out, text, isErr := p.pc(t, valid); isErr || out["status"] != float64(http.StatusOK) {
		t.Fatalf("validate = %s", text)
	}
}

// Platform control A3 and DESIGN B4: an update of something readable needs if_match; a
// stale one is 412 and changes nothing; the fresh one succeeds.
func TestPlatformCallIfMatch(t *testing.T) {
	p := newPlatformFixture(t)
	id, etag := p.newSchedule(t)
	update := func(ifMatch string) (map[string]any, string, bool) {
		args := map[string]any{"operation_id": "updateSchedule", "path_params": map[string]any{"id": id}, "body": map[string]any{"enabled": false},
			"idempotency_key": "update-" + uniqueSuffix()}
		if ifMatch != "" {
			args["if_match"] = ifMatch
		}
		return p.pc(t, args)
	}
	if _, text, isErr := update(""); !isErr || text != "read it first with getSchedule and pass its etag as if_match" {
		t.Fatalf("missing = %v %s", isErr, text)
	}
	out, text, isErr := update(`"0000000000000000"`)
	if isErr || out["status"] != float64(http.StatusPreconditionFailed) ||
		out["body"].(map[string]any)["error"].(map[string]any)["code"] != "version_conflict" || p.ran("PUT /api/schedules/{id}") != 0 {
		t.Fatalf("stale = %s", text)
	}
	out, text, isErr = update(etag)
	if isErr || out["status"] != float64(http.StatusOK) || out["body"].(map[string]any)["enabled"] != false {
		t.Fatalf("fresh = %s", text)
	}
	// The read it was based on is stale now.
	if out, text, _ := update(etag); out["status"] != float64(http.StatusPreconditionFailed) || p.ran("PUT /api/schedules/{id}") != 1 {
		t.Fatalf("after the change = %s", text)
	}
}

// Platform control A3: the machine operations, and a run with machine fields, are refused
// "not available to agents"; so are the mounts the contract leaves out.
func TestPlatformCallRefusesMachine(t *testing.T) {
	p := newPlatformFixture(t)
	for _, args := range []map[string]any{
		{"operation_id": "eventsAll"},
		{"operation_id": "eventsRun", "path_params": map[string]any{"id": "r1"}},
		{"operation_id": "healthz"},
		{"operation_id": "registerScript", "body": map[string]any{"script_id": "s1", "steps": []any{}}, "idempotency_key": "k1"},
		{"operation_id": "importSkillZip", "idempotency_key": "k2"},
		{"operation_id": "createRun", "body": map[string]any{"agent_id": p.agent.ID, "task": "t", "script_id": "c1"}, "idempotency_key": "k3"},
		{"operation_id": "createRun", "body": map[string]any{"agent_id": p.agent.ID, "task": "t", "trigger": "eval"}, "idempotency_key": "k4"},
	} {
		if _, text, isErr := p.pc(t, args); !isErr || text != gateMachine {
			t.Errorf("%v = %v %s", args, isErr, text)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.runs) != 0 {
		t.Fatalf("handlers ran: %v", p.runs)
	}
	// An agent actor reaches neither the MCP server nor the model proxy.
	for _, path := range []string{"/mcp", anthropicProxyPrefix + "/v1/messages"} {
		r := httptest.NewRequestWithContext(actor.With(context.Background(), actor.Actor{Kind: actor.Agent, ID: p.agent.ID, Via: actor.ViaChief}),
			http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		p.s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || w.Header().Get(tools.HeaderPlatformGate) != tools.GateDenied {
			t.Errorf("%s = %d %s", path, w.Code, w.Body)
		}
	}
}

// Platform control A3: an approval is decided, and a question answered, only through the
// tool that carries the person's evidence.
func TestPlatformCallRefusesDedicated(t *testing.T) {
	p := newPlatformFixture(t)
	_, tool, _, _ := seedApprovals(t)
	for op, want := range map[string]string{"approveApproval": "use decide_approval", "rejectApproval": "use decide_approval",
		"answerApproval": "use answer_question"} {
		_, text, isErr := p.pc(t, map[string]any{"operation_id": op, "path_params": map[string]any{"id": tool.ID}, "idempotency_key": "k-" + op})
		if !isErr || text != want {
			t.Errorf("%s = %v %s", op, isErr, text)
		}
	}
	if a, _ := store.GetApproval(context.Background(), testPool, tool.ID); a.Status != store.ApprovalPending {
		t.Fatalf("decided: %+v", a)
	}
}

// Platform control A8: a secret in a body is refused and nothing is created; a schema that
// names a token property is no secret.
func TestPlatformCallRefusesSecrets(t *testing.T) {
	p := newPlatformFixture(t)
	sfx := uniqueSuffix()
	_, text, isErr := p.pc(t, map[string]any{"operation_id": "createProject", "idempotency_key": "p-" + sfx,
		"body": map[string]any{"name": "secret-" + sfx, "repo_url": "https://gitlab.test/g/secret-" + sfx + ".git", "token": acceptedTokenPrefix + sfx}})
	if !isErr || text != gateSecret || strings.Contains(text, acceptedTokenPrefix) {
		t.Fatalf("token = %v %s", isErr, text)
	}
	if _, err := store.GetProjectByName(context.Background(), testPool, "secret-"+sfx); err == nil {
		t.Fatal("a project was created")
	}
	_, text, isErr = p.pc(t, map[string]any{"operation_id": "createWorkflow", "idempotency_key": "w-" + sfx,
		"body": map[string]any{"name": "wf-" + sfx, "description": "push with ghp_abcdefghijklmnopqrstuvwxyz0123", "definition": json.RawMessage(validDefinition(p.agent.ID))}})
	if !isErr || text != gateSecret {
		t.Fatalf("a credential in the text = %v %s", isErr, text)
	}
	def := `{"input_schema":{"type":"object","properties":{"token":{"type":"string"}}},"nodes":[],"edges":[]}`
	if _, text, _ := p.pc(t, map[string]any{"operation_id": "validateWorkflow", "idempotency_key": "v-" + sfx,
		"body": map[string]any{"definition": json.RawMessage(def)}}); strings.Contains(text, gateSecret) {
		t.Fatalf("a schema property named token = %s", text)
	}
	if p.ran("POST /api/projects") != 0 || p.ran("POST /api/workflows") != 0 || p.ran("POST /api/workflows/validate") != 1 {
		t.Fatalf("runs = %v", p.runs)
	}
}

// Platform control A6: a destructive call asks for an approval of exactly that call; the
// person approves it by code; the repeat executes once; a second repeat replays (B3) and
// does not execute again. The approval has no run and is listed and decided like any other.
func TestDestructiveNeedsApproval(t *testing.T) {
	p := newPlatformFixture(t)
	ctx := context.Background()
	id, etag := p.newSchedule(t)
	key := "delete-" + uniqueSuffix()
	del := map[string]any{"operation_id": "deleteSchedule", "path_params": map[string]any{"id": id}, "idempotency_key": key, "if_match": etag}
	out, text, isErr := p.pc(t, del)
	if isErr || out["status"] != tools.GatePending || out["approval_id"] == nil || len(out["code"].(string)) != 4 || p.ran("DELETE /api/schedules/{id}") != 0 {
		t.Fatalf("first = %s", text)
	}
	apID := out["approval_id"].(string)
	if out["code"] != tools.ApprovalCode(p.s.codeKey, apID) {
		t.Fatalf("code = %v", out["code"])
	}
	a, err := store.GetApproval(ctx, testPool, apID)
	mustDo(t, err)
	var b store.PlatformBinding
	mustDo(t, json.Unmarshal(a.Binding, &b))
	if a.Kind != store.ApprovalKindPlatform || a.RunID != "" || a.RunNodeID != "" || a.Status != store.ApprovalPending || a.UsedAt != nil ||
		b != (store.PlatformBinding{AgentID: p.agent.ID, OperationID: "deleteSchedule", Path: "/api/schedules/" + id, BodySHA256: tools.BodySHA256(nil), IdempotencyKey: key}) ||
		a.ExpiresAt.Sub(a.RequestedAt) != time.Hour {
		t.Fatalf("approval = %+v binding %+v", a, b)
	}
	for _, want := range []string{p.agent.Name + " asks to run deleteSchedule: DELETE /api/schedules/" + id, "On behalf of telegram:" + p.conv.ExternalChatID,
		"because it deletes, cancels or merges something", "Now: {", id} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, a.Message)
		}
	}
	// Still pending: the same answer. Another key for the same action asks again.
	if again, text, _ := p.pc(t, del); again["approval_id"] != apID {
		t.Fatalf("repeat while pending = %s", text)
	}
	other := map[string]any{"operation_id": "deleteSchedule", "path_params": map[string]any{"id": id}, "idempotency_key": key + "-b", "if_match": etag}
	if second, text, _ := p.pc(t, other); second["approval_id"] == nil || second["approval_id"] == apID {
		t.Fatalf("another key = %s", text)
	}

	// The operator's inbox lists it without a run; approve always is for commands only.
	code, body := do(t, p.ts, http.MethodGet, "/api/approvals?status=pending&limit=500", "")
	var items []store.ApprovalItem
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil {
		t.Fatalf("list = %d %s", code, body)
	}
	var listed *store.ApprovalItem
	for i := range items {
		if items[i].ID == apID {
			listed = &items[i]
		}
	}
	if listed == nil || listed.RunID != "" || listed.RunStatus != "" || listed.Kind != store.ApprovalKindPlatform || len(listed.Binding) == 0 {
		t.Fatalf("listed = %+v", listed)
	}
	if code, body := do(t, p.ts, http.MethodPost, "/api/approvals/"+apID+"/approve", `{"always":true}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("approve always = %d %s", code, body)
	}

	// The Chief lists it and decides it with the code the person sent.
	lout, text, isErr := p.callJSON(t, "list_approvals", map[string]any{})
	found := false
	for _, it := range lout["approvals"].([]any) {
		if m := it.(map[string]any); m["id"] == apID && m["kind"] == store.ApprovalKindPlatform && m["code"] == out["code"] && m["run_id"] == "" {
			found = true
		}
	}
	if isErr || !found {
		t.Fatalf("list_approvals = %s", text)
	}
	p.say(t, "yes, "+out["code"].(string), "", time.Now().UTC())
	if dout, text, isErr := p.callJSON(t, "decide_approval", map[string]any{"approval_id": apID, "code": out["code"], "decision": "approve"}); isErr ||
		dout["status"] != store.ApprovalApproved {
		t.Fatalf("decide_approval = %s", text)
	}

	// The repeat runs once and spends the approval; the next repeat replays.
	done, text, isErr := p.pc(t, del)
	if isErr || done["status"] != float64(http.StatusNoContent) || done["idempotent_replayed"] != nil || p.ran("DELETE /api/schedules/{id}") != 1 {
		t.Fatalf("approved repeat = %s", text)
	}
	if a, _ := store.GetApproval(ctx, testPool, apID); a.Status != store.ApprovalApproved || a.UsedAt == nil {
		t.Fatalf("approval after use = %+v", a)
	}
	if _, err := store.GetSchedule(ctx, testPool, id); err == nil {
		t.Fatal("the schedule is still there")
	}
	replay, text, isErr := p.pc(t, del)
	if isErr || replay["status"] != float64(http.StatusNoContent) || replay["idempotent_replayed"] != true || p.ran("DELETE /api/schedules/{id}") != 1 {
		t.Fatalf("second repeat = %s", text)
	}
}

// Platform control A6: a rejected approval refuses its call "rejected by <who>"; an
// expired one (pending, or approved and not used in time) refuses "expired; call again to
// ask again" once, and the next call asks again.
func TestPlatformApprovalRejectedExpired(t *testing.T) {
	p := newPlatformFixture(t)
	ctx := context.Background()
	id, etag := p.newSchedule(t)
	call := func(key string) (map[string]any, string, bool) {
		return p.pc(t, map[string]any{"operation_id": "deleteSchedule", "path_params": map[string]any{"id": id}, "idempotency_key": key, "if_match": etag})
	}
	ask := func(key string) string {
		t.Helper()
		out, text, isErr := call(key)
		if isErr || out["status"] != tools.GatePending {
			t.Fatalf("%s = %s", key, text)
		}
		return out["approval_id"].(string)
	}
	lapse := func(apID string) {
		t.Helper()
		_, err := testPool.Exec(ctx, `UPDATE approvals SET expires_at = now() - interval '1 minute' WHERE id = $1`, apID)
		mustDo(t, err)
	}

	// Rejected, by the operator in the UI: the same call stays refused.
	first := ask("k1-" + id)
	if code, body := do(t, p.ts, http.MethodPost, "/api/approvals/"+first+"/reject", `{"reason":"keep it"}`); code != http.StatusOK {
		t.Fatalf("reject = %d %s", code, body)
	}
	for range 2 {
		if _, text, isErr := call("k1-" + id); !isErr || text != "rejected by api" {
			t.Fatalf("rejected = %v %s", isErr, text)
		}
	}

	// Expired while pending (checked at use, before any sweep).
	second := ask("k2-" + id)
	lapse(second)
	if _, text, isErr := call("k2-" + id); !isErr || text != gateExpired {
		t.Fatalf("expired = %v %s", isErr, text)
	}
	if a, _ := store.GetApproval(ctx, testPool, second); a.Status != store.ApprovalExpired || a.UsedAt == nil || deref(a.DecidedBy, "") != "platform" {
		t.Fatalf("expired approval = %+v", a)
	}
	third := ask("k2-" + id)
	if third == second {
		t.Fatal("the expired approval was asked again")
	}

	// Approved, then not used before its expiry.
	if code, body := do(t, p.ts, http.MethodPost, "/api/approvals/"+third+"/approve", ""); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	lapse(third)
	if _, text, isErr := call("k2-" + id); !isErr || text != gateExpired {
		t.Fatalf("approved, lapsed = %v %s", isErr, text)
	}
	if a, _ := store.GetApproval(ctx, testPool, third); a.Status != store.ApprovalExpired || a.UsedAt == nil || deref(a.DecidedBy, "") != "api" {
		t.Fatalf("lapsed approval = %+v", a)
	}

	// Expired by the sweeper: told once, then asked again.
	fourth := ask("k2-" + id)
	_, err := testPool.Exec(ctx, `UPDATE approvals SET status = 'expired', decided_by = 'platform', decided_at = now() WHERE id = $1`, fourth)
	mustDo(t, err)
	if _, text, isErr := call("k2-" + id); !isErr || text != gateExpired {
		t.Fatalf("swept = %v %s", isErr, text)
	}
	if fifth := ask("k2-" + id); fifth == fourth || fifth == third {
		t.Fatal("asked again with a spent approval")
	}
	if p.ran("DELETE /api/schedules/{id}") != 0 {
		t.Fatal("the delete ran")
	}
}

// Platform control A5: each case raises the call to privileged, and its other side does not.
func TestPrivilegedNeedsApproval(t *testing.T) {
	p := newPlatformFixture(t)
	s, ctx, sfx := p.s, context.Background(), uniqueSuffix()
	other := &store.Agent{Name: "Other-" + sfx, Model: "claude-opus-5-5", MaxTokens: defaultMaxTokens, ToolAllowlist: []string{"read", "finish"},
		InteractionRules: map[string]string{"deploy_release": "require_approval"},
		Guardrails:       store.Guardrails{MaxRunsPerHour: 5, BlockedTools: []string{"exec"}}, ExecPolicy: store.ExecPolicy{Security: "allowlist"}}
	mustDo(t, store.CreateAgent(ctx, testPool, other))
	day, month := store.USDMicros(5_000_000), store.USDMicros(50_000_000)
	proj := &store.Project{ID: store.NewID(), Name: "priv-" + sfx, RepoURL: "https://gitlab.test/group/priv-" + sfx + ".git", Provider: "gitlab",
		DefaultBranch: "main", BudgetDayUSDMicros: &day, BudgetMonthUSDMicros: &month}
	mustDo(t, store.CreateProject(ctx, testPool, proj, nil))
	t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, proj.ID) })
	chief := actor.Actor{Kind: actor.Agent, ID: p.agent.ID, Name: p.agent.Name, Via: actor.ViaChief}

	agent := func(mut func(m map[string]any)) map[string]any {
		m := map[string]any{"name": other.Name, "model": other.Model, "tool_allowlist": []string{"read", "finish"},
			"interaction_rules": map[string]string{"deploy_release": "require_approval"},
			"guardrails":        map[string]any{"max_runs_per_hour": 5, "blocked_tools": []string{"exec"}}, "exec_policy": map[string]any{"security": "allowlist"}}
		if mut != nil {
			mut(m)
		}
		return m
	}
	projectBody := func(mut func(m map[string]any)) map[string]any {
		m := map[string]any{"name": proj.Name, "repo_url": proj.RepoURL, "provider": "gitlab", "default_branch": "main",
			"max_cost_usd_per_day": 5, "max_cost_usd_per_month": 50}
		if mut != nil {
			mut(m)
		}
		return m
	}
	for _, tc := range []struct {
		name       string
		op, path   string
		body       any
		privileged bool
	}{
		{"a new agent with tools", "createAgent", "/api/agents", map[string]any{"name": "n", "model": "m", "tool_allowlist": []string{"finish"}}, true},
		{"a new agent with only guardrails", "createAgent", "/api/agents", map[string]any{"name": "n", "model": "m", "guardrails": map[string]any{"max_runs_per_hour": 3}}, false},
		{"a new agent with nothing", "createAgent", "/api/agents", map[string]any{"name": "n", "model": "m"}, false},
		{"adding a tool", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["tool_allowlist"] = []string{"read", "finish", "exec"} }), true},
		{"removing a tool", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["tool_allowlist"] = []string{"read"} }), false},
		{"loosening a rule", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["interaction_rules"] = map[string]string{"deploy_release": "auto"} }), true},
		{"adding a looser rule", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) {
			m["interaction_rules"] = map[string]string{"deploy_release": "require_approval", "yuno__open_merge_request": "auto"}
		}), true},
		{"tightening a rule", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) {
			m["interaction_rules"] = map[string]string{"deploy_release": "blocked", "read": "blocked"}
		}), false},
		{"dropping a rule that is the default", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { delete(m, "interaction_rules") }), false},
		{"raising a guardrail cap", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) {
			m["guardrails"] = map[string]any{"max_runs_per_hour": 10, "blocked_tools": []string{"exec"}}
		}), true},
		{"lowering a guardrail cap", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) {
			m["guardrails"] = map[string]any{"max_runs_per_hour": 2, "blocked_tools": []string{"exec"}}
		}), false},
		{"unblocking a tool", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["guardrails"] = map[string]any{"max_runs_per_hour": 5} }), true},
		{"widening the exec policy", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["exec_policy"] = map[string]any{"security": "full"} }), true},
		{"narrowing the exec policy", "updateAgent", "/api/agents/" + other.ID, agent(func(m map[string]any) { m["exec_policy"] = map[string]any{"security": "deny"} }), false},
		{"the calling agent itself", "updateAgent", "/api/agents/" + p.agent.ID, map[string]any{"name": p.agent.Name, "model": p.agent.Model}, true},
		{"the calling agent's file", "putAgentFile", "/api/agents/" + p.agent.ID + "/files/SOUL.md", map[string]any{"addition": "be bold"}, true},
		{"another agent's file", "putAgentFile", "/api/agents/" + other.ID + "/files/SOUL.md", map[string]any{"addition": "be bold"}, false},
		{"raising a budget cap", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) { m["max_cost_usd_per_day"] = 6 }), true},
		{"removing a budget cap", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) { delete(m, "max_cost_usd_per_month") }), true},
		{"lowering a budget cap", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) { m["max_cost_usd_per_day"] = 4 }), false},
		{"changing the repository", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) {
			m["repo_url"] = "https://gitlab.test/group/elsewhere-" + sfx + ".git"
		}), true},
		{"changing the provider", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) { m["provider"] = "github" }), true},
		{"keeping the repository", "updateProject", "/api/projects/" + proj.ID, projectBody(func(m map[string]any) { m["description"] = "edited" }), false},
		{"run limits above the defaults", "createRun", "/api/runs", map[string]any{"agent_id": other.ID, "task": "t", "limits": map[string]any{"max_steps": 100}}, true},
		{"run limits within the defaults", "createRun", "/api/runs", map[string]any{"agent_id": other.ID, "task": "t", "limits": map[string]any{"max_steps": 10}}, false},
	} {
		op, ok := s.contract.Operation(tc.op)
		if !ok {
			t.Fatalf("no operation %s", tc.op)
		}
		raw, _ := json.Marshal(tc.body)
		risk, reasons, err := s.platformRisk(ctx, op, httptest.NewRequest(op.Method, tc.path, nil), chief, raw)
		mustDo(t, err)
		if got := risk == tools.PlatformPrivileged; got != tc.privileged || got != (len(reasons) > 0) {
			t.Errorf("%s: risk %s, reasons %v", tc.name, risk, reasons)
		}
	}
	if op, _ := s.contract.Operation("bindTelegram"); op.Risk != tools.PlatformPrivileged {
		t.Fatalf("bindTelegram = %s", op.Risk)
	}

	// End to end: adding a tool waits for a human; removing one runs.
	read, text, isErr := p.pc(t, map[string]any{"operation_id": "getAgent", "path_params": map[string]any{"id": other.ID}})
	if isErr || read["etag"] == nil {
		t.Fatalf("read = %s", text)
	}
	cur := read["body"].(map[string]any)
	put := func(tools []string, key string) (map[string]any, string, bool) {
		body := map[string]any{}
		for k, v := range cur {
			body[k] = v
		}
		body["tool_allowlist"] = tools
		return p.pc(t, map[string]any{"operation_id": "updateAgent", "path_params": map[string]any{"id": other.ID}, "body": body,
			"idempotency_key": key, "if_match": read["etag"]})
	}
	if out, text, _ := put([]string{"read", "finish", "exec"}, "grow-"+sfx); out["status"] != tools.GatePending {
		t.Fatalf("adding a tool = %s", text)
	}
	if out, text, _ := put([]string{"read"}, "shrink-"+sfx); out["status"] != float64(http.StatusOK) || p.ran("PUT /api/agents/{id}") != 1 {
		t.Fatalf("removing a tool = %s", text)
	}
}

// Platform control A8: a binary artifact comes back as its type, size and hash, a text one
// as its text.
func TestPlatformCallArtifacts(t *testing.T) {
	p := newPlatformFixture(t)
	run := newArtifactRun(t, testPool)
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01", 64)
	bin := putArtifact(t, artifactStoreDir(), run, "out/chart.png", "image/png", png)
	txt := putArtifact(t, artifactStoreDir(), run, "notes.md", "text/markdown; charset=utf-8", "# Notes\n")
	sum := sha256.Sum256([]byte(png))
	out, text, isErr := p.pc(t, map[string]any{"operation_id": "downloadArtifact", "path_params": map[string]any{"id": run.ID, "artifactId": bin.ID}})
	body, _ := out["body"].(map[string]any)
	if isErr || out["status"] != float64(http.StatusOK) || body["content_type"] != "image/png" || body["bytes"] != float64(len(png)) ||
		body["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("binary = %s", text)
	}
	out, text, isErr = p.pc(t, map[string]any{"operation_id": "downloadArtifact", "path_params": map[string]any{"id": run.ID, "artifactId": txt.ID}})
	if isErr || out["body"] != "# Notes\n" {
		t.Fatalf("text = %s", text)
	}
}

// Platform control A4: the request a platform_call makes carries the agent, via chief, on
// behalf of the conversation's Telegram chat, with the server's own bearer; the gate tells
// the request's note what it decided. (Part B's audit row is not on this branch: the
// handler's view of the actor and the note stand in for it.)
func TestPlatformCallActor(t *testing.T) {
	f := newChatFixture(t, nil)
	s := f.s
	s.cfg.APIToken = "platform-actor-test-api-token"
	var mu sync.Mutex
	var seen []actor.Actor
	var notes []*actor.Note
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, _ := actor.From(r.Context())
		mu.Lock()
		seen = append(seen, a)
		mu.Unlock()
		s.mux.ServeHTTP(w, r)
	})
	// The audit's place in DESIGN B6: it puts a note in the context, before the gate.
	noted := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, n := actor.WithNote(r.Context())
			mu.Lock()
			notes = append(notes, n)
			mu.Unlock()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	chain := s.recoverer(s.requestID(s.logging(s.auth(noted(s.platformGate(fakeIdempotency(capture)))))))
	tool := tools.NewPlatformCall(tools.PlatformCallDeps{Pool: testPool, Dispatcher: platformDispatcher{s: s, handler: chain}})
	tools.NewRegistry().Register(tool)
	run := func(conv string, args map[string]any) map[string]any {
		t.Helper()
		in, _ := json.Marshal(args)
		res, err := tool.Run(context.Background(), tools.Call{AgentID: f.agent.ID, Chat: true, ConversationID: conv, Input: in}, 32768)
		mustDo(t, err)
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	lastNote := func() *actor.Note {
		mu.Lock()
		defer mu.Unlock()
		return notes[len(notes)-1]
	}

	out := run(f.conv.ID, map[string]any{"operation_id": "listTools"})
	if out["status"] != float64(http.StatusOK) {
		t.Fatalf("with the bearer = %v", out)
	}
	want := actor.Actor{Kind: actor.Agent, ID: f.agent.ID, Name: f.agent.Name, Via: actor.ViaChief, OnBehalfOf: "telegram:" + f.conv.ExternalChatID}
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("actor = %+v, want %+v", seen, want)
	}
	if want.Label() != "agent:"+f.agent.Name+" for telegram:"+f.conv.ExternalChatID {
		t.Fatalf("label = %s", want.Label())
	}
	// Without a conversation nobody is named.
	run("", map[string]any{"operation_id": "listTools"})
	if seen[1].OnBehalfOf != "" || seen[1].ID != f.agent.ID {
		t.Fatalf("no conversation = %+v", seen[1])
	}
	// The gate's decisions reach the note: a refusal, a pending approval.
	run(f.conv.ID, map[string]any{"operation_id": "createSchedule", "body": scheduleBody(scheduleWorkflow(t))})
	if n := lastNote(); n.Outcome != outcomeDenied || n.ErrorCode != "idempotency_key_required" {
		t.Fatalf("denied note = %+v", n)
	}
	sched := &store.Schedule{WorkflowID: scheduleWorkflow(t).ID, Kind: store.ScheduleDelay, TZ: "UTC", Input: json.RawMessage(`{}`)}
	at := time.Now().Add(time.Hour).UTC()
	sched.RunAt = &at
	mustDo(t, store.CreateSchedule(context.Background(), testPool, sched))
	out = run(f.conv.ID, map[string]any{"operation_id": "deleteSchedule", "path_params": map[string]any{"id": sched.ID}, "idempotency_key": "a-" + sched.ID,
		"if_match": `"any"`})
	if n := lastNote(); out["status"] != tools.GatePending || n.Outcome != outcomePendingApproval || n.ApprovalID != out["approval_id"] ||
		n.Evidence["risk"] != "destructive" {
		t.Fatalf("pending = %v, note %+v", out, n)
	}
	// A request from the network carries no agent: the gate leaves it to auth and the handler.
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for token, want := range map[string]int{"": http.StatusUnauthorized, s.cfg.APIToken: http.StatusOK} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/workflows/validate", strings.NewReader(`{"definition":{"nodes":[],"edges":[]}}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := ts.Client().Do(req)
		mustDo(t, err)
		resp.Body.Close()
		if (resp.StatusCode == http.StatusUnauthorized) != (want == http.StatusUnauthorized) || resp.Header.Get(tools.HeaderPlatformGate) != "" {
			t.Fatalf("network, bearer %v = %d", token != "", resp.StatusCode)
		}
	}
}
