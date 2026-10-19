package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// T5.29.3: draft create and the ready transition, on the providers and through the tool.

func sp(s string) *string { return &s }
func bp(b bool) *bool     { return &b }

func TestGitLabProviderDraft(t *testing.T) {
	g, srv := newFakeGitLab(t)
	p := gitlabStatusProvider(srv)
	ctx := context.Background()
	g.branches["feat"] = true

	ref, err := p.Create(ctx, "feat", "main", "Add thing", "body", true)
	if err != nil || ref.Number != 1 {
		t.Fatalf("create: %+v %v", ref, err)
	}
	if got := g.mrs[0]["title"]; got != "Draft: Add thing" {
		t.Fatalf("draft title = %v", got)
	}
	if _, err := p.Create(ctx, "feat", "main", "Plain", "b", false); err != nil || g.mrs[1]["title"] != "Plain" {
		t.Fatalf("non-draft create: %v %v", err, g.mrs[1]["title"])
	}

	// Ready: the current title is read, the prefix dropped, the description replaced.
	script(&g.scripted, "GET /merge_requests/1", 200, jm{"title": "Draft: Add thing"})
	script(&g.scripted, "PUT /merge_requests/1", 200, jm{})
	if err := p.Update(ctx, 1, RequestUpdate{Description: sp("new body"), Draft: bp(false)}); err != nil {
		t.Fatal(err)
	}
	if b := g.bodies["PUT /merge_requests/1"]; b["title"] != "Add thing" || b["description"] != "new body" {
		t.Fatalf("PUT body = %v", b)
	}
	// A new title keeps the draft state read from the request; an explicit title and
	// state need no read.
	g.calls = nil
	if err := p.Update(ctx, 1, RequestUpdate{Title: sp("Better"), Draft: bp(true)}); err != nil {
		t.Fatal(err)
	}
	if b := g.bodies["PUT /merge_requests/1"]; b["title"] != "Draft: Better" {
		t.Fatalf("PUT body = %v", b)
	}
	if len(g.calls) != 1 {
		t.Fatalf("calls = %v, want only the PUT", g.calls)
	}
	script(&g.scripted, "GET /merge_requests/1", 200, jm{"title": "[Draft] Keep me"})
	if err := p.Update(ctx, 1, RequestUpdate{Title: sp("Renamed")}); err != nil {
		t.Fatal(err)
	}
	if b := g.bodies["PUT /merge_requests/1"]; b["title"] != "Draft: Renamed" {
		t.Fatalf("PUT body = %v", b)
	}
	script(&g.scripted, "PUT /merge_requests/9", 404, jm{"message": "404 Not found"})
	if err := p.Update(ctx, 9, RequestUpdate{Title: sp("x")}); err == nil || err != ErrRequestNotFound && !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing request: %v", err)
	}
}

func TestGitHubProviderDraft(t *testing.T) {
	g, srv := newFakeGitHub(t, t.TempDir())
	p := githubStatusProvider(srv)
	ctx := context.Background()

	if _, err := p.Create(ctx, "feat", "main", "Add thing", "body", true); err != nil {
		t.Fatal(err)
	}
	if g.pulls[0]["draft"] != true {
		t.Fatalf("create body = %v", g.pulls[0])
	}
	if _, err := p.Create(ctx, "feat2", "main", "Plain", "b", false); err != nil {
		t.Fatal(err)
	}
	if _, has := g.pulls[1]["draft"]; has {
		t.Fatalf("non-draft create sent draft: %v", g.pulls[1])
	}

	script(&g.scripted, "PATCH /pulls/1", 200, jm{})
	script(&g.scripted, "GET /pulls/1", 200, jm{"node_id": "PR_node1", "draft": true})
	script(&g.scripted, "POST /graphql", 200, jm{"data": jm{}})
	if err := p.Update(ctx, 1, RequestUpdate{Title: sp("Final"), Description: sp("done"), Draft: bp(false)}); err != nil {
		t.Fatal(err)
	}
	if b := g.bodies["PATCH /pulls/1"]; b["title"] != "Final" || b["body"] != "done" {
		t.Fatalf("PATCH body = %v", b)
	}
	gq := g.bodies["POST /graphql"]
	vars, _ := gq["variables"].(map[string]any)
	if q, _ := gq["query"].(string); !strings.Contains(q, "markPullRequestReadyForReview") || vars["id"] != "PR_node1" {
		t.Fatalf("graphql = %v", gq)
	}

	// Already ready: no mutation. A GraphQL error is surfaced.
	g.calls = nil
	script(&g.scripted, "GET /pulls/1", 200, jm{"node_id": "PR_node1", "draft": false})
	if err := p.Update(ctx, 1, RequestUpdate{Draft: bp(false)}); err != nil || strings.Contains(strings.Join(g.calls, ","), "graphql") {
		t.Fatalf("no-op: %v %v", err, g.calls)
	}
	script(&g.scripted, "GET /pulls/1", 200, jm{"node_id": "PR_node1", "draft": true})
	script(&g.scripted, "POST /graphql", 200, jm{"errors": []jm{{"message": "Resource not accessible"}}})
	if err := p.Update(ctx, 1, RequestUpdate{Draft: bp(false)}); err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("graphql error: %v", err)
	}
}

// openDraft runs one open_merge_request call that names draft (nil: omitted).
func (e *recordEnv) openDraft(k, title, desc string, draft *bool) (Outcome, mergeRequestOutput, error) {
	e.t.Helper()
	in := map[string]any{"run_token": "r", "title": title, "description": desc}
	if draft != nil {
		in["draft"] = *draft
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

// The tool: a draft create records draft, a later call with draft false marks it ready
// with the new title and description and records that, with no second request; a call that
// does not name draft changes nothing on the provider.
func TestOpenMergeRequestDraftThenReady(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			e.edit("README.md", "# demo\nv1\n")
			out, first, err := e.openDraft(e.key('a'), "Add thing", "first body", bp(true))
			if err != nil || out.Result.IsError || !first.Draft {
				t.Fatalf("draft create: %+v %v", out, err)
			}
			if rec := e.record(); rec == nil || !rec.Draft || rec.Title != "Add thing" {
				t.Fatalf("record = %+v", rec)
			}
			if provider == project.ProviderGitLab {
				if e.gl.mrs[0]["title"] != "Draft: Add thing" {
					t.Fatalf("title = %v", e.gl.mrs[0]["title"])
				}
				script(&e.gl.scripted, "PUT /merge_requests/1", 200, jm{})
			} else {
				if e.gh.pulls[0]["draft"] != true {
					t.Fatalf("pull = %v", e.gh.pulls[0])
				}
				script(&e.gh.scripted, "PATCH /pulls/1", 200, jm{})
				script(&e.gh.scripted, "GET /pulls/1", 200, jm{"node_id": "PR_1", "draft": true})
				script(&e.gh.scripted, "POST /graphql", 200, jm{"data": jm{}})
			}

			// Without draft: the request's text and state are untouched.
			e.edit("added.txt", "one\n")
			if out, second, err := e.openDraft(e.key('b'), "Other title", "other body", nil); err != nil || out.Result.IsError || !second.Updated || !second.Draft {
				t.Fatalf("plain update: %+v %v", out, err)
			}
			if e.gl != nil && e.gl.bodies["PUT /merge_requests/1"] != nil || e.gh != nil && e.gh.bodies["PATCH /pulls/1"] != nil {
				t.Fatal("a call without draft rewrote the request")
			}
			if rec := e.record(); !rec.Draft || rec.Title != "Add thing" {
				t.Fatalf("record after a plain update = %+v", rec)
			}

			// Ready, with a new title and description, nothing new to push.
			out, third, err := e.openDraft(e.key('c'), "Add thing, ready", "final body", bp(false))
			if err != nil || out.Result.IsError || third.Draft || !third.Replayed || third.Number != 1 {
				t.Fatalf("ready: %+v %v", out, err)
			}
			rec := e.record()
			if rec.Draft || rec.Title != "Add thing, ready" || e.requests() != 1 || e.records() != 1 {
				t.Fatalf("record after ready = %+v, requests %d", rec, e.requests())
			}
			if provider == project.ProviderGitLab {
				b := e.gl.bodies["PUT /merge_requests/1"]
				if b["title"] != "Add thing, ready" || b["description"] != "final body" {
					t.Fatalf("PUT = %v", b)
				}
			} else {
				if b := e.gh.bodies["PATCH /pulls/1"]; b["title"] != "Add thing, ready" || b["body"] != "final body" {
					t.Fatalf("PATCH = %v", b)
				}
				if q, _ := e.gh.bodies["POST /graphql"]["query"].(string); !strings.Contains(q, "markPullRequestReadyForReview") {
					t.Fatalf("graphql = %v", e.gh.bodies["POST /graphql"])
				}
			}
		})
	}
}

// L1: a refused ready transition is an error the model reads, carrying the provider's
// message and no token, not an internal error.
func TestOpenMergeRequestReadyRefusedIsAModelError(t *testing.T) {
	for _, provider := range []project.Provider{project.ProviderGitLab, project.ProviderGitHub} {
		t.Run(string(provider), func(t *testing.T) {
			e := newRecordEnv(t, provider)
			e.edit("README.md", "# demo\nv1\n")
			if out, _, err := e.openDraft(e.key('a'), "Add thing", "body", bp(true)); err != nil || out.Result.IsError {
				t.Fatalf("draft create: %+v %v", out, err)
			}
			if provider == project.ProviderGitLab {
				script(&e.gl.scripted, "PUT /merge_requests/1", 403, jm{"message": "403 Forbidden: not allowed to mark ready"})
			} else {
				script(&e.gh.scripted, "PATCH /pulls/1", 200, jm{})
				script(&e.gh.scripted, "GET /pulls/1", 200, jm{"node_id": "PR_1", "draft": true})
				script(&e.gh.scripted, "POST /graphql", 200, jm{"errors": []jm{{"message": "Resource not accessible"}}})
			}
			out, _, err := e.openDraft(e.key('c'), "Add thing", "body", bp(false))
			if err != nil {
				t.Fatalf("an internal error: %v", err)
			}
			text := out.Result.Text
			if !out.Result.IsError || !strings.Contains(text, "ready") || strings.Contains(text, e.pc.Token) ||
				!(strings.Contains(text, "not allowed") || strings.Contains(text, "Resource not accessible")) {
				t.Fatalf("result = %+v", out.Result)
			}
		})
	}
}
