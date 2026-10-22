package mergereq

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yuno/internal/forgefake"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Chief platform control, DESIGN wave 3 rules 8 and 12 against the fake forge: real
// pushes, pipelines of exact commits, the sync tick as the only trigger.

// forgeEnv is a project on the fake forge (acme/app, default branch main) with a sync whose
// re-queue hook (rule 7's; the server re-queues the parked runs through it) counts its calls.
type forgeEnv struct {
	*env
	srv  *httptest.Server
	pc   project.Config
	p    *store.Project
	sync *Sync

	mu    sync.Mutex
	hooks []hookCall
}

type hookCall struct{ id, note string }

func newForgeEnv(t *testing.T) *forgeEnv {
	t.Helper()
	e := newEnv(t)
	projecttest.NeedGit(t)
	f, err := forgefake.New(testToken, t.TempDir())
	must(t, err)
	fe := &forgeEnv{env: e, srv: httptest.NewServer(f)}
	t.Cleanup(fe.srv.Close)
	fe.forge(http.MethodPost, "/_forge/repos", map[string]any{"path": "acme/app", "default_branch": "main"}, http.StatusCreated)
	fe.pc = project.Config{RepoURL: fe.srv.URL + "/acme/app.git", Token: testToken, DefaultBranch: "main", Provider: project.ProviderGitLab}
	fe.p = &store.Project{ID: store.NewID(), Name: "forge-" + store.NewID(), RepoURL: fe.pc.RepoURL, Provider: "gitlab", DefaultBranch: "main"}
	blob, err := e.cipher.Seal(fe.p.ID, testToken)
	must(t, err)
	must(t, store.CreateProject(context.Background(), e.pool, fe.p, blob))
	s, _ := e.withEvents()
	s.Client = tools.ProviderClient(nil)
	s.RequeueDependencyRuns = func(_ context.Context, rec *store.MergeRequest, note string) error {
		fe.mu.Lock()
		defer fe.mu.Unlock()
		fe.hooks = append(fe.hooks, hookCall{rec.ID, note})
		return nil
	}
	fe.sync = s
	return fe
}

func (f *forgeEnv) calls() []hookCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hookCall(nil), f.hooks...)
}

// forge calls the forge with the token and decodes a JSON object answer.
func (f *forgeEnv) forge(method, path string, in any, want int) map[string]any {
	f.t.Helper()
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	must(f.t, err)
	req.Header.Set("PRIVATE-TOKEN", testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(f.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		f.t.Fatalf("%s %s: %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// push commits files on top of from and pushes the commit to branch over the forge's Git
// HTTP, as a person or an agent does; it returns the commit.
func (f *forgeEnv) push(from, branch string, files map[string]string) string {
	f.t.Helper()
	u, err := url.Parse(f.pc.RepoURL)
	must(f.t, err)
	u.User = url.UserPassword("oauth2", testToken)
	work := filepath.Join(f.t.TempDir(), "work")
	projecttest.Git(f.t, filepath.Dir(work), "clone", "--quiet", "--branch", from, u.String(), work)
	for name, body := range files {
		must(f.t, os.WriteFile(filepath.Join(work, name), []byte(body), 0o644))
	}
	projecttest.Git(f.t, work, "add", "--all")
	projecttest.Git(f.t, work, "commit", "--quiet", "-m", "change")
	projecttest.Git(f.t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(f.t, work, "rev-parse", "HEAD")
}

// open pushes branch with files from main, opens its merge request on the forge and stores
// the record for a run of its own (one that reports to a conversation when chat), never
// synced.
func (f *forgeEnv) open(branch string, files map[string]string, chat bool) *store.MergeRequest {
	f.t.Helper()
	f.push("main", branch, files)
	ref, err := tools.ProviderFor(&f.pc, nil).Create(context.Background(), branch, "main", "Change on "+branch, "", false)
	must(f.t, err)
	mr := &store.MergeRequest{ProjectID: f.p.ID, RunID: f.run(f.p, chat).ID, Provider: "gitlab", Number: ref.Number, URL: ref.URL,
		SourceBranch: branch, TargetBranch: "main", Title: "Change on " + branch}
	ok, err := store.InsertMergeRequest(context.Background(), f.pool, mr)
	must(f.t, err)
	if !ok {
		f.t.Fatal("record not inserted")
	}
	return mr
}

// depend makes dep wait on pre, as PUT /api/merge-requests/{id}/dependencies does.
func (f *forgeEnv) depend(dep, pre *store.MergeRequest) {
	f.t.Helper()
	if _, err := store.ReplaceMergeRequestPrerequisites(context.Background(), f.pool, f.p.ID, dep.ID, []string{pre.ID},
		store.DependencyOperator, json.RawMessage(`{"kind":"operator"}`)); err != nil {
		f.t.Fatal(err)
	}
}

// pipeline records a pipeline at ref's head on the forge.
func (f *forgeEnv) pipeline(state, ref string) {
	f.t.Helper()
	f.forge(http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": state, "ref": ref}, http.StatusCreated)
}

// merge merges the request on the forge, as a person does there.
func (f *forgeEnv) merge(mr *store.MergeRequest) {
	f.t.Helper()
	f.forge(http.MethodPost, "/_forge/repos/acme/app/merge-open", map[string]any{"iid": mr.Number}, http.StatusOK)
}

func (f *forgeEnv) tip(branch string) string {
	f.t.Helper()
	b := f.forge(http.MethodGet, "/api/v4/projects/acme%2Fapp/repository/branches/"+url.PathEscape(branch), nil, http.StatusOK)
	commit, _ := b["commit"].(map[string]any)
	id, _ := commit["id"].(string)
	return id
}

func (f *forgeEnv) tick() {
	f.t.Helper()
	if _, failed := f.sync.TickOnce(context.Background()); failed != 0 {
		f.t.Fatalf("a sync failed; logs:\n%s", f.logs)
	}
}

// gateReason is the watcher's reason on a dependent whose prerequisites are satisfied.
func (f *forgeEnv) gateReason(id string) string {
	f.t.Helper()
	g, err := store.GetDependencyGate(context.Background(), f.pool, id)
	must(f.t, err)
	if g == nil || g.Reason == nil {
		return ""
	}
	return *g.Reason
}

func (f *forgeEnv) eventsOf(runID, kind string) int {
	f.t.Helper()
	var n int
	must(f.t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_events WHERE run_id = $1 AND event = $2`, runID, kind).Scan(&n))
	return n
}

// prepared is a dependent whose prerequisite merged with main green on its merge commit,
// and whose own head h1 passed CI: the first pass updates it from main.
func (f *forgeEnv) prepared(chat bool) (pre, dep *store.MergeRequest, h1 string) {
	f.t.Helper()
	pre = f.open("yuno/pre", map[string]string{"pre.txt": "prerequisite\n"}, false)
	dep = f.open("yuno/dep", map[string]string{"dep.txt": "dependent\n"}, chat)
	f.depend(dep, pre)
	h1 = f.tip("yuno/dep")
	f.pipeline("passed", "yuno/dep")
	f.merge(pre)
	f.pipeline("passed", "main")
	return pre, dep, h1
}

// DESIGN wave 3 rule 12 with rule 11: once the platform updated the dependent from main, it
// is let go only when CI passed on exactly its new head; the pass recorded for the old head
// does not count, even when a record claims it.
func TestResumeAfterCIOnNewSHA(t *testing.T) {
	f := newForgeEnv(t)
	_, dep, h1 := f.prepared(false)

	f.tick()
	h2 := f.tip("yuno/dep")
	got := f.get(dep.ID)
	if h2 == h1 || *got.HeadSHA != h2 || *got.CISHA != h2 || got.CIStatus != store.CIStatusPending || got.UpdatedFromDefaultAt == nil {
		t.Fatalf("after the update: tip %s (was %s), record %+v", h2, h1, got)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("let go before CI on the new head: %d", n)
	}

	// A record that still says the old head passed is not enough.
	_, err := f.pool.Exec(context.Background(), `UPDATE merge_requests SET ci_status = 'passed', ci_sha = $2 WHERE id = $1`, dep.ID, h1)
	must(t, err)
	f.sync.WatchDependencies(context.Background())
	if n := len(f.calls()); n != 0 {
		t.Fatalf("let go on the old head's pass: %d", n)
	}

	// CI passes on exactly h2: let go once, with the note.
	f.pipeline("passed", "yuno/dep")
	f.tick()
	f.tick()
	calls := f.calls()
	want := "Its prerequisite !1 is merged; the platform merged main into this branch (" + h2[:7] + ") and CI passed."
	if len(calls) != 1 || calls[0].id != dep.ID || calls[0].note != want {
		t.Fatalf("hook calls = %+v, want one for %s with %q", calls, dep.ID, want)
	}
	if n := f.eventsOf(dep.RunID, EventUnblocked); n != 1 {
		t.Fatalf("merge_request.unblocked events = %d", n)
	}
	edges, err := store.DependenciesOf(context.Background(), f.pool, store.DependencyMergeRequest, dep.ID)
	must(t, err)
	if len(edges) != 1 || edges[0].ClearedAt == nil || edges[0].SatisfiedAt == nil {
		t.Fatalf("edges after the release = %+v", edges)
	}
	if g, err := store.GetDependencyGate(context.Background(), f.pool, dep.ID); err != nil || g != nil {
		t.Fatalf("gate after the release = %+v %v", g, err)
	}
}

// DESIGN wave 3 rule 12: a failed CI on the updated head keeps the dependent held with
// "CI failed on <sha7>: <url>", the operator told once per commit; a new head that already
// contains main is up to date, waits for its own CI, and is let go when it passes.
func TestCIFailedKeepsBlocked(t *testing.T) {
	f := newForgeEnv(t)
	_, dep, _ := f.prepared(true)
	f.tick()
	h2 := f.tip("yuno/dep")

	f.pipeline("failed", "yuno/dep")
	f.tick()
	f.tick()
	got := f.get(dep.ID)
	reason := f.gateReason(dep.ID)
	if got.CIStatus != store.CIStatusFailed || !strings.HasPrefix(reason, "CI failed on "+h2[:7]+": ") || got.CIURL == nil ||
		!strings.HasSuffix(reason, *got.CIURL) {
		t.Fatalf("record %+v, reason %q", got, reason)
	}
	msgs := f.rep.messages()
	if len(msgs) != 1 || msgs[0].runID != dep.RunID || !strings.Contains(msgs[0].text, "merge request !2 is blocked: CI failed on "+h2[:7]) ||
		!strings.HasPrefix(msgs[0].key, "report-"+dep.RunID+"-mr-dependency-") {
		t.Fatalf("messages = %+v", msgs)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("let go on a failed CI: %d", n)
	}

	// A fix pushed on top of h2 already contains main; its CI fails too: told again, once.
	h3 := f.push("yuno/dep", "yuno/dep", map[string]string{"fix.txt": "fix\n"})
	f.pipeline("failed", "yuno/dep")
	f.tick()
	f.tick()
	if tip := f.tip("yuno/dep"); tip != h3 {
		t.Fatalf("an up-to-date branch was pushed to: %s, want %s", tip, h3)
	}
	if msgs := f.rep.messages(); len(msgs) != 2 || !strings.Contains(msgs[1].text, "CI failed on "+h3[:7]) {
		t.Fatalf("messages = %+v", msgs)
	}

	f.pipeline("passed", "yuno/dep")
	f.tick()
	calls := f.calls()
	want := "Its prerequisite !1 is merged; this branch already contains main (" + h3[:7] + ") and CI passed."
	if len(calls) != 1 || calls[0].note != want {
		t.Fatalf("hook calls = %+v, want %q", calls, want)
	}
}

// DESIGN wave 3 rule 9 in the watcher: a dependent that conflicts with main once its
// prerequisite merged stays held, nothing pushed, the operator told once.
func TestDependentConflictsKeepBlocked(t *testing.T) {
	f := newForgeEnv(t)
	pre := f.open("yuno/pre", map[string]string{"shared.txt": "from the prerequisite\n"}, false)
	dep := f.open("yuno/dep", map[string]string{"shared.txt": "from the dependent\n"}, true)
	f.depend(dep, pre)
	h1 := f.tip("yuno/dep")
	f.merge(pre)
	f.pipeline("passed", "main")
	f.tick()
	f.tick()
	if tip := f.tip("yuno/dep"); tip != h1 {
		t.Fatalf("a conflicting merge was pushed: %s", tip)
	}
	if reason := f.gateReason(dep.ID); reason != "conflicts with main in shared.txt" {
		t.Fatalf("reason = %q", reason)
	}
	if got := f.get(dep.ID); got.Conflicts == nil || len(got.Conflicts.Files) != 1 {
		t.Fatalf("conflicts not recorded: %+v", got.Conflicts)
	}
	if msgs := f.rep.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].text, "is blocked: conflicts with main in shared.txt") {
		t.Fatalf("messages = %+v", msgs)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("let go with conflicts: %d", n)
	}
}

// Acceptance, DESIGN wave 3 (TestDependencyAcceptance): !2 (the dependent, "!24") depends on
// !1 (the prerequisite, "!17"). While !1 is open nothing moves; !1 merges and main's
// pipeline passes on its merge commit; the watcher updates !2 from main with a push, its
// ci_status goes pending; the fake pipeline passes on the new commit; the re-queue hook is
// called once for !2.
func TestDependencyAcceptance(t *testing.T) {
	f := newForgeEnv(t)
	ctx := context.Background()
	pre := f.open("yuno/pre", map[string]string{"pre.txt": "prerequisite\n"}, false)
	dep := f.open("yuno/dep", map[string]string{"dep.txt": "dependent\n"}, false)
	f.depend(dep, pre)
	f.pipeline("passed", "main")
	f.pipeline("passed", "yuno/dep")
	h1 := f.tip("yuno/dep")

	f.tick()
	st, err := f.sync.Gate().MergeRequest(ctx, dep.ID)
	must(t, err)
	if st.Satisfied || st.Reason() != "!1 is not merged" || f.tip("yuno/dep") != h1 || len(f.calls()) != 0 {
		t.Fatalf("while !1 is open: %+v, tip %s", st, f.tip("yuno/dep"))
	}

	// !1 merges; main's pipeline has not run on the merge commit yet. A sync of !1 alone looks
	// at its dependents right after it (rule 8).
	f.merge(pre)
	if _, err := f.sync.SyncOne(ctx, pre.ID); err != nil {
		t.Fatal(err)
	}
	merged := f.get(pre.ID)
	if merged.State != store.MergeRequestMerged || merged.MergeSHA == nil || *merged.MergeSHA != f.tip("main") {
		t.Fatalf("prerequisite after the merge = %+v", merged)
	}
	edges, err := store.DependenciesOf(ctx, f.pool, store.DependencyMergeRequest, dep.ID)
	must(t, err)
	if want := "!1 merged; CI on " + (*merged.MergeSHA)[:7] + " is pending"; edges[0].Reason == nil || *edges[0].Reason != want || f.tip("yuno/dep") != h1 {
		t.Fatalf("before main's pipeline: reason %v, want %q", edges[0].Reason, want)
	}

	// Main passes on exactly the merge commit: the watcher updates !2 from main.
	f.pipeline("passed", "main")
	f.tick()
	h2 := f.tip("yuno/dep")
	got := f.get(dep.ID)
	if h2 == h1 || *got.HeadSHA != h2 || got.CIStatus != store.CIStatusPending || *got.CISHA != h2 {
		t.Fatalf("after the update: tip %s, record %+v", h2, got)
	}
	if n := f.eventsOf(dep.RunID, EventUpdated); n != 1 {
		t.Fatalf("merge_request.updated events = %d", n)
	}
	f.tick()
	if reason := f.gateReason(dep.ID); reason != "waiting for CI on "+h2[:7] || len(f.calls()) != 0 {
		t.Fatalf("waiting: reason %q, calls %v", reason, f.calls())
	}

	// The fake pipeline passes on the new commit: the hook is called once for !2.
	f.pipeline("passed", "yuno/dep")
	f.tick()
	f.tick()
	calls := f.calls()
	if len(calls) != 1 || calls[0].id != dep.ID || !strings.Contains(calls[0].note, "("+h2[:7]+") and CI passed") {
		t.Fatalf("hook calls = %+v", calls)
	}
}
