package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/events"
	"yuno/internal/store"
)

// The engine's DB tests run in their own schema of the test database so they never
// collide with other packages' tests running in parallel against the same database.
const testSchema = "yuno_engine_test"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("DATABASE_URL")
	if url != "" {
		if err := setupDB(url); err != nil {
			fmt.Fprintln(os.Stderr, "engine tests: database setup failed:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func setupDB(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	base, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	_, err = base.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+testSchema)
	base.Close()
	if err != nil {
		return err
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	testPool, err = store.Open(ctx, url+sep+"search_path="+testSchema)
	if err != nil {
		return err
	}
	return store.Migrate(ctx, testPool, slog.New(slog.DiscardHandler))
}

// needDB skips without a database and empties the engine's tables otherwise.
func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	_, err := testPool.Exec(context.Background(), `TRUNCATE agents, skills, workflows, runs, run_nodes, model_calls,
		tool_calls, side_effects, approvals, agent_messages, spans, run_events, channel_bindings, conversations,
		conversation_messages, projects CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return testPool
}

func testConfig() config.Config {
	return config.Config{
		EngineWorkers:         4,
		EngineStaleAfter:      5 * time.Minute,
		ToolOutputMaxBytes:    32768,
		MaxSteps:              200,
		MaxDurationSeconds:    3600,
		MaxNodeIterations:     50,
		MaxLoopVisits:         10,
		MaxCostUSD:            20,
		MaxToolCalls:          300,
		ApprovalTimeoutMaxSec: 604800,
		ProjectTokenKey:       "engine-test-project-token-key",
	}
}

type testEnv struct {
	t     *testing.T
	pool  *pgxpool.Pool
	e     *Engine
	fc    *fakeClient
	clock *FakeClock
	dir   string
}

// newEnv builds an engine on the test schema with a fake gateway and a fake clock.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	pool := needDB(t)
	fc := newFakeClient()
	clock := NewFakeClock(time.Now())
	hub := events.NewHub()
	rec := &events.Recorder{Hub: hub, Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		return store.AppendRunEvent(ctx, pool, runID, typ, data)
	}}
	dir := t.TempDir()
	e := New(Deps{Cfg: testConfig(), Log: slog.New(slog.DiscardHandler), Pool: pool, Gateway: fc, Hub: hub,
		Recorder: rec, Clock: clock, StateDir: dir})
	e.tickInterval = 20 * time.Millisecond
	return &testEnv{t: t, pool: pool, e: e, fc: fc, clock: clock, dir: dir}
}

func (env *testEnv) agent(mut func(a *store.Agent)) *store.Agent {
	env.t.Helper()
	a := &store.Agent{Name: "Test Agent", Role: "tester", SystemPrompt: "Be brief.", Model: "claude-opus-5-5",
		ToolAllowlist: []string{"exec", "finish"}}
	if mut != nil {
		mut(a)
	}
	if err := store.CreateAgent(context.Background(), env.pool, a); err != nil {
		env.t.Fatal(err)
	}
	return a
}

// start triggers a quick run, claims it and executes it in the background. The
// returned channel closes when the run is terminal.
func (env *testEnv) start(req TriggerRequest) (*store.Run, <-chan struct{}) {
	env.t.Helper()
	ctx := context.Background()
	run, err := env.e.Trigger(ctx, req)
	if err != nil {
		env.t.Fatalf("trigger: %v", err)
	}
	claimed, err := store.ClaimQueuedRun(ctx, env.pool)
	if err != nil || claimed.ID != run.ID {
		env.t.Fatalf("claim: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.e.runRun(ctx, claimed)
	}()
	return run, done
}

func (env *testEnv) wait(done <-chan struct{}, runID string) *store.Run {
	env.t.Helper()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		env.t.Fatal("run did not finish")
	}
	r, err := store.GetRun(context.Background(), env.pool, runID)
	if err != nil {
		env.t.Fatal(err)
	}
	return r
}

// eventually polls cond until it holds or the timeout passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (env *testEnv) count(sql string, args ...any) int {
	env.t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		env.t.Fatal(err)
	}
	return n
}
