package openclaw

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"yuno/internal/store"
)

// Wave 12 review fixes 3, 5, 6 and 12 at the workspace.

func ls(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	return out
}

// Finding 3: a bundle path that was a file and is a folder now (or the reverse) is replaced.
func TestMaterializeBundleFileFolderSwap(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	sk, ex := bundleSkill("tool", store.SkillFile{Path: "data", Content: []byte("a file"), Mode: 0o644})
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	if wsRead(t, ws, "skills/tool/data") != "a file" {
		t.Fatal("setup")
	}

	// The file becomes a folder.
	sk, ex = bundleSkill("tool", store.SkillFile{Path: "data/one.txt", Content: []byte("1"), Mode: 0o644})
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if wsRead(t, ws, "skills/tool/data/one.txt") != "1" {
		t.Fatal("file to folder")
	}

	// The folder becomes a file.
	sk, ex = bundleSkill("tool", store.SkillFile{Path: "data", Content: []byte("a file again"), Mode: 0o644})
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if wsRead(t, ws, "skills/tool/data") != "a file again" {
		t.Fatal("folder to file")
	}

	// A file the agent put where a parent folder belongs, and a folder where a file
	// belongs, with no manifest entry to prune: the bundle still wins.
	sk, ex = bundleSkill("tool", store.SkillFile{Path: "docs/guide.md", Content: []byte("g"), Mode: 0o644}, store.SkillFile{Path: "run.sh", Content: []byte("r"), Mode: 0o755})
	if err := os.WriteFile(filepath.Join(ws, "skills/tool/docs"), []byte("agent file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "skills/tool/run.sh/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if wsRead(t, ws, "skills/tool/docs/guide.md") != "g" || wsRead(t, ws, "skills/tool/run.sh") != "r" {
		t.Fatalf("conflicting entries not cleared: %v", ls(t, ws))
	}
}

// Finding 5: with an unchanged hash the gateway is not called, but the workspace files come
// back after OpenClaw (or the agent) rewrote them.
func TestMaterializeUnchangedHashRestoresOverrides(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	ex := Extras{Edits: map[string]store.AgentFileEdit{
		"IDENTITY.md": {Name: "IDENTITY.md", Override: strp("operator identity")},
		"AGENTS.md":   {Name: "AGENTS.md", Addition: strp("Rule.")},
	}}
	h := rematerialize(t, m, a, nil, ex)
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	for name, body := range map[string]string{"IDENTITY.md": "openclaw rewrote it", "AGENTS.md": "agent rewrote it"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := len(m.f.allCalls())
	got, err := MaterializeWith(context.Background(), m.gw, m.dir, a, nil, ex)
	if err != nil || got != h {
		t.Fatalf("hash %s %v", got, err)
	}
	if n := len(m.f.allCalls()) - before; n != 0 {
		t.Fatalf("%d gateway calls on an unchanged agent", n)
	}
	if wsRead(t, ws, "IDENTITY.md") != "operator identity" || !strings.HasSuffix(wsRead(t, ws, "AGENTS.md"), "Rule.") {
		t.Fatal("overrides and additions not restored")
	}

	// The same through SyncWorkspace, which the engine calls on every visit and sync.
	if err := os.WriteFile(filepath.Join(ws, "IDENTITY.md"), []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncWorkspace(m.dir, a, nil, ex); err != nil {
		t.Fatal(err)
	}
	if wsRead(t, ws, "IDENTITY.md") != "operator identity" {
		t.Fatal("SyncWorkspace did not restore the override")
	}
}

// Finding 6: only the paths the platform listed are pruned; a file the agent made in an
// assigned skill's folder stays, and the manifest is the platform's.
func TestMaterializeKeepsAgentFilesInSkillFolder(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	sk, ex := bundleSkill("tool",
		store.SkillFile{Path: "old/a.txt", Content: []byte("a"), Mode: 0o644},
		store.SkillFile{Path: "keep.txt", Content: []byte("k"), Mode: 0o644})
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	if got := wsRead(t, ws, "skills/tool/"+store.SkillManifestFile); got != "keep.txt\nold/a.txt\n" {
		t.Fatalf("manifest = %q", got)
	}
	mine := filepath.Join(ws, "skills/tool/notes-by-agent.md")
	if err := os.WriteFile(mine, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "skills/tool/scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "skills/tool/scratch/x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	sk, ex = bundleSkill("tool", store.SkillFile{Path: "keep.txt", Content: []byte("k2"), Mode: 0o644})
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if _, err := os.Stat(filepath.Join(ws, "skills/tool/old")); !os.IsNotExist(err) {
		t.Fatalf("the dropped file and its folder must go: %v", err)
	}
	if wsRead(t, ws, "skills/tool/notes-by-agent.md") != "mine" || wsRead(t, ws, "skills/tool/scratch/x") != "x" {
		t.Fatalf("the agent's files must stay: %v", ls(t, ws))
	}
	if got := wsRead(t, ws, "skills/tool/"+store.SkillManifestFile); got != "keep.txt\n" {
		t.Fatalf("manifest = %q", got)
	}
	if bundlePath("tool", store.SkillManifestFile) != "" {
		t.Fatal("the manifest must not be a bundle file")
	}

	// A manifest the agent edited cannot make the platform delete outside the skill folder.
	if err := os.WriteFile(filepath.Join(ws, "skills/tool/"+store.SkillManifestFile), []byte("keep.txt\n../../AGENTS.md\n../other/SKILL.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Role = "changed"
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if !strings.Contains(wsRead(t, ws, "AGENTS.md"), "yuno__finish") {
		t.Fatal("AGENTS.md must be intact")
	}

	// No bundle files left: the manifest goes too, the agent's files still stay.
	sk, ex = bundleSkill("tool")
	a.Role = "changed again"
	rematerialize(t, m, a, []store.Skill{sk}, ex)
	if _, err := os.Stat(filepath.Join(ws, "skills/tool/"+store.SkillManifestFile)); !os.IsNotExist(err) {
		t.Fatalf("empty bundle keeps a manifest: %v", err)
	}
	if wsRead(t, ws, "skills/tool/notes-by-agent.md") != "mine" {
		t.Fatal("the agent's file must stay")
	}
}

// Finding 12: the hash and the unchanged-workspace path use digests; the bytes are loaded
// once, and only for a file that has to be written.
func TestBundleContentLoadedOnlyToWrite(t *testing.T) {
	m := newMatEnv(t)
	a := testAgent()
	script, other := []byte("#!/bin/sh\necho 1\n"), []byte("other")
	sum := func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }
	loads := 0
	sk := store.Skill{ID: "id-tool", Name: "tool", Description: "d", Body: "b\n"}
	mk := func() Extras {
		return Extras{
			SkillFiles: map[string][]store.SkillFile{"tool": {
				{Path: "run.sh", Mode: 0o755, Size: len(script), SHA256: sum(script)},
				{Path: "o.txt", Mode: 0o644, Size: len(other), SHA256: sum(other)},
			}},
			Load: func() (map[string]map[string][]byte, error) {
				loads++
				return map[string]map[string][]byte{"tool": {"run.sh": script, "o.txt": other}}, nil
			},
		}
	}
	withContent := Extras{SkillFiles: map[string][]store.SkillFile{"tool": {
		{Path: "run.sh", Content: script, Mode: 0o755}, {Path: "o.txt", Content: other, Mode: 0o644}}}}
	if DesiredHashWith(m.dir, a, []store.Skill{sk}, mk()) != DesiredHashWith(m.dir, a, []store.Skill{sk}, withContent) {
		t.Fatal("a digest-only plan must hash like one with the bytes")
	}
	if loads != 0 {
		t.Fatalf("hashing loaded the content %d times", loads)
	}

	rematerialize(t, m, a, []store.Skill{sk}, mk())
	if loads != 1 {
		t.Fatalf("first write loaded %d times, want 1", loads)
	}
	// Everything on disk matches: no load, however often the workspace is synced.
	for i := 0; i < 3; i++ {
		if err := SyncWorkspace(m.dir, a, []store.Skill{sk}, mk()); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 1 {
		t.Fatalf("an in-sync workspace loaded the content (%d)", loads)
	}
	// One file differs: one load, and the file is restored with its mode.
	ws := WorkspaceDir(m.dir, *a.OpenClawAgentID)
	if err := os.WriteFile(filepath.Join(ws, "skills/tool/o.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncWorkspace(m.dir, a, []store.Skill{sk}, mk()); err != nil {
		t.Fatal(err)
	}
	if loads != 2 || wsRead(t, ws, "skills/tool/o.txt") != "other" {
		t.Fatalf("loads %d, o.txt %q", loads, wsRead(t, ws, "skills/tool/o.txt"))
	}
	// Content that no longer matches the digest is refused, not written.
	bad := mk()
	bad.Load = func() (map[string]map[string][]byte, error) {
		return map[string]map[string][]byte{"tool": {"run.sh": script, "o.txt": []byte("different")}}, nil
	}
	if err := os.WriteFile(filepath.Join(ws, "skills/tool/o.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncWorkspace(m.dir, a, []store.Skill{sk}, bad); err == nil {
		t.Fatal("bytes that do not match the digest were written")
	}
}

// fakeQ records the SQL it is asked and answers every query with no rows.
type fakeQ struct{ sql []string }

func (f *fakeQ) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeQ) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	f.sql = append(f.sql, sql)
	return noRows{}, nil
}
func (f *fakeQ) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

type noRows struct{}

func (noRows) Close()                                       {}
func (noRows) Err() error                                   { return nil }
func (noRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (noRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (noRows) Next() bool                                   { return false }
func (noRows) Scan(...any) error                            { return nil }
func (noRows) Values() ([]any, error)                       { return nil, nil }
func (noRows) RawValues() [][]byte                          { return nil }
func (noRows) Conn() *pgx.Conn                              { return nil }
func (noRows) TypeMap() *pgtype.Map                         { return nil }

func TestLoadExtrasQueriesDigestsOnce(t *testing.T) {
	q := &fakeQ{}
	skills := []store.Skill{{ID: "s1", Name: "a"}, {ID: "s2", Name: "b"}, {ID: "s3", Name: "c"}}
	ex, err := LoadExtras(context.Background(), q, "agent", skills)
	if err != nil {
		t.Fatal(err)
	}
	var fileQueries []string
	for _, s := range q.sql {
		if strings.Contains(s, "skill_files") {
			fileQueries = append(fileQueries, s)
		}
	}
	if len(fileQueries) != 1 {
		t.Fatalf("%d skill_files queries for 3 skills, want 1", len(fileQueries))
	}
	if strings.Contains(fileQueries[0], "content") && !strings.Contains(fileQueries[0], "NULL::bytea") {
		t.Fatalf("the digest query reads content: %s", fileQueries[0])
	}
	if strings.Contains(strings.Replace(fileQueries[0], "NULL::bytea", "", 1), "content") {
		t.Fatalf("the digest query reads content: %s", fileQueries[0])
	}
	if ex.Load != nil {
		t.Fatal("no bundle files, nothing to load")
	}
}
