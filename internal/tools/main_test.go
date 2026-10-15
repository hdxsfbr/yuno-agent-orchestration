package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// testPool is the yuno_test database when DATABASE_URL is set; DB-backed tests skip
// otherwise. Tests only add rows with fresh ids, because other packages share it.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "tools tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "tools tests: migrate:", err)
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

// fixture is an agent, a run and one node visit of it, with a workspace on disk.
type fixture struct {
	agent     *store.Agent
	run       *store.Run
	visit     *store.RunNode
	stateDir  string
	workspace string
}

// newFixture creates the rows a platform tool call belongs to. memory enables the
// agent's memory policy with the given max_facts.
func newFixture(t *testing.T, pool *pgxpool.Pool, memory *store.MemoryPolicy) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{stateDir: t.TempDir()}
	// The tail of a UUIDv7 is random; its head is a timestamp shared by fast tests.
	ocID := "agt" + strings.ReplaceAll(store.NewID(), "-", "")[20:]
	f.agent = &store.Agent{Name: "tools-test", Model: "claude-opus-5-5", OpenClawAgentID: &ocID}
	if memory != nil {
		f.agent.MemoryPolicy = *memory
	}
	must(t, store.CreateAgent(ctx, pool, f.agent))
	wf := &store.Workflow{Name: "tools test", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, pool, wf))
	f.run = &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		Status: store.RunRunning}
	must(t, store.CreateRun(ctx, pool, f.run))
	f.visit = f.newVisit(t, pool)
	f.workspace = filepath.Join(f.stateDir, "workspace-"+ocID)
	must(t, os.MkdirAll(filepath.Join(f.workspace, "runs", f.run.ID), 0o755))
	return f
}

func (f *fixture) newVisit(t *testing.T, pool *pgxpool.Pool) *store.RunNode {
	t.Helper()
	now := time.Now().UTC()
	n := &store.RunNode{RunID: f.run.ID, NodeID: "deployer", AgentID: &f.agent.ID, Status: store.NodeRunning, StartedAt: &now}
	must(t, store.CreateRunNode(context.Background(), pool, n))
	return n
}

// writeRunFile writes a file under the run directory.
func (f *fixture) writeRunFile(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.workspace, "runs", f.run.ID, filepath.FromSlash(rel))
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o644))
}

// call builds a call for a visit with its idempotency key and a fresh tool call id.
func (f *fixture) call(t *testing.T, visit *store.RunNode, tool string, input string) Call {
	t.Helper()
	key, err := IdempotencyKey(f.run.ID, visit.ID, tool, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	return Call{RunID: f.run.ID, NodeVisitID: visit.ID, NodeID: visit.NodeID, AgentID: f.agent.ID,
		OpenClawAgentID: *f.agent.OpenClawAgentID, Workspace: f.workspace, Input: json.RawMessage(input),
		IdempotencyKey: key, ToolCallID: store.NewID()}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// noSleep records backoff waits without waiting.
type noSleep struct{ waits []time.Duration }

func (n *noSleep) sleep(_ context.Context, d time.Duration) error {
	n.waits = append(n.waits, d)
	return nil
}
