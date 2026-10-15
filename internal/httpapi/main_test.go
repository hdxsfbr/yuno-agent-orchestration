package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/engine"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// testPool is the yuno_test database when DATABASE_URL is set; DB-backed tests skip
// otherwise. Tests only add rows with fresh ids and never truncate, because other
// packages' tests share the database.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "httpapi tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "httpapi tests: migrate:", err)
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

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	return testPool
}

const testMCPToken = "mcp-test-token-0123456789"

func testConfig() config.Config {
	return config.Config{
		MCPToken:                testMCPToken,
		ToolOutputMaxBytes:      32768,
		EngineWorkers:           2,
		AnthropicBaseURL:        "http://127.0.0.1:1",
		ModelProxyRatePerMinute: 60,
	}
}

// newTestServer builds a server around the fakes and serves it over httptest.
func newTestServer(t *testing.T, pool *pgxpool.Pool, eng engine.Service, gw openclaw.Client) (*Server, *httptest.Server) {
	t.Helper()
	d := Deps{Cfg: testConfig(), Pool: pool, Gateway: gw}
	if eng != nil {
		d.Engine = eng
	}
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// do sends a JSON request and returns the status and body.
func do(t *testing.T, ts *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// apiError decodes the Error shape; details stay raw.
type apiError struct {
	Error struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	} `json:"error"`
}

func decodeError(t *testing.T, b []byte) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("not an Error body: %s", b)
	}
	return e
}

// detailPaths returns the paths of a 422's details.
func detailPaths(t *testing.T, b []byte) []string {
	t.Helper()
	var fe []fieldError
	if err := json.Unmarshal(decodeError(t, b).Error.Details, &fe); err != nil {
		t.Fatalf("details are not field errors: %s", b)
	}
	out := make([]string, len(fe))
	for i, f := range fe {
		out[i] = f.Path
	}
	return out
}

// fakeEngine implements engine.Service. With a pool it writes the rows the real engine
// would (materialisation marker, quick-run workflow and run, cancel flag).
type fakeEngine struct {
	pool *pgxpool.Pool

	mu             sync.Mutex
	materializeErr error
	runtimeErr     error
	triggerErr     error
	cancelErr      error
	finishErr      error
	materialized   []string
	triggers       []engine.TriggerRequest
	cancels        []string
	finishes       []string
	rejections     []string
}

func (f *fakeEngine) MaterializeAgent(ctx context.Context, agentID string) (*engine.RuntimeView, error) {
	f.mu.Lock()
	f.materialized = append(f.materialized, agentID)
	err := f.materializeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if f.pool != nil {
		if err := store.SetAgentMaterialized(ctx, f.pool, agentID, "t-"+agentID, "hash-1"); err != nil {
			return nil, err
		}
	}
	return f.view(agentID), nil
}

func (f *fakeEngine) AgentRuntime(ctx context.Context, agentID string) (*engine.RuntimeView, error) {
	f.mu.Lock()
	err := f.runtimeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.view(agentID), nil
}

func (f *fakeEngine) view(agentID string) *engine.RuntimeView {
	return &engine.RuntimeView{
		OpenClawAgentID: "t-" + agentID,
		Materialized:    true,
		InSync:          true,
		Workspace:       "/home/node/.openclaw/workspace-t",
		ConfigEntry:     json.RawMessage(`{"model":{"primary":"yuno-anthropic/claude-opus-5-5"}}`),
		ExecPolicy:      json.RawMessage(`{"security":"allowlist","ask":"on-miss"}`),
		Files:           []string{"AGENTS.md", "SOUL.md"},
	}
}

func (f *fakeEngine) Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error) {
	f.mu.Lock()
	f.triggers = append(f.triggers, req)
	err := f.triggerErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if f.pool == nil {
		return &store.Run{ID: store.NewID(), Status: store.RunQueued, Trigger: req.Trigger}, nil
	}
	return createTestRun(ctx, f.pool, req.Trigger, req.Input, req.Limits)
}

func (f *fakeEngine) Cancel(ctx context.Context, runID, by string) (*store.Run, error) {
	f.mu.Lock()
	f.cancels = append(f.cancels, runID+"/"+by)
	err := f.cancelErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := store.RequestCancel(ctx, f.pool, runID, by); err != nil {
		return nil, err
	}
	return store.GetRun(ctx, f.pool, runID)
}

func (f *fakeEngine) ResolveRunToken(string) (engine.RunContext, bool) {
	return engine.RunContext{}, false
}

func (f *fakeEngine) AcceptFinish(_ context.Context, token string, result json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, token+" "+string(result))
	return f.finishErr
}

// RejectFinish is the optional count of finish calls the MCP input schema refused.
func (f *fakeEngine) RejectFinish(_ context.Context, token, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejections = append(f.rejections, token+" "+message)
}

var _ engine.Service = (*fakeEngine)(nil)

// createTestRun inserts a one-node workflow and a queued run for it.
func createTestRun(ctx context.Context, pool *pgxpool.Pool, trigger string, input json.RawMessage, limits store.Limits) (*store.Run, error) {
	w := &store.Workflow{Name: "test quick run", Definition: json.RawMessage(`{"nodes":[{"id":"agent","type":"agent"}],"edges":[]}`)}
	if err := store.CreateWorkflow(ctx, pool, w); err != nil {
		return nil, err
	}
	if trigger == "" {
		trigger = store.TriggerManual
	}
	run := &store.Run{WorkflowID: w.ID, WorkflowVersion: 1, WorkflowSnapshot: w.Definition, Trigger: trigger, Input: input, Limits: limits}
	if err := store.CreateRun(ctx, pool, run); err != nil {
		return nil, err
	}
	return store.GetRun(ctx, pool, run.ID)
}

// fakeGateway implements openclaw.Client.
type fakeGateway struct {
	info      openclaw.ConnectInfo
	connected bool
	agentsErr error
}

func (g *fakeGateway) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	if method != "agents.list" {
		return nil, &openclaw.RPCError{Code: "UNKNOWN_METHOD", Message: method}
	}
	if g.agentsErr != nil {
		return nil, g.agentsErr
	}
	return json.RawMessage(`{"defaultId":"main","agents":[{"id":"main","workspace":"/w"},{"id":"coder","workspace":"/w/coder"}]}`), nil
}

func (g *fakeGateway) Subscribe(int) (<-chan openclaw.Event, func()) {
	ch := make(chan openclaw.Event)
	return ch, func() {}
}

func (g *fakeGateway) Info() (openclaw.ConnectInfo, bool) { return g.info, g.connected }

func adminGateway() *fakeGateway {
	return &fakeGateway{connected: true, info: openclaw.ConnectInfo{
		Version: "2026.10.3", ConnID: "conn-1", Methods: 120,
		Scopes: []string{"operator.read", "operator.write", "operator.admin", "operator.approvals"},
	}}
}
