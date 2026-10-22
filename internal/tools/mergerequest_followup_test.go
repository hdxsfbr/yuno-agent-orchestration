package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
)

// openWith runs one open_merge_request call of the run with the given title, description,
// draft (nil: omitted) and paths (nil: the whole working tree).
func (e *recordEnv) openWith(k, title, desc string, draft *bool, paths []string) (Outcome, mergeRequestOutput, error) {
	e.t.Helper()
	in := map[string]any{"run_token": "r", "title": title, "description": desc}
	if draft != nil {
		in["draft"] = *draft
	}
	if paths != nil {
		in["paths"] = paths
	}
	input, _ := json.Marshal(in)
	out, err := e.svc.Execute(context.Background(), e.tool, Call{RunID: e.run.ID, Workspace: e.ws, IdempotencyKey: k,
		ToolCallID: store.NewID(), Input: input})
	var res mergeRequestOutput
	if err == nil && !out.Result.IsError && len(out.Result.Output) > 0 {
		must(e.t, json.Unmarshal(out.Result.Output, &res))
	}
	return out, res, err
}

// mergeEarly is the operator merging the run's first request on the provider before the run
// is done: the default branch fast-forwards to the request's branch, GitLab removes the
// branch, and the provider reports the request merged. The record is left open, as it is
// until the next sync.
func (e *recordEnv) mergeEarly(branch string) {
	e.t.Helper()
	projecttest.Git(e.t, e.bare, "update-ref", "refs/heads/main", "refs/heads/"+branch)
	if e.gl != nil {
		projecttest.Git(e.t, e.bare, "update-ref", "-d", "refs/heads/"+branch)
		e.gl.merge(1)
		return
	}
	e.gh.merge(1)
}

// request is the provider's nth request: its title and description.
func (e *recordEnv) request(n int) (title, description string) {
	e.t.Helper()
	if e.gh != nil {
		e.gh.mu.Lock()
		defer e.gh.mu.Unlock()
		title, _ = e.gh.pulls[n-1]["title"].(string)
		description, _ = e.gh.pulls[n-1]["body"].(string)
		return title, description
	}
	e.gl.mu.Lock()
	defer e.gl.mu.Unlock()
	title, _ = e.gl.mrs[n-1]["title"].(string)
	description, _ = e.gl.mrs[n-1]["description"].(string)
	return title, description
}

func (e *recordEnv) write(name, content string) {
	e.t.Helper()
	must(e.t, os.MkdirAll(filepath.Dir(filepath.Join(e.checkout, name)), 0o755))
	e.edit(name, content)
}

// DESIGN 5.29 rule 4: the draft's description tells the operator not to merge it until
// Yuno marks it ready, and the ready call's description has no such notice.
func TestOpenMergeRequestDraftSaysDoNotMerge(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			e.write("src.go", "package main\n")
			if out, _, err := e.openWith(e.key('a'), "Add thing", "the code", bp(true), nil); err != nil || out.Result.IsError {
				t.Fatalf("draft: %+v %v", out, err)
			}
			_, desc := e.request(1)
			notice := "**Do not merge this " + e.pc.RequestName() + " yet.** Yuno is still working on it"
			if !strings.HasPrefix(desc, notice) || !strings.HasSuffix(desc, "\n\nthe code") {
				t.Fatalf("draft description = %q", desc)
			}
			if provider == project.ProviderGitLab {
				script(&e.gl.scripted, "PUT /merge_requests/1", 200, jm{})
			} else {
				script(&e.gh.scripted, "PATCH /pulls/1", 200, jm{})
				script(&e.gh.scripted, "POST /graphql", 200, jm{"data": jm{}})
			}
			e.write("docs/plans/x/tickets/T1.md", "# T1\n")
			if out, res, err := e.openWith(e.key('b'), "Add thing", "the code and the trace", bp(false), []string{"docs/plans/x"}); err != nil || out.Result.IsError || res.Draft {
				t.Fatalf("ready: %+v %v", out, err)
			}
			var ready string
			if e.gl != nil {
				ready, _ = e.gl.bodies["PUT /merge_requests/1"]["description"].(string)
			} else {
				ready, _ = e.gh.bodies["PATCH /pulls/1"]["body"].(string)
			}
			if ready != "the code and the trace" {
				t.Fatalf("ready description = %q", ready)
			}
		})
	}
}

// DESIGN 5.29 rule 4: the operator merged the draft before the run's last push. The ready
// call reads the request's state from the provider (the record still says open), pushes
// what is left on a new branch from the default branch and opens a follow-up holding only
// those files, recorded as the follow-up of the run's request and answered with
// follow_up; a later call pushes onto that follow-up instead of opening another.
func TestOpenMergeRequestFollowUpAfterAnEarlyMerge(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			ctx := context.Background()
			e.write("src.go", "package main\n")
			out, draft, err := e.openWith(e.key('a'), "Add thing", "the code", bp(true), nil)
			if err != nil || out.Result.IsError {
				t.Fatalf("draft: %+v %v", out, err)
			}
			own := e.record()
			e.mergeEarly(draft.Branch)

			e.write("docs/plans/x/tickets/T1.md", "# T1 trace\n")
			out, res, err := e.openWith(e.key('b'), "Add thing", "the trace", bp(false), []string{"docs/plans/x"})
			if err != nil || out.Result.IsError {
				t.Fatalf("ready after the merge: %+v %v", out, err)
			}
			branch := GitLabBranch(e.key('b'))
			if !res.FollowUp || res.Number != 2 || res.Branch != branch || res.Replayed || res.Updated || res.Draft || res.Merged || e.requests() != 2 {
				t.Fatalf("output = %s, requests %d", out.Result.Output, e.requests())
			}
			if got := projecttest.Git(t, e.bare, "diff", "--name-only", "main", branch); got != "docs/plans/x/tickets/T1.md" {
				t.Fatalf("the follow-up holds %q, want only the trace", got)
			}
			title, desc := e.request(2)
			if title != "Follow-up: Add thing" || !strings.HasPrefix(desc, "Follow-up to "+own.Label()+" ("+own.URL+"), which was merged before Yuno") ||
				!strings.HasSuffix(desc, "\n\nthe trace") || strings.Contains(desc, "Do not merge") {
				t.Fatalf("follow-up title %q, description %q", title, desc)
			}
			fu, err := store.FollowUpMergeRequest(ctx, testPool, own.ID)
			if err != nil || fu == nil || fu.Number != 2 || fu.RunID != e.run.ID || fu.SourceBranch != branch || fu.Title != "Follow-up: Add thing" ||
				fu.Draft || fu.State != store.MergeRequestOpen || fu.URL != res.URL {
				t.Fatalf("follow-up record = %+v %v", fu, err)
			}
			if rec := e.record(); rec == nil || rec.ID != own.ID || e.records() != 2 {
				t.Fatalf("the run's own record = %+v, records %d", rec, e.records())
			}

			// A later call: the follow-up is the run's open request now.
			e.write("docs/plans/x/tickets/T1.md", "# T1 trace, amended\n")
			out, later, err := e.openWith(e.key('c'), "Add thing", "the trace", nil, []string{"docs/plans/x"})
			if err != nil || out.Result.IsError {
				t.Fatalf("later call: %+v %v", out, err)
			}
			if !later.FollowUp || later.Number != 2 || !later.Replayed || !later.Updated || e.requests() != 2 || e.records() != 2 {
				t.Fatalf("later output = %s, requests %d, records %d", out.Result.Output, e.requests(), e.records())
			}
			if projecttest.FileAt(t, e.bare, branch, "docs/plans/x/tickets/T1.md") != "# T1 trace, amended\n" {
				t.Fatal("the later call did not push onto the follow-up")
			}
		})
	}
}

// DESIGN 5.29 rule 4: a merged request with nothing left to push is a no-op: the answer is
// the merged request with merged set, and nothing is pushed, opened or recorded.
func TestOpenMergeRequestMergedWithNothingLeft(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			e.write("src.go", "package main\n")
			out, draft, err := e.openWith(e.key('a'), "Add thing", "the code", bp(true), nil)
			if err != nil || out.Result.IsError {
				t.Fatalf("draft: %+v %v", out, err)
			}
			e.mergeEarly(draft.Branch)
			branches := projecttest.Branches(t, e.bare)

			out, res, err := e.openWith(e.key('b'), "Add thing", "the code", bp(false), nil)
			if err != nil || out.Result.IsError {
				t.Fatalf("call after the merge: %+v %v", out, err)
			}
			if !res.Merged || res.FollowUp || res.Number != 1 || res.URL != draft.URL || !res.Replayed || res.Updated {
				t.Fatalf("output = %s", out.Result.Output)
			}
			if e.requests() != 1 || e.records() != 1 || len(projecttest.Branches(t, e.bare)) != len(branches) {
				t.Fatalf("requests %d, records %d, branches %v (were %v)", e.requests(), e.records(), projecttest.Branches(t, e.bare), branches)
			}
		})
	}
}

// DESIGN 5.20 edge cases: a request the provider reports closed (not merged) is still
// refused, with nothing pushed or opened.
func TestOpenMergeRequestClosedOnTheProvider(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	e.write("src.go", "package main\n")
	if out, _, err := e.openWith(e.key('a'), "Add thing", "the code", bp(true), nil); err != nil || out.Result.IsError {
		t.Fatalf("draft: %+v %v", out, err)
	}
	e.gl.mu.Lock()
	e.gl.mrs[0]["state"] = "closed"
	e.gl.mu.Unlock()
	e.write("more.go", "package main\n")
	out, _, err := e.openWith(e.key('b'), "Add thing", "more", nil, nil)
	if err != nil || !out.Result.IsError || out.Result.Text != "the run's merge request !1 is closed; nothing more can be pushed to it" {
		t.Fatalf("call on a closed request: %+v %v", out, err)
	}
	if e.requests() != 1 || e.records() != 1 {
		t.Fatalf("requests %d, records %d", e.requests(), e.records())
	}
}
