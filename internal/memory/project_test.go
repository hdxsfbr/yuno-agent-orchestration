package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
)

// newProject stores a live project with a name no other test uses.
func newProject(t *testing.T) *store.Project {
	t.Helper()
	pool := needDB(t)
	p := &store.Project{Name: "notes " + strings.ReplaceAll(store.NewID(), "-", "")[16:], RepoURL: "https://gitlab.test/g/notes.git",
		Provider: "gitlab", DefaultBranch: "main"}
	if err := store.CreateProject(context.Background(), pool, p, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DeleteProject(context.Background(), pool, p.ID) })
	return p
}

// saveNote stores key=value for the project as the agent in the run, at now; ttl nil is
// the 90-day default.
func saveNote(t *testing.T, p *store.Project, a *store.Agent, run, key, value string, now time.Time, ttl *int) *store.ProjectFact {
	t.Helper()
	exp := ProjectFactExpiresAt(ttl, now)
	f := &store.ProjectFact{ProjectID: p.ID, Key: key, Value: value, ExpiresAt: &exp}
	if a != nil {
		f.AgentID = &a.ID
	}
	if run != "" {
		f.SourceRunID = &run
	}
	if err := SaveProjectFact(context.Background(), testPool, f, now); err != nil {
		t.Fatalf("save %q: %v", key, err)
	}
	return f
}

func noteKeys(facts []store.ProjectFact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Key
	}
	return out
}

func recallNotes(t *testing.T, p *store.Project, query string, now time.Time) []store.ProjectFact {
	t.Helper()
	got, err := RecallProjectFacts(context.Background(), testPool, p.ID, query, RecallLimit, now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func noteRows(t *testing.T, p *store.Project) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM project_facts WHERE project_id=$1`, p.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T5.18.2: a save upserts by (project, key) and records the agent and the run; the
// later write wins, whichever agent made it; the note ages from its first save; the
// default TTL is 90 days and ttl_days replaces it.
func TestSaveProjectFactUpsertsByKey(t *testing.T) {
	needDB(t)
	p := newProject(t)
	coder, reviewer := newAgent(t), newAgent(t)

	first := saveNote(t, p, coder, "run-1", "test command", "go test ./...", t0, nil)
	if first.ID == "" || first.AgentID == nil || *first.AgentID != coder.ID || first.SourceRunID == nil || *first.SourceRunID != "run-1" {
		t.Fatalf("first = %+v", first)
	}
	if first.ExpiresAt == nil || !first.ExpiresAt.Equal(t0.Add(90*24*time.Hour)) {
		t.Fatalf("expires_at = %v, want 90 days after the save", first.ExpiresAt)
	}

	later := t0.Add(time.Hour)
	second := saveNote(t, p, reviewer, "run-2", "test command", "go test ./... -race", later, intp(5))
	if second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) || !second.UpdatedAt.Equal(later) {
		t.Fatalf("an upsert keeps the row and its created_at: %+v then %+v", first, second)
	}
	if second.Value != "go test ./... -race" || *second.AgentID != reviewer.ID || *second.SourceRunID != "run-2" ||
		!second.ExpiresAt.Equal(later.Add(5*24*time.Hour)) {
		t.Fatalf("the later write must win: %+v", second)
	}
	if noteRows(t, p) != 1 {
		t.Fatalf("rows = %d, want one note for the key", noteRows(t, p))
	}

	// The same key in another project is another note; an agent's own fact of the same
	// key is untouched (their tables are apart).
	other := newProject(t)
	saveNote(t, other, coder, "run-3", "test command", "make test", t0, nil)
	a := save(t, coder, "test command", "my own", t0, nil, 0)
	if got := recallNotes(t, p, "", t0); len(got) != 1 || got[0].Value != "go test ./... -race" {
		t.Fatalf("notes of the first project = %+v", got)
	}
	if !exists(t, a.ID) {
		t.Fatal("an agent fact vanished")
	}

	// A note without an agent or a run (it outlives them) is storable.
	saveNote(t, p, nil, "", "orphan", "kept", t0, nil)
}

// Rule 2: key 1 to 200 bytes, value 1 to 2048 bytes.
func TestSaveProjectFactBounds(t *testing.T) {
	needDB(t)
	p := newProject(t)
	save := func(key, value string) error {
		exp := ProjectFactExpiresAt(nil, t0)
		return SaveProjectFact(context.Background(), testPool, &store.ProjectFact{ProjectID: p.ID, Key: key, Value: value, ExpiresAt: &exp}, t0)
	}
	if err := save("", "v"); !errors.Is(err, ErrProjectKeyLength) {
		t.Errorf("empty key: %v", err)
	}
	if err := save(strings.Repeat("k", 201), "v"); !errors.Is(err, ErrProjectKeyLength) {
		t.Errorf("201-byte key: %v", err)
	}
	if err := save(strings.Repeat("k", 200), "v"); err != nil {
		t.Errorf("200-byte key: %v", err)
	}
	if err := save("é"+strings.Repeat("k", 199), "v"); !errors.Is(err, ErrProjectKeyLength) { // 201 bytes in 200 characters
		t.Errorf("201 bytes in 200 characters: %v", err)
	}
	if err := save("k", ""); !errors.Is(err, ErrEmptyValue) {
		t.Errorf("empty value: %v", err)
	}
	if err := save("k", strings.Repeat("v", 2049)); !errors.Is(err, ErrValueTooLong) {
		t.Errorf("2049-byte value: %v", err)
	}
	if err := save("k", strings.Repeat("v", 2048)); err != nil {
		t.Errorf("2048-byte value: %v", err)
	}
	if noteRows(t, p) != 2 {
		t.Errorf("rows = %d, want the two valid saves", noteRows(t, p))
	}
	if ProjectFactExpiresAt(nil, t0) != t0.Add(90*24*time.Hour) || ProjectFactExpiresAt(intp(-3), t0) != t0 || ProjectFactExpiresAt(intp(0), t0) != t0 {
		t.Error("ttl default 90 days; 0 or less expires at once")
	}
}

// T5.18.5: the 101st note evicts the oldest by created_at, not the least recently
// updated, in the same transaction; other projects' notes do not count.
func TestSaveProjectFactEvictsTheOldestAtTheCap(t *testing.T) {
	needDB(t)
	p, other := newProject(t), newProject(t)
	a := newAgent(t)
	saveNote(t, other, a, "r", "elsewhere", "v", t0, nil)
	for i := range MaxProjectFacts {
		saveNote(t, p, a, "r", fmt.Sprintf("note %03d", i), "v", t0.Add(time.Duration(i)*time.Second), nil)
	}
	if noteRows(t, p) != MaxProjectFacts {
		t.Fatalf("rows = %d, want the cap %d", noteRows(t, p), MaxProjectFacts)
	}
	// Rewriting the oldest note refreshes updated_at, not created_at: it is still the first to go.
	saveNote(t, p, a, "r", "note 000", "rewritten", t0.Add(time.Hour), nil)
	if noteRows(t, p) != MaxProjectFacts {
		t.Fatalf("an upsert at the cap changed the count to %d", noteRows(t, p))
	}
	saveNote(t, p, a, "r", "note 100", "the 101st", t0.Add(2*time.Hour), nil)
	if noteRows(t, p) != MaxProjectFacts {
		t.Fatalf("rows after the 101st = %d, want %d", noteRows(t, p), MaxProjectFacts)
	}
	have := func(key string) bool {
		var n int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM project_facts WHERE project_id=$1 AND key=$2`, p.ID, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if have("note 000") || !have("note 001") || !have("note 100") || !have("note 099") {
		t.Fatalf("eviction: note 000 kept=%v, note 001 kept=%v, note 100 kept=%v", have("note 000"), have("note 001"), have("note 100"))
	}
	// RecallLimit caps what a recall returns, not what is stored.
	if got := recallNotes(t, p, "", t0.Add(3*time.Hour)); len(got) != RecallLimit {
		t.Fatalf("recall returned %d, want %d", len(got), RecallLimit)
	}
	if rows := noteRows(t, other); rows != 1 {
		t.Fatalf("another project's notes = %d", rows)
	}
	// Expired notes go first and do not count toward the cap.
	q := newProject(t)
	for i := range MaxProjectFacts {
		saveNote(t, q, a, "r", fmt.Sprintf("n%03d", i), "v", t0.Add(time.Duration(i)*time.Second), intp(1))
	}
	after := t0.Add(48 * time.Hour)
	saveNote(t, q, a, "r", "fresh", "v", after, nil)
	if noteRows(t, q) != 1 {
		t.Fatalf("rows = %d: the expired notes should have been removed by the save", noteRows(t, q))
	}
}

// T5.18.6: recall matches every word of the query, case-insensitively, in the key or the
// value, over this project's notes only, newest by updated_at first, at most the limit.
func TestRecallProjectFactsWordMatchAndOrder(t *testing.T) {
	needDB(t)
	p, other := newProject(t), newProject(t)
	a := newAgent(t)
	saveNote(t, p, a, "r", "deploy cadence", "Releases go out on Tuesdays", t0, nil)
	saveNote(t, p, a, "r", "python version", "The sandbox runs Python 3.12", t0.Add(time.Minute), nil)
	saveNote(t, p, a, "r", "layout", "cmd holds the binaries; deploy scripts live in ops", t0.Add(2*time.Minute), nil)
	saveNote(t, other, a, "r", "deploy cadence", "elsewhere: Fridays", t0.Add(3*time.Minute), nil)
	save(t, a, "deploy habits", "my own agent fact about deploy", t0.Add(4*time.Minute), nil, 0) // not a project note
	now := t0.Add(time.Hour)

	if got := noteKeys(recallNotes(t, p, "TUESDAYS", now)); fmt.Sprint(got) != "[deploy cadence]" {
		t.Errorf("by value, case-insensitive = %v", got)
	}
	if got := noteKeys(recallNotes(t, p, "sandbox python", now)); fmt.Sprint(got) != "[python version]" {
		t.Errorf("by words in any order = %v", got)
	}
	if got := noteKeys(recallNotes(t, p, "deploy", now)); fmt.Sprint(got) != "[layout deploy cadence]" {
		t.Errorf("newest first = %v", got)
	}
	if got := noteKeys(recallNotes(t, p, "deploy tuesdays", now)); fmt.Sprint(got) != "[deploy cadence]" {
		t.Errorf("a word in the key and one in the value = %v", got)
	}
	if got := recallNotes(t, p, "deploy nothing", now); len(got) != 0 {
		t.Errorf("every word must match: %v", noteKeys(got))
	}
	if got := noteKeys(recallNotes(t, p, "", now)); fmt.Sprint(got) != "[layout python version deploy cadence]" {
		t.Errorf("empty query = %v, want the newest first", got)
	}
	// Rewriting a note moves it to the front.
	saveNote(t, p, a, "r", "deploy cadence", "Releases go out on Wednesdays", t0.Add(30*time.Minute), nil)
	if got := noteKeys(recallNotes(t, p, "", now)); got[0] != "deploy cadence" {
		t.Errorf("after a rewrite = %v", got)
	}
	newest, err := NewestProjectFacts(context.Background(), testPool, p.ID, 2, now)
	if err != nil || fmt.Sprint(noteKeys(newest)) != "[deploy cadence layout]" {
		t.Errorf("newest 2 = %v %v", noteKeys(newest), err)
	}
	// Terms are plain text: a wildcard or a pattern character matches itself only.
	if got := recallNotes(t, p, "%", now); len(got) != 0 {
		t.Errorf("a percent sign matched %v", noteKeys(got))
	}
}

// T5.18.9: an expired note is neither recalled, newest, listed nor deletable-by-listing
// before the sweep runs; ttl 0 is never returned; the sweep deletes it and counts it.
func TestExpiredProjectFactsAreInvisibleBeforeTheSweep(t *testing.T) {
	needDB(t)
	p := newProject(t)
	a := newAgent(t)
	saveNote(t, p, a, "r", "short lived", "gone soon", t0, intp(1))
	saveNote(t, p, a, "r", "long lived", "stays", t0.Add(time.Second), nil)
	saveNote(t, p, a, "r", "instant", "never seen", t0, intp(0))
	ctx := context.Background()

	for _, now := range []time.Time{t0, t0.Add(23 * time.Hour)} {
		if got := noteKeys(recallNotes(t, p, "", now)); fmt.Sprint(got) != "[long lived short lived]" {
			t.Errorf("at %s recall = %v", now.Sub(t0), got)
		}
	}
	// The expiry instant itself is expired: live means expires_at > now.
	at24 := t0.Add(24 * time.Hour)
	if got := noteKeys(recallNotes(t, p, "", at24)); fmt.Sprint(got) != "[long lived]" {
		t.Errorf("at the expiry instant recall = %v", got)
	}
	after := t0.Add(48 * time.Hour)
	if got := noteKeys(recallNotes(t, p, "gone", after)); len(got) != 0 {
		t.Errorf("recall of an expired note = %v", got)
	}
	newest, _ := NewestProjectFacts(ctx, testPool, p.ID, 10, after)
	listed, err := ListProjectFacts(ctx, testPool, p.ID, after)
	if err != nil || fmt.Sprint(noteKeys(newest)) != "[long lived]" || fmt.Sprint(noteKeys(listed)) != "[long lived]" {
		t.Errorf("newest = %v, list = %v (%v)", noteKeys(newest), noteKeys(listed), err)
	}
	// Both expired rows are still in the table: only the filter hides them.
	if noteRows(t, p) != 3 {
		t.Fatalf("rows = %d, want 3 before the sweep", noteRows(t, p))
	}
	n, err := SweepExpiredProjectFacts(ctx, testPool, after)
	if err != nil || n < 2 {
		t.Fatalf("sweep deleted %d (%v), want at least the two expired notes", n, err)
	}
	if noteRows(t, p) != 1 {
		t.Fatalf("rows after the sweep = %d", noteRows(t, p))
	}
}

// The sweeper's default pass takes project notes with agent facts (rule 5).
func TestSweeperSweepsProjectFacts(t *testing.T) {
	pool := needDB(t)
	p := newProject(t)
	saveNote(t, p, nil, "", "old", "v", t0, intp(1))
	sw := &Sweeper{Pool: pool, Now: func() time.Time { return t0.Add(48 * time.Hour) }}
	if n := sw.SweepOnce(context.Background()); n < 1 {
		t.Fatalf("swept %d", n)
	}
	if noteRows(t, p) != 0 {
		t.Fatal("an expired project note survived the sweeper")
	}
}

// Rule 6 (store side): the list is newest first with the saving agent's name, nil once
// the agent row is gone; delete is by project and id; a soft-deleted project keeps its
// notes.
func TestListAndDeleteProjectFacts(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, other := newProject(t), newProject(t)
	coder, gone := newAgent(t), newAgent(t)
	coder.Name = "Coder " + coder.ID[:8]
	if _, err := pool.Exec(ctx, `UPDATE agents SET name=$2 WHERE id=$1`, coder.ID, coder.Name); err != nil {
		t.Fatal(err)
	}
	older := saveNote(t, p, coder, "run-1", "first", "v1", t0, nil)
	newer := saveNote(t, p, gone, "run-2", "second", "v2", t0.Add(time.Minute), nil)
	foreign := saveNote(t, other, coder, "run-3", "foreign", "v3", t0, nil)
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, gone.ID); err != nil {
		t.Fatal(err)
	}

	list, err := ListProjectFacts(ctx, pool, p.ID, t0.Add(time.Hour))
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}
	if list[0].ID != newer.ID || list[0].AgentID != nil || list[0].AgentName != nil || list[0].SourceRunID == nil || *list[0].SourceRunID != "run-2" {
		t.Errorf("a note whose agent is gone: %+v", list[0])
	}
	if list[1].ID != older.ID || list[1].AgentName == nil || *list[1].AgentName != coder.Name || *list[1].AgentID != coder.ID {
		t.Errorf("a note with its agent: %+v", list[1])
	}
	if empty, err := ListProjectFacts(ctx, pool, newProject(t).ID, t0); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("a project without notes lists %v %v, want an empty list", empty, err)
	}

	// A project's path cannot delete another project's note.
	if err := DeleteProjectFact(ctx, pool, p.ID, foreign.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete across projects: %v", err)
	}
	if err := DeleteProjectFact(ctx, pool, p.ID, older.ID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteProjectFact(ctx, pool, p.ID, older.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if noteRows(t, other) != 1 {
		t.Fatal("the other project's note was deleted")
	}

	// Soft-deleting the project keeps its notes (rule 2), readable for the runs in flight.
	if err := store.DeleteProject(ctx, pool, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := NewestProjectFacts(ctx, pool, p.ID, 10, t0.Add(time.Hour)); err != nil || len(got) != 1 {
		t.Fatalf("notes of a deleted project = %v %v", got, err)
	}
}
