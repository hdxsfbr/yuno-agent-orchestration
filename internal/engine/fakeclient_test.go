package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/runtoken"
)

// fakeClient is an in-memory openclaw.Client. Agents, config and exec approvals behave
// like the gateway enough for materialisation; a turn is driven by onSend, which the
// test sets to push the events the gateway would.
type fakeClient struct {
	mu        sync.Mutex
	calls     []fakeCall
	subs      map[chan openclaw.Event]struct{}
	connected bool
	agents    []string
	sends     int
	history   map[string][]json.RawMessage // session key → transcript entries pushed

	// onSend answers chat.send; n counts calls from 1. The default starts the turn.
	onSend func(p openclaw.ChatSendParams, n int) (any, error)
	// abortEmits makes chat.abort push the "aborted" chat event for the run.
	abortEmits bool
	// block makes these methods wait for their context, like a gateway that keeps the
	// connection open but never answers.
	block map[string]bool
	// fail answers these methods with the given error.
	fail map[string]error

	// Plugin approvals the gateway holds (DESIGN 5.3 rule 5a): id → decision, "" while
	// pending. A resolve broadcasts plugin.approval.resolved like the gateway does.
	plugin   map[string]string
	pluginN  int
	resolver string // resolvedBy of the events this client's own resolves produce
	// onExecResolve runs after exec.approval.resolve, e.g. to push the terminal
	// session.approval event the gateway sends.
	onExecResolve func(id, decision string)
	// sessions is what sessions.list answers (the conversation mirror's roster).
	sessions []openclaw.SessionRow
}

type fakeCall struct {
	Method string
	Params json.RawMessage
}

func newFakeClient() *fakeClient {
	return &fakeClient{subs: map[chan openclaw.Event]struct{}{}, connected: true, abortEmits: true, history: map[string][]json.RawMessage{},
		block: map[string]bool{}, fail: map[string]error{}, plugin: map[string]string{}, resolver: "operator:yuno-api"}
}

func (f *fakeClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Params: b})
	up, blocked, failWith := f.connected, f.block[method], f.fail[method]
	f.mu.Unlock()
	if !up {
		return nil, fmt.Errorf("fake: %s: %w", method, openclaw.ErrDisconnected)
	}
	if blocked {
		<-ctx.Done()
		return nil, fmt.Errorf("fake: %s: %w", method, ctx.Err())
	}
	if failWith != nil {
		return nil, failWith
	}
	out, err := f.handle(method, b)
	if err != nil {
		return nil, err
	}
	r, _ := json.Marshal(out)
	return r, nil
}

func (f *fakeClient) handle(method string, p json.RawMessage) (any, error) {
	switch method {
	case "agents.list":
		f.mu.Lock()
		defer f.mu.Unlock()
		list := []map[string]any{{"id": "main"}}
		for _, a := range f.agents {
			list = append(list, map[string]any{"id": a})
		}
		return map[string]any{"defaultId": "main", "agents": list}, nil
	case "agents.create":
		var in struct{ Name, Workspace string }
		_ = json.Unmarshal(p, &in)
		f.mu.Lock()
		f.agents = append(f.agents, in.Name)
		f.mu.Unlock()
		return map[string]any{"ok": true, "agentId": in.Name, "name": in.Name, "workspace": in.Workspace}, nil
	case "config.get":
		return map[string]any{"hash": "h1", "config": map[string]any{"agents": map[string]any{"entries": map[string]any{}}}}, nil
	case "config.patch":
		return map[string]any{"ok": true, "hash": "h2"}, nil
	case "exec.approvals.get":
		return map[string]any{"exists": true, "hash": "e1", "file": map[string]any{"version": 1, "agents": map[string]any{}}}, nil
	case "exec.approvals.set", "sessions.patch", "sessions.delete":
		return map[string]any{"ok": true}, nil
	case "sessions.messages.subscribe", "sessions.subscribe":
		return map[string]any{"subscribed": true}, nil
	case "sessions.list":
		f.mu.Lock()
		defer f.mu.Unlock()
		return map[string]any{"sessions": slices.Clone(f.sessions)}, nil
	case "chat.send":
		var in openclaw.ChatSendParams
		_ = json.Unmarshal(p, &in)
		f.mu.Lock()
		f.sends++
		n, h := f.sends, f.onSend
		f.mu.Unlock()
		if h != nil {
			return h(in, n)
		}
		return openclaw.ChatSendResult{RunID: in.IdempotencyKey, Status: "started"}, nil
	case "chat.abort":
		var in struct{ SessionKey, RunID string }
		_ = json.Unmarshal(p, &in)
		if f.abortEmits {
			go f.push("chat", map[string]any{"runId": in.RunID, "sessionKey": in.SessionKey, "state": "aborted", "stopReason": "rpc"})
		}
		return openclaw.ChatAbortResult{OK: true, Aborted: true, RunIDs: []string{in.RunID}}, nil
	case "plugin.approval.request":
		var in openclaw.PluginApprovalRequestParams
		_ = json.Unmarshal(p, &in)
		if !in.TwoPhase {
			return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "fake: the engine must request two-phase approvals"}
		}
		f.mu.Lock()
		f.pluginN++
		id := fmt.Sprintf("plugin:%04d", f.pluginN)
		f.plugin[id] = ""
		f.mu.Unlock()
		now := time.Now().UnixMilli()
		return openclaw.PluginApprovalRequestResult{Status: "accepted", ID: id, CreatedAtMs: now, ExpiresAtMs: now + in.TimeoutMs}, nil
	case "plugin.approval.resolve":
		var in struct{ ID, Decision string }
		_ = json.Unmarshal(p, &in)
		f.mu.Lock()
		cur, known := f.plugin[in.ID]
		if known && cur == "" {
			f.plugin[in.ID] = in.Decision
		}
		resolver := f.resolver
		f.mu.Unlock()
		switch {
		case !known:
			return nil, &openclaw.RPCError{Code: openclaw.CodeApprovalNotFound, Message: "unknown or expired approval id"}
		case cur != "":
			return nil, &openclaw.RPCError{Code: openclaw.CodeApprovalAlreadyResolved, Message: "approval already resolved"}
		}
		go f.push(openclaw.EventPluginApprovalResolved, map[string]any{"id": in.ID, "decision": in.Decision, "resolvedBy": resolver, "ts": time.Now().UnixMilli()})
		return map[string]any{"ok": true}, nil
	case "exec.approval.resolve":
		var in openclaw.ExecApprovalResolveParams
		_ = json.Unmarshal(p, &in)
		f.mu.Lock()
		hook := f.onExecResolve
		f.mu.Unlock()
		if hook != nil {
			go hook(in.ID, in.Decision)
		}
		return map[string]any{"ok": true}, nil
	case "chat.history":
		var in struct{ SessionKey string }
		_ = json.Unmarshal(p, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		return map[string]any{"sessionKey": in.SessionKey, "messages": slices.Clone(f.history[in.SessionKey])}, nil
	}
	return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "unknown method: " + method}
}

func (f *fakeClient) Subscribe(buffer int) (<-chan openclaw.Event, func()) {
	ch := make(chan openclaw.Event, buffer)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			close(ch)
			f.mu.Unlock()
		})
	}
}

func (f *fakeClient) Info() (openclaw.ConnectInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return openclaw.ConnectInfo{Version: "fake"}, f.connected
}

func (f *fakeClient) setConnected(up bool) {
	f.mu.Lock()
	f.connected = up
	f.mu.Unlock()
}

// push delivers an event to every subscriber; session.message payloads are also kept
// as the session's chat.history.
func (f *fakeClient) push(name string, payload any) {
	b, _ := json.Marshal(payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "session.message" {
		var sm struct {
			SessionKey string          `json:"sessionKey"`
			Message    json.RawMessage `json:"message"`
		}
		if json.Unmarshal(b, &sm) == nil {
			f.history[sm.SessionKey] = append(f.history[sm.SessionKey], sm.Message)
		}
	}
	for ch := range f.subs {
		ch <- openclaw.Event{Name: name, Payload: b, At: time.Now()}
	}
}

func (f *fakeClient) callsFor(method string) []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []json.RawMessage
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

// addHistory appends transcript entries to a session's chat.history without pushing
// them, as entries recorded while the client was not receiving events.
func (f *fakeClient) addHistory(key string, entries ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		f.history[key] = append(f.history[key], mustJSON(e))
	}
}

// methods returns the methods called, in order.
func (f *fakeClient) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Method
	}
	return out
}

func (f *fakeClient) sendParams() []openclaw.ChatSendParams {
	var out []openclaw.ChatSendParams
	for _, raw := range f.callsFor("chat.send") {
		var p openclaw.ChatSendParams
		_ = json.Unmarshal(raw, &p)
		out = append(out, p)
	}
	return out
}

// ---- event builders -----------------------------------------------------------------

var msgSeq struct {
	sync.Mutex
	n int
}

func nextMsgID() string {
	msgSeq.Lock()
	defer msgSeq.Unlock()
	msgSeq.n++
	return fmt.Sprintf("msg-%d", msgSeq.n)
}

// assistantMsg is a session.message for an assistant entry with usage.
func assistantMsg(key, runID string, parts []any, stop string, in, out int64, cost float64) map[string]any {
	id := nextMsgID()
	now := time.Now().UnixMilli()
	return map[string]any{
		"sessionKey": key, "agentId": "x", "messageId": id,
		"message": map[string]any{
			"role": "assistant", "content": parts, "api": "openai-completions", "provider": "scripted", "model": "scripted-1",
			"usage": map[string]any{"input": in, "output": out, "totalTokens": in + out, "cacheRead": 0, "cacheWrite": 0,
				"cost": map[string]any{"total": cost}},
			"stopReason": stop, "timestamp": now, "responseId": "resp-" + id,
			"__openclaw": map[string]any{"runId": runID, "id": id, "recordTimestampMs": now + 5},
		},
	}
}

// toolResultEntry is a chat.history toolResult entry [probed].
func toolResultEntry(runID, toolCallID, toolName, text string, isError bool) map[string]any {
	id := nextMsgID()
	now := time.Now().UnixMilli()
	return map[string]any{"role": "toolResult", "toolCallId": toolCallID, "toolName": toolName, "isError": isError,
		"content": []any{textPart(text)}, "timestamp": now,
		"__openclaw": map[string]any{"runId": runID, "id": id, "recordTimestampMs": now}}
}

func toolCallPart(id, name string, args any) map[string]any {
	return map[string]any{"type": "toolCall", "id": id, "name": name, "arguments": args}
}

func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func chatEvent(key, runID, state string, extra map[string]any) map[string]any {
	ev := map[string]any{"runId": runID, "sessionKey": key, "agentId": "x", "state": state}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

func itemEvent(key, runID, toolCallID, name, phase, status, errText string) map[string]any {
	now := time.Now().UnixMilli()
	data := map[string]any{"itemId": "tool:" + toolCallID, "toolCallId": toolCallID, "name": name, "kind": "tool",
		"phase": phase, "status": status, "startedAt": now - 10}
	if phase == "end" {
		data["endedAt"] = now
	}
	if errText != "" {
		data["error"] = errText
	}
	return map[string]any{"runId": runID, "sessionKey": key, "agentId": "x", "stream": "item", "ts": now, "data": data}
}

// finishThenFinal pushes a finish tool call, accepts it through the engine, then ends
// the turn with a final reply.
func finishThenFinal(t *testing.T, env *testEnv, p openclaw.ChatSendParams, result string) {
	t.Helper()
	token := tokenIn(p.Message)
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey,
		[]any{toolCallPart("call_fin", "yuno__finish", map[string]any{"run_token": token, "result": json.RawMessage(result)})}, "toolUse", 100, 10, 0))
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_fin", "yuno__finish", "start", "running", ""))
	if err := env.e.AcceptFinish(context.Background(), token, json.RawMessage(result)); err != nil {
		t.Errorf("AcceptFinish: %v", err)
	}
	env.fc.push("agent", itemEvent(p.SessionKey, p.IdempotencyKey, "call_fin", "yuno__finish", "end", "completed", ""))
	env.fc.push("session.message", assistantMsg(p.SessionKey, p.IdempotencyKey, []any{textPart("Done.")}, "stop", 120, 5, 0))
	env.fc.push("chat", chatEvent(p.SessionKey, p.IdempotencyKey, "final", map[string]any{"stopReason": "stop",
		"message": map[string]any{"role": "assistant", "content": []any{textPart("Done.")}}}))
}

func tokenIn(message string) string { return runtoken.Find(message) }

// loadFixture reads testdata/<name>.jsonl and rewrites the probe's session key, run id
// and agent id to the ones of the visit under test.
func loadFixture(t *testing.T, name, key, runID, agentID string) []openclaw.Event {
	t.Helper()
	fh, err := os.Open("testdata/" + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	const probeKey, probeRun = "agent:main:main", "52b7dc6e-713e-4897-913b-acee649b028b"
	var out []openclaw.Event
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var raw struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		rewritten := rewrite(raw.Payload, "", map[string]string{probeKey: key, probeRun: runID}, agentID)
		b, _ := json.Marshal(rewritten)
		out = append(out, openclaw.Event{Name: raw.Event, Payload: b})
	}
	return out
}

func rewrite(v any, field string, exact map[string]string, agentID string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = rewrite(c, k, exact, agentID)
		}
		return t
	case []any:
		for i, c := range t {
			t[i] = rewrite(c, field, exact, agentID)
		}
		return t
	case string:
		if r, ok := exact[t]; ok {
			return r
		}
		if field == "agentId" && t == "main" {
			return agentID
		}
	}
	return v
}

// gatewayResolves decides a plugin approval on the gateway's side: another approval
// client (resolvedBy set) or the gateway's own timeout (resolvedBy nil).
func (f *fakeClient) gatewayResolves(id, decision string, resolvedBy any) {
	f.mu.Lock()
	if _, ok := f.plugin[id]; ok {
		f.plugin[id] = decision
	}
	f.mu.Unlock()
	f.push(openclaw.EventPluginApprovalResolved, map[string]any{"id": id, "decision": decision, "resolvedBy": resolvedBy, "ts": time.Now().UnixMilli()})
}

func (f *fakeClient) pluginDecision(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.plugin[id]
}
