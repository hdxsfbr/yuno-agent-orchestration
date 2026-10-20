package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The run repository (DESIGN 5.30 rule 4, D-083): a bare repository per project run under
// the state directory, owned by the platform and mounted in no sandbox, that carries the
// checkout from one step to the next. The first step's clone records its commit there;
// each visit's checkout is saved there as a commit when the visit ends; a later visit's
// checkout is built from the commit the carry rules name. Nothing is pushed to the
// project's remote and no token is used. As everywhere else (5.16 rule 10), git never
// runs inside a checkout an agent has had: a commit is made from a copy of its working
// tree, and a new checkout is a repository the platform creates and fills before the
// agent's first turn.

// RunReposDir is the folder of the run repositories under the state directory.
const RunReposDir = "yuno-runs"

// RunRepoPath is the bare repository of a run: <state>/yuno-runs/<run id>.git.
func RunRepoPath(stateDir, runID string) string {
	return filepath.Join(stateDir, RunReposDir, runID+".git")
}

// BaseRef is the ref the clone made by a visit records its commit under.
func BaseRef(visitID string) string { return "refs/bases/" + visitID }

// VisitRef is the ref of the commit a visit's checkout ended at.
func VisitRef(visitID string) string { return "refs/visits/" + visitID }

// InitRunRepo creates the bare repository dir unless it exists. It is made under a
// temporary name and renamed into place, so two visits of one run that clone at once (the
// heads of a fork) never see a half-made repository. A checkout fetches the commit a visit
// recorded by its id, which uploadpack.allowAnySHA1InWant allows.
func InitRunRepo(ctx context.Context, dir string) error {
	if isRunRepo(dir) {
		return nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := (Config{}).git(ctx, parent, "init", "--quiet", "--bare", tmp); err != nil {
		return err
	}
	if _, err := (Config{}).git(ctx, parent, "--git-dir="+tmp, "config", "uploadpack.allowAnySHA1InWant", "true"); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if isRunRepo(dir) {
			return nil // another visit of the run made it meanwhile
		}
		return err
	}
	return nil
}

func isRunRepo(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, "HEAD"))
	return err == nil && info.Mode().IsRegular()
}

// SaveRunBase records the commit the fresh clone at checkout is at in the run repository
// under ref (BaseRef). The fetch reads the clone's repository, so checkout must be the
// platform's own clone, before any turn ran in it. A clone at depth 1 makes the run
// repository shallow at that commit (--update-shallow).
func SaveRunBase(ctx context.Context, runRepo, checkout, ref string) error {
	_, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "fetch", "--quiet", "--no-tags", "--update-shallow", checkout, "+HEAD:"+ref)
	return err
}

// OpenCheckout opens runs/<run id>/repo of a workspace as an os.Root, refusing a link (or
// anything but a directory) at runs, runs/<run id> or repo, so what is read through it
// is the workspace's own. A part that does not exist is fs.ErrNotExist.
func OpenCheckout(ws, runID string) (*os.Root, error) {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel := ""
	for _, part := range []string{"runs", runID, RepoDir} {
		rel = filepath.Join(rel, part)
		info, err := root.Lstat(rel)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a plain directory", filepath.ToSlash(rel))
		}
	}
	return root.OpenRoot(rel)
}

// CommitCheckout saves the working tree of a visit's checkout in the run repository as one
// commit on top of parent (the commit the checkout started from) with message, points ref
// (VisitRef) at it and returns it. When the tree is parent's, no commit is made: ref names
// parent and parent is returned.
//
// The checkout is copied into a private directory first with the copy rule of 5.16 rule
// 10 (regular files only, links skipped, .git and the Excluded directories left out, at
// most MaxCheckoutBytes), and git works on that copy with the run repository as its git
// directory, so nothing the agent wrote in the checkout's .git is read. Ignored files are
// committed too (--force): the next step gets what this one left, as a copy of the folder
// gave it before; the merge request and the diff stage without --force, as before.
func CommitCheckout(ctx context.Context, runRepo string, checkout *os.Root, parent, ref, message string) (string, error) {
	if !commitIDPattern.MatchString(parent) {
		return "", fmt.Errorf("the parent %q is not a full commit id", tail(parent, 80))
	}
	private, err := os.MkdirTemp("", "yuno-commit-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(private)
	tree := filepath.Join(private, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return "", err
	}
	if _, err := copyTreeRoot(ctx, checkout, tree, MaxCheckoutBytes); err != nil {
		if errors.Is(err, ErrCheckoutTooLarge) {
			return "", err
		}
		return "", fmt.Errorf("copy the checkout: %w", err)
	}
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(private, "index")}
	run := func(args ...string) (string, error) {
		out, err := (Config{}).gitEnv(ctx, tree, env, append([]string{"--git-dir=" + runRepo, "--work-tree=" + tree}, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	if _, err := run("add", "--all", "--force"); err != nil {
		return "", err
	}
	treeID, err := run("write-tree")
	if err != nil {
		return "", err
	}
	parentTree, err := run("rev-parse", "--verify", parent+"^{tree}")
	if err != nil {
		return "", err
	}
	sha := parent
	if treeID != parentTree {
		if sha, err = run("-c", "user.name="+CommitName, "-c", "user.email="+CommitEmail, "-c", "commit.gpgsign=false",
			"commit-tree", treeID, "-p", parent, "-m", message); err != nil {
			return "", err
		}
		if !commitIDPattern.MatchString(sha) {
			return "", fmt.Errorf("git commit-tree: %q is not a full commit id", tail(sha, 80))
		}
	}
	if _, err := run("update-ref", ref, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// runBase is the commit the run cloned that commit descends from: the BaseRef whose commit
// is an ancestor of commit (one per clone; a run clones once, or once per fork head when
// its first step is inside a fork).
func runBase(ctx context.Context, runRepo, commit string) (string, error) {
	out, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "for-each-ref", "--format=%(objectname)", "refs/bases/")
	if err != nil {
		return "", err
	}
	for _, base := range strings.Fields(string(out)) {
		mb, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "merge-base", commit, base)
		if err == nil && strings.TrimSpace(string(mb)) == base {
			return base, nil
		}
	}
	return "", fmt.Errorf("the run repository has no clone that %s descends from", commit)
}

// CheckoutRun builds a visit's checkout at dest from commit of the run repository (DESIGN
// 5.30 rule 4). It is a new repository, made like the clone the run started with: branch
// (the one the run cloned) and origin/<branch> at the commit the run cloned, origin the
// project's URL with no credential, the local exclude file written, and a working tree
// that is commit's tree, so the run's changes so far show as changes against the branch,
// as they did in the checkout the earlier step left. Only commit and its history are
// fetched, never another visit's. dest must not exist; its parent must.
func (c Config) CheckoutRun(ctx context.Context, runRepo, commit, dest, branch string) error {
	if !commitIDPattern.MatchString(commit) {
		return fmt.Errorf("the commit %q is not a full commit id", tail(commit, 80))
	}
	base, err := runBase(ctx, runRepo, commit)
	if err != nil {
		return err
	}
	if _, err := (Config{}).git(ctx, filepath.Dir(dest), "init", "--quiet", dest); err != nil {
		return err
	}
	want := []string{commit}
	if base != commit {
		want = append(want, base)
	}
	for _, args := range [][]string{
		append([]string{"fetch", "--quiet", "--no-tags", "--update-shallow", runRepo}, want...),
		{"update-ref", "refs/heads/" + branch, base},
		{"update-ref", "refs/remotes/origin/" + branch, base},
		{"symbolic-ref", "HEAD", "refs/heads/" + branch},
		{"remote", "add", "-t", branch, "origin", c.RepoURL},
		{"config", "branch." + branch + ".remote", "origin"},
		{"config", "branch." + branch + ".merge", "refs/heads/" + branch},
		{"read-tree", "-u", "--reset", commit},
		{"reset", "--quiet"},
	} {
		if _, err := (Config{}).git(ctx, dest, args...); err != nil {
			return err
		}
	}
	return WriteExclude(dest)
}
