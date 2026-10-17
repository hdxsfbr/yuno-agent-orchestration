// Package projecttest builds local bare repositories that stand in for the provider in
// tests (DESIGN 5.16 tests): no network, the real git.
package projecttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// NeedGit skips the test when git is not installed.
func NeedGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// Git runs git with a fixed identity and returns its trimmed output.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@localhost", "-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// BareRepo creates a bare repository whose main branch holds files, and returns its
// path. The path works as a clone URL for git.
func BareRepo(t testing.TB, files map[string]string) string {
	t.Helper()
	NeedGit(t)
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	Git(t, base, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(base, "seed")
	Git(t, base, "clone", "--quiet", bare, work)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(files[name]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, work, "add", "--all")
	Git(t, work, "commit", "--quiet", "-m", "seed")
	Git(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	return bare
}

// Branches lists the branches of a bare repository.
func Branches(t testing.TB, bare string) []string {
	t.Helper()
	out := Git(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// FileAt returns the content of path on branch, or "" when absent.
func FileAt(t testing.TB, bare, branch, path string) string {
	t.Helper()
	cmd := exec.Command("git", "show", branch+":"+path)
	cmd.Dir = bare
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Subject is the subject line of the newest commit on branch.
func Subject(t testing.TB, bare, branch string) string {
	t.Helper()
	return Git(t, bare, "log", "-1", "--format=%s", branch)
}

// Author is "name <email>" of the newest commit on branch.
func Author(t testing.TB, bare, branch string) string {
	t.Helper()
	return Git(t, bare, "log", "-1", "--format=%an <%ae>", branch)
}
