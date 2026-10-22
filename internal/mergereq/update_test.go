package mergereq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"yuno/internal/events"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// Chief platform control, DESIGN 5.34: what the sync and the update from the default
// branch write on the record (rules 4, 9 to 11 and 13).

// gitProject stores a GitLab project whose repository is a local bare repository with
// main, and a branch yuno/t1 one commit ahead of it; it returns the project, the bare
// repository and the branch's head.
func (e *env) gitProject() (*store.Project, string, string) {
	e.t.Helper()
	bare := projecttest.BareRepo(e.t, map[string]string{"README.md": "# demo\n", "app.txt": "app v1\n"})
	p := &store.Project{ID: store.NewID(), Name: "p-" + store.NewID(), RepoURL: bare, Provider: "gitlab", DefaultBranch: "main"}
	blob, err := e.cipher.Seal(p.ID, testToken)
	must(e.t, err)
	must(e.t, store.CreateProject(context.Background(), e.pool, p, blob))
	head := pushTo(e.t, bare, "main", "yuno/t1", map[string]string{"feature.txt": "feature\n"})
	return p, bare, head
}

// pushTo commits files on top of from and pushes the commit to branch, as a person or an
// agent does, and returns it.
func pushTo(t *testing.T, bare, from, branch string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", from, bare, work)
	for name, body := range files {
		must(t, os.WriteFile(filepath.Join(work, name), []byte(body), 0o644))
	}
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "change")
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

// checkedRecord stores an open record of the project's yuno/t1 whose check passed on head.
func (e *env) checkedRecord(p *store.Project, head string) (*store.Run, *store.MergeRequest) {
	e.t.Helper()
	run := e.run(p, false)
	pipeline := int64(41)
	url := "https://gitlab.test/group/demo/-/pipelines/41"
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: run.ID, Provider: "gitlab", Number: 1, URL: "https://gitlab.test/group/demo/-/merge_requests/1",
		SourceBranch: "yuno/t1", TargetBranch: "main", Title: "A change", CIStatus: store.CIStatusPassed, HeadSHA: &head, CISHA: &head}
	ok, err := store.InsertMergeRequest(context.Background(), e.pool, mr)
	must(e.t, err)
	if !ok {
		e.t.Fatal("record not inserted")
	}
	_, err = e.pool.Exec(context.Background(), `UPDATE merge_requests SET ci_pipeline_id = $2, ci_url = $3 WHERE id = $1`, mr.ID, pipeline, url)
	must(e.t, err)
	return run, e.get(mr.ID)
}

func (e *env) withEvents() (*Sync, *events.Hub) {
	hub := events.NewHub()
	s := e.newSync()
	s.Events = &events.Recorder{Hub: hub, Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		return store.AppendRunEvent(ctx, e.pool, runID, typ, data)
	}}
	return s, hub
}

func (e *env) eventCount(runID string) int {
	e.t.Helper()
	var n int
	must(e.t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_events WHERE run_id = $1 AND event = $2`, runID, EventUpdated).Scan(&n))
	return n
}

// Rules 9 to 11 on the record: a push invalidates the stale check in one write and emits
// merge_request.updated; conflicts are recorded with nothing pushed, and an update that
// finds the branch up to date clears them; a head that moved, a record that is not open
// and an unknown id change nothing.
func TestUpdateFromDefaultRecords(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p, bare, head := e.gitProject()
	run, rec := e.checkedRecord(p, head)
	s, hub := e.withEvents()
	sub, cancel := hub.Subscribe("", 8)
	defer cancel()

	// Up to date: nothing pushed, nothing invalidated.
	cur, res, err := s.UpdateFromDefault(ctx, rec.ID, head)
	if err != nil || res.Outcome != project.UpdateUpToDate || cur.CIStatus != store.CIStatusPassed || *cur.HeadSHA != head || cur.UpdatedFromDefaultAt != nil {
		t.Fatalf("up to date = %+v %+v %v", cur, res, err)
	}

	// Main gains a file: the merge is pushed and the record follows it.
	mainTip := pushTo(t, bare, "main", "main", map[string]string{"main.txt": "main\n"})
	cur, res, err = s.UpdateFromDefault(ctx, rec.ID, head)
	if err != nil || res.Outcome != project.UpdateUpdated || res.DefaultSHA != mainTip || !project.IsCommitID(res.Commit) {
		t.Fatalf("update = %+v %v", res, err)
	}
	if tip := projecttest.Git(t, bare, "rev-parse", "yuno/t1"); tip != res.Commit {
		t.Fatalf("branch at %s, want %s", tip, res.Commit)
	}
	if cur.HeadSHA == nil || *cur.HeadSHA != res.Commit || cur.CISHA == nil || *cur.CISHA != res.Commit || cur.CIStatus != store.CIStatusPending ||
		cur.CIPipelineID != nil || cur.CIURL != nil || cur.UpdatedFromDefaultAt == nil || !cur.UpdatedFromDefaultAt.Equal(epoch) {
		t.Fatalf("record after the push = %+v", cur)
	}
	if n := e.eventCount(run.ID); n != 1 {
		t.Fatalf("merge_request.updated events = %d", n)
	}
	select {
	case ev := <-sub:
		var data struct {
			MergeRequest store.MergeRequest `json:"merge_request"`
		}
		if ev.Type != EventUpdated || ev.RunID != run.ID || json.Unmarshal(ev.Data, &data) != nil || data.MergeRequest.ID != rec.ID ||
			*data.MergeRequest.HeadSHA != res.Commit {
			t.Fatalf("event = %+v %s", ev, ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("no event published")
	}

	// The head moved since it was read: 409 material, nothing written.
	before := e.get(rec.ID)
	_, _, err = s.UpdateFromDefault(ctx, rec.ID, head)
	var moved *project.ErrHeadMoved
	if !errors.As(err, &moved) || moved.Current != res.Commit {
		t.Fatalf("stale expected head = %v", err)
	}
	if after := e.get(rec.ID); !after.UpdatedAt.Equal(before.UpdatedAt) || *after.HeadSHA != *before.HeadSHA {
		t.Fatalf("a refused update wrote the record: %+v", after)
	}

	// Main and the branch change the same file: conflicts recorded, nothing pushed.
	branchTip := pushTo(t, bare, "yuno/t1", "yuno/t1", map[string]string{"app.txt": "app from the branch\n"})
	conflictBase := pushTo(t, bare, "main", "main", map[string]string{"app.txt": "app from main\n"})
	cur, res, err = s.UpdateFromDefault(ctx, rec.ID, branchTip)
	if err != nil || res.Outcome != project.UpdateConflicts || !slices.Equal(res.Conflicts, []string{"app.txt"}) {
		t.Fatalf("conflicting update = %+v %v", res, err)
	}
	if cur.Conflicts == nil || !slices.Equal(cur.Conflicts.Files, []string{"app.txt"}) || cur.Conflicts.BaseSHA != conflictBase || !cur.Conflicts.At.Equal(epoch) {
		t.Fatalf("conflicts = %+v", cur.Conflicts)
	}
	if tip := projecttest.Git(t, bare, "rev-parse", "yuno/t1"); tip != branchTip {
		t.Fatal("a conflicting merge was pushed")
	}
	if n := e.eventCount(run.ID); n != 1 {
		t.Fatalf("a conflict emitted an event: %d", n)
	}

	// The branch resolves it by taking main's version: up to date, conflicts cleared.
	resolved := pushTo(t, bare, "yuno/t1", "yuno/t1", map[string]string{"app.txt": "app from main\n"})
	projecttest.Git(t, bare, "update-ref", "refs/heads/yuno/t1", mergeOf(t, bare, resolved, conflictBase))
	resolvedHead := projecttest.Git(t, bare, "rev-parse", "yuno/t1")
	cur, res, err = s.UpdateFromDefault(ctx, rec.ID, resolvedHead)
	if err != nil || res.Outcome != project.UpdateUpToDate || cur.Conflicts != nil {
		t.Fatalf("after the resolution = %+v %+v %v", res, cur.Conflicts, err)
	}

	// A record that is not open and an unknown id.
	if _, err := store.MarkMergeRequestMerged(ctx, e.pool, rec.ID, "operator", epoch); err != nil {
		t.Fatal(err)
	}
	var notOpen *ErrNotOpen
	if _, _, err := s.UpdateFromDefault(ctx, rec.ID, resolvedHead); !errors.As(err, &notOpen) || notOpen.State != store.MergeRequestMerged {
		t.Fatalf("merged record = %v", err)
	}
	if _, _, err := s.UpdateFromDefault(ctx, "nope", resolvedHead); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

// mergeOf records a merge commit of ours and theirs that keeps ours's tree, as a person
// resolving a conflict by hand does, and returns it.
func mergeOf(t *testing.T, bare, ours, theirs string) string {
	t.Helper()
	tree := projecttest.Git(t, bare, "rev-parse", ours+"^{tree}")
	return projecttest.Git(t, bare, "commit-tree", tree, "-p", ours, "-p", theirs, "-m", "resolve by hand")
}

// DESIGN 5.34 rules 4 and 13: the sync persists the merge commit of a merged request,
// and for an open one the check of exactly its head with the pipeline behind it and the
// provider's account of the merge.
func TestMergeSHAPersistedOnSync(t *testing.T) {
	e := newEnv(t)
	p := e.project(testToken)
	merged := e.record(p, e.run(p, false), 1, time.Hour)
	open := e.record(p, e.run(p, false), 2, time.Hour)
	e.fake.request(1, "merged", "sha-merged", "success", 0)
	e.fake.request(2, "opened", "sha-open", "success", 0)
	e.fake.set("GET /merge_requests/2", 200, map[string]any{"state": "opened", "sha": "sha-open",
		"detailed_merge_status": "mergeable", "has_conflicts": false})

	if synced, failed := e.sync.TickOnce(context.Background()); synced != 2 || failed != 0 {
		t.Fatalf("synced %d, failed %d; logs:\n%s", synced, failed, e.logs)
	}
	got := e.get(merged.ID)
	if got.State != store.MergeRequestMerged || got.MergeSHA == nil || *got.MergeSHA != "mergesha1" {
		t.Fatalf("merged = %+v (merge_sha %v)", got, got.MergeSHA)
	}
	got = e.get(open.ID)
	if got.CIStatus != store.CIStatusPassed || got.CISHA == nil || *got.CISHA != "sha-open" || got.CIPipelineID == nil || *got.CIPipelineID != 102 ||
		got.CIURL == nil || *got.CIURL != "https://gitlab.test/group/demo/-/pipelines/102" || got.Mergeable == nil || !*got.Mergeable ||
		got.MergeStatus == nil || *got.MergeStatus != "mergeable" {
		t.Fatalf("open = %+v", got)
	}

	// A new head with no pipeline of its own: the earlier head's pass does not carry over.
	e.fake.request(2, "opened", "sha-newer", "", 0)
	e.fake.set("GET /merge_requests/2", 200, map[string]any{"state": "opened", "sha": "sha-newer", "detailed_merge_status": "conflict", "has_conflicts": true})
	if _, err := e.sync.SyncOne(context.Background(), open.ID); err != nil {
		t.Fatal(err)
	}
	got = e.get(open.ID)
	if got.CIStatus != store.CIStatusNone || *got.CISHA != "sha-newer" || *got.HeadSHA != "sha-newer" || got.CIPipelineID != nil || got.CIURL != nil ||
		got.Mergeable == nil || *got.Mergeable || *got.MergeStatus != "conflict" {
		t.Fatalf("new head = %+v", got)
	}
	for _, c := range e.fake.seen() {
		if c == fmt.Sprintf("GET /merge_requests/%d/pipelines", 2) {
			t.Fatalf("the newest pipeline of the request was read: %v", e.fake.seen())
		}
	}
}
