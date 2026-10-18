package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/project/projecttest"
)

// diffFixture is a bare repository standing in for the provider, a config naming it with
// a token, and a checkout of its default branch the way the engine clones one.
type diffFixture struct {
	bare     string
	cfg      Config
	checkout string
	base     string // the commit the checkout was cloned from
}

func newDiffFixture(t *testing.T, files map[string]string) *diffFixture {
	t.Helper()
	bare := projecttest.BareRepo(t, files)
	f := &diffFixture{bare: bare, checkout: filepath.Join(t.TempDir(), "repo"),
		cfg: Config{RepoURL: bare, Token: "glpat-not-a-real-token-0", DefaultBranch: "main", Provider: ProviderGitLab}}
	must(t, f.cfg.Clone(context.Background(), f.checkout))
	var err error
	if f.base, err = f.cfg.HeadSHA(context.Background(), f.checkout); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *diffFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.checkout, rel)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(content), 0o644))
}

// DESIGN 5.25 rule 3 (Diff): the patch is what the agents changed in the checkout against
// the commit it was cloned from: an edit is a hunk, a new file a new-file entry, a
// deleted file a deletion; dependency directories, links and the checkout's own .git
// are left out; no git ran inside the checkout (a hook and a configuration entry planted
// there do nothing); the private clone is gone afterwards; the token is nowhere in it.
func TestDiffYieldsTheCheckoutsChanges(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n", "keep/old.txt": "old\n", "src/main.py": "print('hi')\n"})
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx := context.Background()

	f.write(t, "README.md", "# demo\nsecond line\n")
	f.write(t, "extra.txt", "extra\n")
	must(t, os.RemoveAll(filepath.Join(f.checkout, "keep")))
	f.write(t, "node_modules/x/i.js", "x\n")
	f.write(t, "pkg/.venv/lib.py", "x\n")
	must(t, os.Symlink("/etc/passwd", filepath.Join(f.checkout, "link")))
	// What an agent plants in the checkout's .git must not run on the api side.
	marker := filepath.Join(t.TempDir(), "ran")
	must(t, os.WriteFile(filepath.Join(f.checkout, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho x > "+marker+"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(f.checkout, ".git", "config"),
		[]byte("[core]\n\tfsmonitor = echo x > "+marker+" ; true\n\thooksPath = "+filepath.Join(f.checkout, ".git", "hooks")+"\n"), 0o644))

	patch, err := f.cfg.Diff(ctx, f.checkout, "main", f.base)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patch)
	for _, want := range []string{
		"diff --git a/README.md b/README.md", "+second line",
		"diff --git a/extra.txt b/extra.txt", "new file mode", "+extra",
		"diff --git a/keep/old.txt b/keep/old.txt", "deleted file mode", "-old",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the patch lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"node_modules", ".venv", "link", "passwd", ".git/", "main.py", f.cfg.Token} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the patch holds %q:\n%s", unwanted, text)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("something planted in the checkout's .git ran")
	}
	if left, _ := os.ReadDir(scratch); len(left) != 0 {
		t.Fatalf("the private clone was not removed: %v", left)
	}
	// The checkout itself was only read.
	if got := projecttest.Git(t, f.checkout, "rev-parse", "HEAD"); got != f.base {
		t.Fatalf("the checkout's HEAD moved: %s", got)
	}
}

// DESIGN 5.25 rule 3: a checkout that equals the commit it was cloned from (or differs
// only in excluded directories) has an empty patch, and with base "" the branch's head is
// the base.
func TestDiffIsEmptyWithoutChanges(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n", "a/b.txt": "b\n"})
	ctx := context.Background()
	for _, base := range []string{f.base, ""} {
		if patch, err := f.cfg.Diff(ctx, f.checkout, "main", base); err != nil || len(patch) != 0 {
			t.Fatalf("clean checkout, base %q: %q, %v", base, patch, err)
		}
	}
	f.write(t, "node_modules/dep/index.js", "module.exports = 1\n")
	f.write(t, "a/__pycache__/b.pyc", "\x00\x01")
	f.write(t, ".cache/x", "x")
	if patch, err := f.cfg.Diff(ctx, f.checkout, "main", f.base); err != nil || len(patch) != 0 {
		t.Fatalf("only excluded directories changed: %q, %v", patch, err)
	}
	// An empty branch name is the default branch.
	if patch, err := f.cfg.Diff(ctx, f.checkout, "", f.base); err != nil || len(patch) != 0 {
		t.Fatalf("empty branch: %q, %v", patch, err)
	}
}

// DESIGN 5.25 rule 3: a branch that moved since the clone is diffed against the commit
// the checkout was cloned from (fetched at depth 1 and checked out detached), so the
// patch shows this run's changes only; without the base it would show the person's commit
// as a deletion.
func TestDiffAgainstTheCommitTheRunCloned(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n"})
	ctx := context.Background()
	f.write(t, "README.md", "# demo\nmine\n")

	handed := pushByHand(t, f.bare, "main", "hand.txt", "theirs\n")
	if handed == f.base {
		t.Fatal("the branch did not move")
	}
	patch, err := f.cfg.Diff(ctx, f.checkout, "main", f.base)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(patch); !strings.Contains(text, "+mine") || strings.Contains(text, "hand.txt") {
		t.Fatalf("against the clone's commit:\n%s", text)
	}
	// Against the branch as it is now, the person's file would be this run's deletion.
	patch, err = f.cfg.Diff(ctx, f.checkout, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if text := string(patch); !strings.Contains(text, "deleted file mode") || !strings.Contains(text, "hand.txt") {
		t.Fatalf("without a base:\n%s", text)
	}
	// And the base being the branch head needs no fetch at all.
	f.base = handed
	if patch, err = f.cfg.Diff(ctx, f.checkout, "main", handed); err != nil || !strings.Contains(string(patch), "hand.txt") {
		t.Fatalf("base = head: %s, %v", patch, err)
	}
}

// DESIGN 5.25 rule 3 (a 5.22 run): the branch is the request's source branch, and the
// patch shows what the run changed on top of it, not what the branch holds.
func TestDiffOnTheRequestsBranch(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n"})
	ctx := context.Background()
	f.write(t, "README.md", "# demo\nopened\n")
	_, err := f.cfg.Propose(ctx, f.checkout, "yuno/abc", "Open the request")
	must(t, err)

	onBranch := filepath.Join(t.TempDir(), "run")
	must(t, f.cfg.CloneBranch(ctx, onBranch, "yuno/abc"))
	base, err := f.cfg.HeadSHA(ctx, onBranch)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(onBranch, "README.md"), []byte("# demo\nopened\nfixed\n"), 0o644))

	patch, err := f.cfg.Diff(ctx, onBranch, "yuno/abc", base)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patch)
	if !strings.Contains(text, "+fixed") || strings.Contains(text, "+opened") {
		t.Fatalf("the patch should show only this run's line:\n%s", text)
	}
	// A branch the remote lacks is the clone's error, redacted.
	if _, err := f.cfg.Diff(ctx, onBranch, "yuno/missing", base); err == nil || !strings.Contains(err.Error(), "git clone") || strings.Contains(err.Error(), f.cfg.Token) {
		t.Fatalf("missing branch: %v", err)
	}
}

// DESIGN 5.25 rule 3: a base the remote will not serve is an error carrying git's reason,
// without the token, and the caller skips the diff; a base that is not a commit id is
// refused before git runs.
func TestDiffRefusesBasesItCannotUse(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n"})
	ctx := context.Background()
	f.write(t, "README.md", "# demo\nmine\n")

	patch, err := f.cfg.Diff(ctx, f.checkout, "main", "0123456789abcdef0123456789abcdef01234567")
	if err == nil || patch != nil || !strings.Contains(err.Error(), "git fetch") || strings.Contains(err.Error(), f.cfg.Token) {
		t.Fatalf("unfetchable sha: %q, %v", patch, err)
	}
	for _, base := range []string{"--upload-pack=touch /tmp/x", "main", "0123", strings.Repeat("g", 40)} {
		if _, err := f.cfg.Diff(ctx, f.checkout, "main", base); err == nil || !strings.Contains(err.Error(), "not a full commit id") {
			t.Errorf("base %q: %v", base, err)
		}
	}
}

// DESIGN 5.25 rule 3: a directory without a .git directory is no checkout, whether it is
// missing, empty, has a .git that is a file or a link to a directory.
func TestDiffRefusesWhatIsNoCheckout(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n"})
	ctx := context.Background()
	plain := t.TempDir()
	withFile := t.TempDir()
	must(t, os.WriteFile(filepath.Join(withFile, ".git"), []byte("gitdir: /elsewhere\n"), 0o644))
	withLink := t.TempDir()
	must(t, os.Symlink(filepath.Join(f.checkout, ".git"), filepath.Join(withLink, ".git")))
	for name, dir := range map[string]string{"empty": plain, "gitfile": withFile, "gitlink": withLink, "missing": filepath.Join(plain, "nope")} {
		if patch, err := f.cfg.Diff(ctx, dir, "main", f.base); !errors.Is(err, ErrNoCheckout) || patch != nil {
			t.Errorf("%s: %q, %v; want ErrNoCheckout", name, patch, err)
		}
	}
}

// DESIGN 5.25 rule 3: binary files are in the patch (--binary), and a patch is read up to
// a limit only: a longer one is cut at the limit and git ends on a broken pipe, so a
// checkout of large files cannot exhaust the api's memory.
func TestDiffBinaryAndOutputLimit(t *testing.T) {
	f := newDiffFixture(t, map[string]string{"README.md": "# demo\n"})
	ctx := context.Background()
	f.write(t, "logo.bin", "\x00\x01\x02binary\xff\xfe")
	patch, err := f.cfg.Diff(ctx, f.checkout, "main", f.base)
	if err != nil || !strings.Contains(string(patch), "GIT binary patch") {
		t.Fatalf("binary: %s, %v", patch, err)
	}

	f.write(t, "big.txt", strings.Repeat("a line of text\n", 100000))
	old := diffOutputLimit
	diffOutputLimit = 4096
	t.Cleanup(func() { diffOutputLimit = old })
	patch, err = f.cfg.Diff(ctx, f.checkout, "main", f.base)
	if err != nil || int64(len(patch)) != diffOutputLimit {
		t.Fatalf("a long patch: %d bytes, %v; want it cut at %d", len(patch), err, diffOutputLimit)
	}
	diffOutputLimit = old
	if patch, err = f.cfg.Diff(ctx, f.checkout, "main", f.base); err != nil || len(patch) <= 1<<20 {
		t.Fatalf("uncut: %d bytes, %v", len(patch), err)
	}
}
