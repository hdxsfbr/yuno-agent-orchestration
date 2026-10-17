package project

import (
	"context"
	"errors"
	"strings"
	"testing"

	"yuno/internal/config"
	"yuno/internal/store"
)

// T5.17.5: the PROJECT_* settings become one project, once, named from the path; a
// taken name falls back to the whole path; no key or an incomplete setting creates
// nothing.
func TestBootstrap(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	c, err := NewCipher("bootstrap-test-key")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ProjectRepoURL: "https://labs.gauntletai.com/andrebatista/yuno-playground.git",
		ProjectGitToken: "glpat-not-a-real-token-00", ProjectDefaultBranch: "main", ProjectSandboxNetwork: true}

	p, created, err := Bootstrap(ctx, pool, c, cfg)
	if err != nil || !created || p == nil {
		t.Fatalf("first = %+v %v %v", p, created, err)
	}
	if p.Name != "yuno-playground" || p.Provider != "gitlab" || p.DefaultBranch != "main" || !p.SandboxNetwork || !p.TokenSet ||
		p.RepoURL != cfg.ProjectRepoURL || p.Description != "" {
		t.Fatalf("project = %+v", p)
	}
	r := &Resolver{DB: pool, Cipher: c}
	if tok, err := r.Token(ctx, p.ID); err != nil || tok != cfg.ProjectGitToken {
		t.Fatalf("token = %q %v", tok, err)
	}
	// Idempotent: the same setting finds the project; an edited row is not overwritten.
	p.Name = "Playground"
	if err := store.UpdateProject(ctx, pool, p); err != nil {
		t.Fatal(err)
	}
	cfg.ProjectDefaultBranch = "develop"
	again, created, err := Bootstrap(ctx, pool, c, cfg)
	if err != nil || created || again.ID != p.ID || again.Name != "Playground" || again.DefaultBranch != "main" {
		t.Fatalf("second = %+v %v %v", again, created, err)
	}
	if list, _ := store.ListProjects(ctx, pool); len(list) != 1 {
		t.Fatalf("projects = %d", len(list))
	}
	// Another repository whose last segment is taken (ignoring case, by the renamed
	// project) gets the whole path as its name.
	other := cfg
	other.ProjectRepoURL = "https://github.com/someone/playground"
	gh, created, err := Bootstrap(ctx, pool, c, other)
	if err != nil || !created || gh.Name != "someone/playground" || gh.Provider != "github" {
		t.Fatalf("taken name = %+v %v %v", gh, created, err)
	}
	// Nothing to do, no key, a wrong setting.
	if p, created, err := Bootstrap(ctx, pool, c, config.Config{}); p != nil || created || err != nil {
		t.Fatalf("unset = %+v %v %v", p, created, err)
	}
	if _, _, err := Bootstrap(ctx, pool, nil, other); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key = %v", err)
	}
	bad := cfg
	bad.ProjectGitToken = ""
	if _, _, err := Bootstrap(ctx, pool, c, bad); err == nil || !strings.Contains(err.Error(), "PROJECT_GIT_TOKEN is required") {
		t.Fatalf("incomplete = %v", err)
	}
	if list, _ := store.ListProjects(ctx, pool); len(list) != 2 {
		t.Fatalf("projects = %d", len(list))
	}
	// A project the operator deleted is not brought back by the next start.
	if err := store.DeleteProject(ctx, pool, gh.ID); err != nil {
		t.Fatal(err)
	}
	if p, created, err := Bootstrap(ctx, pool, c, other); p != nil || created || err != nil {
		t.Fatalf("after a delete = %+v %v %v", p, created, err)
	}
	if list, _ := store.ListProjects(ctx, pool); len(list) != 1 {
		t.Fatalf("projects after the delete = %d", len(list))
	}
}

// The resolver's reasons: no row, no token, a blob that does not open.
func TestResolverToken(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	c, _ := NewCipher("resolver-test-key")
	r := &Resolver{DB: pool, Cipher: c}
	if _, err := r.Token(ctx, "missing"); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("missing = %v", err)
	}
	p := &store.Project{Name: "NoToken", RepoURL: "https://x/y.git", Provider: "gitlab", DefaultBranch: "main"}
	if err := store.CreateProject(ctx, pool, p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Token(ctx, p.ID); err == nil || !strings.Contains(err.Error(), "no token is stored") {
		t.Fatalf("no token = %v", err)
	}
	if err := store.SetProjectToken(ctx, pool, p.ID, []byte("garbage-garbage-garbage-garbage")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Token(ctx, p.ID); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("garbage = %v", err)
	}
	settings := p.Settings()
	run := &store.Run{ProjectID: &p.ID, Project: &settings}
	if _, err := r.ForRun(ctx, run); !errors.Is(err, ErrCannotRead) {
		t.Fatalf("for run = %v", err)
	}
	if _, err := r.ForRun(ctx, &store.Run{}); !errors.Is(err, ErrNoProject) {
		t.Fatalf("run without a project = %v", err)
	}
	blob, _ := c.Seal(p.ID, "tok")
	if err := store.SetProjectToken(ctx, pool, p.ID, blob); err != nil {
		t.Fatal(err)
	}
	pc, err := r.ForRun(ctx, run)
	if err != nil || pc.Token != "tok" || pc.RepoURL != "https://x/y.git" || pc.Provider != ProviderGitLab {
		t.Fatalf("for run = %+v %v", pc, err)
	}
}
