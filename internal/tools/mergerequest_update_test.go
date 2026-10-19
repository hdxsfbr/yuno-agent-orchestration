package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// The tools half of DESIGN 5.22 rule 4 and 5.23 rule 7: open_merge_request in a run that
// names a request pushes onto that request's branch under the branch guard, and a run
// from an issue closes the issue when its request is created.

// branchMovedText is the model-facing error of 5.22 rule 4.
func branchMovedText(branch string) string {
	return "the branch " + branch + " has new commits since this run cloned it; nothing was pushed. " +
		"Finish and report it; a new run on the request starts from the branch as it is now"
}

// updater is a run that names the request e's first run opened, with a checkout cloned
// from the request's branch (the way the engine clones it) and its head recorded as the
// run's checkout_sha.
type updater struct {
	run      *store.Run
	ws       string
	checkout string
	// base is the commit the checkout was cloned at.
	base string
}

// openFirst opens the request with the environment's own run (a change to README.md) and
// returns the record.
func (e *recordEnv) openFirst() *store.MergeRequest {
	e.t.Helper()
	e.edit("README.md", "# demo\nv1\n")
	if out, _, err := e.open(e.key('a'), "First title"); err != nil || out.Result.IsError {
		e.t.Fatalf("open the request: %+v %v", out, err)
	}
	rec := e.record()
	if rec == nil {
		e.t.Fatal("no record after the first call")
	}
	// The opening run is done, as the queue-time check of rule 1 would need.
	_, err := testPool.Exec(context.Background(), `UPDATE runs SET status = 'succeeded', finished_at = now() WHERE id = $1`, e.run.ID)
	must(e.t, err)
	return rec
}

// newUpdater queues a run that names rec and clones its branch. mut adjusts the run first.
func (e *recordEnv) newUpdater(rec *store.MergeRequest, mut func(r *store.Run)) *updater {
	e.t.Helper()
	run := projectRun(e.t, e.project, func(r *store.Run) {
		r.MergeRequestID = &rec.ID
		if mut != nil {
			mut(r)
		}
	})
	u := &updater{run: run, ws: e.t.TempDir()}
	must(e.t, os.MkdirAll(filepath.Join(u.ws, "runs", run.ID), 0o755))
	u.checkout = filepath.Join(u.ws, project.RepoRel(run.ID))
	ctx := context.Background()
	must(e.t, e.pc.CloneBranch(ctx, u.checkout, rec.SourceBranch))
	var err error
	if u.base, err = e.pc.HeadSHA(ctx, u.checkout); err != nil {
		e.t.Fatal(err)
	}
	must(e.t, store.SetRunCheckoutSHA(ctx, testPool, run.ID, u.base))
	return u
}

func (u *updater) edit(t *testing.T, name, content string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(u.checkout, name), []byte(content), 0o644))
}

// call runs open_merge_request for the updating run under the nth key of that run.
func (e *recordEnv) call(u *updater, n byte, title string) (Outcome, mergeRequestOutput, error) {
	e.t.Helper()
	sum := sha256.Sum256([]byte(u.run.ID + string(n)))
	input, _ := json.Marshal(map[string]string{"run_token": "r", "title": title, "description": "the description"})
	out, err := e.svc.Execute(context.Background(), e.tool, Call{RunID: u.run.ID, Workspace: u.ws, IdempotencyKey: hex.EncodeToString(sum[:]),
		ToolCallID: store.NewID(), Input: input})
	var res mergeRequestOutput
	if err == nil && !out.Result.IsError && len(out.Result.Output) > 0 {
		must(e.t, json.Unmarshal(out.Result.Output, &res))
	}
	return out, res, err
}

func (e *recordEnv) checkoutSHA(run *store.Run) string {
	e.t.Helper()
	r, err := store.GetRun(context.Background(), testPool, run.ID)
	must(e.t, err)
	if r.CheckoutSHA == nil {
		return "<null>"
	}
	return *r.CheckoutSHA
}

func (e *recordEnv) tip(branch string) string {
	e.t.Helper()
	return projecttest.Git(e.t, e.bare, "rev-parse", branch)
}

func (e *recordEnv) countRecords() int {
	e.t.Helper()
	var n int
	must(e.t, testPool.QueryRow(context.Background(), `SELECT count(*) FROM merge_requests WHERE project_id = $1`, e.project.ID).Scan(&n))
	return n
}

// pushByHand puts one commit on branch of the bare repository from a clone of its own, the
// way a person pushing to the request's branch does, and returns the commit's id.
func pushByHand(t *testing.T, bare, branch, file, content string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "hand")
	projecttest.Git(t, filepath.Dir(work), "clone", "--quiet", "--branch", branch, bare, work)
	must(t, os.WriteFile(filepath.Join(work, file), []byte(content), 0o644))
	projecttest.Git(t, work, "add", "--all")
	projecttest.Git(t, work, "commit", "--quiet", "-m", "by hand: "+file)
	projecttest.Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return projecttest.Git(t, work, "rev-parse", "HEAD")
}

// T5.22.4: on a run that names a request, the call pushes one commit on top of the branch
// the run cloned (the clone's commit is its parent), answers the record's number and URL
// with replayed and updated, writes no second record and moves checkout_sha to the new
// commit; a second call of the same run passes the guard; an unchanged tree answers
// replayed only.
func TestOpenMergeRequestOnANamedRequest(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			rec := e.openFirst()
			branch := rec.SourceBranch
			u := e.newUpdater(rec, nil)
			if u.base != e.tip(branch) || e.checkoutSHA(u.run) != u.base {
				t.Fatalf("setup: base %s, branch %s, checkout_sha %s", u.base, e.tip(branch), e.checkoutSHA(u.run))
			}
			branches := len(projecttest.Branches(t, e.bare))
			requests, seen := e.requests(), e.seen()

			// The agents' change: an edit and a new file on top of the branch's tree.
			u.edit(t, "README.md", "# demo\nv1\nfixed the review comment\n")
			u.edit(t, "added.txt", "more\n")
			out, got, err := e.call(u, 'a', "Address the review")
			if err != nil || out.Result.IsError {
				t.Fatalf("first call: %+v %v", out, err)
			}
			if got.Number != rec.Number || got.URL != rec.URL || got.Branch != branch || got.Provider != string(provider) || !got.Replayed || !got.Updated || out.Replayed {
				t.Fatalf("output = %s (service replay %v)", out.Result.Output, out.Replayed)
			}
			first := e.tip(branch)
			if first == u.base || projecttest.Git(t, e.bare, "rev-parse", branch+"~1") != u.base {
				t.Fatalf("the new commit must sit on top of the clone's commit %s: tip %s, parent %s", u.base, first, projecttest.Git(t, e.bare, "rev-parse", branch+"~1"))
			}
			if projecttest.Subject(t, e.bare, branch) != "Address the review" || projecttest.FileAt(t, e.bare, branch, "README.md") != "# demo\nv1\nfixed the review comment\n" ||
				projecttest.FileAt(t, e.bare, branch, "added.txt") != "more\n" {
				t.Fatal("the branch does not hold the run's tree")
			}
			// No second record, no new request, no new branch, nothing asked of the provider.
			if e.countRecords() != 1 || e.requests() != requests || len(projecttest.Branches(t, e.bare)) != branches || e.seen() != seen {
				t.Fatalf("records %d, requests %d, branches %v, provider calls %d -> %d", e.countRecords(), e.requests(), projecttest.Branches(t, e.bare), seen, e.seen())
			}
			if own, err := store.MergeRequestByRun(context.Background(), testPool, u.run.ID); err != nil || own != nil {
				t.Fatalf("the updating run got a record of its own: %+v %v", own, err)
			}
			if after := e.record(); after.RunID != e.run.ID || after.Title != "First title" || after.Number != rec.Number || after.State != store.MergeRequestOpen {
				t.Fatalf("the record changed: %+v", after)
			}
			if got := e.checkoutSHA(u.run); got != first {
				t.Fatalf("checkout_sha = %s, want the pushed commit %s", got, first)
			}
			// Nothing token-shaped is stored.
			if _, stored := Render(out.Result, 0); strings.Contains(string(stored), e.pc.Token) {
				t.Fatal("the token is in the stored result")
			}

			// A second call of the same run passes the guard: the checkout matches the branch.
			u.edit(t, "added.txt", "more and more\n")
			out2, second, err := e.call(u, 'b', "Address it again")
			if err != nil || out2.Result.IsError || !second.Replayed || !second.Updated || second.Number != rec.Number {
				t.Fatalf("second call: %+v %v", out2, err)
			}
			if e.tip(branch) == first || projecttest.Git(t, e.bare, "rev-parse", branch+"~1") != first || e.checkoutSHA(u.run) != e.tip(branch) {
				t.Fatal("the second push is not one commit on top of the first, or checkout_sha lags")
			}

			// Nothing changed since: replayed only, nothing pushed, checkout_sha as it was.
			tip := e.tip(branch)
			out3, third, err := e.call(u, 'c', "And again")
			if err != nil || out3.Result.IsError || !third.Replayed || third.Updated || strings.Contains(string(out3.Result.Output), "updated") || third.Number != rec.Number {
				t.Fatalf("third call: %+v %v", out3, err)
			}
			if e.tip(branch) != tip || e.checkoutSHA(u.run) != tip || e.countRecords() != 1 || e.requests() != requests {
				t.Fatal("an unchanged tree pushed or recorded something")
			}

			// The same key as the second call: the stored answer, no new work.
			out4, fourth, err := e.call(u, 'b', "Address it again")
			if err != nil || !out4.Replayed || !fourth.Updated || e.tip(branch) != tip {
				t.Fatalf("same key again: %+v %v", out4, err)
			}
		})
	}
}

// T5.22.5: a commit pushed to the branch between the clone and the call makes the tool
// answer the text of rule 4 and push nothing (the key is released, checkout_sha stays,
// the record is untouched); a null or empty checkout_sha skips the guard; the same-run
// path of 5.20 keeps its behaviour (no guard, no checkout_sha written: L4 stays recorded).
func TestOpenMergeRequestBranchGuard(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			ctx := context.Background()
			rec := e.openFirst()
			branch := rec.SourceBranch
			u := e.newUpdater(rec, nil)
			hand := pushByHand(t, e.bare, branch, "by-hand.txt", "someone else's work\n")
			branches := projecttest.Branches(t, e.bare)

			u.edit(t, "README.md", "# demo\nmy fix\n")
			out, _, err := e.call(u, 'a', "Address the review")
			if err != nil || !out.Result.IsError || out.Result.Text != branchMovedText(branch) {
				t.Fatalf("moved branch: %+v %v\nwant %q", out, err, branchMovedText(branch))
			}
			if e.tip(branch) != hand || projecttest.FileAt(t, e.bare, branch, "by-hand.txt") != "someone else's work\n" ||
				projecttest.FileAt(t, e.bare, branch, "README.md") != "# demo\nv1\n" {
				t.Fatal("the refused call pushed something")
			}
			if strings.Join(projecttest.Branches(t, e.bare), ",") != strings.Join(branches, ",") {
				t.Fatalf("branches changed: %v -> %v", branches, projecttest.Branches(t, e.bare))
			}
			if e.checkoutSHA(u.run) != u.base || e.countRecords() != 1 || e.record().Title != "First title" {
				t.Fatalf("checkout_sha %s (base %s), records %d", e.checkoutSHA(u.run), u.base, e.countRecords())
			}
			// The run's checkout keeps the agents' work, and the key is free again.
			if got, _ := os.ReadFile(filepath.Join(u.checkout, "README.md")); string(got) != "# demo\nmy fix\n" {
				t.Fatalf("the checkout lost the agents' work: %q", got)
			}
			sum := sha256.Sum256([]byte(u.run.ID + "a"))
			if _, err := store.GetSideEffect(ctx, testPool, hex.EncodeToString(sum[:])); err == nil {
				t.Fatal("a refused call left its side effect")
			}

			// A null checkout_sha (a run cloned before the migration) skips the guard: the
			// push goes on top of the branch as it is now.
			_, err = testPool.Exec(ctx, `UPDATE runs SET checkout_sha = NULL WHERE id = $1`, u.run.ID)
			must(t, err)
			out, got, err := e.call(u, 'b', "Address the review")
			if err != nil || out.Result.IsError || !got.Updated {
				t.Fatalf("null checkout_sha: %+v %v", out, err)
			}
			pushed := e.tip(branch)
			if pushed == hand || projecttest.Git(t, e.bare, "rev-parse", branch+"~1") != hand || e.checkoutSHA(u.run) != pushed {
				t.Fatalf("the unguarded push: tip %s, hand %s, checkout_sha %s", pushed, hand, e.checkoutSHA(u.run))
			}

			// An empty checkout_sha skips it too.
			hand2 := pushByHand(t, e.bare, branch, "by-hand-2.txt", "again\n")
			_, err = testPool.Exec(ctx, `UPDATE runs SET checkout_sha = '' WHERE id = $1`, u.run.ID)
			must(t, err)
			u.edit(t, "README.md", "# demo\nmy second fix\n")
			out, got, err = e.call(u, 'c', "Address the review again")
			if err != nil || out.Result.IsError || !got.Updated || projecttest.Git(t, e.bare, "rev-parse", branch+"~1") != hand2 {
				t.Fatalf("empty checkout_sha: %+v %v", out, err)
			}
		})
	}
}

// T5.22.5 (the same-run path): a run that opened its own request and pushes onto it
// again is not guarded and writes no checkout_sha (5.20 rule 2, L4 recorded).
func TestOpenMergeRequestSameRunPathIsUnguarded(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.edit("README.md", "# demo\nv1\n")
	if out, _, err := e.open(e.key('a'), "First title"); err != nil || out.Result.IsError {
		t.Fatalf("open: %+v %v", out, err)
	}
	branch := e.record().SourceBranch
	hand := pushByHand(t, e.bare, branch, "by-hand.txt", "someone else's work\n")
	e.edit("README.md", "# demo\nv2\n")
	out, got, err := e.open(e.key('b'), "Second title")
	if err != nil || out.Result.IsError || !got.Replayed || !got.Updated {
		t.Fatalf("same-run update: %+v %v", out, err)
	}
	if e.tip(branch) == hand || projecttest.FileAt(t, e.bare, branch, "by-hand.txt") != "" {
		t.Fatal("the same-run path is expected to replace the tree (L4)")
	}
	if e.checkoutSHA(e.run) != "<null>" {
		t.Fatalf("the same-run path wrote checkout_sha %s", e.checkoutSHA(e.run))
	}
}

// T5.22.6 (tool half): a named request that turned merged (or closed) after the run was
// queued: the call answers the final-state text and pushes nothing.
func TestOpenMergeRequestOnAFinalNamedRequest(t *testing.T) {
	for _, tc := range []struct {
		provider project.Provider
		state    string
		want     string
	}{
		{project.ProviderGitLab, store.MergeRequestMerged, "the run's merge request !1 is merged; nothing more can be pushed to it"},
		{project.ProviderGitHub, store.MergeRequestClosed, "the run's pull request #1 is closed; nothing more can be pushed to it"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			e := newRecordEnv(t, tc.provider)
			rec := e.openFirst()
			u := e.newUpdater(rec, nil)
			_, err := testPool.Exec(context.Background(), `UPDATE merge_requests SET state = $2, closed_at = now() WHERE id = $1`, rec.ID, tc.state)
			must(t, err)
			tip := e.tip(rec.SourceBranch)
			u.edit(t, "README.md", "# demo\nlate fix\n")
			out, _, err := e.call(u, 'a', "Too late")
			if err != nil || !out.Result.IsError || out.Result.Text != tc.want {
				t.Fatalf("final request: %+v %v\nwant %q", out, err, tc.want)
			}
			if e.tip(rec.SourceBranch) != tip || e.checkoutSHA(u.run) != u.base || e.requests() != 1 || e.countRecords() != 1 {
				t.Fatal("a call on a final request changed something")
			}
		})
	}
}

// The failure handling of 5.22: a checkout_sha write that fails after the push is a tool
// error that names the step and says the push landed.
func TestOpenMergeRequestCheckoutSHAWriteFailure(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	rec := e.openFirst()
	u := e.newUpdater(rec, nil)
	u.edit(t, "README.md", "# demo\nfixed\n")
	tx, err := testPool.Begin(context.Background())
	must(t, err)
	must(t, tx.Rollback(context.Background())) // every statement on it now fails
	m := &mergeRequest{}
	run, err := store.GetRun(context.Background(), testPool, u.run.ID)
	must(t, err)
	res, err := m.updateRecorded(context.Background(), Call{RunID: u.run.ID, Tx: tx}, &e.pc, nil, rec, mergeRequestInput{Title: "Fix"}, u.checkout, run, run)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "record the new checkout commit after the push") ||
		!strings.Contains(res.Text, "pushed to "+rec.SourceBranch) || strings.Contains(res.Text, e.pc.Token) {
		t.Fatalf("result = %+v %v", res, err)
	}
	if e.tip(rec.SourceBranch) == u.base {
		t.Fatal("the push did not land")
	}
}

// issueRun switches the environment to a run that came from issue n, with its own checkout
// of the default branch.
func (e *recordEnv) issueRun(n string) {
	e.t.Helper()
	run := projectRun(e.t, e.project, func(r *store.Run) {
		ref := e.project.ID + "#" + n
		r.Trigger, r.TriggerRef = store.TriggerIssue, &ref
	})
	e.run = run
	e.ws = e.t.TempDir()
	must(e.t, os.MkdirAll(filepath.Join(e.ws, "runs", run.ID), 0o755))
	e.checkout = filepath.Join(e.ws, project.RepoRel(run.ID))
	must(e.t, e.pc.Clone(context.Background(), e.checkout))
}

// T5.23.8 (the description): "\n\nCloses #<n>" is appended for an issue run unless the
// description already says "closes #<n>" in any case; no other run's description changes.
func TestWithIssueCloses(t *testing.T) {
	issue := func(ref string) *store.Run { return &store.Run{Trigger: store.TriggerIssue, TriggerRef: &ref} }
	for _, tc := range []struct {
		name string
		run  *store.Run
		desc string
		want string
	}{
		{"appended", issue("pr-1#7"), "Fixes the login bug.", "Fixes the login bug.\n\nCloses #7"},
		{"already there", issue("pr-1#7"), "Fixes the login bug.\n\nCloses #7", "Fixes the login bug.\n\nCloses #7"},
		{"already there, any case", issue("pr-1#7"), "It CLOSES #7 for good.", "It CLOSES #7 for good."},
		{"already there mid-line", issue("pr-1#7"), "closes #7, closes #8", "closes #7, closes #8"},
		{"another issue's number", issue("pr-1#7"), "Closes #8", "Closes #8\n\nCloses #7"},
		{"a longer number is not this one", issue("pr-1#7"), "Closes #70", "Closes #70\n\nCloses #7"},
		{"mentioned, not closed", issue("pr-1#7"), "See #7 for context", "See #7 for context\n\nCloses #7"},
		{"empty description", issue("pr-1#7"), "", "\n\nCloses #7"},
		{"a project id with a hash in it", issue("a#b#12"), "d", "d\n\nCloses #12"},
		{"a manual run", &store.Run{Trigger: store.TriggerManual}, "Fixes it.", "Fixes it."},
		{"a manual run with a reference", &store.Run{Trigger: store.TriggerManual, TriggerRef: ptr("pr-1#7")}, "Fixes it.", "Fixes it."},
		{"no run", nil, "Fixes it.", "Fixes it."},
		{"an issue run without a reference", &store.Run{Trigger: store.TriggerIssue}, "Fixes it.", "Fixes it."},
		{"a reference without a number", issue("pr-1#"), "Fixes it.", "Fixes it."},
		{"a reference that is not a number", issue("pr-1#x"), "Fixes it.", "Fixes it."},
	} {
		if got := withIssueCloses(tc.desc, tc.run); got != tc.want {
			t.Errorf("%s: description = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// T5.23.8 (through the tool): the request an issue run creates gets the line, a manual
// run's request does not, and an update changes nothing on the provider.
func TestOpenMergeRequestClosesTheIssue(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			open := func(e *recordEnv, desc string) string {
				t.Helper()
				e.edit("README.md", "# demo\nchange\n")
				input, _ := json.Marshal(map[string]string{"run_token": "r", "title": "T", "description": desc})
				out, err := e.svc.Execute(context.Background(), e.tool, Call{RunID: e.run.ID, Workspace: e.ws, IdempotencyKey: e.key('a'),
					ToolCallID: store.NewID(), Input: input})
				if err != nil || out.Result.IsError {
					t.Fatalf("open: %+v %v", out, err)
				}
				return e.description()
			}
			e := newRecordEnv(t, provider)
			e.issueRun("7")
			if got := open(e, "Fixes the login bug."); got != "Fixes the login bug.\n\nCloses #7" {
				t.Errorf("issue run: description = %q", got)
			}
			e = newRecordEnv(t, provider)
			e.issueRun("7")
			if got := open(e, "Fixes it. Closes #7"); got != "Fixes it. Closes #7" {
				t.Errorf("issue run, already closing: description = %q", got)
			}
			e = newRecordEnv(t, provider)
			if got := open(e, "Fixes the login bug."); got != "Fixes the login bug." {
				t.Errorf("manual run: description = %q", got)
			}

			// Not on an update: a request opened by a manual run, updated by an issue run that
			// names it; the provider is not asked anything and the description stays.
			e = newRecordEnv(t, provider)
			rec := e.openFirst()
			before, seen := e.description(), e.seen()
			u := e.newUpdater(rec, func(r *store.Run) {
				ref := e.project.ID + "#9"
				r.Trigger, r.TriggerRef = store.TriggerIssue, &ref
			})
			u.edit(t, "README.md", "# demo\nfixed\n")
			out, got, err := e.call(u, 'a', "Fix it")
			if err != nil || out.Result.IsError || !got.Updated {
				t.Fatalf("update: %+v %v", out, err)
			}
			if e.description() != before || strings.Contains(e.description(), "Closes") || e.seen() != seen {
				t.Fatalf("an update touched the description: %q (was %q), provider calls %d -> %d", e.description(), before, seen, e.seen())
			}
		})
	}
}

// T5.23.8 (retry): a creation that failed after the push and is retried under the same key
// still gets the line, once.
func TestOpenMergeRequestClosesTheIssueAfterARetry(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.issueRun("7")
	e.gl.failMR = 1
	e.edit("README.md", "# demo\nchange\n")
	input, _ := json.Marshal(map[string]string{"run_token": "r", "title": "T", "description": "Fixes it."})
	call := Call{RunID: e.run.ID, Workspace: e.ws, IdempotencyKey: e.key('a'), ToolCallID: store.NewID(), Input: input}
	if out, err := e.svc.Execute(context.Background(), e.tool, call); err != nil || !out.Result.IsError {
		t.Fatalf("the failed creation: %+v %v", out, err)
	}
	if out, err := e.svc.Execute(context.Background(), e.tool, call); err != nil || out.Result.IsError {
		t.Fatalf("the retry: %+v %v", out, err)
	}
	if got := e.description(); got != "Fixes it.\n\nCloses #7" {
		t.Fatalf("description = %q", got)
	}
}

// Review M1: an iteration that pushes onto its ticket's request returns the ticket from
// building to in_review, so another change request can follow.
func TestIterationReturnsTheTicketToReview(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	ctx := context.Background()
	rec := e.openFirst()
	plan := &store.Plan{ProjectID: e.project.ID, Feature: "iter-" + store.NewID()[:8], PlanRunID: e.run.ID, Status: store.PlanBuilding}
	must(t, store.CreatePlan(ctx, testPool, plan))
	n := int64(7)
	tk := store.PlanTicket{PlanID: plan.ID, TicketID: "T1", Wave: 1, Title: "T1", IssueNumber: &n}
	must(t, store.UpsertPlanTicket(ctx, testPool, &tk))
	must(t, store.SetTicketMR(ctx, testPool, plan.ID, "T1", rec.ID))
	must(t, store.SetTicketStatus(ctx, testPool, plan.ID, "T1", store.TicketInReview))
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM plans WHERE id=$1`, plan.ID) })
	status := func() string {
		tks, err := store.ListPlanTickets(ctx, testPool, plan.ID)
		must(t, err)
		return tks[0].Status
	}
	for i, key := range []byte{'a', 'b'} {
		u := e.newUpdater(rec, nil)
		if moved, err := store.LinkTicketIteration(ctx, testPool, plan.ID, "T1", u.run.ID); err != nil || !moved || status() != store.TicketBuilding {
			t.Fatalf("iteration %d: link %v %v, status %s", i, moved, err, status())
		}
		u.edit(t, "README.md", "# demo\niteration "+string(rune('1'+i))+"\n")
		if out, _, err := e.call(u, key, "Address the review"); err != nil || out.Result.IsError {
			t.Fatalf("iteration %d call: %+v %v", i, out, err)
		}
		if got := status(); got != store.TicketInReview {
			t.Fatalf("iteration %d: ticket is %s, want in_review", i, got)
		}
		must(t, store.SetRunStatus(ctx, testPool, u.run.ID, store.RunSucceeded, nil))
	}
}
