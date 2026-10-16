package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// DialConfig is how the control plane reaches the gateway (DESIGN 5.14 rules 1 and 2).
type DialConfig struct {
	URL     string // ws://127.0.0.1:18789
	Token   string // shared gateway token
	Version string // reported as client.version
	// BackoffBase and BackoffMax shape the reconnect schedule (DESIGN 5.5 rule 1:
	// 1 s doubling up to 30 s). Zero means the default.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// HandshakeTimeout bounds one connect attempt (challenge plus hello). Zero means 15 s.
	HandshakeTimeout time.Duration
	// RetryRefused makes Dial retry a refused handshake like a transport failure, until
	// ctx ends. Without it only refusals the gateway marks retryable are retried.
	RetryRefused bool
}

// RequiredScopes are the operator scopes the connect frame asks for (DESIGN 5.14 rule 2).
var RequiredScopes = []string{"operator.read", "operator.write", "operator.admin", "operator.approvals"}

// ClientCaps declares this connection as an approval client so exec approvals route to
// it (DESIGN 5.3 rule 1).
var ClientCaps = []string{"approvals", "exec-approvals"}

// Synthetic events the client publishes to subscribers when the connection drops and
// when a re-handshake succeeds. They never come from the gateway itself.
const (
	EventDisconnected = "client.disconnected"
	EventReconnected  = "client.reconnected"
	// EventGap goes to one subscriber whose buffer was full: events after the ones it
	// already holds were dropped, so whatever it mirrors must be re-read (chat.history).
	EventGap = "client.gap"
)

// ErrDisconnected is returned by Call while the gateway connection is down, and by
// calls that were in flight when it dropped.
var ErrDisconnected = errors.New("openclaw: gateway disconnected")

// HandshakeError is a connect attempt the gateway refused or answered without the
// required scopes. Retrying with the same configuration does not help.
type HandshakeError struct {
	Reason string
	RPC    *RPCError
}

func (e *HandshakeError) Error() string {
	if e.RPC != nil {
		return "openclaw: connect rejected: " + e.RPC.Message
	}
	return "openclaw: connect rejected: " + e.Reason
}

func (e *HandshakeError) Unwrap() error {
	if e.RPC == nil {
		return nil
	}
	return e.RPC
}

// Retryable reports whether the gateway said the refusal will lift by itself (it is
// starting or restarting).
func (e *HandshakeError) Retryable() bool { return e.RPC != nil && e.RPC.Retryable }

const (
	readLimit       = 32 << 20 // the gateway advertises maxPayload 25 MiB
	minIdleTimeout  = 90 * time.Second
	writeTimeout    = 15 * time.Second
	connectFrameID  = "1"
	protocolVersion = 4
)

// Gateway is the WebSocket client behind Client.
type Gateway struct {
	cfg DialConfig
	log *slog.Logger

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
	info      ConnectInfo
	idle      time.Duration
	pending   map[string]chan Frame
	changed   chan struct{} // closed and replaced on every connected/disconnected change

	seq atomic.Uint64

	subMu   sync.RWMutex
	subs    map[*subscriber]struct{}
	dropped atomic.Int64
}

type subscriber struct {
	ch   chan Event
	room int // events the buffer holds; one more slot is kept for EventGap

	mu     sync.Mutex
	gapped bool // an EventGap is queued and no event was delivered after it
}

var _ Client = (*Gateway)(nil)

// Dial connects as client.id "gateway-client", client.mode "backend", role operator,
// scopes read/write/admin/approvals, caps ["approvals", "exec-approvals"], and keeps
// the connection alive with reconnect and backoff until ctx ends. It returns once the
// first handshake succeeded. Transport failures before that, and refusals the gateway
// marks retryable ("gateway starting"), are retried with backoff, each attempt logged
// at warn; any other refused handshake or a missing operator.admin scope is returned at
// once unless cfg.RetryRefused. The concrete type is *Gateway.
func Dial(ctx context.Context, cfg DialConfig, log *slog.Logger) (Client, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 15 * time.Second
	}
	g := &Gateway{
		cfg:     cfg,
		log:     log,
		pending: map[string]chan Frame{},
		changed: make(chan struct{}),
		subs:    map[*subscriber]struct{}{},
	}
	g.seq.Store(1) // "1" is the connect frame

	for attempt := 0; ; attempt++ {
		conn, info, idle, err := g.handshake(ctx)
		if err == nil {
			g.setConnected(conn, info, idle)
			go g.serve(ctx, conn)
			log.Info("openclaw gateway connected", "version", info.Version, "conn_id", info.ConnID, "methods", info.Methods)
			return g, nil
		}
		var he *HandshakeError
		if errors.As(err, &he) && !he.Retryable() && !cfg.RetryRefused {
			return nil, err
		}
		wait := g.backoff(attempt)
		log.Warn("openclaw gateway dial failed", "attempt", attempt+1, "error", err, "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("openclaw: dial %s: %w", cfg.URL, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// backoff returns base·2^attempt capped at max.
func (g *Gateway) backoff(attempt int) time.Duration {
	d := g.cfg.BackoffBase
	for i := 0; i < attempt && d < g.cfg.BackoffMax; i++ {
		d *= 2
	}
	return min(d, g.cfg.BackoffMax)
}

type connectParams struct {
	MinProtocol int            `json:"minProtocol"`
	MaxProtocol int            `json:"maxProtocol"`
	Client      connectClient  `json:"client"`
	Role        string         `json:"role"`
	Scopes      []string       `json:"scopes"`
	Caps        []string       `json:"caps"`
	Commands    []string       `json:"commands"`
	Permissions map[string]any `json:"permissions"`
	Auth        connectAuth    `json:"auth"`
	Locale      string         `json:"locale"`
	UserAgent   string         `json:"userAgent"`
}

type connectClient struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"`
}

type connectAuth struct {
	Token string `json:"token"`
}

type helloOK struct {
	Type   string `json:"type"`
	Server struct {
		Version string `json:"version"`
		ConnID  string `json:"connId"`
	} `json:"server"`
	Auth struct {
		Scopes []string `json:"scopes"`
	} `json:"auth"`
	Features struct {
		Methods []string `json:"methods"`
	} `json:"features"`
	Policy struct {
		TickIntervalMs int64 `json:"tickIntervalMs"`
	} `json:"policy"`
}

// handshake opens one connection, answers connect.challenge and checks hello-ok.
func (g *Gateway) handshake(ctx context.Context) (*websocket.Conn, ConnectInfo, time.Duration, error) {
	hctx, cancel := context.WithTimeout(ctx, g.cfg.HandshakeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(hctx, g.cfg.URL, nil)
	if err != nil {
		return nil, ConnectInfo{}, 0, fmt.Errorf("openclaw: dial %s: %w", g.cfg.URL, err)
	}
	conn.SetReadLimit(readLimit)
	fail := func(err error) (*websocket.Conn, ConnectInfo, time.Duration, error) {
		conn.CloseNow() //nolint:errcheck
		return nil, ConnectInfo{}, 0, err
	}

	challenged := false
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			return fail(fmt.Errorf("openclaw: handshake read: %w", err))
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch {
		case f.Type == "event" && f.Event == "connect.challenge" && !challenged:
			challenged = true
			version := g.cfg.Version
			if version == "" {
				version = "dev"
			}
			params, _ := json.Marshal(connectParams{
				MinProtocol: protocolVersion,
				MaxProtocol: protocolVersion,
				Client:      connectClient{ID: "gateway-client", Version: version, Platform: "linux", Mode: "backend"},
				Role:        "operator",
				Scopes:      RequiredScopes,
				Caps:        ClientCaps,
				Commands:    []string{},
				Permissions: map[string]any{},
				Auth:        connectAuth{Token: g.cfg.Token},
				Locale:      "en-US",
				UserAgent:   "yuno-api/" + version,
			})
			req, _ := json.Marshal(Frame{Type: "req", ID: connectFrameID, Method: "connect", Params: params})
			if err := conn.Write(hctx, websocket.MessageText, req); err != nil {
				return fail(fmt.Errorf("openclaw: send connect: %w", err))
			}
		case f.Type == "res" && f.ID == connectFrameID:
			if f.OK == nil || !*f.OK {
				rpc := f.Error
				if rpc == nil {
					rpc = &RPCError{Code: "UNKNOWN", Message: "connect refused"}
				}
				return fail(&HandshakeError{RPC: rpc})
			}
			var h helloOK
			if err := json.Unmarshal(f.Payload, &h); err != nil {
				return fail(fmt.Errorf("openclaw: decode hello: %w", err))
			}
			if !slices.Contains(h.Auth.Scopes, "operator.admin") {
				granted := strings.Join(h.Auth.Scopes, ",")
				if granted == "" {
					granted = "none"
				}
				return fail(&HandshakeError{Reason: "operator.admin scope not granted (granted: " + granted +
					"); the api must reach the gateway over loopback with the shared token"})
			}
			info := ConnectInfo{
				Version: h.Server.Version,
				ConnID:  h.Server.ConnID,
				Scopes:  h.Auth.Scopes,
				Methods: len(h.Features.Methods),
			}
			idle := minIdleTimeout
			if t := time.Duration(h.Policy.TickIntervalMs) * time.Millisecond * 3; t > idle {
				idle = t
			}
			return conn, info, idle, nil
		}
	}
}

func (g *Gateway) setConnected(conn *websocket.Conn, info ConnectInfo, idle time.Duration) {
	g.mu.Lock()
	g.conn, g.info, g.idle, g.connected = conn, info, idle, true
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

// serve reads frames until the connection drops, then reconnects until ctx ends.
func (g *Gateway) serve(ctx context.Context, conn *websocket.Conn) {
	stop := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		c := g.conn
		g.mu.Unlock()
		if c != nil {
			c.Close(websocket.StatusNormalClosure, "shutdown") //nolint:errcheck
		}
	})
	defer stop()
	for {
		err := g.readLoop(ctx, conn)
		g.dropConnection(conn, err)
		if ctx.Err() != nil {
			return
		}
		next, ok := g.reconnect(ctx)
		if !ok {
			return
		}
		conn = next
	}
}

func (g *Gateway) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		g.mu.Lock()
		idle := g.idle
		g.mu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, idle)
		_, data, err := conn.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			g.log.Warn("openclaw: undecodable frame", "error", err)
			continue
		}
		switch f.Type {
		case "res":
			g.mu.Lock()
			ch := g.pending[f.ID]
			delete(g.pending, f.ID)
			g.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case "event":
			if f.Event == "connect.challenge" {
				continue
			}
			g.publish(Event{Name: f.Event, Payload: f.Payload, At: time.Now().UTC()})
		}
	}
}

// dropConnection marks the client disconnected and fails every in-flight call.
func (g *Gateway) dropConnection(conn *websocket.Conn, cause error) {
	g.mu.Lock()
	if g.conn == conn {
		g.connected = false
		close(g.changed)
		g.changed = make(chan struct{})
	}
	pending := g.pending
	g.pending = map[string]chan Frame{}
	g.mu.Unlock()
	conn.CloseNow() //nolint:errcheck
	for _, ch := range pending {
		close(ch)
	}
	g.log.Warn("openclaw gateway connection lost", "error", cause)
	g.publish(Event{Name: EventDisconnected, Payload: json.RawMessage(`{}`), At: time.Now().UTC()})
}

func (g *Gateway) reconnect(ctx context.Context) (*websocket.Conn, bool) {
	for attempt := 0; ; attempt++ {
		wait := g.backoff(attempt)
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		conn, info, idle, err := g.handshake(ctx)
		if err != nil {
			g.log.Warn("openclaw gateway reconnect failed", "attempt", attempt+1, "error", err)
			continue
		}
		g.setConnected(conn, info, idle)
		g.log.Info("openclaw gateway reconnected", "conn_id", info.ConnID, "attempts", attempt+1)
		g.publish(Event{Name: EventReconnected, Payload: json.RawMessage(`{}`), At: time.Now().UTC()})
		return conn, true
	}
}

// Call sends a req frame and waits for its res frame, ctx, or a disconnect.
func (g *Gateway) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw := json.RawMessage(`{}`)
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("openclaw: %s: encode params: %w", method, err)
		}
		raw = b
	}
	id := strconv.FormatUint(g.seq.Add(1), 10)
	req, err := json.Marshal(Frame{Type: "req", ID: id, Method: method, Params: raw})
	if err != nil {
		return nil, fmt.Errorf("openclaw: %s: encode frame: %w", method, err)
	}
	ch := make(chan Frame, 1)
	g.mu.Lock()
	if !g.connected {
		g.mu.Unlock()
		return nil, fmt.Errorf("openclaw: %s: %w", method, ErrDisconnected)
	}
	conn := g.conn
	g.pending[id] = ch
	g.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	err = conn.Write(wctx, websocket.MessageText, req)
	cancel()
	if err != nil {
		g.forget(id)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("openclaw: %s: %w", method, ctx.Err())
		}
		return nil, fmt.Errorf("openclaw: %s: %w (%v)", method, ErrDisconnected, err)
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("openclaw: %s: %w", method, ErrDisconnected)
		}
		if f.OK == nil || !*f.OK {
			if f.Error == nil {
				return nil, &RPCError{Code: "UNKNOWN", Message: method + " failed without an error object"}
			}
			return nil, f.Error
		}
		return f.Payload, nil
	case <-ctx.Done():
		g.forget(id)
		return nil, fmt.Errorf("openclaw: %s: %w", method, ctx.Err())
	}
}

func (g *Gateway) forget(id string) {
	g.mu.Lock()
	delete(g.pending, id)
	g.mu.Unlock()
}

// Subscribe returns a channel of every event the client receives, including the
// synthetic EventDisconnected and EventReconnected. The channel survives reconnects
// and is closed by the cancel func. A full buffer drops events (see Dropped) and queues
// one EventGap for the subscriber, in a slot reserved for it, so the loss is never
// silent.
func (g *Gateway) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	s := &subscriber{ch: make(chan Event, buffer+1), room: buffer}
	g.subMu.Lock()
	g.subs[s] = struct{}{}
	g.subMu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			g.subMu.Lock()
			delete(g.subs, s)
			close(s.ch)
			g.subMu.Unlock()
		})
	}
}

func (g *Gateway) publish(e Event) {
	g.subMu.RLock()
	defer g.subMu.RUnlock()
	for s := range g.subs {
		s.deliver(e, &g.dropped)
	}
}

// deliver queues e when the buffer has room. Otherwise e is dropped and, unless one is
// already waiting, an EventGap takes the reserved slot.
func (s *subscriber) deliver(e Event, dropped *atomic.Int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ch) < s.room {
		select {
		case s.ch <- e:
			s.gapped = false
			return
		default:
		}
	}
	dropped.Add(1)
	if s.gapped {
		return
	}
	select {
	case s.ch <- Event{Name: EventGap, Payload: json.RawMessage(`{}`), At: time.Now().UTC()}:
		s.gapped = true
	default:
	}
}

// Dropped counts events discarded because a subscriber's buffer was full.
func (g *Gateway) Dropped() int64 { return g.dropped.Load() }

// Info reports the last successful handshake and whether the connection is up.
func (g *Gateway) Info() (ConnectInfo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.info, g.connected
}

// WaitConnected blocks until the connection is up or ctx ends.
func (g *Gateway) WaitConnected(ctx context.Context) error {
	for {
		g.mu.Lock()
		up, changed := g.connected, g.changed
		g.mu.Unlock()
		if up {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
