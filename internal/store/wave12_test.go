package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// Wave 12 (DESIGN 5.27, migration 0016): agent file edits, skill sources and skill files.

func w12Ptr(s string) *string { return &s }

func w12Skill(t *testing.T, name string) *Skill {
	t.Helper()
	s := &Skill{Name: name, Description: "d", Body: "b"}
	mustDo(t, CreateSkill(context.Background(), testPool, s))
	t.Cleanup(func() { _ = DeleteSkill(context.Background(), testPool, s.ID) })
	return s
}

func TestAgentFileEdits(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a := testAgent(t, "w12-edits")
	got, err := GetAgentFileEdits(ctx, pool, a.ID)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no edits = %#v %v", got, err)
	}
	mustDo(t, UpsertAgentFileAddition(ctx, pool, a.ID, "AGENTS.md", "be kind"))
	mustDo(t, UpsertAgentFileOverride(ctx, pool, a.ID, "TOOLS.md", w12Ptr("mine")))
	mustDo(t, UpsertAgentFileOverride(ctx, pool, a.ID, "USER.md", w12Ptr("")))
	got, _ = GetAgentFileEdits(ctx, pool, a.ID)
	if len(got) != 3 || got["AGENTS.md"].Addition == nil || *got["AGENTS.md"].Addition != "be kind" || got["AGENTS.md"].Override != nil {
		t.Fatalf("edits = %+v", got)
	}
	if o := got["USER.md"].Override; o == nil || *o != "" {
		t.Fatalf("an emptied override must be kept, got %v", o)
	}
	if got["TOOLS.md"].UpdatedAt.IsZero() {
		t.Fatal("updated_at not set")
	}
	// Replace, then clear: the row goes when both are empty.
	mustDo(t, UpsertAgentFileAddition(ctx, pool, a.ID, "AGENTS.md", "be brief"))
	got, _ = GetAgentFileEdits(ctx, pool, a.ID)
	if *got["AGENTS.md"].Addition != "be brief" {
		t.Fatalf("replace: %+v", got["AGENTS.md"])
	}
	mustDo(t, UpsertAgentFileAddition(ctx, pool, a.ID, "AGENTS.md", ""))
	mustDo(t, UpsertAgentFileOverride(ctx, pool, a.ID, "TOOLS.md", nil))
	mustDo(t, UpsertAgentFileOverride(ctx, pool, a.ID, "HEARTBEAT.md", nil)) // nothing stored: no-op
	got, _ = GetAgentFileEdits(ctx, pool, a.ID)
	if _, ok := got["AGENTS.md"]; ok {
		t.Fatal("row kept after the addition was cleared")
	}
	if _, ok := got["TOOLS.md"]; ok {
		t.Fatal("row kept after the override was cleared")
	}
	if len(got) != 1 {
		t.Fatalf("left: %+v", got)
	}
	// Cascade with the agent.
	_, err = pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, a.ID)
	mustDo(t, err)
	var n int
	mustDo(t, pool.QueryRow(ctx, `SELECT count(*) FROM agent_file_edits WHERE agent_id=$1`, a.ID).Scan(&n))
	if n != 0 {
		t.Fatalf("edits survived the agent: %d", n)
	}
}

func TestSkillSourceColumns(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	u := w12Skill(t, "w12-user")
	got, err := GetSkill(ctx, testPool, u.ID)
	mustDo(t, err)
	if got.Source != "user" || got.SourceURL != "" || got.SourceRef != "" || got.License != "" || got.FilesCount != 0 {
		t.Fatalf("defaults: %+v", got)
	}
	lib := &Skill{Name: "w12-lib", Body: "b", Source: SkillSourceLibrary, SourceURL: "https://github.com/x/y", SourceRef: "abc123", License: "MIT"}
	mustDo(t, CreateSkill(ctx, testPool, lib))
	t.Cleanup(func() { _ = DeleteSkill(ctx, testPool, lib.ID) })
	got, _ = GetSkill(ctx, testPool, lib.ID)
	if got.Source != "library" || got.SourceURL != "https://github.com/x/y" || got.SourceRef != "abc123" || got.License != "MIT" {
		t.Fatalf("library: %+v", got)
	}
	// UpdateSkill leaves the source alone; SetSkillSource changes it.
	got.Body = "new"
	mustDo(t, UpdateSkill(ctx, testPool, &Skill{ID: lib.ID, Name: lib.Name, Body: "new"}))
	got, _ = GetSkill(ctx, testPool, lib.ID)
	if got.Source != "library" || got.Body != "new" {
		t.Fatalf("after update: %+v", got)
	}
	mustDo(t, SetSkillSource(ctx, testPool, lib.ID, SkillSourceGit, "https://gitlab.com/a/b", "def", "Apache-2.0"))
	got, _ = GetSkill(ctx, testPool, lib.ID)
	if got.Source != "git" || got.License != "Apache-2.0" || got.SourceRef != "def" {
		t.Fatalf("after set: %+v", got)
	}
	if err := SetSkillSource(ctx, testPool, "nope", "git", "", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown skill: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE skills SET source='bogus' WHERE id=$1`, lib.ID); err == nil {
		t.Fatal("an unknown source was accepted")
	}
	names, err := ListSkillsByName(ctx, testPool, []string{"w12-lib"})
	if err != nil || len(names) != 1 || names[0].Source != "git" {
		t.Fatalf("ListSkillsByName: %+v %v", names, err)
	}
}

func TestSkillFilesCRUD(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	sk := w12Skill(t, "w12-files")
	list, err := ListSkillFiles(ctx, testPool, sk.ID)
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty = %#v %v", list, err)
	}
	mustDo(t, PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755}))
	mustDo(t, PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "notes.md", Content: []byte("hi"), Mode: 0o644}))
	list, _ = ListSkillFiles(ctx, testPool, sk.ID)
	if len(list) != 2 || list[0].Path != "notes.md" || list[1].Path != "scripts/run.sh" || list[1].Mode != 0o755 || list[1].Size != 10 || string(list[1].Content) != "#!/bin/sh\n" {
		t.Fatalf("list = %+v", list)
	}
	meta, _ := ListSkillFileMeta(ctx, testPool, sk.ID)
	if len(meta) != 2 || meta[0].Content != nil || meta[1].Size != 10 || meta[1].Mode != 0o755 {
		t.Fatalf("meta = %+v", meta)
	}
	got, _ := GetSkill(ctx, testPool, sk.ID)
	if got.FilesCount != 2 {
		t.Fatalf("files_count = %d", got.FilesCount)
	}
	// Replace one file's content and mode.
	mustDo(t, PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "notes.md", Content: []byte("longer text"), Mode: 0o755}))
	f, err := GetSkillFile(ctx, testPool, sk.ID, "notes.md")
	mustDo(t, err)
	if string(f.Content) != "longer text" || f.Size != 11 || f.Mode != 0o755 {
		t.Fatalf("put replace = %+v", f)
	}
	if _, err := GetSkillFile(ctx, testPool, sk.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if err := PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "x", Mode: 0o600}); err == nil {
		t.Fatal("mode 0600 accepted")
	}
	if err := PutSkillFile(ctx, testPool, "nope", &SkillFile{Path: "x", Mode: 0o644}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown skill: %v", err)
	}
	// Binary content round-trips.
	bin := []byte{0, 1, 2, 0xff, 0xfe}
	mustDo(t, PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "b.bin", Content: bin, Mode: 0o644}))
	f, _ = GetSkillFile(ctx, testPool, sk.ID, "b.bin")
	if !bytes.Equal(f.Content, bin) {
		t.Fatalf("binary = %v", f.Content)
	}
	mustDo(t, DeleteSkillFile(ctx, testPool, sk.ID, "b.bin"))
	if err := DeleteSkillFile(ctx, testPool, sk.ID, "b.bin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Cascade with the skill.
	mustDo(t, DeleteSkill(ctx, testPool, sk.ID))
	var n int
	mustDo(t, testPool.QueryRow(ctx, `SELECT count(*) FROM skill_files WHERE skill_id=$1`, sk.ID).Scan(&n))
	if n != 0 {
		t.Fatalf("files survived the skill: %d", n)
	}
}

func TestReplaceSkillFiles(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	sk := w12Skill(t, "w12-replace")
	mustDo(t, PutSkillFile(ctx, testPool, sk.ID, &SkillFile{Path: "old.txt", Content: []byte("old"), Mode: 0o644}))
	mustDo(t, ReplaceSkillFiles(ctx, testPool, sk.ID, []SkillFile{
		{Path: "a.sh", Content: []byte("echo"), Mode: 0o755},
		{Path: "dir/b.txt", Content: []byte("bb"), Mode: 0o644},
	}))
	list, _ := ListSkillFiles(ctx, testPool, sk.ID)
	if len(list) != 2 || list[0].Path != "a.sh" || list[0].Size != 4 || list[1].Path != "dir/b.txt" {
		t.Fatalf("after replace = %+v", list)
	}
	// A failing replacement inside a transaction leaves the old bundle in place.
	tx, err := testPool.Begin(ctx)
	mustDo(t, err)
	err = ReplaceSkillFiles(ctx, tx, sk.ID, []SkillFile{
		{Path: "c.txt", Content: []byte("c"), Mode: 0o644},
		{Path: "c.txt", Content: []byte("dup"), Mode: 0o644},
	})
	_ = tx.Rollback(ctx)
	if err == nil {
		t.Fatal("a duplicate path was accepted")
	}
	list, _ = ListSkillFiles(ctx, testPool, sk.ID)
	if len(list) != 2 || list[0].Path != "a.sh" {
		t.Fatalf("rollback lost the bundle: %+v", list)
	}
	// Committed in a transaction, an empty replacement clears the bundle.
	tx, err = testPool.Begin(ctx)
	mustDo(t, err)
	mustDo(t, ReplaceSkillFiles(ctx, tx, sk.ID, nil))
	mustDo(t, tx.Commit(ctx))
	list, _ = ListSkillFiles(ctx, testPool, sk.ID)
	if len(list) != 0 {
		t.Fatalf("not cleared: %+v", list)
	}
}

// Finding 12: a digest is stored with every write, and the digest read returns it for many
// skills at once without the content.
func TestSkillFileDigests(t *testing.T) {
	needDB(t)
	ctx := context.Background()
	s1, s2, s3 := w12Skill(t, "w12-dig-a"), w12Skill(t, "w12-dig-b"), w12Skill(t, "w12-dig-c")
	want := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	mustDo(t, PutSkillFile(ctx, testPool, s1.ID, &SkillFile{Path: "a.txt", Content: []byte("alpha"), Mode: 0o644}))
	mustDo(t, PutSkillFile(ctx, testPool, s1.ID, &SkillFile{Path: "run.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755}))
	mustDo(t, ReplaceSkillFiles(ctx, testPool, s2.ID, []SkillFile{{Path: "b.bin", Content: []byte{0, 1, 2}, Mode: 0o644}, {Path: "empty", Mode: 0o644}}))
	// An overwrite moves the digest with the content.
	mustDo(t, PutSkillFile(ctx, testPool, s1.ID, &SkillFile{Path: "a.txt", Content: []byte("alpha two"), Mode: 0o644}))

	got, err := ListSkillFileDigests(ctx, testPool, []string{s1.ID, s2.ID, s3.ID})
	mustDo(t, err)
	if len(got) != 2 || len(got[s1.ID]) != 2 || len(got[s2.ID]) != 2 || got[s3.ID] != nil {
		t.Fatalf("digests = %+v", got)
	}
	for _, f := range append(got[s1.ID], got[s2.ID]...) {
		if f.Content != nil {
			t.Fatalf("%s: digest read returned content", f.Path)
		}
	}
	a, r := got[s1.ID][0], got[s1.ID][1]
	if a.Path != "a.txt" || hex.EncodeToString(a.SHA256) != want([]byte("alpha two")) || a.Size != 9 {
		t.Fatalf("a.txt = %+v", a)
	}
	if r.Path != "run.sh" || r.Mode != 0o755 || hex.EncodeToString(r.SHA256) != want([]byte("#!/bin/sh\n")) {
		t.Fatalf("run.sh = %+v", r)
	}
	if e := got[s2.ID][0]; e.Path != "b.bin" || hex.EncodeToString(e.SHA256) != want([]byte{0, 1, 2}) {
		t.Fatalf("b.bin = %+v", e)
	}
	if e := got[s2.ID][1]; e.Path != "empty" || e.Size != 0 || hex.EncodeToString(e.SHA256) != want(nil) {
		t.Fatalf("empty = %+v", e)
	}
	if none, err := ListSkillFileDigests(ctx, testPool, nil); err != nil || len(none) != 0 {
		t.Fatalf("no ids: %v %v", none, err)
	}

	// The content read returns the bytes for all of them in one call.
	full, err := ListSkillFilesContent(ctx, testPool, []string{s1.ID, s2.ID})
	mustDo(t, err)
	if string(full[s1.ID][0].Content) != "alpha two" || !bytes.Equal(full[s2.ID][0].Content, []byte{0, 1, 2}) {
		t.Fatalf("content = %+v", full)
	}
	// The single-skill reads carry the digest too.
	one, _ := GetSkillFile(ctx, testPool, s1.ID, "a.txt")
	if hex.EncodeToString(one.SHA256) != want([]byte("alpha two")) {
		t.Fatalf("GetSkillFile digest = %x", one.SHA256)
	}
}
