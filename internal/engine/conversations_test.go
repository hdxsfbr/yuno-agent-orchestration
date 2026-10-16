package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"yuno/internal/events"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

const convKey = "agent:agconv:telegram:direct:100000001"

// convEnv is an engine whose agent "agconv" is bound to Telegram, with the mirror
// running and an SSE subscriber on every event.
type convEnv struct {
	*testEnv
	agent  *store.Agent
	sse    <-chan events.Event
	cancel context.CancelFunc
	done   chan struct{}
}

func newConvEnv(t *testing.T, bind bool, rows ...openclaw.SessionRow) *convEnv {
	t.Helper()
	c := newConvEnvOn(t, newEnv(t), bind, rows...)
	eventually(t, "roster listed", func() bool { return len(c.fc.callsFor("sessions.list")) > 0 })
	return c
}

func (c *convEnv) messages(convID string) []store.ConversationMessage {
	c.t.Helper()
	ms, err := store.ListConversationMessages(context.Background(), c.pool, convID, 100, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	return ms
}

func (c *convEnv) conversation() *store.Conversation {
	c.t.Helper()
	var id string
	if err := c.pool.QueryRow(context.Background(), `SELECT id FROM conversations WHERE session_key = $1`, convKey).Scan(&id); err != nil {
		return nil
	}
	conv, err := store.GetConversation(context.Background(), c.pool, id)
	if err != nil {
		c.t.Fatal(err)
	}
	return conv
}

func (c *convEnv) sseTypes(want string, n int) []events.Event {
	c.t.Helper()
	var got []events.Event
	deadline := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case e := <-c.sse:
			if e.Type == want {
				got = append(got, e)
			}
		case <-deadline:
			c.t.Fatalf("got %d %s events, want %d", len(got), want, n)
		}
	}
	return got
}

func changed(key, agent string) map[string]any {
	return map[string]any{"sessionKey": key, "agentId": agent, "phase": "message", "channel": "telegram",
		"displayName": "Test Operator (@test_operator) id:100000001",
		"origin":      map[string]any{"provider": "telegram", "from": "telegram:100000001", "to": "telegram:100000001", "chatType": "direct"}}
}

func userEntry(id, text string, ts int64) map[string]any {
	return map[string]any{"role": "user", "content": text, "timestamp": ts, "__openclaw": map[string]any{"id": id, "seq": 1}}
}

func assistantEntry(id string, ts int64, parts ...map[string]any) map[string]any {
	return map[string]any{"role": "assistant", "content": parts, "timestamp": ts, "provider": "yuno-anthropic",
		"model": "claude-sonnet-5-5", "usage": map[string]any{"input": 120, "output": 30, "totalTokens": 150, "cost": map[string]any{"total": 0.0012}},
		"__openclaw": map[string]any{"id": id, "seq": 2}}
}

// T5.8.4: a Telegram session of the bound agent appears on the roster; the engine
// subscribes to it with that agent's id and mirrors what the person and the agent said,
// once, without reasoning, tool calls or tool results; each new message is an SSE event.
func TestConversationMirror(t *testing.T) {
	c := newConvEnv(t, true)
	c.fc.push(openclaw.EventSessionsChanged, changed(convKey, "agconv"))
	eventually(t, "subscribed to the conversation", func() bool { return len(c.fc.callsFor("sessions.messages.subscribe")) == 1 })
	var sub struct {
		Key              string `json:"key"`
		AgentID          string `json:"agentId"`
		IncludeApprovals bool   `json:"includeApprovals"`
	}
	_ = json.Unmarshal(c.fc.callsFor("sessions.messages.subscribe")[0], &sub)
	if sub.Key != convKey || sub.AgentID != "agconv" || !sub.IncludeApprovals {
		t.Fatalf("subscribe params = %+v", sub)
	}
	conv := c.conversation()
	if conv == nil || conv.ExternalChatID != "100000001" || conv.AgentID != c.agent.ID || conv.AgentName != "Coder" ||
		conv.Channel != "telegram" || conv.DisplayName != "Test Operator (@test_operator) id:100000001" {
		t.Fatalf("conversation = %+v", conv)
	}

	t0 := time.Now().Add(-time.Minute).UnixMilli() // inside the conversation TTL
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m1", "message": userEntry("m1", "hello", t0)})
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m2",
		"message": assistantEntry("m2", t0+1000, map[string]any{"type": "thinking", "thinking": "private reasoning"},
			map[string]any{"type": "toolCall", "id": "tc1", "name": "exec", "arguments": map[string]any{"command": "ls"}})})
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m3",
		"message": map[string]any{"role": "toolResult", "toolCallId": "tc1", "toolName": "exec", "content": []any{map[string]any{"type": "text", "text": "AGENTS.md"}}}})
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m4",
		"message": assistantEntry("m4", t0+6000, map[string]any{"type": "thinking", "thinking": "more reasoning"}, map[string]any{"type": "text", "text": "Hi Andre"})})
	// The same message again (a replay) changes nothing.
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m1", "message": userEntry("m1", "hello", t0)})

	got := c.sseTypes(EventConversationMessage, 2)
	eventually(t, "two messages stored", func() bool { return len(c.messages(conv.ID)) == 2 })
	ms := c.messages(conv.ID)
	if ms[0].Direction != store.DirectionIn || ms[0].Text != "hello" || ms[0].ExternalMessageID != "m1" || !ms[0].CreatedAt.Equal(time.UnixMilli(t0)) {
		t.Fatalf("in message = %+v", ms[0])
	}
	if ms[1].Direction != store.DirectionOut || ms[1].Text != "Hi Andre" || ms[1].RunID != nil {
		t.Fatalf("out message = %+v", ms[1])
	}
	var usage openclaw.Usage
	if err := json.Unmarshal(ms[1].Usage, &usage); err != nil || usage.Input != 120 || usage.Output != 30 {
		t.Fatalf("usage = %s", ms[1].Usage)
	}
	var payload struct {
		Conversation store.Conversation        `json:"conversation"`
		Message      store.ConversationMessage `json:"message"`
	}
	if err := json.Unmarshal(got[1].Data, &payload); err != nil || payload.Message.Text != "Hi Andre" || payload.Conversation.ID != conv.ID ||
		payload.Conversation.MessageCount != 2 || got[1].RunID != "" {
		t.Fatalf("SSE conversation.message = %s (%v)", got[1].Data, err)
	}
	var stored string
	_ = c.pool.QueryRow(context.Background(), `SELECT string_agg(text, ' ') FROM conversation_messages`).Scan(&stored)
	if strings.Contains(stored, "reasoning") || strings.Contains(stored, "AGENTS.md") {
		t.Fatalf("reasoning or tool output stored: %q", stored)
	}
	if n := c.count(`SELECT count(*) FROM run_events WHERE event = 'conversation.message' AND run_id IS NULL`); n != 2 {
		t.Fatalf("durable conversation.message events = %d, want 2", n)
	}
	if n := c.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("a conversation turn must not create runs: %d", n)
	}
}

// Messages sent while the api was away arrive through the roster at start and the
// session's history; a reconnect re-subscribes without duplicating anything.
func TestConversationMirrorCatchUp(t *testing.T) {
	t0 := time.Now().Add(-time.Hour).UnixMilli()
	row := openclaw.SessionRow{Key: convKey, AgentID: "agconv", Channel: "telegram", DisplayName: "Andre"}
	env := newEnv(t) // history must exist before the mirror starts
	env.fc.addHistory(convKey, userEntry("h1", "are you there?", t0),
		assistantEntry("h2", t0+2000, map[string]any{"type": "text", "text": "Yes."}))
	c := newConvEnvOn(t, env, true, row)
	eventually(t, "history mirrored", func() bool {
		conv := c.conversation()
		return conv != nil && len(c.messages(conv.ID)) == 2
	})
	c.fc.push(openclaw.EventReconnected, map[string]any{})
	eventually(t, "re-subscribed after the reconnect", func() bool { return len(c.fc.callsFor("sessions.messages.subscribe")) == 2 })
	eventually(t, "history read again", func() bool { return len(c.fc.callsFor("chat.history")) == 2 })
	if ms := c.messages(c.conversation().ID); len(ms) != 2 {
		t.Fatalf("messages after the reconnect = %d, want 2", len(ms))
	}
}

// DESIGN 5.10 rule 6: replaying history after a restart does not bring back messages
// the conversation's TTL has expired (the sweep may already have deleted them); the
// agent's own conversation_ttl_days applies, 7 days by default.
func TestConversationReplaySkipsExpired(t *testing.T) {
	for _, tc := range []struct {
		name string
		ttl  int
		kept []string
	}{
		{"default seven days", 0, []string{"recent", "six days"}},
		{"agent's two days", 2, []string{"recent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
			env := newEnv(t)
			env.fc.addHistory(convKey,
				userEntry("old", "eight days", ago(8*24*time.Hour)),
				userEntry("mid", "six days", ago(6*24*time.Hour)),
				assistantEntry("new", ago(time.Hour), map[string]any{"type": "text", "text": "recent"}))
			c := newConvEnvOnAgent(t, env, func(a *store.Agent) { a.MemoryPolicy.ConversationTTLDays = tc.ttl },
				openclaw.SessionRow{Key: convKey, AgentID: "agconv", Channel: "telegram"})
			eventually(t, "history read", func() bool { return len(c.fc.callsFor("chat.history")) == 1 })
			eventually(t, "kept messages mirrored", func() bool {
				conv := c.conversation()
				return conv != nil && len(c.messages(conv.ID)) == len(tc.kept)
			})
			var texts []string
			rows, err := c.pool.Query(context.Background(), `SELECT text FROM conversation_messages ORDER BY created_at DESC`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var text string
				_ = rows.Scan(&text)
				texts = append(texts, text)
			}
			rows.Close()
			if strings.Join(texts, ",") != strings.Join(tc.kept, ",") {
				t.Fatalf("stored %v, want %v", texts, tc.kept)
			}
		})
	}
}

// A chat.history that fails during the first catch-up leaves the session unsynced: the
// retry tick tries again with backoff, and so does the session's next roster event.
func TestConversationCatchUpRetries(t *testing.T) {
	row := openclaw.SessionRow{Key: convKey, AgentID: "agconv", Channel: "telegram", DisplayName: "Andre"}
	for _, tc := range []struct {
		name  string
		tick  time.Duration
		nudge bool // a roster event for the session after the gateway recovers
	}{
		{"the retry tick", 20 * time.Millisecond, false},
		{"a roster event", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			env.e.convRetry = tc.tick
			env.fc.addHistory(convKey, userEntry("h1", "are you there?", time.Now().Add(-time.Hour).UnixMilli()))
			env.fc.mu.Lock()
			env.fc.fail["chat.history"] = errors.New("fake: chat.history unavailable")
			env.fc.mu.Unlock()
			c := newConvEnvOn(t, env, true, row)
			eventually(t, "first catch-up tried", func() bool { return len(c.fc.callsFor("chat.history")) >= 1 })
			env.fc.mu.Lock()
			delete(env.fc.fail, "chat.history")
			env.fc.mu.Unlock()
			if tc.nudge {
				c.fc.push(openclaw.EventSessionsChanged, changed(convKey, "agconv"))
			}
			eventually(t, "the message mirrored after the retry", func() bool {
				conv := c.conversation()
				return conv != nil && len(c.messages(conv.ID)) == 1
			})
		})
	}
}

// newConvEnvOn starts the mirror on env (whose gateway history a test may prepare
// first) with agent "agconv" materialised and, when bind, bound to Telegram.
func newConvEnvOn(t *testing.T, env *testEnv, bind bool, rows ...openclaw.SessionRow) *convEnv {
	t.Helper()
	return startConvMirror(t, env, bind, nil, rows...)
}

// newConvEnvOnAgent is newConvEnvOn with the bound agent changed by mut first.
func newConvEnvOnAgent(t *testing.T, env *testEnv, mut func(a *store.Agent), rows ...openclaw.SessionRow) *convEnv {
	t.Helper()
	return startConvMirror(t, env, true, mut, rows...)
}

func startConvMirror(t *testing.T, env *testEnv, bind bool, mut func(a *store.Agent), rows ...openclaw.SessionRow) *convEnv {
	t.Helper()
	ctx := context.Background()
	a := env.agent(func(a *store.Agent) {
		a.Name = "Coder"
		if mut != nil {
			mut(a)
		}
	})
	if err := store.SetAgentMaterialized(ctx, env.pool, a.ID, "agconv", "h"); err != nil {
		t.Fatal(err)
	}
	if bind {
		if _, err := store.BindChannel(ctx, env.pool, store.ChannelTelegram, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	env.fc.mu.Lock()
	env.fc.sessions = rows
	env.fc.mu.Unlock()
	sse, unsub := env.e.deps.Hub.Subscribe("", 64)
	t.Cleanup(unsub)
	runCtx, cancel := context.WithCancel(ctx)
	c := &convEnv{testEnv: env, agent: a, sse: sse, cancel: cancel, done: make(chan struct{})}
	go func() { env.e.mirrorConversations(runCtx); close(c.done) }()
	t.Cleanup(func() { cancel(); <-c.done })
	return c
}

// D-025: Telegram sessions of an agent the platform did not bind (the gateway's own
// main, a previously bound agent, any agent while nothing is bound) are not followed.
func TestConversationMirrorIgnoresUnboundAgents(t *testing.T) {
	tests := []struct {
		name string
		bind bool
		key  string
		agt  string
	}{
		{name: "the default agent's main session", bind: true, key: "agent:main:main", agt: "main"},
		{name: "another agent's telegram session", bind: true, key: "agent:agother:telegram:direct:100000001", agt: "agother"},
		{name: "nothing bound", bind: false, key: convKey, agt: "agconv"},
		{name: "a node session of the bound agent", bind: true, key: "agent:agconv:yuno-run-1-coder-1", agt: "agconv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newConvEnv(t, tc.bind)
			c.fc.push(openclaw.EventSessionsChanged, changed(tc.key, tc.agt))
			c.fc.push("session.message", map[string]any{"sessionKey": tc.key, "messageId": "x1", "message": userEntry("x1", "hi", time.Now().UnixMilli())})
			time.Sleep(100 * time.Millisecond)
			if n := len(c.fc.callsFor("sessions.messages.subscribe")); n != 0 {
				t.Fatalf("subscribed %d times", n)
			}
			if n := c.count(`SELECT count(*) FROM conversations`); n != 0 {
				t.Fatalf("%d conversations", n)
			}
		})
	}
}

// DESIGN 5.8 rule 5: an exec approval raised by a conversation turn is an approval of
// the conversation (run_id NULL), mirrored through its lifecycle like any exec approval.
func TestConversationExecApproval(t *testing.T) {
	c := newConvEnv(t, true)
	c.fc.push(openclaw.EventSessionsChanged, changed(convKey, "agconv"))
	eventually(t, "subscribed", func() bool { return len(c.fc.callsFor("sessions.messages.subscribe")) == 1 })
	now := time.Now().UnixMilli()
	pending := map[string]any{"sessionKey": convKey, "phase": "pending", "approval": map[string]any{
		"id": "exec-7", "status": "pending", "createdAtMs": now, "expiresAtMs": now + 30*60*1000,
		"presentation": map[string]any{"kind": "exec", "commandText": "sudo systemctl restart nginx", "agentId": "agconv"}}}
	c.fc.push("session.approval", pending)
	c.fc.push("session.approval", pending) // the replay on re-subscribe
	req := c.sseTypes("approval.requested", 1)
	var row struct {
		RunID, RunNodeID, ConvID *string
		Kind, Status, Message    string
	}
	read := func() {
		t.Helper()
		if err := c.pool.QueryRow(context.Background(), `SELECT run_id, run_node_id, conversation_id, kind, status, message
			FROM approvals WHERE external_id = 'exec-7'`).Scan(&row.RunID, &row.RunNodeID, &row.ConvID, &row.Kind, &row.Status, &row.Message); err != nil {
			t.Fatal(err)
		}
	}
	read()
	conv := c.conversation()
	if row.RunID != nil || row.RunNodeID != nil || row.ConvID == nil || *row.ConvID != conv.ID || row.Kind != "exec" ||
		row.Status != "pending" || row.Message != "sudo systemctl restart nginx" {
		t.Fatalf("approval row = %+v", row)
	}
	if n := c.count(`SELECT count(*) FROM approvals`); n != 1 {
		t.Fatalf("approvals = %d, want 1", n)
	}
	if req[0].RunID != "" || !strings.Contains(string(req[0].Data), conv.ID) {
		t.Fatalf("approval.requested = %s", req[0].Data)
	}

	// Answered in the chat: the terminal event decides the row.
	terminal := map[string]any{"sessionKey": convKey, "phase": "terminal", "approval": map[string]any{
		"id": "exec-7", "status": "allowed", "decision": "allow-once", "resolvedAtMs": now + 5000,
		"presentation": map[string]any{"kind": "exec", "commandText": "sudo systemctl restart nginx"}}}
	c.fc.push("session.approval", terminal)
	dec := c.sseTypes("approval.decided", 1)
	read()
	if row.Status != store.ApprovalApproved {
		t.Fatalf("status after the chat decision = %s", row.Status)
	}
	var decided struct {
		Approval store.Approval `json:"approval"`
	}
	if err := json.Unmarshal(dec[0].Data, &decided); err != nil || decided.Approval.DecidedBy == nil || *decided.Approval.DecidedBy != "openclaw" ||
		decided.Approval.RunID != "" {
		t.Fatalf("approval.decided = %s", dec[0].Data)
	}
	// A second terminal event (the platform decided first, or a replay) changes nothing.
	c.fc.push("session.approval", terminal)
	time.Sleep(50 * time.Millisecond)
	if n := c.count(`SELECT count(*) FROM run_events WHERE event = 'approval.decided'`); n != 1 {
		t.Fatalf("approval.decided events = %d, want 1", n)
	}
}
