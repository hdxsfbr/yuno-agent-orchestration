package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// ---- fake gateway ------------------------------------------------------------------

// fakeGW holds a config with a hash like the gateway: config.patch merges raw with the
// probed rules (baseHash must match, array-bearing paths need replacePaths) and
// gateway.restart.request is recorded.
type fakeGW struct {
	mu        sync.Mutex
	cfg       map[string]any
	rev       int
	calls     []string
	patches   []openclaw.ConfigPatchParams
	conflicts int   // config.patch answers a baseHash conflict this many times first
	patchErr  error // config.patch fails with this
	subs      []chan openclaw.Event
}

func newFakeGW(cfg string) *fakeGW {
	f := &fakeGW{}
	_ = json.Unmarshal([]byte(cfg), &f.cfg)
	return f
}

func (f *fakeGW) hash() string { return fmt.Sprintf("h%d", f.rev) }

func (f *fakeGW) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	switch method {
	case "config.get":
		b, _ := json.Marshal(map[string]any{"hash": f.hash(), "config": f.cfg})
		return b, nil
	case "config.patch":
		p := params.(openclaw.ConfigPatchParams)
		f.patches = append(f.patches, p)
		if f.patchErr != nil {
			return nil, f.patchErr
		}
		if f.conflicts > 0 {
			f.conflicts--
			f.rev++ // someone else wrote meanwhile
			return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "config changed since last load; re-run config.get (baseHash mismatch)"}
		}
		if p.BaseHash != f.hash() {
			return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "config changed since last load; re-run config.get (baseHash mismatch)"}
		}
		var patch map[string]any
		if err := json.Unmarshal([]byte(p.Raw), &patch); err != nil {
			return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "raw must be JSON"}
		}
		if _, ok := patch["bindings"]; ok && f.cfg["bindings"] != nil && !contains(p.ReplacePaths, "bindings") {
			return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "refusing to replace array-bearing path bindings without replacePaths"}
		}
		merge(f.cfg, patch)
		f.rev++
		return json.RawMessage(`{"ok":true}`), nil
	case "gateway.restart.request":
		return json.RawMessage(`{"ok":true,"status":"scheduled","preflight":{"safe":true,"summary":"safe to restart now"}}`), nil
	}
	return nil, &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "unknown method: " + method}
}

func (f *fakeGW) Subscribe(int) (<-chan openclaw.Event, func()) {
	ch := make(chan openclaw.Event, 16)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fakeGW) Info() (openclaw.ConnectInfo, bool) { return openclaw.ConnectInfo{}, true }

func (f *fakeGW) push(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- openclaw.Event{Name: name, Payload: json.RawMessage(`{}`)}
	}
}

func (f *fakeGW) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (f *fakeGW) config() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.cfg)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func merge(dst, patch map[string]any) {
	for k, v := range patch {
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, pm)
				continue
			}
		}
		if v == nil {
			delete(dst, k)
			continue
		}
		dst[k] = v
	}
}

const baselineTelegram = `{"channels":{"telegram":{"enabled":true,"dmPolicy":"disabled","groupPolicy":"disabled","allowFrom":[42]}}}`

// ---- the patch (T5.8.3, D-025) -------------------------------------------------------

func TestTelegramPatch(t *testing.T) {
	ours := func(id string) any {
		return map[string]any{"agentId": id, "match": map[string]any{"channel": "telegram"}, "session": map[string]any{"dmScope": "per-channel-peer"}}
	}
	slack := map[string]any{"agentId": "other", "match": map[string]any{"channel": "slack"}}
	tests := []struct {
		name        string
		cfg         string
		agent       string
		wantPatch   map[string]any
		wantReplace []string
	}{
		{name: "bind on the baseline: binding added, DMs opened", cfg: baselineTelegram, agent: "agcoder",
			wantPatch:   map[string]any{"bindings": []any{ours("agcoder")}, "channels": map[string]any{"telegram": map[string]any{"dmPolicy": "allowlist"}}},
			wantReplace: []string{"bindings"}},
		{name: "already bound: nothing to do", agent: "agcoder",
			cfg: `{"bindings":[{"agentId":"agcoder","match":{"channel":"telegram"},"session":{"dmScope":"per-channel-peer"}}],"channels":{"telegram":{"dmPolicy":"allowlist","groupPolicy":"disabled"}}}`},
		{name: "rebind: the other agent's binding replaced, other channels kept", agent: "agnew",
			cfg:         `{"bindings":[{"agentId":"other","match":{"channel":"slack"}},{"agentId":"agold","match":{"channel":"telegram"}}],"channels":{"telegram":{"dmPolicy":"allowlist","groupPolicy":"disabled"}}}`,
			wantPatch:   map[string]any{"bindings": []any{slack, ours("agnew")}},
			wantReplace: []string{"bindings"}},
		{name: "unbind: binding removed, DMs closed", agent: "",
			cfg:         `{"bindings":[{"agentId":"agold","match":{"channel":"telegram"}}],"channels":{"telegram":{"dmPolicy":"allowlist","groupPolicy":"disabled"}}}`,
			wantPatch:   map[string]any{"bindings": []any{}, "channels": map[string]any{"telegram": map[string]any{"dmPolicy": "disabled"}}},
			wantReplace: []string{"bindings"}},
		{name: "nothing bound on a gateway that answers DMs: closed (D-025)", agent: "",
			cfg:       `{"channels":{"telegram":{"enabled":true,"dmPolicy":"allowlist","groupPolicy":"allowlist"}}}`,
			wantPatch: map[string]any{"channels": map[string]any{"telegram": map[string]any{"dmPolicy": "disabled", "groupPolicy": "disabled"}}}},
		{name: "no telegram block: only the binding", agent: "agcoder", cfg: `{}`,
			wantPatch: map[string]any{"bindings": []any{ours("agcoder")}}, wantReplace: []string{"bindings"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg map[string]any
			_ = json.Unmarshal([]byte(tc.cfg), &cfg)
			patch, replace := TelegramPatch(cfg, tc.agent)
			if !sameValue(patch, tc.wantPatch) {
				got, _ := json.Marshal(patch)
				want, _ := json.Marshal(tc.wantPatch)
				t.Fatalf("patch\n got %s\nwant %s", got, want)
			}
			if !reflect.DeepEqual(replace, tc.wantReplace) {
				t.Fatalf("replacePaths = %v, want %v", replace, tc.wantReplace)
			}
		})
	}
}

// sameValue compares through JSON so []any{} and typed maps line up.
func sameValue(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(ab, &x)
	_ = json.Unmarshal(bb, &y)
	return reflect.DeepEqual(x, y)
}

// T5.8.3 against the gateway: the patch, then the gateway's safe restart; nothing when
// the config already routes the channel; a lost baseHash race is retried.
func TestApplyTelegram(t *testing.T) {
	ctx := context.Background()
	gw := newFakeGW(baselineTelegram)
	gw.conflicts = 1
	res, err := ApplyTelegram(ctx, gw, "agcoder", "test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Restart != "scheduled" || gw.count("gateway.restart.request") != 1 {
		t.Fatalf("res = %+v, restarts = %d", res, gw.count("gateway.restart.request"))
	}
	if gw.count("config.patch") != 2 {
		t.Fatalf("config.patch calls = %d, want the conflict retried once", gw.count("config.patch"))
	}
	cfg := gw.config()
	b := cfg["bindings"].([]any)[0].(map[string]any)
	if b["agentId"] != "agcoder" || b["session"].(map[string]any)["dmScope"] != "per-channel-peer" {
		t.Fatalf("binding = %v", b)
	}
	if cfg["channels"].(map[string]any)["telegram"].(map[string]any)["dmPolicy"] != "allowlist" {
		t.Fatalf("DMs not opened: %v", cfg["channels"])
	}

	res, err = ApplyTelegram(ctx, gw, "agcoder", "test")
	if err != nil || res.Changed || gw.count("gateway.restart.request") != 1 {
		t.Fatalf("second apply should change nothing: %+v %v", res, err)
	}

	if _, err := ApplyTelegram(ctx, gw, "", "test"); err != nil {
		t.Fatal(err)
	}
	cfg = gw.config()
	if len(cfg["bindings"].([]any)) != 0 || cfg["channels"].(map[string]any)["telegram"].(map[string]any)["dmPolicy"] != "disabled" {
		t.Fatalf("unbind left %v", cfg)
	}
}

func TestApplyTelegramFailures(t *testing.T) {
	gw := newFakeGW(baselineTelegram)
	gw.patchErr = &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "invalid config"}
	if _, err := ApplyTelegram(context.Background(), gw, "agcoder", "test"); err == nil || gw.count("gateway.restart.request") != 0 {
		t.Fatalf("a refused patch must fail without a restart: %v", err)
	}
	if _, err := ApplyTelegram(context.Background(), nil, "agcoder", "test"); err == nil {
		t.Fatal("no gateway must fail")
	}
}

// ---- re-assert at start and reconnect (needs the database) ------------------------------

const testSchema = "yuno_channels_test"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pool, err := openSchema(url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "channels tests: database setup failed:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func openSchema(url string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := store.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	_, err = base.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+testSchema)
	base.Close()
	if err != nil {
		return nil, err
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := store.Open(ctx, url+sep+"search_path="+testSchema)
	if err != nil {
		return nil, err
	}
	return pool, store.Migrate(ctx, pool, slog.New(slog.DiscardHandler))
}

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	if _, err := testPool.Exec(context.Background(), `TRUNCATE agents, channel_bindings, conversations CASCADE`); err != nil {
		t.Fatal(err)
	}
	return testPool
}

func boundAgent(t *testing.T, pool *pgxpool.Pool, ocID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := &store.Agent{Name: "Coder", Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"}}
	if err := store.CreateAgent(ctx, pool, a); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentMaterialized(ctx, pool, a.ID, ocID, "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindChannel(ctx, pool, store.ChannelTelegram, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// The binding lives in the table; the keeper puts it back on a gateway that lost it, at
// start and after a reconnect, and closes the channel when nothing is bound.
func TestKeeperReasserts(t *testing.T) {
	pool := needDB(t)
	boundAgent(t, pool, "agkeeper")
	gw := newFakeGW(baselineTelegram)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		(&Keeper{Pool: pool, Gateway: gw, Log: slog.New(slog.DiscardHandler)}).Run(ctx)
		close(done)
	}()
	waitFor(t, "binding applied at start", func() bool { return gw.count("gateway.restart.request") == 1 })

	// A gateway that came back without the binding (a fresh state directory) gets it again.
	gw.mu.Lock()
	gw.cfg = map[string]any{"channels": map[string]any{"telegram": map[string]any{"dmPolicy": "disabled", "groupPolicy": "disabled"}}}
	gw.rev++
	gw.mu.Unlock()
	gw.push(openclaw.EventReconnected)
	waitFor(t, "binding re-applied after reconnect", func() bool { return gw.count("gateway.restart.request") == 2 })
	if b := gw.config()["bindings"].([]any); len(b) != 1 {
		t.Fatalf("bindings = %v", b)
	}

	// A reconnect with nothing to change patches nothing.
	gw.push(openclaw.EventReconnected)
	waitFor(t, "config read after the second reconnect", func() bool { return gw.count("config.get") >= 3 })
	if gw.count("config.patch") != 2 {
		t.Fatalf("config.patch calls = %d, want 2", gw.count("config.patch"))
	}

	// Unbound in the table: the next re-assert closes the channel.
	if _, err := store.UnbindChannel(context.Background(), pool, store.ChannelTelegram); err != nil {
		t.Fatal(err)
	}
	gw.push(openclaw.EventReconnected)
	waitFor(t, "channel closed", func() bool {
		tg := gw.config()["channels"].(map[string]any)["telegram"].(map[string]any)
		return tg["dmPolicy"] == "disabled" && len(gw.config()["bindings"].([]any)) == 0
	})
	cancel()
	<-done
}

// A failed re-assert is retried with backoff rather than waiting for a reconnect.
func TestKeeperRetries(t *testing.T) {
	pool := needDB(t)
	boundAgent(t, pool, "agretry")
	gw := newFakeGW(baselineTelegram)
	gw.patchErr = &openclaw.RPCError{Code: "UNAVAILABLE", Message: "busy"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Keeper{Pool: pool, Gateway: gw, Log: slog.New(slog.DiscardHandler), BackoffBase: 5 * time.Millisecond, BackoffMax: 10 * time.Millisecond}).Run(ctx)
	waitFor(t, "three attempts", func() bool { return gw.count("config.patch") >= 3 })
	gw.mu.Lock()
	gw.patchErr = nil
	gw.mu.Unlock()
	waitFor(t, "applied once the gateway accepts", func() bool { return gw.count("gateway.restart.request") == 1 })
}

// T5.8.5: reading and patching the config never puts the bot token in a log line, even
// when the gateway echoes it in an error.
func TestTokenNeverLogged(t *testing.T) {
	pool := needDB(t)
	boundAgent(t, pool, "agtoken")
	const token = "1234567890:AAFakeTokenForRedactionTests000000000"
	gw := newFakeGW(`{"channels":{"telegram":{"enabled":true,"botToken":"` + token + `","dmPolicy":"disabled"}}}`)
	gw.patchErr = &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "invalid channels.telegram.botToken " + token}

	out := captureStdout(t, func(log *slog.Logger) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for gw.count("config.patch") < 2 {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		(&Keeper{Pool: pool, Gateway: gw, Log: log, BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond}).Run(ctx)
		gw.mu.Lock()
		gw.patchErr = nil
		gw.mu.Unlock()
		if _, err := Reassert(context.Background(), pool, gw, log); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, token) || strings.Contains(out, "AAEhBP0av28X3Ff9kq7bX2LkA6vYdGHi0Qz") {
		t.Fatalf("token in the log:\n%s", out)
	}
	if !strings.Contains(out, "telegram binding re-assert failed") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("expected the failure logged with the token redacted:\n%s", out)
	}
}

// captureStdout runs fn with the platform's logger (obs.NewLogger writes to stdout)
// and returns what it wrote.
func captureStdout(t *testing.T, fn func(*slog.Logger)) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	log := obs.NewLogger("info")
	os.Stdout = orig
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(copied) }()
	fn(log)
	_ = w.Close()
	<-copied
	return buf.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// DESIGN 5.10 rule 6: conversation messages older than the agent's
// conversation_ttl_days (default 7) are swept; newer ones stay.
func TestConversationRetention(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	short := &store.Agent{Name: "Short", Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"},
		MemoryPolicy: store.MemoryPolicy{ConversationTTLDays: 2}}
	plain := &store.Agent{Name: "Plain", Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"}}
	for _, a := range []*store.Agent{short, plain} {
		if err := store.CreateAgent(ctx, pool, a); err != nil {
			t.Fatal(err)
		}
	}
	add := func(agentID, key string, ages ...int) {
		c, err := store.UpsertConversation(ctx, pool, &store.Conversation{Channel: "telegram", ExternalChatID: "1", AgentID: agentID, SessionKey: key})
		if err != nil {
			t.Fatal(err)
		}
		for i, days := range ages {
			m := &store.ConversationMessage{ConversationID: c.ID, Direction: "in", ExternalMessageID: fmt.Sprint(key, i), Text: "x",
				CreatedAt: now.Add(-time.Duration(days)*24*time.Hour - time.Minute)}
			if _, err := store.InsertConversationMessage(ctx, pool, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(short.ID, "agent:s:telegram:direct:1", 1, 2, 5)
	add(plain.ID, "agent:p:telegram:direct:1", 2, 6, 7, 30)
	n, err := store.DeleteExpiredConversationMessages(ctx, pool, now)
	if err != nil || n != 4 {
		t.Fatalf("swept %d, %v; want 4 (two of Short's, two of Plain's)", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages`).Scan(&left)
	if left != 3 {
		t.Fatalf("left %d, want 3", left)
	}
}

// T5.8.3 in the store: one agent per channel; the bound agent may bind again; a deleted
// agent's binding does not hold the channel.
func TestBindChannelOneAgent(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a := boundAgent(t, pool, "agfirst")
	b := &store.Agent{Name: "Second", Model: "claude-opus-5-5", ToolAllowlist: []string{"finish"}}
	if err := store.CreateAgent(ctx, pool, b); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindChannel(ctx, pool, store.ChannelTelegram, b.ID); !errors.Is(err, store.ErrChannelBound) {
		t.Fatalf("second agent: %v", err)
	}
	if got, err := store.BindChannel(ctx, pool, store.ChannelTelegram, a.ID); err != nil || got.AgentID != a.ID {
		t.Fatalf("rebinding the bound agent: %+v %v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agents SET deleted_at = now() WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetChannelBinding(ctx, pool, store.ChannelTelegram); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted agent's binding still counts: %v", err)
	}
	if got, err := store.BindChannel(ctx, pool, store.ChannelTelegram, b.ID); err != nil || got.AgentID != b.ID {
		t.Fatalf("binding after the bound agent was deleted: %+v %v", got, err)
	}
}
