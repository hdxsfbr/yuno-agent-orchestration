//go:build unix

package openclaw

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"yuno/internal/store"
)

// Finding 1: a named pipe the agent creates where the platform writes (or sweeps) is
// replaced, never opened: opening a FIFO with no writer blocks forever.
func TestMaterializeReplacesNamedPipes(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	sk, ex := bundleSkill("tool", store.SkillFile{Path: "run.sh", Content: []byte("r"), Mode: 0o755})
	ex.Edits = map[string]store.AgentFileEdit{"IDENTITY.md": {Name: "IDENTITY.md", Override: strp("operator identity")}}
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)

	fifo := func(rel string) {
		t.Helper()
		_ = os.Remove(filepath.Join(ws, rel))
		if err := syscall.Mkfifo(filepath.Join(ws, rel), 0o644); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	fifo("IDENTITY.md")
	fifo("AGENTS.md")
	fifo("skills/tool/run.sh")
	// An unassigned dir whose SKILL.md is a pipe is not a platform skill and is not opened.
	if err := os.MkdirAll(filepath.Join(ws, "skills", "gone"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo("skills/gone/SKILL.md")

	done := make(chan error, 1)
	go func() {
		a.Role = "changed"
		_, err := MaterializeWith(t.Context(), m.gw, m.dir, a, []store.Skill{sk}, ex)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("materialize blocked on a named pipe")
	}
	if wsRead(t, ws, "IDENTITY.md") != "operator identity" || wsRead(t, ws, "skills/tool/run.sh") != "r" {
		t.Fatal("the pipes were not replaced by the platform's files")
	}
	if fi, _ := os.Lstat(filepath.Join(ws, "AGENTS.md")); fi == nil || !fi.Mode().IsRegular() {
		t.Fatal("AGENTS.md is still a pipe")
	}
}
