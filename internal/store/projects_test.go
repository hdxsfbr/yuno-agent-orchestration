package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// T5.17.1 (store): create, read by id, name (ignoring case) and URL, the unique name,
// update without touching the token, replace the token, soft delete, the last run.
func TestProjectsStore(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	blob := []byte("sealed-blob-1")
	p := &Project{Name: "Playground", RepoURL: "https://gitlab.example/g/play.git", Provider: "gitlab", DefaultBranch: "main", Description: "demo"}
	mustDo(t, CreateProject(ctx, pool, p, blob))
	if p.ID == "" || !p.TokenSet {
		t.Fatalf("created = %+v", p)
	}
	got, err := GetProject(ctx, pool, p.ID)
	if err != nil || got.Name != "Playground" || !got.TokenSet || got.Description != "demo" || got.DeletedAt != nil {
		t.Fatalf("get = %+v %v", got, err)
	}
	if b, _ := json.Marshal(got); bytes.Contains(b, blob) || bytes.Contains(b, []byte("token_enc")) {
		t.Fatalf("the project's JSON carries the blob: %s", b)
	}
	if byName, err := GetProjectByName(ctx, pool, "  playGROUND "); err != nil || byName.ID != p.ID {
		t.Fatalf("by name = %+v %v", byName, err)
	}
	if byURL, err := GetProjectByRepoURL(ctx, pool, p.RepoURL); err != nil || byURL.ID != p.ID {
		t.Fatalf("by url = %+v %v", byURL, err)
	}
	var pgErr *pgconn.PgError
	if err := CreateProject(ctx, pool, &Project{Name: "PLAYGROUND", RepoURL: "https://x/y.git", Provider: "gitlab", DefaultBranch: "main"}, nil); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate name: %v", err)
	}
	// Update keeps the token; SetProjectToken replaces it.
	p.Name, p.SandboxNetwork = "Playground 2", true
	mustDo(t, UpdateProject(ctx, pool, p))
	if enc, err := GetProjectTokenEnc(ctx, pool, p.ID); err != nil || !bytes.Equal(enc, blob) {
		t.Fatalf("token after update = %q %v", enc, err)
	}
	mustDo(t, SetProjectToken(ctx, pool, p.ID, []byte("sealed-blob-2")))
	if enc, _ := GetProjectTokenEnc(ctx, pool, p.ID); string(enc) != "sealed-blob-2" {
		t.Fatalf("token after replace = %q", enc)
	}
	other := &Project{Name: "Alpha", RepoURL: "https://github.com/o/alpha", Provider: "github", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, other, nil))
	if list, err := ListProjects(ctx, pool); err != nil || len(list) != 2 || list[0].Name != "Alpha" || list[1].Name != "Playground 2" || list[0].TokenSet {
		t.Fatalf("list = %+v %v", list, err)
	}
	// The last run per project.
	wf := &Workflow{Name: "wf", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	settings := p.Settings()
	first := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual, ProjectID: &p.ID, Project: &settings}
	mustDo(t, CreateRun(ctx, pool, first))
	second := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual, ProjectID: &p.ID, Project: &settings}
	mustDo(t, CreateRun(ctx, pool, second))
	mustDo(t, SetRunToolchain(ctx, pool, second.ID, "go"))
	read, err := GetRun(ctx, pool, second.ID)
	if err != nil || read.ProjectID == nil || *read.ProjectID != p.ID || read.Project == nil || read.Project.Name != "Playground 2" || !read.Project.SandboxNetwork {
		t.Fatalf("run = %+v %+v %v", read, read.Project, err)
	}
	list, _ := ListProjects(ctx, pool)
	mustDo(t, FillProjectLastRuns(ctx, pool, list))
	if list[0].LastRun != nil || list[1].LastRun == nil || list[1].LastRun.ID != second.ID || list[1].LastRun.Toolchain == nil || *list[1].LastRun.Toolchain != "go" {
		t.Fatalf("last runs = %+v %+v", list[0].LastRun, list[1].LastRun)
	}
	// Soft delete: gone from the reads, the token still opens for the runs in flight.
	mustDo(t, DeleteProject(ctx, pool, p.ID))
	if _, err := GetProject(ctx, pool, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := GetProjectByName(ctx, pool, "playground 2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("by name after delete: %v", err)
	}
	if _, err := GetProjectByRepoURL(ctx, pool, p.RepoURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("by url after delete: %v", err)
	}
	if found, err := FindProjectByRepoURL(ctx, pool, p.RepoURL); err != nil || found.ID != p.ID || found.DeletedAt == nil {
		t.Fatalf("find by url after delete = %+v %v", found, err)
	}
	if list, _ := ListProjects(ctx, pool); len(list) != 1 {
		t.Fatalf("list after delete = %+v", list)
	}
	if enc, err := GetProjectTokenEnc(ctx, pool, p.ID); err != nil || string(enc) != "sealed-blob-2" {
		t.Fatalf("token after delete = %q %v", enc, err)
	}
	if err := DeleteProject(ctx, pool, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := GetProjectTokenEnc(ctx, pool, "no-such-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token of an unknown project: %v", err)
	}
	// The name is free again for a new project.
	mustDo(t, CreateProject(ctx, pool, &Project{Name: "playground 2", RepoURL: "https://x/z.git", Provider: "gitlab", DefaultBranch: "main"}, nil))
}

// MergeRequestOfRun: the newest executed open_merge_request call with a URL wins; a
// failed call or a run without one gives nil.
func TestMergeRequestOfRun(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "wf", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	run := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual}
	mustDo(t, CreateRun(ctx, pool, run))
	if mr, err := MergeRequestOfRun(ctx, pool, run.ID); err != nil || mr != nil {
		t.Fatalf("none = %+v %v", mr, err)
	}
	t1 := mustParse(t, "2026-10-17T10:00:00Z")
	t2 := mustParse(t, "2026-10-17T10:05:00Z")
	mustDo(t, InsertToolCall(ctx, pool, &ToolCall{RunID: run.ID, ToolUseID: "a", ToolName: "open_merge_request", Status: ToolFailed, IsError: true,
		Output: json.RawMessage(`{"text":"refused"}`), StartedAt: &t2, FinishedAt: &t2}))
	if mr, _ := MergeRequestOfRun(ctx, pool, run.ID); mr != nil {
		t.Fatalf("a failed call counted: %+v", mr)
	}
	mustDo(t, InsertToolCall(ctx, pool, &ToolCall{RunID: run.ID, ToolUseID: "b", ToolName: "open_merge_request", Status: ToolExecuted,
		Output: json.RawMessage(`{"url":"https://gitlab.test/-/merge_requests/1","provider":"gitlab"}`), StartedAt: &t1, FinishedAt: &t1}))
	mustDo(t, InsertToolCall(ctx, pool, &ToolCall{RunID: run.ID, ToolUseID: "c", ToolName: "yuno__open_merge_request", Status: ToolExecuted,
		Output: json.RawMessage(`{"url":"https://github.test/pull/2","provider":"github","replayed":true}`), StartedAt: &t2, FinishedAt: &t2}))
	mr, err := MergeRequestOfRun(ctx, pool, run.ID)
	if err != nil || mr == nil || mr.URL != "https://github.test/pull/2" || mr.Provider != "github" {
		t.Fatalf("newest = %+v %v", mr, err)
	}
}
