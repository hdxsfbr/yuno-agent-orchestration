package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// testPool is the yuno_test database when DATABASE_URL is set; DB-backed tests skip
// otherwise. Each test uses fresh agents, so other packages' runs never count.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "guardrails tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "guardrails tests: migrate:", err)
			os.Exit(1)
		}
		// The workflows runFor creates are fixtures only; retire any an interrupted run
		// left in the shared workflow list.
		if _, err := pool.Exec(ctx, `UPDATE workflows SET deleted_at = now() WHERE name = $1 AND deleted_at IS NULL`, rateTestWorkflow); err != nil {
			fmt.Fprintln(os.Stderr, "guardrails tests: retire fixtures:", err)
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

const rateTestWorkflow = "rate test"

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	return testPool
}

func newAgent(t *testing.T, name string, perHour int) *store.Agent {
	t.Helper()
	a := &store.Agent{Name: name, Model: "claude-opus-5-5", Guardrails: store.Guardrails{MaxRunsPerHour: perHour}}
	if err := store.CreateAgent(context.Background(), needDB(t), a); err != nil {
		t.Fatal(err)
	}
	return a
}

// runFor inserts a run whose snapshot names the agents, created at the given time.
func runFor(t *testing.T, q store.Querier, createdAt time.Time, agents ...*store.Agent) {
	t.Helper()
	ctx := context.Background()
	def := workflow.Definition{Nodes: []workflow.Node{{ID: "start", Kind: workflow.KindStart}}, Edges: []workflow.Edge{}}
	for i, a := range agents {
		def.Nodes = append(def.Nodes, workflow.Node{ID: fmt.Sprintf("n%d", i), Kind: workflow.KindAgent, AgentID: a.ID})
	}
	snap, _ := json.Marshal(def)
	w := &store.Workflow{Name: rateTestWorkflow, Definition: snap}
	if err := store.CreateWorkflow(ctx, q, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DeleteWorkflow(context.Background(), testPool, w.ID) })
	r := &store.Run{WorkflowID: w.ID, WorkflowVersion: 1, WorkflowSnapshot: snap, Trigger: store.TriggerManual}
	if err := store.CreateRun(ctx, q, r); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, `UPDATE runs SET created_at=$2 WHERE id=$1`, r.ID, createdAt); err != nil {
		t.Fatal(err)
	}
}

// T5.11.2: with max_runs_per_hour n, the trigger after n-1 runs in the hour passes and
// the one after n is refused with the rule's message.
func TestRateLimitAtNAndNMinusOne(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	limited := newAgent(t, "Limited", 3)
	other := newAgent(t, "Other", 0)

	runFor(t, pool, now.Add(-RateWindow-time.Second), limited) // outside the window
	runFor(t, pool, now.Add(-59*time.Minute), limited, other)
	runFor(t, pool, now.Add(-time.Minute), other) // another agent's run
	runFor(t, pool, now.Add(-30*time.Second), limited)
	if n, err := RunsInWindow(ctx, pool, limited.ID, now); err != nil || n != 2 {
		t.Fatalf("runs in window = %d, %v; want 2", n, err)
	}
	// n-1 = 2 runs: allowed.
	if err := CheckRateLimit(ctx, pool, []*store.Agent{other, limited}, now); err != nil {
		t.Fatalf("at n-1: %v", err)
	}
	runFor(t, pool, now.Add(-time.Second), limited)
	// n = 3 runs: refused.
	err := CheckRateLimit(ctx, pool, []*store.Agent{other, limited}, now)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.AgentID != limited.ID || rl.Limit != 3 {
		t.Fatalf("at n: %v", err)
	}
	if err.Error() != "rate limit: 3 runs/hour for agent Limited" {
		t.Fatalf("message = %q", err.Error())
	}
	// The window rolls: an hour after the oldest counted run, there is room again.
	if err := CheckRateLimit(ctx, pool, []*store.Agent{limited}, now.Add(59*time.Minute+time.Second)); err != nil {
		t.Fatalf("after the window moved: %v", err)
	}
	// An agent without a limit is never refused.
	if err := CheckRateLimit(ctx, pool, []*store.Agent{other}, now); err != nil {
		t.Fatalf("unlimited agent: %v", err)
	}
}

// Rule 2 consistency: the check and the run insert share a transaction, and a second
// trigger for the same agent waits for the first to commit, so both cannot take the
// last slot.
func TestRateLimitSerialisesTriggers(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := newAgent(t, "Once", 1)

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx) //nolint:errcheck
	if err := CheckRateLimit(ctx, tx1, []*store.Agent{a}, now); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	runFor(t, tx1, now, a)

	second := make(chan error, 1)
	go func() {
		second <- pgx.BeginFunc(ctx, pool, func(tx2 pgx.Tx) error {
			return CheckRateLimit(ctx, tx2, []*store.Agent{a}, now)
		})
	}()
	select {
	case err := <-second:
		t.Fatalf("second trigger did not wait for the first: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second:
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("second trigger after the first committed: %v, want a rate limit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second trigger still waiting")
	}
}

func TestParticipatingAgents(t *testing.T) {
	pool := needDB(t)
	a, b := newAgent(t, "P1", 0), newAgent(t, "P2", 0)
	def := &workflow.Definition{Nodes: []workflow.Node{
		{ID: "x", Kind: workflow.KindAgent, AgentID: b.ID},
		{ID: "y", Kind: workflow.KindAgent, AgentID: "no-such-agent"},
		{ID: "z", Kind: workflow.KindAgent, AgentID: a.ID},
		{ID: "w", Kind: workflow.KindAgent, AgentID: b.ID},
	}}
	got, err := ParticipatingAgents(context.Background(), pool, def)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("agents = %v", got)
	}
}
