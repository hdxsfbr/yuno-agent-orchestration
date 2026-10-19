package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/store"
)

func strp(s string) *string { return &s }

func wsRead(t *testing.T, ws, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

// rematerialize pushes with extras and records the hash on the agent, as the engine does.
func rematerialize(t *testing.T, m *matEnv, a *store.Agent, skills []store.Skill, ex Extras) string {
	t.Helper()
	h, err := MaterializeWith(context.Background(), m.gw, m.dir, a, skills, ex)
	if err != nil {
		t.Fatal(err)
	}
	a.MaterializedHash = h
	return h
}

// T5.27.2: an addition follows the generated part, survives a re-materialise, and an empty
// addition removes the section; the platform's rules stay.
func TestMaterializeAdditionAfterGeneratedPart(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	h0 := rematerialize(t, m, a, nil, Extras{})
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)

	edits := map[string]store.AgentFileEdit{
		"AGENTS.md": {Name: "AGENTS.md", Addition: strp("Always greet in Spanish.")},
		"SOUL.md":   {Name: "SOUL.md", Addition: strp("Be kind.")},
		"MEMORY.md": {Name: "MEMORY.md", Addition: strp("Remember the budget.")},
	}
	ex := Extras{Edits: edits}
	if DesiredHashWith(m.dir, a, nil, ex) == h0 {
		t.Fatal("an addition must change the desired state")
	}
	h1 := rematerialize(t, m, a, nil, ex)
	if got, want := wsRead(t, ws, "AGENTS.md"), AgentsMD+"\n\n## Operator additions\n\nAlways greet in Spanish."; got != want {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if got := wsRead(t, ws, "SOUL.md"); !strings.HasPrefix(got, "# Quick Coder\n") || !strings.HasSuffix(got, "\n\n## Operator additions\n\nBe kind.") {
		t.Fatalf("SOUL.md = %q", got)
	}
	if got, want := wsRead(t, ws, "MEMORY.md"), "# Memory\n\n\n## Operator additions\n\nRemember the budget."; got != want {
		t.Fatalf("MEMORY.md = %q", got)
	}
	// The rendered MEMORY.md (facts) carries the same addition after the facts.
	if got := ComposeMemory("# Memory\n\n- k: v\n", edits); got != "# Memory\n\n- k: v\n\n\n## Operator additions\n\nRemember the budget." {
		t.Fatalf("composed memory = %q", got)
	}
	if got := ComposeMemory("# Memory\n", nil); got != "# Memory\n" {
		t.Fatalf("no addition: %q", got)
	}

	// A change elsewhere re-materialises; the addition is still there.
	a.Role = "Writes Go."
	if DesiredHashWith(m.dir, a, nil, ex) == h1 {
		t.Fatal("role change must change the hash")
	}
	rematerialize(t, m, a, nil, ex)
	if got := wsRead(t, ws, "AGENTS.md"); !strings.HasSuffix(got, "Always greet in Spanish.") || !strings.Contains(got, "yuno__finish") {
		t.Fatalf("AGENTS.md after re-materialise = %q", got)
	}

	// Emptying the addition removes the section.
	delete(edits, "AGENTS.md")
	rematerialize(t, m, a, nil, Extras{Edits: edits})
	if got := wsRead(t, ws, "AGENTS.md"); got != AgentsMD {
		t.Fatalf("AGENTS.md without addition = %q", got)
	}
	// A blank addition is no addition.
	if WithAddition("x\n", "  \n") != "x\n" {
		t.Fatal("blank addition must not add a section")
	}
}

// T5.27.3: a runtime override is written over OpenClaw's file; without one the file is
// never touched, and removing the override leaves the last text in place.
func TestMaterializeRuntimeOverride(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	rematerialize(t, m, a, nil, Extras{})
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)

	// OpenClaw seeds a runtime file; the platform leaves it.
	if err := os.WriteFile(filepath.Join(ws, "IDENTITY.md"), []byte("openclaw wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Role = "changed"
	rematerialize(t, m, a, nil, Extras{})
	if got := wsRead(t, ws, "IDENTITY.md"); got != "openclaw wrote this" {
		t.Fatalf("untouched runtime file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "USER.md")); !os.IsNotExist(err) {
		t.Fatalf("USER.md must not be created by the platform: %v", err)
	}

	ex := Extras{Edits: map[string]store.AgentFileEdit{
		"IDENTITY.md": {Name: "IDENTITY.md", Override: strp("operator identity")},
		"USER.md":     {Name: "USER.md", Override: strp("")}, // emptied on purpose
	}}
	rematerialize(t, m, a, nil, ex)
	if got := wsRead(t, ws, "IDENTITY.md"); got != "operator identity" {
		t.Fatalf("override = %q", got)
	}
	if got := wsRead(t, ws, "USER.md"); got != "" {
		t.Fatalf("empty override = %q", got)
	}

	// OpenClaw rewrites it later; removing the override stops the platform writing it.
	if err := os.WriteFile(filepath.Join(ws, "IDENTITY.md"), []byte("openclaw again"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Role = "changed again"
	rematerialize(t, m, a, nil, Extras{})
	if got := wsRead(t, ws, "IDENTITY.md"); got != "openclaw again" {
		t.Fatalf("after removing the override = %q", got)
	}
}

func bundleSkill(name string, files ...store.SkillFile) (store.Skill, Extras) {
	return store.Skill{ID: "id-" + name, Name: name, Description: "d", Body: "b\n"},
		Extras{SkillFiles: map[string][]store.SkillFile{name: files}}
}

// T5.27.9: bundle files are written with their modes, the hash covers digests, and files a
// skill dropped, and skills no longer assigned, are removed.
func TestMaterializeBundleFiles(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	script := []byte("#!/bin/sh\necho hi\n")
	bin := []byte{0x00, 0xff, 0x10, 0x80, 0x00}
	sk, ex := bundleSkill("tool",
		store.SkillFile{Path: "scripts/run.sh", Content: script, Mode: 0o755},
		store.SkillFile{Path: "data/blob.bin", Content: bin, Mode: 0o644},
		store.SkillFile{Path: "REFERENCE.md", Content: []byte("ref"), Mode: 0o644},
	)
	other, otherEx := bundleSkill("other", store.SkillFile{Path: "x.txt", Content: []byte("x"), Mode: 0o644})
	ex.SkillFiles["other"] = otherEx.SkillFiles["other"]

	h0 := DesiredHashWith(m.dir, a, nil, Extras{})
	h1 := rematerialize(t, m, a, []store.Skill{sk, other}, ex)
	if h1 == h0 {
		t.Fatal("bundle files must change the hash")
	}
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	for rel, want := range map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"skills/tool/scripts/run.sh": {script, 0o755},
		"skills/tool/data/blob.bin":  {bin, 0o644},
		"skills/tool/REFERENCE.md":   {[]byte("ref"), 0o644},
		"skills/other/x.txt":         {[]byte("x"), 0o644},
	} {
		b, err := os.ReadFile(filepath.Join(ws, rel))
		fi, _ := os.Stat(filepath.Join(ws, rel))
		if err != nil || string(b) != string(want.data) || fi.Mode().Perm() != want.mode {
			t.Fatalf("%s = %q mode %v err %v", rel, b, fi, err)
		}
	}
	if !strings.HasPrefix(wsRead(t, ws, "skills/tool/SKILL.md"), "---\nname: tool\n") {
		t.Fatal("SKILL.md missing")
	}

	// The hash covers size, digest and mode, not bytes: same size, different content
	// changes it; same content, different mode changes it; the JSON holds no bytes.
	d := planWith(m.dir, a, []store.Skill{sk, other}, ex)
	if f := d.Bundle[filepath.Join("skills", "tool", "scripts", "run.sh")]; f.Size != len(script) || len(f.SHA256) != 64 || f.Mode != 0o755 {
		t.Fatalf("bundle entry = %+v", f)
	}
	_, same := bundleSkill("tool", store.SkillFile{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\necho ho\n"), Mode: 0o755},
		store.SkillFile{Path: "data/blob.bin", Content: bin, Mode: 0o644}, store.SkillFile{Path: "REFERENCE.md", Content: []byte("ref"), Mode: 0o644})
	same.SkillFiles["other"] = ex.SkillFiles["other"]
	if DesiredHashWith(m.dir, a, []store.Skill{sk, other}, same) == h1 {
		t.Fatal("different content of the same size must change the hash")
	}
	_, modeEx := bundleSkill("tool", store.SkillFile{Path: "scripts/run.sh", Content: script, Mode: 0o644},
		store.SkillFile{Path: "data/blob.bin", Content: bin, Mode: 0o644}, store.SkillFile{Path: "REFERENCE.md", Content: []byte("ref"), Mode: 0o644})
	modeEx.SkillFiles["other"] = ex.SkillFiles["other"]
	if DesiredHashWith(m.dir, a, []store.Skill{sk, other}, modeEx) == h1 {
		t.Fatal("a mode change must change the hash")
	}

	// The skill drops two files; the mode of one changes on disk.
	_, ex2 := bundleSkill("tool", store.SkillFile{Path: "scripts/run.sh", Content: script, Mode: 0o644})
	ex2.SkillFiles["other"] = ex.SkillFiles["other"]
	rematerialize(t, m, a, []store.Skill{sk, other}, ex2)
	for _, gone := range []string{"skills/tool/data/blob.bin", "skills/tool/data", "skills/tool/REFERENCE.md"} {
		if _, err := os.Stat(filepath.Join(ws, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed: %v", gone, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(ws, "skills/tool/scripts/run.sh")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("run.sh after mode change: %v %v", fi, err)
	}

	// A skill dir the platform did not write is kept; an unassigned platform skill goes.
	if err := os.MkdirAll(filepath.Join(ws, "skills", "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "skills", "mine", "SKILL.md"), []byte("own"), 0o644); err != nil {
		t.Fatal(err)
	}
	rematerialize(t, m, a, []store.Skill{sk}, ex2)
	if _, err := os.Stat(filepath.Join(ws, "skills", "other")); !os.IsNotExist(err) {
		t.Fatalf("unassigned skill dir must be removed: %v", err)
	}
	if wsRead(t, ws, "skills/mine/SKILL.md") != "own" {
		t.Fatal("a skill dir without a platform SKILL.md must be kept")
	}
	if wsRead(t, ws, "skills/tool/scripts/run.sh") != string(script) {
		t.Fatal("the assigned skill's file must stay")
	}
}

// Paths that leave the skill directory are never written, whatever the database holds, and
// a symlink the agent planted in a skill dir is not followed by the sweep.
func TestMaterializeBundleRefusesUnsafePaths(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	sk, ex := bundleSkill("tool",
		store.SkillFile{Path: "../escape.txt", Content: []byte("x"), Mode: 0o644},
		store.SkillFile{Path: "/abs.txt", Content: []byte("x"), Mode: 0o644},
		store.SkillFile{Path: "a/../../b.txt", Content: []byte("x"), Mode: 0o644},
		store.SkillFile{Path: `back\slash`, Content: []byte("x"), Mode: 0o644},
		store.SkillFile{Path: "SKILL.md", Content: []byte("evil"), Mode: 0o644},
		store.SkillFile{Path: "ok.txt", Content: []byte("fine"), Mode: 0o644},
	)
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	if wsRead(t, ws, "skills/tool/ok.txt") != "fine" {
		t.Fatal("the safe file must be written")
	}
	if !strings.HasPrefix(wsRead(t, ws, "skills/tool/SKILL.md"), "---\nname: tool\n") {
		t.Fatal("SKILL.md must stay the platform's")
	}
	for _, p := range []string{filepath.Join(ws, "skills", "escape.txt"), filepath.Join(ws, "escape.txt"), filepath.Join(ws, "abs.txt")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist: %v", p, err)
		}
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "skills", "tool", "link")); err != nil {
		t.Fatal(err)
	}
	a.Role = "again"
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if wsRead(t, outside, "keep.txt") != "keep" {
		t.Fatal("the sweep must not follow a symlink out of the workspace")
	}
	if fi, err := os.Lstat(filepath.Join(ws, "skills", "tool", "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("a file the agent made stays, untouched, as the link it is: %v", err)
	}
}
