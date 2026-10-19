package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// Materialising an agent whose configuration changed must not leave MEMORY.md as the
// bare header: the saved facts are rendered back.
func TestMaterializeRendersMemoryFacts(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(func(a *store.Agent) {
		a.Name = "Rememberer"
		a.MemoryPolicy = store.MemoryPolicy{Enabled: true}
	})
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := memory.Save(ctx, env.pool, &store.MemoryFact{AgentID: a.ID, Scope: store.MemoryScopeAgent, Key: "colour", Value: "teal-marker"}, 100, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Change the configuration so the next materialise rewrites the workspace.
	if _, err := env.pool.Exec(ctx, `UPDATE agents SET role = 'changed role' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAgent(ctx, env.pool, a.ID)
	if err != nil || got.OpenClawAgentID == nil {
		t.Fatalf("agent: %+v %v", got, err)
	}
	b, err := os.ReadFile(filepath.Join(openclaw.WorkspaceDir(env.dir, *got.OpenClawAgentID), "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "teal-marker") {
		t.Fatalf("MEMORY.md lost the saved fact:\n%s", b)
	}
}
