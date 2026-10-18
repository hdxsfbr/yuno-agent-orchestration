package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// recordEnv is open_merge_request wired the way the platform runs it: the tool behind the
// executor (so its call carries the side effect's transaction), a project and a run in
// the database, a checkout cloned from a local bare repository, and a fake provider.
type recordEnv struct {
	t        *testing.T
	svc      *Service
	tool     *Tool
	project  *store.Project
	run      *store.Run
	bare     string
	ws       string
	checkout string
	// requests is how many merge or pull requests the provider holds; seen how many API
	// calls it has answered.
	requests func() int
	seen     func() int
}

func newRecordEnv(t *testing.T, provider project.Provider) *recordEnv {
	t.Helper()
	pool := needDB(t)
	ctx := context.Background()
	e := &recordEnv{t: t, bare: projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "keep/old.txt": "old\n"})}
	pc := project.Config{RepoURL: e.bare, Token: "glpat-not-a-real-token-00", DefaultBranch: "main", Provider: provider}
	switch provider {
	case project.ProviderGitHub:
		g, srv := newFakeGitHub(t, e.bare)
		e.tool = forProject(pc, srv.URL+"/repos/owner/demo", "owner/demo")
		e.requests = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.pulls) }
		e.seen = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.calls) }
	default:
		g, srv := newFakeGitLab(t)
		gitlabOnBare(t, g, e.bare)
		e.tool = forProject(pc, srv.URL+"/api/v4/projects/group%2Fdemo", "")
		e.requests = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.mrs) }
		e.seen = func() int { g.mu.Lock(); defer g.mu.Unlock(); return len(g.calls) }
	}
	e.svc = &Service{Pool: pool, Registry: NewRegistry(), Sleep: (&noSleep{}).sleep}

	e.project = &store.Project{Name: "record-" + store.NewID(), RepoURL: "https://gitlab.test/group/demo.git",
		Provider: string(provider), DefaultBranch: "main"}
	must(t, store.CreateProject(ctx, pool, e.project, []byte("sealed")))
	e.run = e.newRun(t)
	e.ws = t.TempDir()
	must(t, os.MkdirAll(filepath.Join(e.ws, "runs", e.run.ID), 0o755))
	e.checkout = filepath.Join(e.ws, project.RepoRel(e.run.ID))
	must(t, pc.Clone(ctx, e.checkout))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM merge_requests WHERE project_id = $1`, e.project.ID)
		_ = store.DeleteProject(context.Background(), pool, e.project.ID)
	})
	return e
}

// newRun queues another run of the environment's project.
func (e *recordEnv) newRun(t *testing.T) *store.Run {
	t.Helper()
	ctx := context.Background()
	wf := &store.Workflow{Name: "record " + store.NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	must(t, store.CreateWorkflow(ctx, testPool, wf))
	settings := e.project.Settings()
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerManual,
		Status: store.RunRunning, ProjectID: &e.project.ID, Project: &settings}
	must(t, store.CreateRun(ctx, testPool, run))
	return run
}

// edit writes a file in the run's checkout.
func (e *recordEnv) edit(name, content string) {
	e.t.Helper()
	must(e.t, os.WriteFile(filepath.Join(e.checkout, name), []byte(content), 0o644))
}

// key is the idempotency key of the nth distinct call of this run (the keys of the shared
// test database must differ between tests); the branch is GitLabBranch(key).
func (e *recordEnv) key(n byte) string {
	sum := sha256.Sum256([]byte(e.run.ID + string(n)))
	return hex.EncodeToString(sum[:])
}

// open runs one open_merge_request call under key through the executor.
func (e *recordEnv) open(k, title string) (Outcome, mergeRequestOutput, error) {
	e.t.Helper()
	input, _ := json.Marshal(map[string]string{"run_token": "r", "title": title, "description": "the description"})
	out, err := e.svc.Execute(context.Background(), e.tool, Call{RunID: e.run.ID, Workspace: e.ws, IdempotencyKey: k,
		ToolCallID: store.NewID(), Input: input})
	var res mergeRequestOutput
	if err == nil && !out.Result.IsError && len(out.Result.Output) > 0 {
		must(e.t, json.Unmarshal(out.Result.Output, &res))
	}
	return out, res, err
}

func (e *recordEnv) record() *store.MergeRequest {
	e.t.Helper()
	mr, err := store.MergeRequestByRun(context.Background(), testPool, e.run.ID)
	must(e.t, err)
	return mr
}

func (e *recordEnv) records() int {
	e.t.Helper()
	var n int
	must(e.t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM merge_requests WHERE run_id = $1`, e.run.ID).Scan(&n))
	return n
}

// T5.20.1 and T5.20.2: the first call writes the record with the side effect; a later call
// of the run, with another key, pushes a commit on top of the same branch and answers
// replayed and updated; one with an unchanged tree answers replayed only; the same key
// again replays the stored answer; and the provider is never asked for a second request.
func TestOpenMergeRequestOnePerRun(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			e.edit("README.md", "# demo\nv1\n")

			out1, first, err := e.open(e.key('a'), "First title")
			if err != nil || out1.Result.IsError {
				t.Fatalf("first call: %+v %v", out1, err)
			}
			branch := GitLabBranch(e.key('a'))
			if first.Number != 1 || first.Branch != branch || first.Replayed || first.Updated || first.Provider != string(provider) || first.URL == "" {
				t.Fatalf("first output = %s", out1.Result.Output)
			}
			rec := e.record()
			if rec == nil || rec.Number != 1 || rec.URL != first.URL || rec.SourceBranch != branch || rec.TargetBranch != "main" || rec.Title != "First title" ||
				rec.State != store.MergeRequestOpen || rec.CIStatus != store.CIStatusNone || rec.SyncedAt != nil || rec.Provider != string(provider) ||
				rec.ProjectID != e.project.ID || rec.ReportedState != nil {
				t.Fatalf("record = %+v", rec)
			}
			firstTip := projecttest.Git(t, e.bare, "rev-parse", branch)
			if projecttest.Subject(t, e.bare, branch) != "First title" || e.requests() != 1 {
				t.Fatalf("subject %q, requests %d", projecttest.Subject(t, e.bare, branch), e.requests())
			}

			// A later call of the same run (a loop revisit): the tree moved on.
			e.edit("README.md", "# demo\nv2\n")
			e.edit("added.txt", "more\n")
			seen := e.seen()
			out2, second, err := e.open(e.key('b'), "Second title")
			if err != nil || out2.Result.IsError {
				t.Fatalf("second call: %+v %v", out2, err)
			}
			if second.Number != 1 || second.Branch != branch || second.URL != first.URL || !second.Replayed || !second.Updated || out2.Replayed {
				t.Fatalf("second output = %s (service replay %v)", out2.Result.Output, out2.Replayed)
			}
			if projecttest.Git(t, e.bare, "rev-parse", branch+"~1") != firstTip {
				t.Fatal("the second commit is not on top of the first")
			}
			if projecttest.Subject(t, e.bare, branch) != "Second title" || projecttest.FileAt(t, e.bare, branch, "README.md") != "# demo\nv2\n" ||
				projecttest.FileAt(t, e.bare, branch, "added.txt") != "more\n" {
				t.Fatal("the branch does not hold the second tree")
			}
			for _, b := range projecttest.Branches(t, e.bare) {
				if b == GitLabBranch(e.key('b')) {
					t.Fatalf("a branch for the second key: %v", projecttest.Branches(t, e.bare))
				}
			}
			if e.requests() != 1 || e.seen() != seen {
				t.Fatalf("the provider was asked again: requests %d, calls %d -> %d", e.requests(), seen, e.seen())
			}
			if rec := e.record(); rec.Title != "First title" || rec.Number != 1 {
				t.Fatalf("the record changed: %+v", rec)
			}
			secondTip := projecttest.Git(t, e.bare, "rev-parse", branch)

			// Nothing changed since: replayed, no updated, nothing pushed.
			out3, third, err := e.open(e.key('c'), "Third title")
			if err != nil || out3.Result.IsError {
				t.Fatalf("third call: %+v %v", out3, err)
			}
			if third.Number != 1 || !third.Replayed || third.Updated || strings.Contains(string(out3.Result.Output), "updated") {
				t.Fatalf("third output = %s", out3.Result.Output)
			}
			if projecttest.Git(t, e.bare, "rev-parse", branch) != secondTip {
				t.Fatal("an unchanged tree pushed a commit")
			}

			// The same key as the second call: the stored answer, no new work.
			out4, fourth, err := e.open(e.key('b'), "Second title")
			if err != nil || !out4.Replayed || !fourth.Updated || fourth.Number != 1 {
				t.Fatalf("same key again: %+v %v", out4, err)
			}
			if projecttest.Git(t, e.bare, "rev-parse", branch) != secondTip || e.requests() != 1 || e.records() != 1 {
				t.Fatalf("the same key did work: requests %d, records %d", e.requests(), e.records())
			}
		})
	}
}

// T5.20.1 through the tool: when the record cannot be written the whole call is rolled
// back: no side effect row, no record, the key is free, and the retry under the same key
// finds the request the provider already holds and records it.
func TestOpenMergeRequestRecordFailureReleasesTheKey(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.edit("README.md", "# demo\nchange\n")
	ctx := context.Background()

	// Another run holds the number the provider is about to hand out.
	other := e.newRun(t)
	if ok, err := store.InsertMergeRequest(ctx, testPool, &store.MergeRequest{ProjectID: e.project.ID, RunID: other.ID, Provider: "gitlab",
		Number: 1, URL: "https://gitlab.test/group/demo/-/merge_requests/1", SourceBranch: "yuno/other", TargetBranch: "main"}); err != nil || !ok {
		t.Fatalf("seed: %v %v", ok, err)
	}
	out, _, err := e.open(e.key('d'), "Title")
	if err == nil {
		t.Fatalf("a record that cannot be written must fail the call: %+v", out)
	}
	if strings.Contains(err.Error(), "glpat-") {
		t.Fatalf("the error carries a token: %v", err)
	}
	if _, err := store.GetSideEffect(ctx, testPool, e.key('d')); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the failed call left its side effect: %v", err)
	}
	if e.record() != nil {
		t.Fatal("a record survived the failed call")
	}
	if e.requests() != 1 {
		t.Fatalf("the provider holds %d requests, want the one it made", e.requests())
	}

	// The conflict goes away; the retry replays the provider's request and records it.
	_, err = testPool.Exec(ctx, `DELETE FROM merge_requests WHERE run_id = $1`, other.ID)
	must(t, err)
	out, res, err := e.open(e.key('d'), "Title")
	if err != nil || out.Result.IsError {
		t.Fatalf("retry: %+v %v", out, err)
	}
	if res.Number != 1 || !res.Replayed || e.requests() != 1 {
		t.Fatalf("retry output = %s, requests %d", out.Result.Output, e.requests())
	}
	if rec := e.record(); rec == nil || rec.Number != 1 || rec.SourceBranch != GitLabBranch(e.key('d')) {
		t.Fatalf("record after the retry = %+v", rec)
	}
	if _, err := store.GetSideEffect(ctx, testPool, e.key('d')); err != nil {
		t.Fatalf("the retry's side effect is not stored: %v", err)
	}
}

// Rule 2, concurrency: two calls of one run with different keys at the same time open one
// request between them.
func TestOpenMergeRequestConcurrentCallsOpenOneRequest(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.edit("README.md", "# demo\nchange\n")
	var wg sync.WaitGroup
	results := make([]mergeRequestOutput, 2)
	errs := make([]error, 2)
	for i, k := range []byte{'e', 'f'} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, res, err := e.open(e.key(k), "Title "+string(k))
			if err == nil && out.Result.IsError {
				err = errors.New(out.Result.Text)
			}
			results[i], errs[i] = res, err
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if results[i].Number != 1 {
			t.Fatalf("call %d: %+v", i, results[i])
		}
	}
	if e.requests() != 1 || e.records() != 1 {
		t.Fatalf("requests %d, records %d, want one of each", e.requests(), e.records())
	}
	// Exactly one of them opened it; the other found it.
	if results[0].Replayed == results[1].Replayed {
		t.Fatalf("outputs: %+v / %+v", results[0], results[1])
	}
}
