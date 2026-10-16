package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// channelGateway is a gateway with a config: config.patch merges, a restart is
// recorded, and channels.status answers the bot as the Telegram plugin's getMe probe.
type channelGateway struct {
	mu       sync.Mutex
	cfg      map[string]any
	patches  []string
	restarts int
	patchErr error
}

func newChannelGateway() *channelGateway {
	return &channelGateway{cfg: map[string]any{"channels": map[string]any{"telegram": map[string]any{
		"enabled": true, "dmPolicy": "disabled", "groupPolicy": "disabled", "allowFrom": []any{42}}}}}
}

func (g *channelGateway) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch method {
	case "config.get":
		b, err := json.Marshal(map[string]any{"hash": "h", "config": g.cfg})
		return b, err
	case "config.patch":
		if g.patchErr != nil {
			return nil, g.patchErr
		}
		p := params.(openclaw.ConfigPatchParams)
		g.patches = append(g.patches, p.Raw)
		var patch map[string]any
		_ = json.Unmarshal([]byte(p.Raw), &patch)
		mergeInto(g.cfg, patch)
		return json.RawMessage(`{"ok":true}`), nil
	case "gateway.restart.request":
		g.restarts++
		return json.RawMessage(`{"ok":true,"status":"scheduled"}`), nil
	case "channels.status":
		return json.RawMessage(`{"channels":{"telegram":{"configured":true,"running":true,"mode":"polling",
			"probe":{"ok":true,"bot":{"id":1,"isBot":true,"username":"yuno_test_bot"}}}}}`), nil
	}
	return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "unknown method: " + method}
}

func (g *channelGateway) Subscribe(int) (<-chan openclaw.Event, func()) {
	return make(chan openclaw.Event), func() {}
}

func (g *channelGateway) Info() (openclaw.ConnectInfo, bool) {
	return openclaw.ConnectInfo{Scopes: openclaw.RequiredScopes}, true
}

func (g *channelGateway) telegram() (bindings []any, dmPolicy any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, _ := g.cfg["bindings"].([]any)
	return b, g.cfg["channels"].(map[string]any)["telegram"].(map[string]any)["dmPolicy"]
}

func mergeInto(dst, patch map[string]any) {
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, pm)
				continue
			}
		}
		dst[k] = v
	}
}

type channelsBody struct {
	Telegram struct {
		Enabled        bool    `json:"enabled"`
		Mode           string  `json:"mode"`
		BotUsername    string  `json:"bot_username"`
		AllowedUserIDs []int64 `json:"allowed_user_ids"`
		Binding        *struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
		} `json:"binding"`
	} `json:"telegram"`
}

func decodeChannels(t *testing.T, b []byte) channelsBody {
	t.Helper()
	var v channelsBody
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("not a channels body: %s", b)
	}
	return v
}

// channelServer serves the API with Telegram in polling mode and no binding.
func channelServer(t *testing.T, gw *channelGateway) (*fakeEngine, func(method, path, body string) (int, []byte)) {
	t.Helper()
	pool := needDB(t)
	clearBinding := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM channel_bindings`); err != nil {
			t.Fatal(err)
		}
	}
	clearBinding()
	t.Cleanup(clearBinding)
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	cfg.TelegramAllowedUserIDs = []int64{42}
	eng := &fakeEngine{pool: pool}
	s, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: gw}, eng)
	s.refreshBotUsername(context.Background()) // as the api does at start
	return eng, func(method, path, body string) (int, []byte) { return do(t, ts, method, path, body) }
}

func channelAgent(t *testing.T, name string) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: name + " " + store.NewID()[24:], Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"}}
	if err := store.CreateAgent(context.Background(), needDB(t), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// T5.8.3: binding writes bindings[] (per-channel-peer) and opens DMs, refuses a second
// agent while one is bound, and unbinding closes the channel again (D-025).
func TestTelegramBinding(t *testing.T) {
	gw := newChannelGateway()
	eng, call := channelServer(t, gw)
	coder, reviewer := channelAgent(t, "Coder"), channelAgent(t, "Reviewer")

	code, body := call(http.MethodGet, "/api/channels", "")
	v := decodeChannels(t, body)
	if code != http.StatusOK || !v.Telegram.Enabled || v.Telegram.Mode != "polling" || v.Telegram.Binding != nil ||
		v.Telegram.BotUsername != "yuno_test_bot" || len(v.Telegram.AllowedUserIDs) != 1 || v.Telegram.AllowedUserIDs[0] != 42 {
		t.Fatalf("GET /api/channels: %d %s", code, body)
	}

	code, body = call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+coder.ID+`"}`)
	v = decodeChannels(t, body)
	if code != http.StatusOK || v.Telegram.Binding == nil || v.Telegram.Binding.AgentID != coder.ID || v.Telegram.Binding.AgentName != coder.Name {
		t.Fatalf("bind: %d %s", code, body)
	}
	if len(eng.materialized) != 1 || eng.materialized[0] != coder.ID {
		t.Fatalf("an agent never materialised is materialised before binding: %v", eng.materialized)
	}
	bindings, dm := gw.telegram()
	if len(bindings) != 1 || dm != "allowlist" || gw.restarts != 1 {
		t.Fatalf("gateway: bindings %v, dmPolicy %v, restarts %d", bindings, dm, gw.restarts)
	}
	b := bindings[0].(map[string]any)
	if b["agentId"] != "t-"+coder.ID || b["match"].(map[string]any)["channel"] != "telegram" ||
		b["session"].(map[string]any)["dmScope"] != "per-channel-peer" {
		t.Fatalf("binding entry = %v", b)
	}
	stored, err := store.GetChannelBinding(context.Background(), needDB(t), store.ChannelTelegram)
	if err != nil || stored.ExternalID != "yuno_test_bot" {
		t.Fatalf("stored binding = %+v, %v", stored, err)
	}

	code, body = call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+reviewer.ID+`"}`)
	if code != http.StatusConflict || decodeError(t, body).Error.Code != "channel_bound" {
		t.Fatalf("second agent: %d %s", code, body)
	}
	code, _ = call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+coder.ID+`"}`)
	if code != http.StatusOK || len(gw.patches) != 1 || gw.restarts != 1 {
		t.Fatalf("binding the bound agent again: %d, patches %d, restarts %d", code, len(gw.patches), gw.restarts)
	}

	code, body = call(http.MethodDelete, "/api/channels/telegram/binding", "")
	if code != http.StatusOK || decodeChannels(t, body).Telegram.Binding != nil {
		t.Fatalf("unbind: %d %s", code, body)
	}
	bindings, dm = gw.telegram()
	if len(bindings) != 0 || dm != "disabled" || gw.restarts != 2 {
		t.Fatalf("after unbind: bindings %v, dmPolicy %v, restarts %d", bindings, dm, gw.restarts)
	}
	code, body = call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+reviewer.ID+`"}`)
	if code != http.StatusOK || decodeChannels(t, body).Telegram.Binding.AgentID != reviewer.ID {
		t.Fatalf("bind after unbind: %d %s", code, body)
	}
}

func TestTelegramBindingValidation(t *testing.T) {
	_, call := channelServer(t, newChannelGateway())
	tests := []struct {
		name, body string
		want       int
		path       string
	}{
		{name: "missing agent", body: `{}`, want: http.StatusUnprocessableEntity, path: "agent_id"},
		{name: "unknown field", body: `{"agent_id":"x","agent":"y"}`, want: http.StatusUnprocessableEntity, path: "agent"},
		{name: "unknown agent", body: `{"agent_id":"` + store.NewID() + `"}`, want: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := call(http.MethodPut, "/api/channels/telegram/binding", tc.body)
			if code != tc.want {
				t.Fatalf("%d %s", code, body)
			}
			if tc.path != "" {
				if paths := detailPaths(t, body); len(paths) != 1 || paths[0] != tc.path {
					t.Fatalf("details %v", paths)
				}
			}
		})
	}
}

// The binding is the record; a gateway that refuses it answers 502 with the binding's
// id, and the keeper applies it again later.
func TestTelegramBindingGatewayFailure(t *testing.T) {
	gw := newChannelGateway()
	gw.patchErr = &openclaw.RPCError{Code: "UNAVAILABLE", Message: "gateway starting; retry shortly", Retryable: true}
	_, call := channelServer(t, gw)
	a := channelAgent(t, "Coder")
	code, body := call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+a.ID+`"}`)
	e := decodeError(t, body)
	var details map[string]string
	_ = json.Unmarshal(e.Error.Details, &details)
	if code != http.StatusBadGateway || e.Error.Code != "gateway_error" || details["binding_id"] == "" {
		t.Fatalf("%d %s", code, body)
	}
	if b, err := store.GetChannelBinding(context.Background(), needDB(t), store.ChannelTelegram); err != nil || b.AgentID != a.ID {
		t.Fatalf("binding not kept: %+v %v", b, err)
	}
}

func TestConversationsAPI(t *testing.T) {
	_, call := channelServer(t, newChannelGateway())
	pool := needDB(t)
	ctx := context.Background()
	a := channelAgent(t, "Coder")
	key := "agent:t-" + store.NewID()[24:] + ":telegram:direct:42"
	conv, err := store.UpsertConversation(ctx, pool, &store.Conversation{Channel: "telegram", ExternalChatID: "42", AgentID: a.ID,
		SessionKey: key, DisplayName: "Test Operator (@test_operator) id:42"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond) // newest in the shared database
	for i, m := range []store.ConversationMessage{
		{Direction: "in", ExternalMessageID: "m1", Text: "hello"},
		{Direction: "out", ExternalMessageID: "m2", Text: "Hi Andre", Usage: json.RawMessage(`{"input":10,"output":3}`)},
	} {
		m.ConversationID, m.CreatedAt = conv.ID, t0.Add(time.Duration(i)*time.Second)
		if ok, err := store.InsertConversationMessage(ctx, pool, &m); err != nil || !ok {
			t.Fatalf("insert: %v %v", ok, err)
		}
	}
	dup := store.ConversationMessage{ConversationID: conv.ID, Direction: "in", ExternalMessageID: "m1", Text: "hello"}
	if ok, err := store.InsertConversationMessage(ctx, pool, &dup); err != nil || ok {
		t.Fatalf("a message id is stored once: %v %v", ok, err)
	}

	code, body := call(http.MethodGet, "/api/conversations", "")
	var list []store.Conversation
	if err := json.Unmarshal(body, &list); code != http.StatusOK || err != nil || len(list) == 0 {
		t.Fatalf("list: %d %s", code, body)
	}
	got := list[0]
	if got.ID != conv.ID || got.AgentName != a.Name || got.MessageCount != 2 || got.LastMessageAt == nil ||
		!got.LastMessageAt.Equal(t0.Add(time.Second)) || got.ExternalChatID != "42" || got.DisplayName == "" {
		t.Fatalf("conversation = %+v", got)
	}

	code, body = call(http.MethodGet, "/api/conversations/"+conv.ID+"/messages", "")
	var ms []map[string]any
	if err := json.Unmarshal(body, &ms); code != http.StatusOK || err != nil || len(ms) != 2 {
		t.Fatalf("messages: %d %s", code, body)
	}
	if ms[0]["direction"] != "in" || ms[0]["text"] != "hello" || ms[1]["direction"] != "out" || ms[1]["run_id"] != nil {
		t.Fatalf("messages = %s", body)
	}
	if code, _ := call(http.MethodGet, "/api/conversations/"+store.NewID()+"/messages", ""); code != http.StatusNotFound {
		t.Fatalf("unknown conversation: %d", code)
	}
	if code, body := call(http.MethodGet, "/api/conversations?limit=0", ""); code != http.StatusUnprocessableEntity || detailPaths(t, body)[0] != "limit" {
		t.Fatalf("bad limit: %d %s", code, body)
	}
}

// Deleting the bound agent (agents.go calls unbindAgent after the delete) closes the
// channel at once.
func TestDeletingBoundAgentClosesTelegram(t *testing.T) {
	gw := newChannelGateway()
	_, call := channelServer(t, gw)
	a := channelAgent(t, "Coder")
	if code, body := call(http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+a.ID+`"}`); code != http.StatusOK {
		t.Fatalf("bind: %d %s", code, body)
	}
	s, _ := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: needDB(t), Gateway: gw}, nil)
	other := channelAgent(t, "Other")
	s.unbindAgent(context.Background(), other.ID) // not bound: nothing happens
	if b, _ := gw.telegram(); len(b) != 1 || gw.restarts != 1 {
		t.Fatalf("an unbound agent's delete touched the gateway: %v", b)
	}
	if err := store.DeleteAgent(context.Background(), needDB(t), a.ID); err != nil {
		t.Fatal(err)
	}
	s.unbindAgent(context.Background(), a.ID)
	bindings, dm := gw.telegram()
	if len(bindings) != 0 || dm != "disabled" {
		t.Fatalf("after deleting the bound agent: bindings %v, dmPolicy %v", bindings, dm)
	}
	if code, body := call(http.MethodGet, "/api/channels", ""); code != http.StatusOK || decodeChannels(t, body).Telegram.Binding != nil {
		t.Fatalf("channels: %d %s", code, body)
	}
}

// OpenAPI conformance for the channel and conversation paths, and for an approval
// raised in a conversation as the inbox lists it.
func TestOpenAPIConformanceChannels(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	router := contract(t)
	clear := func() { _, _ = pool.Exec(ctx, `DELETE FROM channel_bindings`) }
	clear()
	t.Cleanup(clear)
	gw := newChannelGateway()
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	cfg.TelegramAllowedUserIDs = []int64{42}
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: gw}, &fakeEngine{pool: pool})
	coder, reviewer := channelAgent(t, "Coder"), channelAgent(t, "Reviewer")

	key := "agent:t-" + store.NewID()[24:] + ":telegram:direct:42"
	conv, err := store.UpsertConversation(ctx, pool, &store.Conversation{Channel: "telegram", ExternalChatID: "42",
		AgentID: coder.ID, SessionKey: key, DisplayName: "Andre"})
	mustDo(t, err)
	for _, m := range []store.ConversationMessage{
		{Direction: "in", ExternalMessageID: "m1", Text: "hello"},
		{Direction: "out", ExternalMessageID: "m2", Text: "Hi", Usage: json.RawMessage(`{"input":10,"output":3}`)},
	} {
		m.ConversationID = conv.ID
		_, err := store.InsertConversationMessage(ctx, pool, &m)
		mustDo(t, err)
	}
	ext := "exec-" + store.NewID()
	appr := &store.Approval{Kind: store.ApprovalKindExec, Message: "sudo ls", ExternalID: &ext, ExpiresAt: time.Now().Add(time.Hour)}
	mustDo(t, store.InsertConversationApproval(ctx, pool, appr, conv.ID))

	steps := []conformanceStep{
		{http.MethodGet, "/api/channels", "", http.StatusOK},
		{http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"` + coder.ID + `"}`, http.StatusOK},
		{http.MethodGet, "/api/channels", "", http.StatusOK},
		{http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"` + reviewer.ID + `"}`, http.StatusConflict},
		{http.MethodPut, "/api/channels/telegram/binding", `{}`, http.StatusUnprocessableEntity},
		{http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"` + store.NewID() + `"}`, http.StatusNotFound},
		{http.MethodGet, "/api/conversations", "", http.StatusOK},
		{http.MethodGet, "/api/conversations?limit=5", "", http.StatusOK},
		{http.MethodGet, "/api/conversations?limit=0", "", http.StatusUnprocessableEntity},
		{http.MethodGet, "/api/conversations/" + conv.ID + "/messages", "", http.StatusOK},
		{http.MethodGet, "/api/conversations/" + conv.ID + "/messages?limit=1", "", http.StatusOK},
		{http.MethodGet, "/api/conversations/no-such-conversation/messages", "", http.StatusNotFound},
		{http.MethodGet, "/api/approvals?status=pending&limit=500", "", http.StatusOK},
		{http.MethodDelete, "/api/channels/telegram/binding", "", http.StatusOK},
	}
	for _, st := range steps {
		code, body := checkConformance(t, router, ts.URL, st.method, st.path, st.body, st.want < 300)
		if code != st.want {
			t.Fatalf("%s %s: status %d, want %d: %s", st.method, st.path, code, st.want, body)
		}
		if st.path == "/api/approvals?status=pending&limit=500" && !strings.Contains(string(body), `"conversation_id":"`+conv.ID+`"`) {
			t.Fatalf("the conversation's approval is not listed with its conversation: %s", body)
		}
	}
	gw.mu.Lock()
	gw.patchErr = &openclaw.RPCError{Code: "UNAVAILABLE", Message: "gateway starting; retry shortly", Retryable: true}
	gw.mu.Unlock()
	if code, body := checkConformance(t, router, ts.URL, http.MethodPut, "/api/channels/telegram/binding",
		`{"agent_id":"`+reviewer.ID+`"}`, true); code != http.StatusBadGateway {
		t.Fatalf("gateway refusal: %d %s", code, body)
	}
}

// /api/status carries the scheduler's tick and the earliest due schedule.
func TestStatusScheduler(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.SchedulerTick = 7 * time.Second
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool}, nil)
	wf := &store.Workflow{Name: "status", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, store.CreateWorkflow(ctx, pool, wf))
	due := time.Date(2098, 1, 2, 3, 4, 5, 0, time.UTC)
	expr := "4 3 2 1 *"
	sc := &store.Schedule{WorkflowID: wf.ID, Kind: store.ScheduleCron, CronExpr: &expr, TZ: "UTC", Enabled: true, NextRunAt: &due}
	mustDo(t, store.CreateSchedule(ctx, pool, sc))
	t.Cleanup(func() { _ = store.DeleteSchedule(ctx, pool, sc.ID) })

	code, body := do(t, ts, http.MethodGet, "/api/status", "")
	var st struct {
		Scheduler *struct {
			TickSeconds float64    `json:"tick_seconds"`
			NextDueAt   *time.Time `json:"next_due_at"`
		} `json:"scheduler"`
	}
	if err := json.Unmarshal(body, &st); code != http.StatusOK || err != nil || st.Scheduler == nil {
		t.Fatalf("status: %d %s", code, body)
	}
	// Other packages' tests share the database, so only an upper bound is certain here;
	// store.NextScheduleDue's own test pins the value.
	if st.Scheduler.TickSeconds != 7 || st.Scheduler.NextDueAt == nil || st.Scheduler.NextDueAt.After(due) {
		t.Fatalf("scheduler = tick %v next %v, want tick 7 and a due time no later than %s", st.Scheduler.TickSeconds, st.Scheduler.NextDueAt, due)
	}
}

// slowStatusGateway never answers the bot probe, like a gateway mid-restart or a
// Telegram API that does not respond.
type slowStatusGateway struct{ *channelGateway }

func (g slowStatusGateway) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method == "channels.status" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return g.channelGateway.Call(ctx, method, params)
}

// GET /api/channels reads the cached bot username and never waits for the gateway's
// getMe probe (audit #27): "" until it is known.
func TestChannelsNeverWaitOnBotName(t *testing.T) {
	pool := needDB(t)
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	s, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: slowStatusGateway{newChannelGateway()}}, &fakeEngine{pool: pool})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.learnBotUsername(ctx) // a lookup in flight, as at api start
	for i := range 3 {
		start := time.Now()
		code, body := do(t, ts, http.MethodGet, "/api/channels", "")
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("request %d took %v", i, elapsed)
		}
		if v := decodeChannels(t, body); code != http.StatusOK || v.Telegram.BotUsername != "" || !v.Telegram.Enabled {
			t.Fatalf("GET /api/channels: %d %s", code, body)
		}
	}
}

// Binding learns the bot username in the background when the api did not know it yet.
func TestBindLearnsBotName(t *testing.T) {
	pool := needDB(t)
	unbindAll := func() { _, _ = pool.Exec(context.Background(), `DELETE FROM channel_bindings`) }
	unbindAll()
	t.Cleanup(unbindAll)
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	s, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: newChannelGateway()}, &fakeEngine{pool: pool})
	a := channelAgent(t, "Namer")
	if code, body := do(t, ts, http.MethodPut, "/api/channels/telegram/binding", `{"agent_id":"`+a.ID+`"}`); code != http.StatusOK {
		t.Fatalf("bind: %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.botUsername() != "yuno_test_bot" {
		if time.Now().After(deadline) {
			t.Fatal("the bot username was not learnt after the bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, body := do(t, ts, http.MethodGet, "/api/channels", "")
	if v := decodeChannels(t, body); v.Telegram.BotUsername != "yuno_test_bot" {
		t.Fatalf("GET /api/channels after the bind: %s", body)
	}
}

// DESIGN 5.15 rule 14: the seed binds through the Channels page's own bind code:
// materialised first, stored, applied with a restart; refused while another agent is
// bound, which it never changes.
func TestSharedBindTelegram(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	clearBinding := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM channel_bindings`); err != nil {
			t.Fatal(err)
		}
	}
	clearBinding()
	t.Cleanup(clearBinding)
	gw := newChannelGateway()
	eng := &fakeEngine{pool: pool}
	cfg := testConfig()
	cfg.TelegramMode = "polling"
	sh := NewShared(Deps{Cfg: cfg, Pool: pool, Engine: eng, Gateway: gw})
	chief, other := channelAgent(t, "Chief of Staff"), channelAgent(t, "Coder")

	if err := sh.BindTelegram(ctx, chief.ID); err != nil {
		t.Fatal(err)
	}
	if len(eng.materialized) != 1 || eng.materialized[0] != chief.ID {
		t.Fatalf("materialised = %v", eng.materialized)
	}
	bindings, dm := gw.telegram()
	if len(bindings) != 1 || bindings[0].(map[string]any)["agentId"] != "t-"+chief.ID || dm != "allowlist" || gw.restarts != 1 {
		t.Fatalf("gateway: bindings %v, dmPolicy %v, restarts %d", bindings, dm, gw.restarts)
	}
	err := sh.BindTelegram(ctx, other.ID)
	var bound *boundError
	if !errors.As(err, &bound) || bound.cur.AgentID != chief.ID {
		t.Fatalf("second agent: %v", err)
	}
	if b, err := store.GetChannelBinding(ctx, pool, store.ChannelTelegram); err != nil || b.AgentID != chief.ID || gw.restarts != 1 {
		t.Fatalf("binding changed: %+v %v, restarts %d", b, err, gw.restarts)
	}
}
