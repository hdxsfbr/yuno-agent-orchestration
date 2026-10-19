package openclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// fakeGateway is an httptest WebSocket server speaking the gateway protocol as probed
// against OpenClaw 2026.10.3: connect.challenge, a validated connect frame, hello-ok,
// scripted responses per method, and pushed events.
type fakeGateway struct {
	t      *testing.T
	srv    *httptest.Server
	token  string
	scopes []string

	mu          sync.Mutex
	handlers    map[string]func(params json.RawMessage) (any, *RPCError)
	calls       []fakeCall
	conns       map[*websocket.Conn]context.CancelFunc
	connects    int
	lastConnect connectParams
	// refuse answers this many connect frames with refusal, as a gateway that is
	// starting answers {code: UNAVAILABLE, retryable: true} [probed].
	refuse   int
	refusal  *RPCError
	refusedN int
}

type fakeCall struct {
	Method string
	Params json.RawMessage
}

var allowedClientIDs = []string{"gateway-client", "control-ui", "cli", "node-host"}
var allowedClientModes = []string{"backend", "ui", "cli", "node"}

func newFakeGateway(t *testing.T, token string) *fakeGateway {
	t.Helper()
	f := &fakeGateway{
		t: t, token: token, scopes: slices.Clone(RequiredScopes),
		handlers: map[string]func(json.RawMessage) (any, *RPCError){},
		conns:    map[*websocket.Conn]context.CancelFunc{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.dropAll()
		f.srv.Close()
	})
	return f
}

func (f *fakeGateway) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeGateway) setScopes(s ...string) {
	f.mu.Lock()
	f.scopes = s
	f.mu.Unlock()
}

// refuseConnects makes the next n connects fail with refusal.
func (f *fakeGateway) refuseConnects(n int, refusal *RPCError) {
	f.mu.Lock()
	f.refuse, f.refusal = n, refusal
	f.mu.Unlock()
}

func (f *fakeGateway) refusedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusedN
}

// on installs the handler for a method; a nil RPCError means ok=true.
func (f *fakeGateway) on(method string, h func(params json.RawMessage) (any, *RPCError)) {
	f.mu.Lock()
	f.handlers[method] = h
	f.mu.Unlock()
}

func (f *fakeGateway) callsFor(method string) []json.RawMessage {
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

func (f *fakeGateway) allCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeGateway) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects
}

// push sends an event frame to every connected client.
func (f *fakeGateway) push(event string, payload any) {
	b, _ := json.Marshal(payload)
	frame, _ := json.Marshal(Frame{Type: "event", Event: event, Payload: b})
	f.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.Write(context.Background(), websocket.MessageText, frame)
	}
}

// dropAll closes every client connection, as a gateway restart would.
func (f *fakeGateway) dropAll() {
	f.mu.Lock()
	conns := f.conns
	f.conns = map[*websocket.Conn]context.CancelFunc{}
	f.mu.Unlock()
	for c, cancel := range conns {
		cancel()
		c.CloseNow() //nolint:errcheck
	}
}

func (f *fakeGateway) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(32 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.CloseNow() //nolint:errcheck

	challenge, _ := json.Marshal(Frame{Type: "event", Event: "connect.challenge", Payload: json.RawMessage(`{"nonce":"n-1","ts":1}`)})
	if err := c.Write(ctx, websocket.MessageText, challenge); err != nil {
		return
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var req Frame
	if json.Unmarshal(data, &req) != nil || req.Type != "req" || req.Method != "connect" || req.ID != "1" {
		return
	}
	var p connectParams
	_ = json.Unmarshal(req.Params, &p)
	f.mu.Lock()
	f.lastConnect = p
	f.mu.Unlock()
	reply := func(fr Frame) { b, _ := json.Marshal(fr); _ = c.Write(ctx, websocket.MessageText, b) }
	no := false
	if !slices.Contains(allowedClientIDs, p.Client.ID) || !slices.Contains(allowedClientModes, p.Client.Mode) {
		reply(Frame{Type: "res", ID: "1", OK: &no, Error: &RPCError{Code: "INVALID_REQUEST",
			Message: "invalid connect params: at /client/id: must be equal to one of the allowed values"}})
		c.Close(websocket.StatusPolicyViolation, "invalid connect params") //nolint:errcheck
		return
	}
	f.mu.Lock()
	refuse := f.refuse > 0
	refusal := f.refusal
	if refuse {
		f.refuse--
		f.refusedN++
	}
	f.mu.Unlock()
	if refuse {
		reply(Frame{Type: "res", ID: "1", OK: &no, Error: refusal})
		c.Close(websocket.StatusTryAgainLater, "starting") //nolint:errcheck
		return
	}
	if p.Auth.Token != f.token {
		reply(Frame{Type: "res", ID: "1", OK: &no, Error: &RPCError{Code: "UNAUTHORIZED", Message: "gateway token mismatch"}})
		c.Close(websocket.StatusPolicyViolation, "unauthorized") //nolint:errcheck
		return
	}
	f.mu.Lock()
	f.connects++
	connID := "conn-" + string(rune('0'+f.connects))
	hello, _ := json.Marshal(map[string]any{
		"type": "hello-ok", "protocol": 4,
		"server":   map[string]any{"version": "2026.10.3", "connId": connID},
		"auth":     map[string]any{"method": "token", "role": "operator", "scopes": f.scopes},
		"features": map[string]any{"methods": []string{"chat.send", "chat.abort", "config.get", "config.patch"}},
		"policy":   map[string]any{"tickIntervalMs": 30000},
	})
	f.conns[c] = cancel
	f.mu.Unlock()
	yes := true
	reply(Frame{Type: "res", ID: "1", OK: &yes, Payload: hello})

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			f.mu.Lock()
			delete(f.conns, c)
			f.mu.Unlock()
			return
		}
		var fr Frame
		if json.Unmarshal(data, &fr) != nil || fr.Type != "req" {
			continue
		}
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{Method: fr.Method, Params: fr.Params})
		h := f.handlers[fr.Method]
		f.mu.Unlock()
		if h == nil {
			reply(Frame{Type: "res", ID: fr.ID, OK: &no, Error: &RPCError{Code: "INVALID_REQUEST", Message: "unknown method: " + fr.Method}})
			continue
		}
		payload, rpcErr := h(fr.Params)
		if rpcErr != nil {
			reply(Frame{Type: "res", ID: fr.ID, OK: &no, Error: rpcErr})
			continue
		}
		b, _ := json.Marshal(payload)
		reply(Frame{Type: "res", ID: fr.ID, OK: &yes, Payload: b})
	}
}

// fakeOpenClawState is the slice of gateway state the materialiser touches: agents,
// the config with its hash and replacePaths rule, and the exec approvals file.
type fakeOpenClawState struct {
	mu       sync.Mutex
	agents   []map[string]any
	config   map[string]any
	cfgRev   int
	exec     map[string]any
	execRev  int
	rejected []string // config.patch refusals, for assertions
	// recoveryPatches makes that many config.patch calls persist and then answer UNAVAILABLE
	// "recovery restart is required" (gateway 2026.10.3); a restart request clears it.
	recoveryPatches int
	stuck           bool
}

func newFakeOpenClawState() *fakeOpenClawState {
	return &fakeOpenClawState{
		agents: []map[string]any{{"id": "main", "workspace": "/home/node/.openclaw/workspace"}},
		config: map[string]any{"agents": map[string]any{"entries": map[string]any{"main": map[string]any{"name": "Gateway default"}}}},
		exec:   map[string]any{"version": 1, "defaults": map[string]any{}, "agents": map[string]any{}},
	}
}

func (s *fakeOpenClawState) cfgHash() string  { return "h" + string(rune('a'+s.cfgRev%26)) }
func (s *fakeOpenClawState) execHash() string { return "e" + string(rune('a'+s.execRev%26)) }

// install registers the agents/config/exec handlers on a fake gateway.
func (s *fakeOpenClawState) install(f *fakeGateway) {
	f.on("agents.list", func(json.RawMessage) (any, *RPCError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return map[string]any{"defaultId": "main", "agents": s.agents}, nil
	})
	f.on("agents.create", func(p json.RawMessage) (any, *RPCError) {
		var in map[string]any
		_ = json.Unmarshal(p, &in)
		for k := range in {
			if k != "name" && k != "workspace" {
				return nil, &RPCError{Code: "INVALID_REQUEST", Message: "invalid agents.create params: at root: unexpected property '" + k + "'"}
			}
		}
		name, _ := in["name"].(string)
		id := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, a := range s.agents {
			if a["id"] == id {
				return nil, &RPCError{Code: "INVALID_REQUEST", Message: `agent "` + id + `" already exists`}
			}
		}
		s.agents = append(s.agents, map[string]any{"id": id, "name": name, "workspace": in["workspace"]})
		entries := s.config["agents"].(map[string]any)["entries"].(map[string]any)
		entries[id] = map[string]any{"name": name, "workspace": in["workspace"], "identity": map[string]any{"name": name}}
		s.cfgRev++
		return map[string]any{"ok": true, "agentId": id, "name": name, "workspace": in["workspace"]}, nil
	})
	f.on("config.get", func(json.RawMessage) (any, *RPCError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		cfg, _ := json.Marshal(s.config)
		return map[string]any{"hash": s.cfgHash(), "config": json.RawMessage(cfg)}, nil
	})
	f.on("config.patch", func(p json.RawMessage) (any, *RPCError) {
		var in ConfigPatchParams
		_ = json.Unmarshal(p, &in)
		s.mu.Lock()
		defer s.mu.Unlock()
		if in.BaseHash != s.cfgHash() {
			return nil, &RPCError{Code: "INVALID_REQUEST", Message: "config changed since last load; re-run config.get (baseHash mismatch)"}
		}
		var patch map[string]any
		if err := json.Unmarshal([]byte(in.Raw), &patch); err != nil {
			return nil, &RPCError{Code: "INVALID_REQUEST", Message: "raw must be JSON"}
		}
		if path := mergeChecked(s.config, patch, "", in.ReplacePaths); path != "" {
			s.rejected = append(s.rejected, path)
			return nil, &RPCError{Code: "INVALID_REQUEST", Message: "refusing to replace array-bearing path " + path + " without replacePaths"}
		}
		s.cfgRev++
		if s.stuck || s.recoveryPatches > 0 {
			if s.recoveryPatches > 0 {
				s.recoveryPatches--
				s.stuck = true
			}
			return nil, &RPCError{Code: "UNAVAILABLE", Message: "config.patch persisted and updated the active Gateway, but a recovery restart is required; wait for the Gateway to restart, then run config.get to confirm the active revision"}
		}
		return map[string]any{"ok": true, "hash": s.cfgHash()}, nil
	})
	f.on("gateway.restart.request", func(json.RawMessage) (any, *RPCError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stuck = false
		return map[string]any{"ok": true, "status": "scheduled"}, nil
	})
	f.on("exec.approvals.get", func(json.RawMessage) (any, *RPCError) {
		s.mu.Lock()
		defer s.mu.Unlock()
		file, _ := json.Marshal(s.exec)
		return map[string]any{"exists": true, "file": json.RawMessage(file), "hash": s.execHash()}, nil
	})
	f.on("exec.approvals.set", func(p json.RawMessage) (any, *RPCError) {
		var in struct {
			File     map[string]any `json:"file"`
			BaseHash string         `json:"baseHash"`
		}
		_ = json.Unmarshal(p, &in)
		s.mu.Lock()
		defer s.mu.Unlock()
		if in.BaseHash != s.execHash() {
			return nil, &RPCError{Code: "INVALID_REQUEST", Message: "exec approvals changed (baseHash mismatch)"}
		}
		s.exec = in.File
		s.execRev++
		return map[string]any{"hash": s.execHash()}, nil
	})
}

// mergeChecked merges patch into dst like config.patch: null deletes, objects merge,
// anything else replaces. Replacing or deleting a value that is or contains an array
// requires the exact dotted path in replace; the offending path is returned otherwise.
func mergeChecked(dst, patch map[string]any, prefix string, replace []string) string {
	for k, pv := range patch {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		cur, exists := dst[k]
		if pm, ok := pv.(map[string]any); ok {
			if cm, ok := cur.(map[string]any); ok {
				if bad := mergeChecked(cm, pm, path, replace); bad != "" {
					return bad
				}
				continue
			}
		}
		if exists && holdsArray(cur) && !slices.Contains(replace, path) {
			return path
		}
		if pv == nil {
			delete(dst, k)
			continue
		}
		dst[k] = pv
	}
	return ""
}

func holdsArray(v any) bool {
	switch t := v.(type) {
	case []any:
		return true
	case map[string]any:
		for _, c := range t {
			if holdsArray(c) {
				return true
			}
		}
	}
	return false
}
