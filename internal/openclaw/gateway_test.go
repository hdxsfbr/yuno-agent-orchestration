package openclaw

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testToken = "test-gateway-token"

func dialFake(t *testing.T, f *fakeGateway, cfg DialConfig) (*Gateway, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if cfg.URL == "" {
		cfg.URL = f.URL()
	}
	if cfg.Token == "" {
		cfg.Token = testToken
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = 20 * time.Millisecond
		cfg.BackoffMax = 100 * time.Millisecond
	}
	c, err := Dial(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c.(*Gateway), cancel
}

func TestHandshake(t *testing.T) {
	f := newFakeGateway(t, testToken)
	g, _ := dialFake(t, f, DialConfig{Version: "1.2.3"})
	info, up := g.Info()
	if !up {
		t.Fatal("not connected")
	}
	if info.Version != "2026.10.3" || info.ConnID == "" || info.Methods != 4 {
		t.Fatalf("info = %+v", info)
	}
	if strings.Join(info.Scopes, ",") != strings.Join(RequiredScopes, ",") {
		t.Fatalf("scopes = %v", info.Scopes)
	}
}

func TestHandshakeConnectFrame(t *testing.T) {
	f := newFakeGateway(t, testToken)
	dialFake(t, f, DialConfig{Version: "9.9.9"})
	f.mu.Lock()
	got := f.lastConnect
	f.mu.Unlock()
	if got.MinProtocol != 4 || got.MaxProtocol != 4 || got.Role != "operator" {
		t.Fatalf("protocol/role: %+v", got)
	}
	if got.Client != (connectClient{ID: "gateway-client", Version: "9.9.9", Platform: "linux", Mode: "backend"}) {
		t.Fatalf("client = %+v", got.Client)
	}
	if strings.Join(got.Caps, ",") != "approvals,exec-approvals" {
		t.Fatalf("caps = %v", got.Caps)
	}
	if strings.Join(got.Scopes, ",") != "operator.read,operator.write,operator.admin,operator.approvals" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
	if got.Auth.Token != testToken || got.Locale != "en-US" || got.UserAgent != "yuno-api/9.9.9" || got.Commands == nil || got.Permissions == nil {
		t.Fatalf("auth/locale/ua/commands/permissions: %+v", got)
	}
}

func TestHandshakeRejections(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		scopes  []string
		wantErr string
	}{
		{name: "bad token", token: "wrong", wantErr: "gateway token mismatch"},
		{name: "no admin scope", scopes: []string{"operator.read"}, wantErr: "granted: operator.read"},
		{name: "no scopes at all", scopes: []string{}, wantErr: "granted: none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGateway(t, testToken)
			if tc.scopes != nil {
				f.setScopes(tc.scopes...)
			}
			token := tc.token
			if token == "" {
				token = testToken
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := Dial(ctx, DialConfig{URL: f.URL(), Token: token, BackoffBase: 10 * time.Millisecond}, nil)
			var he *HandshakeError
			if !errors.As(err, &he) {
				t.Fatalf("err = %v, want HandshakeError", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestFakeRejectsNonEnumClientID checks the fake enforces the client.id enum the real
// gateway enforces [probed], using a raw client that sends a non-enum id.
func TestFakeRejectsNonEnumClientID(t *testing.T) {
	f := newFakeGateway(t, testToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, f.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow() //nolint:errcheck
	// The first frame is connect.challenge.
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(connectParams{MinProtocol: 4, MaxProtocol: 4,
		Client: connectClient{ID: "yuno-api", Version: "1", Platform: "linux", Mode: "backend"},
		Role:   "operator", Auth: connectAuth{Token: testToken}})
	req, _ := json.Marshal(Frame{Type: "req", ID: "1", Method: "connect", Params: params})
	if err := c.Write(ctx, websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var res Frame
	_ = json.Unmarshal(data, &res)
	if res.OK == nil || *res.OK || res.Error == nil || !strings.Contains(res.Error.Message, "must be equal to one of the allowed values") {
		t.Fatalf("res = %s", data)
	}
}

func TestCallAndRPCError(t *testing.T) {
	f := newFakeGateway(t, testToken)
	f.on("chat.send", func(p json.RawMessage) (any, *RPCError) {
		var in ChatSendParams
		_ = json.Unmarshal(p, &in)
		return ChatSendResult{RunID: in.IdempotencyKey, Status: "started"}, nil
	})
	f.on("agents.create", func(json.RawMessage) (any, *RPCError) {
		return nil, &RPCError{Code: "INVALID_REQUEST", Message: "invalid agents.create params: at root: unexpected property 'id'"}
	})
	g, _ := dialFake(t, f, DialConfig{})
	ctx := context.Background()

	res, err := ChatSend(ctx, g, ChatSendParams{SessionKey: "agent:a:s", AgentID: "a", Message: "hi", IdempotencyKey: "k1"})
	if err != nil || res.RunID != "k1" || res.Status != "started" {
		t.Fatalf("chat.send = %+v, %v", res, err)
	}
	var sent ChatSendParams
	_ = json.Unmarshal(f.callsFor("chat.send")[0], &sent)
	if sent != (ChatSendParams{SessionKey: "agent:a:s", AgentID: "a", Message: "hi", IdempotencyKey: "k1"}) {
		t.Fatalf("params = %+v", sent)
	}

	_, err = AgentsCreate(ctx, g, "x", "/w")
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != "INVALID_REQUEST" || !strings.Contains(rpc.Message, "unexpected property") {
		t.Fatalf("err = %v", err)
	}

	if _, err := g.Call(ctx, "no.such.method", nil); err == nil {
		t.Fatal("unknown method should fail")
	}

	cctx, cancel := context.WithCancel(ctx)
	f.on("slow", func(json.RawMessage) (any, *RPCError) {
		time.Sleep(300 * time.Millisecond)
		return map[string]any{}, nil
	})
	cancel()
	if _, err := g.Call(cctx, "slow", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call err = %v", err)
	}
}

func TestCallIDsIncrement(t *testing.T) {
	f := newFakeGateway(t, testToken)
	f.on("health", func(json.RawMessage) (any, *RPCError) { return map[string]any{"ok": true}, nil })
	g, _ := dialFake(t, f, DialConfig{})
	for i := 0; i < 5; i++ {
		if _, err := g.Call(context.Background(), "health", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := g.seq.Load(); n != 6 {
		t.Fatalf("seq = %d, want 6 (connect is 1)", n)
	}
}

func loadEvents(t *testing.T) []Event {
	t.Helper()
	fh, err := os.Open("testdata/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var out []Event
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func TestEventFanOutAndDrops(t *testing.T) {
	f := newFakeGateway(t, testToken)
	g, _ := dialFake(t, f, DialConfig{})
	a, cancelA := g.Subscribe(16)
	defer cancelA()
	b, cancelB := g.Subscribe(1) // fills after one event
	defer cancelB()

	evs := loadEvents(t)
	for _, e := range evs {
		f.push(e.Name, e.Payload)
	}
	for i, want := range evs {
		got := recv(t, a)
		if got.Name != want.Name {
			t.Fatalf("event %d = %s, want %s", i, got.Name, want.Name)
		}
		if got.At.IsZero() {
			t.Fatal("event without arrival time")
		}
	}
	var ce ChatEvent
	_ = json.Unmarshal(evs[0].Payload, &ce)
	if ce.State != "status" || ce.RunID == "" {
		t.Fatalf("first fixture event decoded as %+v", ce)
	}
	if got := recv(t, b); got.Name != evs[0].Name {
		t.Fatalf("small subscriber got %s", got.Name)
	}
	deadline := time.Now().Add(2 * time.Second)
	for g.Dropped() < int64(len(evs)-1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d := g.Dropped(); d != int64(len(evs)-1) {
		t.Fatalf("dropped = %d, want %d", d, len(evs)-1)
	}
	// The overflow is signalled once, after the events that fit; the full subscriber
	// sees nothing else, the roomy one sees no gap.
	if got := recv(t, b); got.Name != EventGap {
		t.Fatalf("after the drops the small subscriber got %s, want %s", got.Name, EventGap)
	}
	select {
	case e := <-b:
		t.Fatalf("one gap per overflow, got another %s", e.Name)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case e := <-a:
		t.Fatalf("subscriber with room got %s", e.Name)
	default:
	}
	// Delivery resumes once there is room, and a second overflow is signalled again.
	f.push("tick", map[string]any{"ts": 1})
	f.push("tick", map[string]any{"ts": 2})
	for g.Dropped() < int64(len(evs)) && time.Now().Before(deadline.Add(2*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := recv(t, b); got.Name != "tick" {
		t.Fatalf("after draining got %s", got.Name)
	}
	if got := recv(t, b); got.Name != EventGap {
		t.Fatalf("second overflow got %s, want %s", got.Name, EventGap)
	}
	for range 2 {
		recv(t, a)
	}
	cancelB()
	cancelB() // idempotent
	if _, ok := <-b; ok {
		t.Fatal("cancelled subscription should be closed")
	}
}

func TestReconnectAfterServerClose(t *testing.T) {
	f := newFakeGateway(t, testToken)
	release := make(chan struct{})
	f.on("block", func(json.RawMessage) (any, *RPCError) { <-release; return map[string]any{}, nil })
	f.on("health", func(json.RawMessage) (any, *RPCError) { return map[string]any{"ok": true}, nil })
	g, _ := dialFake(t, f, DialConfig{})
	events, cancel := g.Subscribe(64)
	defer cancel()

	inflight := make(chan error, 1)
	go func() {
		_, err := g.Call(context.Background(), "block", nil)
		inflight <- err
	}()
	time.Sleep(50 * time.Millisecond)
	f.dropAll()
	close(release)

	select {
	case err := <-inflight:
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("in-flight call err = %v, want ErrDisconnected", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight call did not fail")
	}
	if e := recv(t, events); e.Name != EventDisconnected {
		t.Fatalf("first event = %s", e.Name)
	}
	if _, up := g.Info(); up {
		// Reconnect may already have happened; only a down state without the event is wrong.
		t.Log("reconnected before Info was read")
	}
	if e := recv(t, events); e.Name != EventReconnected {
		t.Fatalf("second event = %s", e.Name)
	}
	ctx, cancelWait := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWait()
	if err := g.WaitConnected(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(context.Background(), "health", nil); err != nil {
		t.Fatalf("call after reconnect: %v", err)
	}
	if f.connectCount() != 2 {
		t.Fatalf("connects = %d", f.connectCount())
	}
	f.push("tick", map[string]any{"ts": 1})
	if e := recv(t, events); e.Name != "tick" {
		t.Fatalf("subscription did not survive the reconnect: %s", e.Name)
	}
}

func TestCallWhileDisconnected(t *testing.T) {
	f := newFakeGateway(t, testToken)
	g, _ := dialFake(t, f, DialConfig{BackoffBase: time.Second, BackoffMax: time.Second})
	events, cancel := g.Subscribe(8)
	defer cancel()
	f.dropAll()
	if e := recv(t, events); e.Name != EventDisconnected {
		t.Fatalf("event = %s", e.Name)
	}
	if _, up := g.Info(); up {
		t.Fatal("Info reports connected during the outage")
	}
	if _, err := g.Call(context.Background(), "health", nil); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoffSchedule(t *testing.T) {
	g := &Gateway{cfg: DialConfig{BackoffBase: time.Second, BackoffMax: 30 * time.Second}}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := g.backoff(i); got != w*time.Second {
			t.Fatalf("backoff(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
}

func TestShutdownStopsReconnecting(t *testing.T) {
	f := newFakeGateway(t, testToken)
	g, cancel := dialFake(t, f, DialConfig{})
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, up := g.Info(); !up {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, up := g.Info(); up {
		t.Fatal("still connected after ctx ended")
	}
	time.Sleep(200 * time.Millisecond)
	if f.connectCount() != 1 {
		t.Fatalf("reconnected after shutdown: %d connects", f.connectCount())
	}
}
