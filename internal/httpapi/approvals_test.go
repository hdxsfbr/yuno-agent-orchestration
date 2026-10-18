package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// seedApprovals creates a run with a pending tool approval (joined to its tool call),
// a pending exec approval and a decided node approval.
func seedApprovals(t *testing.T) (run *store.Run, tool, exec, node *store.Approval) {
	t.Helper()
	ctx := context.Background()
	run, err := createTestRun(ctx, testPool, store.TriggerManual, nil, store.Limits{})
	mustDo(t, err)
	now := time.Now().UTC()
	visit := &store.RunNode{RunID: run.ID, NodeID: "deployer", Status: store.NodeWaitingApproval, StartedAt: &now}
	mustDo(t, store.CreateRunNode(ctx, testPool, visit))
	key := strings.Repeat("a", 64)
	tc := &store.ToolCall{RunID: run.ID, RunNodeID: visit.ID, ToolUseID: "mcp-1", ToolName: "deploy_release",
		Input: json.RawMessage(`{"version":"1.0.0","paths":["app.py"],"run_token":"[redacted]"}`), Status: store.ToolAwaitingApproval,
		IdempotencyKey: &key, StartedAt: &now}
	mustDo(t, store.InsertToolCall(ctx, testPool, tc))
	mk := func(kind, status string, toolCallID *string, at time.Time) *store.Approval {
		a := &store.Approval{RunID: run.ID, RunNodeID: visit.ID, ToolCallID: toolCallID, Kind: kind, NodeID: "deployer",
			Message: kind + " approval", Status: status, RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
		mustDo(t, store.InsertApproval(ctx, testPool, a))
		return a
	}
	node = mk(store.ApprovalKindNode, store.ApprovalApproved, nil, now.Add(-2*time.Second))
	exec = mk(store.ApprovalKindExec, store.ApprovalPending, nil, now.Add(-time.Second))
	tool = mk(store.ApprovalKindTool, store.ApprovalPending, &tc.ID, now)
	return run, tool, exec, node
}

func TestListApprovals(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	run, tool, exec, _ := seedApprovals(t)

	code, body := do(t, ts, http.MethodGet, "/api/approvals?run_id="+run.ID, "")
	var items []store.ApprovalItem
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 3 {
		t.Fatalf("status %d: %s", code, body)
	}
	if items[0].ID != tool.ID || items[0].ToolCall == nil || items[0].ToolCall.ToolName != "deploy_release" ||
		!strings.Contains(string(items[0].ToolCall.Input), `"paths"`) {
		t.Fatalf("newest item is not the tool approval with its call: %s", body)
	}
	if items[1].ID != exec.ID || items[1].ToolCall != nil || strings.Contains(string(body), `"tool_call":null`) {
		t.Fatalf("exec approval: %s", body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/approvals?status=pending&run_id="+run.ID+"&limit=1", "")
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil || len(items) != 1 || items[0].ID != tool.ID {
		t.Fatalf("filtered: %d %s", code, body)
	}
	for _, q := range []string{"status=maybe", "limit=0", "limit=x"} {
		if code, body := do(t, ts, http.MethodGet, "/api/approvals?"+q, ""); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
}

func TestDecideApprovals(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	_, tool, exec, node := seedApprovals(t)

	code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/approve", `{"reason":"looks right"}`)
	var a store.Approval
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil || a.Status != store.ApprovalApproved ||
		a.DecidedBy == nil || *a.DecidedBy != "api" || a.Reason == nil || *a.Reason != "looks right" {
		t.Fatalf("approve: %d %s", code, body)
	}
	if fe.decisions[0] != tool.ID+"/approve/api/looks right" {
		t.Fatalf("engine got %q", fe.decisions[0])
	}

	// T5.3.8: a decision on a decided approval is 409.
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/reject", ""); code != http.StatusConflict ||
		decodeError(t, body).Error.Code != "not_pending" {
		t.Fatalf("second decision: %d %s", code, body)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/approvals/"+node.ID+"/approve", `{}`); code != http.StatusConflict {
		t.Fatalf("decided node approval: %d", code)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/approvals/no-such-approval/approve", ""); code != http.StatusNotFound {
		t.Fatalf("unknown approval: %d", code)
	}
	for _, verb := range []string{"approve", "reject"} { // an id with a NUL is no approval's either
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/no%00such/"+verb, ""); code != http.StatusNotFound {
			t.Fatalf("%s with a NUL in the id: %d %s", verb, code, body)
		}
	}

	// always is for exec approvals only; on one it is the approve_always decision.
	_, tool2, _, _ := seedApprovals(t)
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool2.ID+"/approve", `{"always":true}`); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(body), `"always"`) {
		t.Fatalf("always on a tool approval: %d %s", code, body)
	}
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+exec.ID+"/approve", `{"always":true}`); code != http.StatusOK {
		t.Fatalf("always on exec: %d %s", code, body)
	}
	if last := fe.decisions[len(fe.decisions)-1]; last != exec.ID+"/approve_always/api/" {
		t.Fatalf("engine got %q", last)
	}

	code, body = do(t, ts, http.MethodPost, "/api/approvals/"+tool2.ID+"/reject", `{"reason":"wrong version"}`)
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil || a.Status != store.ApprovalRejected {
		t.Fatalf("reject: %d %s", code, body)
	}

	for _, tc := range []struct{ path, body string }{
		{"/approve", `{"reason":` + jsonQuote(strings.Repeat("r", 2001)) + `}`},
		{"/reject", `{"always":true}`},
		{"/approve", `{"reason":5}`},
	} {
		_, fresh, _, _ := seedApprovals(t)
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+fresh.ID+tc.path, tc.body); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, code, body)
		}
	}
}

// The engine's own answers map onto the contract: ErrNotPending (a decision raced
// this one) 409, unknown 404, not implemented 503.
func TestDecideApprovalEngineErrors(t *testing.T) {
	pool := needDB(t)
	tests := []struct {
		err  error
		want int
	}{
		{engine.ErrNotPending, http.StatusConflict},
		{store.ErrNotFound, http.StatusNotFound},
		{engine.ErrNotImplemented, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		fe := &fakeEngine{pool: pool, decideErr: tc.err}
		_, ts := newTestServer(t, pool, fe, nil)
		_, tool, _, _ := seedApprovals(t)
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+tool.ID+"/approve", ""); code != tc.want {
			t.Fatalf("%v: %d %s", tc.err, code, body)
		}
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---- questions to the operator (DESIGN 5.24 rules 3 and 11, T5.24.12) -----------------------

// answeringEngine is the engine fake with AnswerQuestion playing the engine's rules over the
// store (a question, pending), and recording the calls it gets.
type answeringEngine struct {
	*fakeEngine
	mu      sync.Mutex
	answers []string
	err     error
}

func (e *answeringEngine) AnswerQuestion(ctx context.Context, id, answer, by string) (*store.Approval, error) {
	e.mu.Lock()
	e.answers = append(e.answers, id+"/"+answer+"/"+by)
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a, err := store.GetApproval(ctx, e.pool, id)
	if err != nil {
		return nil, err
	}
	switch {
	case a.Kind != store.ApprovalKindQuestion:
		return nil, engine.ErrNotQuestion
	case a.Status != store.ApprovalPending:
		return nil, engine.ErrNotPending
	}
	return store.AnswerApprovalAt(ctx, e.pool, id, answer, by, time.Now())
}

func (e *answeringEngine) calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.answers)
}

// questionRows are a run's questions and a tool approval, as the inbox lists them.
type questionRows struct {
	run                                   *store.Run
	withOptions, freeText, answered, tool *store.Approval
}

func seedQuestions(t *testing.T) questionRows {
	t.Helper()
	ctx := context.Background()
	run, tool, _, _ := seedApprovals(t)
	visit, err := store.LatestNodeVisit(ctx, testPool, run.ID, "deployer")
	mustDo(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mk := func(message string, options json.RawMessage, status string, answer *string, at time.Time) *store.Approval {
		a := &store.Approval{RunID: run.ID, RunNodeID: visit.ID, Kind: store.ApprovalKindQuestion, NodeID: "deployer", Message: message,
			Options: options, Status: status, Answer: answer, RequestedAt: at, ExpiresAt: at.Add(time.Hour)}
		if status != store.ApprovalPending {
			by := "api"
			a.DecidedAt, a.DecidedBy = &now, &by
		}
		mustDo(t, store.InsertApproval(ctx, testPool, a))
		return a
	}
	text := "Portuguese"
	return questionRows{run: run, tool: tool,
		withOptions: mk("Which greeting language?", json.RawMessage(`["English","Portuguese"]`), store.ApprovalPending, nil, now.Add(time.Second)),
		freeText:    mk("What is the staging host?", nil, store.ApprovalPending, nil, now.Add(2*time.Second)),
		answered:    mk("Which region?", json.RawMessage(`["eu","us"]`), store.ApprovalApproved, &text, now.Add(3*time.Second))}
}

// T5.24.12: answer on a question: 200 with the row (status, answer, decider, options); the
// answer is trimmed; 404, 409 and the 422 texts of rule 3.
func TestAnswerQuestionEndpoint(t *testing.T) {
	pool := needDB(t)
	eng := &answeringEngine{fakeEngine: &fakeEngine{pool: pool}}
	_, ts := newTestServer(t, pool, eng, nil)
	rows := seedQuestions(t)
	post := func(id, body string) (int, []byte) {
		return do(t, ts, http.MethodPost, "/api/approvals/"+id+"/answer", body)
	}

	code, body := post(rows.withOptions.ID, `{"answer":"  Portuguese \n"}`)
	var a store.Approval
	if code != http.StatusOK || json.Unmarshal(body, &a) != nil || a.ID != rows.withOptions.ID || a.Kind != store.ApprovalKindQuestion ||
		a.Status != store.ApprovalApproved || a.Answer == nil || *a.Answer != "Portuguese" || a.DecidedBy == nil || *a.DecidedBy != "api" ||
		a.DecidedAt == nil || string(a.Options) != `["English","Portuguese"]` {
		t.Fatalf("answer: %d %s", code, body)
	}
	if got := eng.calls(); !slices.Equal(got, []string{rows.withOptions.ID + "/Portuguese/api"}) {
		t.Fatalf("engine calls = %v", got)
	}
	// An answer need not be one of the options, and may use the full 4096 bytes.
	if code, body := post(rows.freeText.ID, `{"answer":`+jsonQuote(strings.Repeat("h", 4096))+`}`); code != http.StatusOK {
		t.Fatalf("4096 bytes: %d %s", code, body)
	}

	// 409: a decided question, whichever way it was decided.
	for _, id := range []string{rows.withOptions.ID, rows.answered.ID} {
		code, body := post(id, `{"answer":"again"}`)
		if code != http.StatusConflict || decodeError(t, body).Error.Code != "not_pending" || !strings.Contains(string(body), "approval is already approved") {
			t.Fatalf("answer to a decided question: %d %s", code, body)
		}
	}
	// 404.
	if code, _ := post("no-such-approval", `{"answer":"x"}`); code != http.StatusNotFound {
		t.Fatalf("unknown approval: %d", code)
	}
	// An id with a NUL is no approval's: 404, not the database's refusal as a 500 (review L1).
	for _, id := range []string{"no%00such", "%00", "no%01such"} {
		if code, body := post(id, `{"answer":"x"}`); code != http.StatusNotFound || decodeError(t, body).Error.Code != "not_found" {
			t.Fatalf("approval id %q: %d %s", id, code, body)
		}
	}
	// 422: another kind of approval; the engine is not asked.
	before := len(eng.calls())
	code, body = post(rows.tool.ID, `{"answer":"yes"}`)
	if code != http.StatusUnprocessableEntity || !slices.Equal(detailPaths(t, body), []string{"answer"}) ||
		!strings.Contains(string(body), "only a question can be answered") {
		t.Fatalf("answer to a tool approval: %d %s", code, body)
	}
	if len(eng.calls()) != before {
		t.Fatal("the engine was asked to answer a tool approval")
	}
	if cur, _ := store.GetApproval(context.Background(), pool, rows.tool.ID); cur.Status != store.ApprovalPending {
		t.Fatalf("the tool approval changed: %s", cur.Status)
	}
	// 422: the body.
	fresh := seedQuestions(t)
	for name, tc := range map[string]struct{ body, want string }{
		"no body":          {"", "body is required"},
		"no answer":        {`{}`, "1 to 4096 characters"},
		"blank answer":     {`{"answer":" \n\t "}`, "1 to 4096 characters"},
		"too long":         {`{"answer":` + jsonQuote(strings.Repeat("h", 4097)) + `}`, "1 to 4096 characters"},
		"a NUL":            {`{"answer":` + jsonQuote("a\x00b") + `}`, "1 to 4096 characters"}, // review L1: the insert would fail
		"only a NUL":       {`{"answer":` + jsonQuote("\x00") + `}`, "1 to 4096 characters"},
		"not a string":     {`{"answer":5}`, "must be a string"},
		"an unknown field": {`{"answer":"x","reason":"y"}`, "unknown field"},
		"malformed":        {`{"answer":`, "unexpected EOF"},
	} {
		code, body := post(fresh.withOptions.ID, tc.body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: %d %s; want 422 with %q", name, code, body, tc.want)
		}
	}
	if cur, _ := store.GetApproval(context.Background(), pool, fresh.withOptions.ID); cur.Status != store.ApprovalPending {
		t.Fatalf("a refused answer decided the question: %s", cur.Status)
	}
	// The kind and the answer are both reported when both are wrong.
	code, body = post(fresh.tool.ID, `{"answer":""}`)
	if code != http.StatusUnprocessableEntity || len(detailPaths(t, body)) != 2 {
		t.Fatalf("a blank answer to a tool approval: %d %s", code, body)
	}
}

// The engine's own answers map onto the contract: a decision that raced this one is 409,
// an unknown row 404, a row of another kind 422, a missing engine path 503.
func TestAnswerQuestionEngineErrors(t *testing.T) {
	pool := needDB(t)
	tests := []struct {
		err  error
		want int
	}{
		{engine.ErrNotPending, http.StatusConflict},
		{store.ErrNotFound, http.StatusNotFound},
		{engine.ErrNotQuestion, http.StatusUnprocessableEntity},
		{&engine.ValidationError{Messages: []string{"answer: 1 to 4096 characters"}}, http.StatusUnprocessableEntity},
		{engine.ErrNotImplemented, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		eng := &answeringEngine{fakeEngine: &fakeEngine{pool: pool}, err: tc.err}
		_, ts := newTestServer(t, pool, eng, nil)
		rows := seedQuestions(t)
		if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/answer", `{"answer":"x"}`); code != tc.want {
			t.Fatalf("%v: %d %s", tc.err, code, body)
		}
	}
	// No engine at all.
	_, ts := newTestServer(t, pool, nil, nil)
	rows := seedQuestions(t)
	if code, body := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/answer", `{"answer":"x"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("no engine: %d %s", code, body)
	}
}

// T5.24.12: approve on a question is 422 "answer: a question needs an answer" and decides
// nothing; reject on a question is a decline: 200 with the operator's reason.
func TestApproveAndRejectOnAQuestion(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	_, ts := newTestServer(t, pool, fe, nil)
	rows := seedQuestions(t)

	for _, body := range []string{"", `{"reason":"ok"}`, `{"always":true}`} {
		code, resp := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/approve", body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(resp), "answer") ||
			!strings.Contains(string(resp), "a question needs an answer") || !slices.Contains(detailPaths(t, resp), "answer") {
			t.Fatalf("approve %q: %d %s", body, code, resp)
		}
	}
	if len(fe.decisions) != 0 {
		t.Fatalf("the engine was asked to approve a question: %v", fe.decisions)
	}
	if cur, _ := store.GetApproval(context.Background(), pool, rows.withOptions.ID); cur.Status != store.ApprovalPending {
		t.Fatalf("approve decided the question: %s", cur.Status)
	}

	code, resp := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/reject", `{"reason":"not now"}`)
	var a store.Approval
	if code != http.StatusOK || json.Unmarshal(resp, &a) != nil || a.Status != store.ApprovalRejected || a.Reason == nil || *a.Reason != "not now" ||
		a.Answer != nil || a.Kind != store.ApprovalKindQuestion {
		t.Fatalf("decline: %d %s", code, resp)
	}
	if want := rows.withOptions.ID + "/reject/api/not now"; len(fe.decisions) != 1 || fe.decisions[0] != want {
		t.Fatalf("engine decisions = %v, want [%s]", fe.decisions, want)
	}
	// A decline without a reason, and a decision on a declined question.
	if code, resp := do(t, ts, http.MethodPost, "/api/approvals/"+rows.freeText.ID+"/reject", ""); code != http.StatusOK {
		t.Fatalf("decline without a reason: %d %s", code, resp)
	}
	if code, _ := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/reject", ""); code != http.StatusConflict {
		t.Fatalf("second decline: %d", code)
	}
	if code, resp := do(t, ts, http.MethodPost, "/api/approvals/"+rows.withOptions.ID+"/answer", `{"answer":"late"}`); code != http.StatusConflict {
		t.Fatalf("answer to a declined question: %d %s", code, resp)
	}
}

// The inbox lists a question with its options and, once answered, its answer; the other
// kinds carry neither.
func TestListApprovalsShowsOptionsAndAnswer(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	rows := seedQuestions(t)
	code, body := do(t, ts, http.MethodGet, "/api/approvals?run_id="+rows.run.ID, "")
	var items []map[string]json.RawMessage
	if code != http.StatusOK || json.Unmarshal(body, &items) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	byID := map[string]map[string]json.RawMessage{}
	for _, it := range items {
		var id string
		mustDo(t, json.Unmarshal(it["id"], &id))
		byID[id] = it
	}
	if got := string(byID[rows.withOptions.ID]["options"]); got != `["English","Portuguese"]` || byID[rows.withOptions.ID]["answer"] != nil {
		t.Fatalf("pending question: options %s, answer %s", got, byID[rows.withOptions.ID]["answer"])
	}
	if f := byID[rows.freeText.ID]; f["options"] != nil || f["answer"] != nil || string(f["kind"]) != `"question"` {
		t.Fatalf("free-text question: %v", f)
	}
	if got := string(byID[rows.answered.ID]["answer"]); got != `"Portuguese"` || string(byID[rows.answered.ID]["options"]) != `["eu","us"]` {
		t.Fatalf("answered question: %v", byID[rows.answered.ID])
	}
	if f := byID[rows.tool.ID]; f["options"] != nil || f["answer"] != nil {
		t.Fatalf("tool approval: %v", f)
	}
	code, body = do(t, ts, http.MethodGet, "/api/approvals?status=pending&run_id="+rows.run.ID, "")
	if code != http.StatusOK || strings.Contains(string(body), rows.answered.ID) {
		t.Fatalf("pending filter: %d %s", code, body)
	}
}

// questionConformanceSteps exercises the answer path and the listing of questions against
// the contract (DESIGN 5.24 rules 3 and 11): 200, 404, 409, 422 on a tool approval and on a
// bad answer, approve on a question and the decline. They need an engine whose
// AnswerQuestion decides the row (answeringEngine, or the fake once it does). The
// coordinator appends them to TestOpenAPIConformance.
func questionConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	rows := seedQuestions(t)
	declined := seedQuestions(t)
	return []conformanceStep{
		{http.MethodGet, "/api/approvals?run_id=" + rows.run.ID, "", http.StatusOK},
		{http.MethodPost, "/api/approvals/" + rows.withOptions.ID + "/answer", `{"answer":"Portuguese"}`, http.StatusOK},
		{http.MethodGet, "/api/approvals?status=approved&run_id=" + rows.run.ID, "", http.StatusOK},
		{http.MethodPost, "/api/approvals/" + rows.withOptions.ID + "/answer", `{"answer":"again"}`, http.StatusConflict},
		{http.MethodPost, "/api/approvals/" + rows.answered.ID + "/answer", `{"answer":"again"}`, http.StatusConflict},
		{http.MethodPost, "/api/approvals/no-such-approval/answer", `{"answer":"x"}`, http.StatusNotFound},
		{http.MethodPost, "/api/approvals/" + rows.tool.ID + "/answer", `{"answer":"yes"}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/approvals/" + rows.freeText.ID + "/answer", `{"answer":""}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/approvals/" + rows.freeText.ID + "/approve", "", http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/approvals/" + declined.freeText.ID + "/reject", `{"reason":"not now"}`, http.StatusOK},
		{http.MethodPost, "/api/approvals/" + declined.withOptions.ID + "/answer", `{"answer":"free text, not an option"}`, http.StatusOK},
	}
}

// TestQuestionConformanceSteps runs the question steps against the contract on a server of
// their own.
func TestQuestionConformanceSteps(t *testing.T) {
	pool := needDB(t)
	router := contract(t)
	_, ts := newTestServer(t, pool, &answeringEngine{fakeEngine: &fakeEngine{pool: pool}}, nil)
	for _, st := range questionConformanceSteps(t) {
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
	}
}
