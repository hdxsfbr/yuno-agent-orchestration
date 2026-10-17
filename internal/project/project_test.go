package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// T5.16.1: the PROJECT_* settings parse as rule 1 says.
func TestParse(t *testing.T) {
	cases := []struct {
		name                               string
		url, token, branch, provider       string
		wantProvider, wantBranch, wantPath string
		wantAPI                            string
		wantErr                            string
	}{
		{name: "gitlab by host", url: "https://gitlab.com/group/sub/proj.git", token: "t",
			wantProvider: "gitlab", wantBranch: "main", wantPath: "group/sub/proj", wantAPI: "https://gitlab.com/api/v4/projects/group%2Fsub%2Fproj"},
		{name: "github by host", url: "https://github.com/owner/repo", token: "t", branch: "develop",
			wantProvider: "github", wantBranch: "develop", wantPath: "owner/repo", wantAPI: "https://api.github.com/repos/owner/repo"},
		{name: "self-hosted gitlab", url: "https://git.example.com/a/b.git", token: "t",
			wantProvider: "gitlab", wantBranch: "main", wantPath: "a/b", wantAPI: "https://git.example.com/api/v4/projects/a%2Fb"},
		{name: "github enterprise, explicit provider", url: "https://ghe.example.com/o/r.git", token: "t", provider: "GitHub",
			wantProvider: "github", wantBranch: "main", wantPath: "o/r", wantAPI: "https://ghe.example.com/api/v3/repos/o/r"},
		{name: "not configured", wantErr: "no project repository configured"},
		{name: "http refused", url: "http://gitlab.com/g/p.git", token: "t", wantErr: "must be an https URL"},
		{name: "credentials in the url refused", url: "https://oauth2:glpat-abc@gitlab.com/g/p.git", token: "t", wantErr: "must not carry credentials"},
		{name: "no path", url: "https://gitlab.com/", token: "t", wantErr: "names no repository path"},
		{name: "token required", url: "https://gitlab.com/g/p.git", wantErr: "PROJECT_GIT_TOKEN is required"},
		{name: "unknown provider", url: "https://gitlab.com/g/p.git", token: "t", provider: "bitbucket", wantErr: "must be gitlab or github"},
		{name: "bad branch", url: "https://gitlab.com/g/p.git", token: "t", branch: "-x", wantErr: "is not a branch name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse(c.url, c.token, c.branch, c.provider, false)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				if cfg.Enabled() {
					t.Fatal("a refused setting must not enable the project")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(cfg.Provider) != c.wantProvider || cfg.DefaultBranch != c.wantBranch || cfg.Path() != c.wantPath || cfg.APIBase() != c.wantAPI {
				t.Fatalf("got %+v path %q api %q", cfg, cfg.Path(), cfg.APIBase())
			}
			if !cfg.Enabled() {
				t.Fatal("not enabled")
			}
		})
	}
	if _, err := Parse("", "", "", "", false); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty: %v", err)
	}
}

// T5.16.2: the detection table of rule 5, one case per row, and precedence.
func TestDetect(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		want    string
		install string
		test    string
	}{
		{"go", []string{"go.mod", "package.json", "pyproject.toml"}, ToolchainGo, "go mod download", "go test ./..."},
		{"node pnpm", []string{"package.json", "pnpm-lock.yaml", "package-lock.json"}, ToolchainNode, "pnpm install --frozen-lockfile", "pnpm test"},
		{"node yarn", []string{"package.json", "yarn.lock", "package-lock.json"}, ToolchainNode, "yarn install --frozen-lockfile", "yarn test"},
		{"node npm", []string{"package.json", "package-lock.json", "uv.lock"}, ToolchainNode, "npm ci", "npm test"},
		{"node without a lockfile", []string{"package.json"}, ToolchainNode, "npm install", "npm test"},
		{"python uv", []string{"uv.lock", "requirements.txt", "pyproject.toml"}, ToolchainPython, "uv sync", "uv run pytest"},
		{"python requirements", []string{"requirements-dev.txt", "requirements.txt", "pyproject.toml"}, ToolchainPython,
			"python -m venv .venv && .venv/bin/pip install -r requirements.txt && .venv/bin/pip install -r requirements-dev.txt", ".venv/bin/pytest"},
		{"python pyproject", []string{"pyproject.toml", "README.md"}, ToolchainPython, "python -m venv .venv && .venv/bin/pip install -e .", ".venv/bin/pytest"},
		{"nothing recognised", []string{"README.md", "Makefile"}, ToolchainDefault, "", ""},
		{"a lockfile alone is not a project", []string{"pnpm-lock.yaml"}, ToolchainDefault, "", ""},
		{"empty", nil, ToolchainDefault, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range c.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// A directory named like a manifest does not count.
			if err := os.Mkdir(filepath.Join(dir, "go.mod.d"), 0o755); err != nil {
				t.Fatal(err)
			}
			got := Detect(dir)
			if got.Name != c.want || got.Install != c.install || got.Test != c.test {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if got := Detect(filepath.Join(t.TempDir(), "missing")); got.Name != ToolchainDefault {
		t.Fatalf("missing dir: %+v", got)
	}
	// go.mod as a directory is not a manifest.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "go.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Detect(dir); got.Name != ToolchainDefault {
		t.Fatalf("directory go.mod: %+v", got)
	}
}

func TestSandboxImage(t *testing.T) {
	cases := []struct{ base, tc, want string }{
		{"yuno-sandbox:latest", "go", "yuno-sandbox-go:latest"},
		{"yuno-sandbox:latest", "node", "yuno-sandbox-node:latest"},
		{"yuno-sandbox:latest", "python", "yuno-sandbox-python:latest"},
		{"yuno-sandbox:latest", "default", "yuno-sandbox:latest"},
		{"yuno-sandbox:latest", "", "yuno-sandbox:latest"},
		{"yuno-sandbox", "go", "yuno-sandbox-go"},
		{"registry.example:5000/yuno/sandbox:v3", "python", "registry.example:5000/yuno/sandbox-python:v3"},
		{"registry.example:5000/yuno/sandbox", "node", "registry.example:5000/yuno/sandbox-node"},
	}
	for _, c := range cases {
		if got := SandboxImage(c.base, c.tc); got != c.want {
			t.Errorf("SandboxImage(%q, %q) = %q, want %q", c.base, c.tc, got, c.want)
		}
	}
}

func TestInstructions(t *testing.T) {
	cfg := Config{RepoURL: "https://gitlab.com/g/p.git", DefaultBranch: "main", Provider: ProviderGitLab}
	tc := Toolchain{Name: "go", Install: "go mod download", Test: "go test ./..."}
	off := Instructions(cfg, "run-1", tc)
	for _, want := range []string{"runs/run-1/repo/", "https://gitlab.com/g/p.git", "branch main", "toolchain go",
		"Do not clone, commit or push", "Sandbox network: off"} {
		if !strings.Contains(off, want) {
			t.Errorf("network off: missing %q in %q", want, off)
		}
	}
	if strings.Contains(off, "go mod download") {
		t.Error("network off must not say to install")
	}
	cfg.SandboxNetwork = true
	on := Instructions(cfg, "run-1", tc)
	for _, want := range []string{"Sandbox network: on", "(go mod download)", "(go test ./...)"} {
		if !strings.Contains(on, want) {
			t.Errorf("network on: missing %q in %q", want, on)
		}
	}
	none := Instructions(cfg, "run-1", Toolchain{Name: ToolchainDefault})
	if !strings.Contains(none, "No manifest was recognised") {
		t.Errorf("default toolchain with network: %q", none)
	}
}

// Rule 4: the token travels through git's environment only, never into the
// checkout's configuration.
func TestAuthEnvKeepsTheTokenOutOfFiles(t *testing.T) {
	cfg := Config{RepoURL: "https://gitlab.com/g/p.git", Token: "glpat-secret-token-value-1", Provider: ProviderGitLab}
	env := strings.Join(cfg.authEnv(), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.extraheader") || !strings.Contains(env, "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Fatalf("env = %q", env)
	}
	if strings.Contains(env, cfg.Token) {
		t.Fatal("the raw token is in the environment value; it must be base64 of user:token")
	}
	gh := Config{Provider: ProviderGitHub, Token: "x"}
	if gh.authUser() != "x-access-token" || cfg.authUser() != "oauth2" {
		t.Fatalf("auth users: %s %s", gh.authUser(), cfg.authUser())
	}
	if (Config{Token: ""}).authEnv()[0] != "GIT_TERMINAL_PROMPT=0" {
		t.Fatal("prompts must be off")
	}
}

// T5.16.3 (git part): clone, edit, propose against a local bare repository.
func TestCloneAndPropose(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n", "keep/old.txt": "old\n"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}
	ctx := context.Background()
	ws := t.TempDir()
	checkout := filepath.Join(ws, "repo")
	if err := cfg.Clone(ctx, checkout); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(checkout, "README.md")); string(got) != "# demo\n" {
		t.Fatalf("README = %q", got)
	}
	gitConfig, _ := os.ReadFile(filepath.Join(checkout, ".git", "config"))
	if strings.Contains(string(gitConfig), cfg.Token) || strings.Contains(string(gitConfig), "extraheader") {
		t.Fatalf(".git/config carries credentials:\n%s", gitConfig)
	}
	exclude, _ := os.ReadFile(filepath.Join(checkout, ".git", "info", "exclude"))
	if !strings.Contains(string(exclude), "node_modules/") || !strings.Contains(string(exclude), ".venv/") {
		t.Fatalf("exclude = %q", exclude)
	}
	if Detect(checkout).Name != ToolchainGo {
		t.Fatal("toolchain not detected from the checkout")
	}

	// Nothing changed yet: no proposal.
	if _, err := cfg.Propose(ctx, checkout, "yuno/abc", "nothing"); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("no changes: %v", err)
	}
	if slices.Contains(projecttest.Branches(t, bare), "yuno/abc") {
		t.Fatal("a branch was pushed for no changes")
	}

	// The agent edits, adds, deletes, drops a dependency directory and a link.
	must(t, os.WriteFile(filepath.Join(checkout, "README.md"), []byte("# demo\nchanged\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(checkout, "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(checkout, "src", "main.go"), []byte("package main\n"), 0o644))
	must(t, os.RemoveAll(filepath.Join(checkout, "keep")))
	must(t, os.MkdirAll(filepath.Join(checkout, "node_modules", "left-pad"), 0o755))
	must(t, os.WriteFile(filepath.Join(checkout, "node_modules", "left-pad", "index.js"), []byte("x"), 0o644))
	must(t, os.Symlink("/etc/hostname", filepath.Join(checkout, "link")))
	// A hook the agent planted must not run on the api side.
	must(t, os.WriteFile(filepath.Join(checkout, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho HOOK > "+filepath.Join(ws, "hook-ran")+"\n"), 0o755))

	prop, err := cfg.Propose(ctx, checkout, "yuno/abc", "Change the readme")
	if err != nil {
		t.Fatal(err)
	}
	if prop.Branch != "yuno/abc" || prop.Commit == "" || prop.Files < 3 {
		t.Fatalf("proposal = %+v", prop)
	}
	if !slices.Contains(projecttest.Branches(t, bare), "yuno/abc") {
		t.Fatalf("branches = %v", projecttest.Branches(t, bare))
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "README.md"); got != "# demo\nchanged\n" {
		t.Fatalf("README on the branch = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "src/main.go"); got != "package main\n" {
		t.Fatalf("main.go on the branch = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "keep/old.txt"); got != "" {
		t.Fatal("a deleted file survived on the branch")
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "node_modules/left-pad/index.js"); got != "" {
		t.Fatal("node_modules was committed")
	}
	if got := projecttest.FileAt(t, bare, "yuno/abc", "link"); got != "" {
		t.Fatal("a link was committed")
	}
	if got := projecttest.Subject(t, bare, "yuno/abc"); got != "Change the readme" {
		t.Fatalf("subject = %q", got)
	}
	if got := projecttest.Author(t, bare, "yuno/abc"); got != "Yuno <yuno@localhost>" {
		t.Fatalf("author = %q", got)
	}
	if got := projecttest.FileAt(t, bare, "main", "README.md"); got != "# demo\n" {
		t.Fatal("main moved")
	}
	if _, err := os.Stat(filepath.Join(ws, "hook-ran")); err == nil {
		t.Fatal("the checkout's pre-commit hook ran on the api side")
	}
}

// A clone of a branch the remote lacks fails with git's message, redacted.
func TestCloneMissingBranch(t *testing.T) {
	bare := projecttest.BareRepo(t, map[string]string{"README.md": "x"})
	cfg := Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "release", Provider: ProviderGitLab}
	err := cfg.Clone(context.Background(), filepath.Join(t.TempDir(), "repo"))
	if err == nil || !strings.Contains(err.Error(), "git clone") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), cfg.Token) {
		t.Fatal("the token is in the error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
