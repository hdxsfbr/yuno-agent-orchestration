package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yuno/internal/engine"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// chatFixture is an agent chatting in a conversation, with a chat token the fake engine
// resolves, and a server around them.
type chatFixture struct {
	s     *Server
	ts    *httptest.Server
	fe    *fakeEngine
	agent *store.Agent
	conv  *store.Conversation
	token string
}

func newChatFixture(t *testing.T, rules map[string]string) *chatFixture {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	sfx := uniqueSuffix()
	f := &chatFixture{}
	f.agent = &store.Agent{Name: "Chief-" + sfx, Model: "claude-opus-5-5", ToolAllowlist: append([]string{"finish"}, tools.ChatToolNames...),
		InteractionRules: rules, MemoryPolicy: store.MemoryPolicy{Enabled: true}}
	mustDo(t, store.CreateAgent(ctx, pool, f.agent))
	f.token = f.agent.ChatToken
	conv, err := store.UpsertConversation(ctx, pool, &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: "7" + sfx[:4],
		AgentID: f.agent.ID, SessionKey: "agent:ag" + sfx + ":telegram:direct:7" + sfx[:4]})
	mustDo(t, err)
	f.conv = conv
	f.fe = &fakeEngine{pool: pool, chats: map[string]engine.ChatContext{f.token: {
		AgentID: f.agent.ID, AgentName: f.agent.Name, ConversationID: conv.ID,
		ToolAllowlist: f.agent.ToolAllowlist, InteractionRules: rules,
	}}}
	f.s, f.ts = newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool}, f.fe)
	return f
}

func (f *chatFixture) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	cs, err := connectMCP(t, f.ts.URL, testMCPToken)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func (f *chatFixture) rows(t *testing.T) []store.ToolCall {
	t.Helper()
	rows, err := store.ListToolCallsByConversation(context.Background(), testPool, f.conv.ID, 100)
	mustDo(t, err)
	return rows
}

// DESIGN 5.15 rules 1 and 3: a chat token resolves to its agent; the call has no run
// and is recorded against the agent's latest conversation, the token redacted.
func TestMCPChatTokenResolves(t *testing.T) {
	f := newChatFixture(t, nil)
	text, isErr := f.call(t, "list_agents", map[string]any{"chat_token": f.token})
	if isErr || !strings.Contains(text, `"agents"`) || strings.Contains(text, `"name":"`+f.agent.Name+`"`) {
		t.Fatalf("list_agents = %v %s", isErr, text)
	}
	text, isErr = f.call(t, "memory_save", map[string]any{"chat_token": f.token, "key": "tone", "value": "brief answers"})
	if isErr || !strings.Contains(text, `"saved"`) {
		t.Fatalf("memory_save in chat = %v %s", isErr, text)
	}
	rows := f.rows(t)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		checkRow(t, r, r.ToolName, store.ToolExecuted)
		if r.RunID != "" || r.RunNodeID != "" || r.ConversationID == nil || *r.ConversationID != f.conv.ID {
			t.Fatalf("row = %+v", r)
		}
		var in map[string]any
		if strings.Contains(string(r.Input), f.token) || json.Unmarshal(r.Input, &in) != nil || in["chat_token"] != "[redacted]" {
			t.Fatalf("chat token stored: %s", r.Input)
		}
	}
	// The conversation's tool calls, as the Channels page reads them, per the contract.
	router := contract(t)
	code, body := checkConformance(t, router, f.ts.URL, http.MethodGet, "/api/conversations/"+f.conv.ID+"/tool-calls", "", true)
	if code != http.StatusOK || !strings.Contains(string(body), `"tool_name":"memory_save"`) || strings.Contains(string(body), f.token) ||
		!strings.Contains(string(body), `"conversation_id":"`+f.conv.ID+`"`) {
		t.Fatalf("tool-calls = %d %s", code, body)
	}
	if code, _ := checkConformance(t, router, f.ts.URL, http.MethodGet, "/api/conversations/no-such/tool-calls", "", true); code != http.StatusNotFound {
		t.Fatalf("unknown conversation = %d", code)
	}
	if code, _ := checkConformance(t, router, f.ts.URL, http.MethodGet, "/api/conversations/"+f.conv.ID+"/tool-calls?limit=0", "", false); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit = %d", code)
	}
}

// DESIGN 5.15 rules 1 and 2: unknown token, both tokens, neither on a chat tool, and a
// run tool called with a chat token.
func TestMCPChatTokenRefusals(t *testing.T) {
	f := newChatFixture(t, nil)
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"unknown token", "list_agents", map[string]any{"chat_token": "c_" + strings.Repeat("a", 32)}, "invalid chat token"},
		{"both tokens", "list_agents", map[string]any{"chat_token": f.token, "run_token": "r_" + strings.Repeat("b", 24)}, "one of run_token or chat_token"},
		{"neither token on a chat tool", "run_status", map[string]any{"run_id": "x"}, "one of run_token or chat_token"},
	} {
		text, isErr := f.call(t, tc.tool, tc.args)
		if !isErr || text != tc.want {
			t.Errorf("%s: %v %q, want %q", tc.name, isErr, text, tc.want)
		}
	}
	if rows := f.rows(t); len(rows) != 0 {
		t.Fatalf("unresolved calls recorded: %+v", rows)
	}
	// A run tool with a valid chat token: refused before validation (its schema would
	// ask for run_token), recorded as blocked.
	for _, tool := range []string{"finish", "deploy_release", "http_fetch"} {
		text, isErr := f.call(t, tool, map[string]any{"chat_token": f.token})
		if !isErr || text != tool+" is not available in a conversation" {
			t.Errorf("%s: %v %q", tool, isErr, text)
		}
	}
	rows := f.rows(t)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		checkRow(t, r, r.ToolName, store.ToolBlocked)
	}
}

// DESIGN 5.15 rules 6 and 7: delegate_task in a conversation triggers a channel run
// with the conversation as its ref; gated by an operator, it is refused in chat.
func TestMCPChatDelegates(t *testing.T) {
	f := newChatFixture(t, nil)
	coder := "Coder-" + uniqueSuffix()
	coderID := testAgent(t, coder)
	text, isErr := f.call(t, "delegate_task", map[string]any{"chat_token": f.token, "agent_name": coder, "task": "Write hello.py"})
	var out struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out.RunID == "" || out.URL != "/runs/"+out.RunID {
		t.Fatalf("delegate_task = %v %s", isErr, text)
	}
	f.fe.mu.Lock()
	req := f.fe.triggers[0]
	f.fe.mu.Unlock()
	if req.Trigger != store.TriggerChannel || req.TriggerRef != "conversation:"+f.conv.ID || req.Task != "Write hello.py" ||
		req.AgentID != coderID || req.WorkflowID != "" || req.Limits != (store.Limits{}) {
		t.Fatalf("trigger = %+v", req)
	}

	gated := newChatFixture(t, map[string]string{"delegate_task": "require_approval"})
	text, isErr = gated.call(t, "delegate_task", map[string]any{"chat_token": gated.token, "agent_name": coder, "task": "t"})
	if !isErr || !strings.Contains(text, "needs a human approval") {
		t.Fatalf("gated = %v %q", isErr, text)
	}
	if len(gated.fe.triggers) != 0 {
		t.Fatalf("gated call triggered: %+v", gated.fe.triggers)
	}
	if rows := gated.rows(t); len(rows) != 1 || rows[0].Status != store.ToolBlocked {
		t.Fatalf("gated rows = %+v", rows)
	}
}

// DESIGN 5.15 rule 3: an agent with no conversation still has its call recorded, with
// neither a run nor a conversation.
func TestMCPChatWithoutConversation(t *testing.T) {
	f := newChatFixture(t, nil)
	f.fe.mu.Lock()
	cc := f.fe.chats[f.token]
	cc.ConversationID = ""
	f.fe.chats[f.token] = cc
	f.fe.mu.Unlock()
	// The run id is unique to this test run: the shared schema keeps earlier runs' rows.
	runID := "no-such-run-" + uniqueSuffix()
	if text, isErr := f.call(t, "run_status", map[string]any{"chat_token": f.token, "run_id": runID}); !isErr || text != "no run "+runID {
		t.Fatalf("run_status = %v %q", isErr, text)
	}
	var n int
	mustDo(t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM tool_calls WHERE run_id IS NULL AND conversation_id IS NULL
		AND tool_name = 'run_status' AND input->>'run_id' = $1 AND status = 'failed'`, runID).Scan(&n))
	if n != 1 {
		t.Fatalf("calls without a conversation recorded = %d, want 1", n)
	}
}

// DESIGN 5.15 rule 1: the chat token is never returned by the API.
func TestAgentsAPINeverReturnsChatToken(t *testing.T) {
	pool := needDB(t)
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	code, body := do(t, ts, http.MethodPost, "/api/agents", `{"name":"Token keeper `+uniqueSuffix()+`","model":"claude-opus-5-5"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var a store.Agent
	mustDo(t, json.Unmarshal(body, &a))
	stored, err := store.GetAgent(context.Background(), pool, a.ID)
	mustDo(t, err)
	if !strings.HasPrefix(stored.ChatToken, "c_") {
		t.Fatalf("no chat token minted: %q", stored.ChatToken)
	}
	for _, path := range []string{"/api/agents/" + a.ID, "/api/agents"} {
		code, body := do(t, ts, http.MethodGet, path, "")
		if code != http.StatusOK || strings.Contains(string(body), stored.ChatToken) || strings.Contains(string(body), "chat_token") {
			t.Fatalf("GET %s leaks the chat token: %s", path, body)
		}
	}
}
