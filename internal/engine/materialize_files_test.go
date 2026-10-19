package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// DESIGN 5.27 through the engine: the agent's file edits and its skills' bundle files are
// loaded, written, part of the hash, and rendered MEMORY.md keeps the addition.
func TestMaterializeAgentFilesAndBundles(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	sk := &store.Skill{Name: fmt.Sprintf("bundle-%d", time.Now().UnixNano()), Description: "d", Body: "body\n"}
	if err := store.CreateSkill(ctx, env.pool, sk); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSkillFile(ctx, env.pool, sk.ID, &store.SkillFile{Path: "bin/go.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	a := env.agent(func(a *store.Agent) {
		a.Skills = []string{sk.Name}
		a.MemoryPolicy = store.MemoryPolicy{Enabled: true}
	})
	if err := store.UpsertAgentFileAddition(ctx, env.pool, a.ID, "AGENTS.md", "Operator rule."); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgentFileAddition(ctx, env.pool, a.ID, "MEMORY.md", "Pinned note."); err != nil {
		t.Fatal(err)
	}
	ov := "custom heartbeat"
	if err := store.UpsertAgentFileOverride(ctx, env.pool, a.ID, "HEARTBEAT.md", &ov); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetAgent(ctx, env.pool, a.ID)
	if _, skills, err := env.e.loadAgent(ctx, a.ID); err != nil || got.MaterializedHash != env.e.desiredHash(ctx, got, skills) {
		t.Fatalf("recorded hash does not match the desired hash: %v", err)
	}
	ws := openclaw.WorkspaceDir(env.dir, *got.OpenClawAgentID)
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(ws, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if !strings.HasSuffix(read("AGENTS.md"), "\n\n## Operator additions\n\nOperator rule.") {
		t.Fatalf("AGENTS.md = %q", read("AGENTS.md"))
	}
	if read("HEARTBEAT.md") != "custom heartbeat" {
		t.Fatalf("HEARTBEAT.md = %q", read("HEARTBEAT.md"))
	}
	fi, err := os.Stat(filepath.Join(ws, "skills", sk.Name, "bin", "go.sh"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("bundle script: %v %v", fi, err)
	}
	if !strings.HasSuffix(read("MEMORY.md"), "Pinned note.") {
		t.Fatalf("MEMORY.md = %q", read("MEMORY.md"))
	}

	// A fact is rendered before the addition, by the engine's and the package's writers.
	if err := memory.Save(ctx, env.pool, &store.MemoryFact{AgentID: a.ID, Scope: store.MemoryScopeAgent, Key: "colour", Value: "teal-marker"}, 100, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := openclaw.RenderMemory(ctx, env.pool, env.dir, got, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	md := read("MEMORY.md")
	if i, j := strings.Index(md, "teal-marker"), strings.Index(md, "Pinned note."); i < 0 || j < i {
		t.Fatalf("facts must come before the addition: %q", md)
	}

	// Editing a bundle file or an addition changes the desired hash and the next
	// materialise converges the workspace.
	if err := store.PutSkillFile(ctx, env.pool, sk.ID, &store.SkillFile{Path: "bin/go.sh", Content: []byte("#!/bin/sh\necho 2\n"), Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	a2, skills, err := env.e.loadAgent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a2.MaterializedHash == env.e.desiredHash(ctx, a2, skills) {
		t.Fatal("a bundle edit must change the desired hash")
	}
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read("skills/"+sk.Name+"/bin/go.sh"), "echo 2") {
		t.Fatal("the edited script was not written")
	}
	if err := store.DeleteSkillFile(ctx, env.pool, sk.ID, "bin/go.sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", sk.Name, "bin")); !os.IsNotExist(err) {
		t.Fatalf("dropped file's directory must go: %v", err)
	}
}

// Finding 5 through the engine: the sync of an agent whose hash is unchanged still writes
// the operator's overrides back after OpenClaw rewrote the file.
func TestSyncAgentRestoresOverrideWithUnchangedHash(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	ov := "custom heartbeat"
	if err := store.UpsertAgentFileOverride(ctx, env.pool, a.ID, "HEARTBEAT.md", &ov); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.MaterializeAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetAgent(ctx, env.pool, a.ID)
	file := filepath.Join(openclaw.WorkspaceDir(env.dir, *got.OpenClawAgentID), "HEARTBEAT.md")
	if err := os.WriteFile(file, []byte("openclaw rewrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	var res AgentSync
	env.e.syncAgent(ctx, a.ID, map[string]bool{*got.OpenClawAgentID: true}, &res)
	if b, _ := os.ReadFile(file); string(b) != ov {
		t.Fatalf("HEARTBEAT.md = %q, want the override back", b)
	}
	if res.Materialized != 0 || res.MaterializeFailed != 0 {
		t.Fatalf("an unchanged agent was pushed: %+v", res)
	}
}
