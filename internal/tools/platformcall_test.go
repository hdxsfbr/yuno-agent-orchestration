package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The contract loads with every operation classed, and the operations an agent may not
// run are classed so (platform control A1).
func TestPlatformContract(t *testing.T) {
	c, err := Contract()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.Operations()); n < 80 {
		t.Fatalf("%d operations", n)
	}
	for id, want := range map[string]PlatformRisk{
		"healthz": PlatformMachine, "eventsAll": PlatformMachine, "registerScript": PlatformMachine, "importSkillZip": PlatformMachine,
		"approveApproval": PlatformDedicated, "answerApproval": PlatformDedicated, "listAgents": PlatformRead,
		"createAgent": PlatformRoutine, "deleteAgent": PlatformDestructive, "bindTelegram": PlatformPrivileged,
		"mergeMergeRequest": PlatformDestructive, "validateWorkflow": PlatformRoutine,
	} {
		if op, ok := c.Operation(id); !ok || op.Risk != want {
			t.Errorf("%s = %+v, want %s", id, op, want)
		}
	}
	if op, _ := c.Operation("answerApproval"); op.Tool != AnswerQuestionName {
		t.Errorf("answerApproval names %q", op.Tool)
	}
	if op, _ := c.Operation("updateAgent"); op.GetTwin != "getAgent" {
		t.Errorf("updateAgent's GET twin = %q", op.GetTwin)
	}
	if op, _ := c.Operation("putAgentFile"); op.GetTwin != "" {
		t.Errorf("putAgentFile's GET twin = %q", op.GetTwin)
	}
	if op, ok := c.ForPattern("PUT /api/skills/{id}/files/{path...}"); !ok || op.ID != "putSkillFile" {
		t.Errorf("a {path...} pattern = %+v", op)
	}
	if _, ok := c.ForPattern("/mcp"); ok {
		t.Error("a pattern without a method names an operation")
	}
	// A contract that misses a class, or names an operationId twice, does not load.
	for name, doc := range map[string]string{
		"no risk":        `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/a":{"get":{"operationId":"a","responses":{"200":{"description":"ok"}}}}}}`,
		"no operationId": `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/a":{"get":{"x-yuno-risk":"read","responses":{"200":{"description":"ok"}}}}}}`,
		"dedicated, no tool": `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/a":{"post":{"operationId":"a","x-yuno-risk":"dedicated",` +
			`"responses":{"200":{"description":"ok"}}}}}}`,
	} {
		if _, err := LoadPlatformContract([]byte(doc)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

// Platform control A2: by text, by tag, and the empty query; at most 20, never a machine
// operation, the body schema resolved to depth 2.
func TestPlatformOperationsSearch(t *testing.T) {
	tool := NewPlatformOperations()
	NewRegistry().Register(tool)
	search := func(in string) map[string]any {
		t.Helper()
		res := invoke(t, tool, Call{Chat: true, Input: json.RawMessage(in)})
		if res.IsError {
			t.Fatalf("%s: %s", in, res.Text)
		}
		var out map[string]any
		must(t, json.Unmarshal(res.Output, &out))
		return out
	}
	ops := func(out map[string]any) []map[string]any {
		var list []map[string]any
		for _, o := range out["operations"].([]any) {
			list = append(list, o.(map[string]any))
		}
		return list
	}
	ids := func(list []map[string]any) []string {
		var out []string
		for _, o := range list {
			out = append(out, o["operation_id"].(string))
		}
		return out
	}

	// The empty query: the tags and how many operations each offers.
	tags := search(`{"chat_token":"c_x"}`)["tags"].([]any)
	counts := map[string]float64{}
	for _, tg := range tags {
		m := tg.(map[string]any)
		counts[m["tag"].(string)] = m["operations"].(float64)
	}
	if counts["agents"] < 8 || counts["schedules"] != 5 || counts["ops"] != 4 || len(counts) < 10 {
		t.Fatalf("tags = %v", tags)
	}

	// By text: every word must appear; machine operations never do.
	found := ops(search(`{"chat_token":"c_x","query":"Schedule"}`))
	if got := ids(found); !slices.Equal(got, []string{"listSchedules", "createSchedule", "deleteSchedule", "getSchedule", "updateSchedule"}) {
		t.Fatalf("schedule = %v", got)
	}
	for _, o := range found {
		switch o["operation_id"] {
		case "createSchedule":
			body, _ := o["request_body"].(map[string]any)
			props, _ := body["properties"].(map[string]any)
			if props["cron_expr"] == nil || !slices.Equal(anyStrings(o["needs"]), []string{"idempotency_key"}) || o["read_first"] != nil {
				t.Fatalf("createSchedule = %v", o)
			}
		case "deleteSchedule":
			if o["risk"] != "destructive" || o["read_first"] != "getSchedule" || !slices.Equal(anyStrings(o["needs"]), []string{"idempotency_key", "if_match"}) ||
				o["approval"] == nil || o["request_body"] != nil {
				t.Fatalf("deleteSchedule = %v", o)
			}
		case "getSchedule":
			pp, _ := o["path_params"].([]any)
			if o["risk"] != "read" || o["needs"] != nil || len(pp) != 1 || pp[0].(map[string]any)["name"] != "id" {
				t.Fatalf("getSchedule = %v", o)
			}
		}
	}
	if got := ids(ops(search(`{"chat_token":"c_x","query":"events"}`))); slices.Contains(got, "eventsAll") || slices.Contains(got, "eventsRun") {
		t.Fatalf("machine operations offered: %v", got)
	}
	if got := ids(ops(search(`{"chat_token":"c_x","query":"agents/{id}/files"}`))); !slices.Equal(got, []string{"listAgentFiles", "putAgentFile"}) {
		t.Fatalf("agent files = %v", got)
	}
	// Every word counts, in any field: the descriptions too.
	if got := ids(ops(search(`{"chat_token":"c_x","query":"agent files"}`))); !slices.Contains(got, "listAgentFiles") || slices.Contains(got, "listAgents") {
		t.Fatalf("agent files, as words = %v", got)
	}

	// By tag, with a query or alone; a dedicated operation names its tool.
	byTag := ops(search(`{"chat_token":"c_x","tag":"approvals"}`))
	if got := ids(byTag); !slices.Equal(got, []string{"listApprovals", "answerApproval", "approveApproval", "rejectApproval"}) {
		t.Fatalf("approvals = %v", got)
	}
	if byTag[2]["use_tool"] != DecideApprovalName || byTag[1]["use_tool"] != AnswerQuestionName {
		t.Fatalf("dedicated = %v", byTag)
	}
	if got := ids(ops(search(`{"chat_token":"c_x","tag":"agents","query":"getAgent"}`))); !slices.Equal(got, []string{"getAgent"}) {
		t.Fatalf("tag and query = %v", got)
	}
	if got := ids(ops(search(`{"chat_token":"c_x","tag":"memory"}`))); !slices.Equal(got, []string{"listAgentMemory", "deleteAgentMemory"}) {
		t.Fatalf("tag memory = %v", got)
	}

	// At most 20, saying how many more matched.
	many := search(`{"chat_token":"c_x","query":"/api/"}`)
	if len(ops(many)) != 20 || many["more"].(float64) < 40 {
		t.Fatalf("a wide query = %d operations, more %v", len(ops(many)), many["more"])
	}

	// The request body: AgentInput (level 1) resolves its Guardrails (level 2); depth 1
	// leaves it a reference.
	c, _ := Contract()
	create, _ := c.Operation("createAgent")
	body := operationView(create, 2)["request_body"].(map[string]any)
	g := body["properties"].(map[string]any)["guardrails"].(map[string]any)
	if g["properties"] == nil || slices.Contains(anyStrings(body["required"]), "id") || !slices.Contains(anyStrings(body["required"]), "name") {
		t.Fatalf("depth 2 = %v", body)
	}
	shallow := operationView(create, 1)["request_body"].(map[string]any)
	if ref := shallow["properties"].(map[string]any)["guardrails"].(map[string]any)["$ref"]; ref != "Guardrails" {
		t.Fatalf("depth 1 guardrails = %v", ref)
	}
}

func anyStrings(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, s := range x {
			out = append(out, s.(string))
		}
	case []string:
		out = x
	}
	return out
}

// A request is built from the contract: path params escaped as one segment, the query
// sorted, unknown and missing params refused.
func TestPlatformTarget(t *testing.T) {
	c, _ := Contract()
	op, _ := c.Operation("putSkillFile")
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	got, err := platformTarget(op, map[string]json.RawMessage{"id": raw(`"sk 1"`), "path": raw(`"docs/a b.md"`)}, nil)
	if err != nil || got != "/api/skills/sk%201/files/docs%2Fa%20b.md" {
		t.Fatalf("target = %q, %v", got, err)
	}
	list, _ := c.Operation("listRuns")
	if got, err := platformTarget(list, nil, map[string]json.RawMessage{"status": raw(`"queued"`), "limit": raw(`5`)}); err != nil ||
		got != "/api/runs?limit=5&status=queued" {
		t.Fatalf("query = %q, %v", got, err)
	}
	for _, tc := range []struct {
		params map[string]json.RawMessage
		want   string
	}{
		{map[string]json.RawMessage{"id": raw(`"x"`)}, "path_params: path is required"},
		{map[string]json.RawMessage{"id": raw(`"x"`), "path": raw(`"p"`), "other": raw(`"y"`)}, "path_params: other is not a parameter of putSkillFile"},
		{map[string]json.RawMessage{"id": raw(`{"a":1}`), "path": raw(`"p"`)}, "path_params: id is required"},
	} {
		if _, err := platformTarget(op, tc.params, nil); err == nil || err.Error() != tc.want {
			t.Errorf("%v: %v, want %q", tc.params, err, tc.want)
		}
	}
	if _, err := platformTarget(list, nil, map[string]json.RawMessage{"status": raw(`{"a":1}`)}); err == nil {
		t.Error("an object query value was accepted")
	}
}

// Platform control A8: what counts as a secret in a body.
func TestPlatformSecretInBody(t *testing.T) {
	for _, tc := range []struct {
		body   string
		secret bool
	}{
		{`{"name":"p","token":"glpat-abc"}`, true},
		{`{"token":"plain-value"}`, true},
		{`{"settings":{"api_key":"k-1"}}`, true},
		{`{"password":12345}`, true},
		{`{"description":"use ghp_abcdefghijklmnopqrstuvwxyz0123"}`, true},
		{`{"list":["Bearer abcdefghijklmnopqrstuvwxyz"]}`, true},
		{`{"tokens":["a"],"token":""}`, false},
		{`{"token":null,"token_set":true}`, false},
		{`{"input_schema":{"properties":{"token":{"type":"string"}}}}`, false},
		{`{"max_tokens":4096,"idempotency_key":"k1"}`, false},
		{``, false},
	} {
		if got := SecretInBody([]byte(tc.body)); got != tc.secret {
			t.Errorf("%s: secret = %v, want %v", tc.body, got, tc.secret)
		}
	}
	// The placeholder a read leaves under a secret key means "unchanged" and is dropped;
	// the same text under another key stays.
	got, err := canonicalBody(json.RawMessage(`{"z":1,"token":"[redacted]","token_set":"[redacted]","note":"[redacted]","a":{"secret":"[redacted]"}}`))
	if err != nil || string(got) != `{"a":{},"note":"[redacted]","z":1}` {
		t.Fatalf("canonical = %s, %v", got, err)
	}
}

// Platform control A9 (and A8 for the shape of the body): status, request id, etag,
// idempotent_replayed and the body as data; the gate's refusal as a refused call.
func TestPlatformCallResult(t *testing.T) {
	resp := func(status int, ctype, body string, h map[string]string) *PlatformResponse {
		hdr := http.Header{}
		if ctype != "" {
			hdr.Set("Content-Type", ctype)
		}
		for k, v := range h {
			hdr.Set(k, v)
		}
		sum := sha256.Sum256([]byte(body))
		return &PlatformResponse{Status: status, Header: hdr, Body: []byte(body), Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	}
	decode := func(r Result) map[string]any {
		t.Helper()
		var out map[string]any
		must(t, json.Unmarshal(r.Output, &out))
		return out
	}
	ok := decode(platformResult(resp(200, "application/json", `{"id":"a1","token_set":true,"note":"glpat-abcdefghijklmnopqrstu"}`,
		map[string]string{"ETag": `"abc"`, "X-Request-Id": "req-1", "Idempotency-Replayed": "true"})))
	body := ok["body"].(map[string]any)
	if ok["status"] != float64(200) || ok["request_id"] != "req-1" || ok["etag"] != `"abc"` || ok["idempotent_replayed"] != true ||
		body["id"] != "a1" || body["token_set"] != "[redacted]" || strings.Contains(body["note"].(string), "glpat") {
		t.Fatalf("ok = %v", ok)
	}
	conflict := decode(platformResult(resp(412, "application/json", `{"error":{"code":"version_conflict","details":{"current_etag":"\"x\""}}}`, nil)))
	if conflict["status"] != float64(412) || conflict["etag"] != nil || conflict["idempotent_replayed"] != nil ||
		conflict["body"].(map[string]any)["error"].(map[string]any)["code"] != "version_conflict" {
		t.Fatalf("an error body is data: %v", conflict)
	}
	if empty := decode(platformResult(resp(204, "", "", nil))); empty["status"] != float64(204) || empty["body"] != nil {
		t.Fatalf("204 = %v", empty)
	}
	yaml := decode(platformResult(resp(200, "application/yaml", "openapi: 3.1.0\n", nil)))
	if yaml["body"] != "openapi: 3.1.0\n" {
		t.Fatalf("text = %v", yaml)
	}
	// Longer than 32 KiB: trimmed with the marker, the whole result within the cap.
	big := `{"items":["` + strings.Repeat(`x\"y`, 20000) + `"]}`
	long := platformResult(resp(200, "application/json", big, nil))
	if len(long.Output) > PlatformResultMax || !json.Valid(long.Output) || !strings.Contains(decode(long)["body"].(string), "[truncated ") {
		t.Fatalf("long = %d bytes, valid %v", len(long.Output), json.Valid(long.Output))
	}
	// The gate's refusal and its pending answer.
	denied := platformResult(resp(428, "application/json", `{"error":{"code":"idempotency_key_required","message":"idempotency_key is required"}}`,
		map[string]string{HeaderPlatformGate: GateDenied}))
	if !denied.IsError || !denied.Blocked || denied.Text != "idempotency_key is required" {
		t.Fatalf("denied = %+v", denied)
	}
	pending := decode(platformResult(resp(202, "application/json", `{"status":"pending_approval","approval_id":"ap1","code":"K7QM"}`,
		map[string]string{HeaderPlatformGate: GatePending})))
	if pending["status"] != "pending_approval" || pending["approval_id"] != "ap1" || pending["code"] != "K7QM" {
		t.Fatalf("pending = %v", pending)
	}
}

// Platform control A8: a binary body is its metadata, never its bytes.
func TestPlatformCallBinary(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	sum := sha256.Sum256([]byte(png))
	hdr := http.Header{}
	hdr.Set("Content-Type", "image/png")
	res := platformResult(&PlatformResponse{Status: 200, Header: hdr, Body: []byte(png), Size: int64(len(png)), SHA256: hex.EncodeToString(sum[:])})
	var out struct {
		Body map[string]any `json:"body"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	if out.Body["content_type"] != "image/png" || out.Body["bytes"] != float64(len(png)) || out.Body["sha256"] != hex.EncodeToString(sum[:]) ||
		strings.Contains(string(res.Output), "PNG") {
		t.Fatalf("binary = %s", res.Output)
	}
}

// platform_call refuses what it cannot build before anything is sent.
func TestPlatformCallInputRefusals(t *testing.T) {
	sent := 0
	tool := NewPlatformCall(PlatformCallDeps{Dispatcher: dispatcherFunc(func(context.Context, PlatformRequest) (*PlatformResponse, error) {
		sent++
		return &PlatformResponse{Status: 200, Header: http.Header{}}, nil
	})})
	NewRegistry().Register(tool)
	for in, want := range map[string]string{
		`{"chat_token":"c_x","operation_id":"noSuchOperation"}`:                           "unknown operation",
		`{"chat_token":"c_x","operation_id":"getAgent"}`:                                  "path_params: id is required",
		`{"chat_token":"c_x","operation_id":"listAgents","body":{"a":1}}`:                 "body: listAgents takes no body",
		`{"chat_token":"c_x","operation_id":"getAgent","path_params":{"id":"a","x":"b"}}`: "path_params: x is not a parameter of getAgent",
	} {
		if res := invoke(t, tool, Call{Chat: true, Input: json.RawMessage(in)}); !res.IsError || res.Text != want {
			t.Errorf("%s = %+v, want %q", in, res, want)
		}
	}
	if sent != 0 {
		t.Fatalf("%d requests sent", sent)
	}
}

type dispatcherFunc func(context.Context, PlatformRequest) (*PlatformResponse, error)

func (f dispatcherFunc) Dispatch(ctx context.Context, req PlatformRequest) (*PlatformResponse, error) {
	return f(ctx, req)
}
