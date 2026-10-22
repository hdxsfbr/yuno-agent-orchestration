package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The run repository (DESIGN 5.30 rules 4 and 10, D-083, D-087): a bare repository per
// project run under the state directory, owned by the platform and mounted in no sandbox,
// that carries the checkout from one step to the next. The first step fetches the run's
// branch and the default branch into it from the project, with history; each visit's
// checkout is saved there as a commit when the visit ends; every visit's checkout, the
// first one's included, is built from the commit the carry rules name. Nothing is pushed
// to the project's remote, and the token is used by the first step's fetch alone. As
// everywhere else (5.16 rule 10), git never runs inside a checkout an agent has had: a
// commit is made from a copy of its working tree, and a new checkout is a repository the
// platform creates and fills before the agent's first turn.

// RunReposDir is the folder of the run repositories under the state directory.
const RunReposDir = "yuno-runs"

// RunRepoPath is the bare repository of a run: <state>/yuno-runs/<run id>.git.
func RunRepoPath(stateDir, runID string) string {
	return filepath.Join(stateDir, RunReposDir, runID+".git")
}

// BaseRef is the ref the clone made by a visit records its commit under.
func BaseRef(visitID string) string { return "refs/bases/" + visitID }

// DefaultRef is the ref the clone made by a visit records the project's default branch
// under, when the visit's branch is another one (DESIGN 5.30 rule 10).
func DefaultRef(visitID string) string { return "refs/defaults/" + visitID }

// MergeRef is the ref of the merge commit the platform made in a visit's fresh checkout
// when it merged the default branch into a merge request's branch (DESIGN 5.22 rule 13).
func MergeRef(visitID string) string { return "refs/merges/" + visitID }

// VisitRef is the ref of the commit a visit's checkout ended at.
func VisitRef(visitID string) string { return "refs/visits/" + visitID }

// historyDepth is how many commits of its branch, and of the project's default branch, a
// run's checkout gets (DESIGN 5.30 rule 10, D-087), and the step by which the fetch deepens
// while it looks for the two branches' merge base; maxHistoryDepth is how deep it looks.
// Variables so a test can lower them.
var (
	historyDepth    = 100
	maxHistoryDepth = 2000
)

// runRepoLocks serialize the fetches into a run repository: two visits of a run that clone
// at once (the heads of a fork) would race for its shallow file. A run repository takes the
// lock its path hashes to.
var runRepoLocks [64]sync.Mutex

func lockRunRepo(dir string) func() {
	h := fnv.New32a()
	h.Write([]byte(dir))
	mu := &runRepoLocks[h.Sum32()%uint32(len(runRepoLocks))]
	mu.Lock()
	return mu.Unlock
}

// Fetched is what FetchRun recorded in the run repository.
type Fetched struct {
	// Base is the branch's commit, where the checkout starts (BaseRef).
	Base string
	// Default is the default branch's commit (DefaultRef); "" when the branch is the
	// default one.
	Default string
	// MergeBase is the two branches' merge base; "" without Default, or when none is within
	// maxHistoryDepth commits.
	MergeBase string
}

// FetchRun fetches branch, and the project's default branch when it is another one, from
// the project into the run repository (DESIGN 5.30 rule 10, D-087), historyDepth commits
// deep, recording their commits as BaseRef(visitID) and DefaultRef(visitID). For two
// branches it deepens the fetch, historyDepth commits at a time and to at most
// maxHistoryDepth, until their merge base is in it. The token goes to these fetches alone,
// through the environment (5.16 rule 4): the run repository is the platform's, mounted in no
// sandbox, and the checkouts built from it get the history and no credential.
func (c Config) FetchRun(ctx context.Context, runRepo, visitID, branch string) (Fetched, error) {
	defer lockRunRepo(runRepo)()
	run := func(args ...string) (string, error) {
		out, err := c.git(ctx, runRepo, append([]string{"--git-dir=" + runRepo}, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	specs := []string{"+refs/heads/" + branch + ":" + BaseRef(visitID)}
	two := c.DefaultBranch != "" && c.DefaultBranch != branch
	if two {
		specs = append(specs, "+refs/heads/"+c.DefaultBranch+":"+DefaultRef(visitID))
	}
	if _, err := run(append([]string{"fetch", "--quiet", "--no-tags", "--depth", strconv.Itoa(historyDepth), "--", c.RepoURL}, specs...)...); err != nil {
		return Fetched{}, err
	}
	var f Fetched
	var err error
	if f.Base, err = refCommit(ctx, runRepo, BaseRef(visitID)); err == nil && f.Base == "" {
		err = errors.New("the fetch recorded no commit for " + branch)
	}
	if err != nil || !two {
		return f, err
	}
	if f.Default, err = refCommit(ctx, runRepo, DefaultRef(visitID)); err == nil && f.Default == "" {
		err = errors.New("the fetch recorded no commit for " + c.DefaultBranch)
	}
	if err != nil {
		return Fetched{}, err
	}
	for depth := historyDepth; ; depth += historyDepth {
		if mb, err := run("merge-base", f.Base, f.Default); err == nil && commitIDPattern.MatchString(mb) {
			f.MergeBase = mb
			return f, nil
		}
		if depth >= maxHistoryDepth {
			return f, nil
		}
		if shallow, _ := run("rev-parse", "--is-shallow-repository"); shallow != "true" {
			return f, nil // the whole history is here: the branches share none
		}
		// By id, so the commits recorded above stay the ones deepened.
		if _, err := run("fetch", "--quiet", "--no-tags", "--deepen", strconv.Itoa(historyDepth), "--", c.RepoURL, f.Base, f.Default); err != nil {
			return f, nil
		}
	}
}

// refCommit is the commit ref names in the run repository, "" when there is no such ref.
func refCommit(ctx context.Context, runRepo, ref string) (string, error) {
	out, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if sha != "" && !commitIDPattern.MatchString(sha) {
		return "", fmt.Errorf("%s: %q is not a full commit id", ref, tail(sha, 80))
	}
	return sha, nil
}

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

// runBase is the commit the run cloned that commit descends from, and the visit that
// cloned it: the BaseRef whose commit is an ancestor of commit (one per clone; a run clones
// once, or once per fork head when its first step is inside a fork).
func runBase(ctx context.Context, runRepo, commit string) (string, string, error) {
	out, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "for-each-ref", "--format=%(objectname) %(refname)", "refs/bases/")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		base, ref, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if isAncestor(ctx, runRepo, base, commit) {
			return base, strings.TrimPrefix(ref, "refs/bases/"), nil
		}
	}
	return "", "", fmt.Errorf("the run repository has no clone that %s descends from", commit)
}

// isAncestor tells whether ancestor is commit or one of its ancestors in the run repository.
func isAncestor(ctx context.Context, runRepo, ancestor, commit string) bool {
	_, err := (Config{}).git(ctx, runRepo, "--git-dir="+runRepo, "merge-base", "--is-ancestor", ancestor, commit)
	return err == nil
}

// CheckoutRun builds a visit's checkout at dest from commit of the run repository (DESIGN
// 5.30 rules 4 and 10). It is a new repository, made like the clone the run started with:
// branch (the one the run cloned) and origin/<branch> at the commit the run cloned, origin
// the project's URL with no credential, the local exclude file written, and a working tree
// that is commit's tree, so the run's changes so far show as changes against the branch,
// as they did in the checkout the earlier step left. When the run's branch is not the
// project's default branch, origin/<default branch> is at the commit the clone fetched with
// it; and when the platform merged that branch into the checkout (MergeDefault), branch is
// at the merge commit instead, so only what the agents changed since shows as changes.
// The history the run repository holds comes with them. Only those commits are fetched,
// never another visit's. dest must not exist; its parent must.
func (c Config) CheckoutRun(ctx context.Context, runRepo, commit, dest, branch string) error {
	if !commitIDPattern.MatchString(commit) {
		return fmt.Errorf("the commit %q is not a full commit id", tail(commit, 80))
	}
	base, visit, err := runBase(ctx, runRepo, commit)
	if err != nil {
		return err
	}
	head, def := base, ""
	if c.DefaultBranch != "" && c.DefaultBranch != branch {
		if def, err = refCommit(ctx, runRepo, DefaultRef(visit)); err != nil {
			return err
		}
		merge, err := refCommit(ctx, runRepo, MergeRef(visit))
		if err != nil {
			return err
		}
		if merge != "" && isAncestor(ctx, runRepo, merge, commit) {
			head = merge
		}
	}
	if _, err := (Config{}).git(ctx, filepath.Dir(dest), "init", "--quiet", dest); err != nil {
		return err
	}
	var want []string
	for _, sha := range []string{commit, base, head, def} {
		if sha != "" && !slices.Contains(want, sha) {
			want = append(want, sha)
		}
	}
	steps := [][]string{
		append([]string{"fetch", "--quiet", "--no-tags", "--update-shallow", runRepo}, want...),
		{"update-ref", "refs/heads/" + branch, head},
		{"update-ref", "refs/remotes/origin/" + branch, base},
	}
	remote := []string{"remote", "add", "-t", branch}
	if def != "" {
		steps = append(steps, []string{"update-ref", "refs/remotes/origin/" + c.DefaultBranch, def})
		remote = append(remote, "-t", c.DefaultBranch)
	}
	steps = append(steps,
		[]string{"symbolic-ref", "HEAD", "refs/heads/" + branch},
		append(remote, "origin", c.RepoURL),
		[]string{"config", "branch." + branch + ".remote", "origin"},
		[]string{"config", "branch." + branch + ".merge", "refs/heads/" + branch},
		[]string{"read-tree", "-u", "--reset", commit},
		[]string{"reset", "--quiet"},
	)
	for _, args := range steps {
		if _, err := (Config{}).git(ctx, dest, args...); err != nil {
			return err
		}
	}
	return WriteExclude(dest)
}

// PreMerge is what MergeDefault did to a checkout (DESIGN 5.22 rule 13, D-087).
type PreMerge struct {
	// Merged is the default branch's commit merged in; "" when nothing was (the branch has
	// it already).
	Merged string
	// Commit is the merge commit when the merge was clean, recorded as MergeRef; "" otherwise.
	Commit string
	// Conflicts are the files the merge left with conflict markers, sorted.
	Conflicts []string
}

// ErrNoMergeBase is MergeDefault's answer when the fetch found no history the branch shares
// with the default branch within maxHistoryDepth commits: nothing was merged.
var ErrNoMergeBase = errors.New("the branch shares no history with the default branch within the fetched commits")

// MergeDefault merges the default branch (origin/<default branch>, f.Default) into the
// branch of the checkout CheckoutRun just built from f.Base for visitID, before any turn ran
// in it (DESIGN 5.22 rule 13, D-087). A clean merge is a merge commit as Yuno, recorded in
// the run repository as MergeRef(visitID), so the run's later checkouts start from it. A
// conflict leaves the merge in progress: the conflicting files hold their markers and are
// returned. Nothing happens when the branch has the default branch's commit already. No
// token is used: everything is local.
func (c Config) MergeDefault(ctx context.Context, checkout, runRepo, visitID string, f Fetched) (PreMerge, error) {
	if f.Default == "" || f.Default == f.Base || f.MergeBase == f.Default {
		return PreMerge{}, nil
	}
	if f.MergeBase == "" {
		return PreMerge{}, ErrNoMergeBase
	}
	local := Config{}
	_, err := local.git(ctx, checkout, "-c", "user.name="+CommitName, "-c", "user.email="+CommitEmail, "-c", "commit.gpgsign=false",
		"merge", "--quiet", "--no-ff", "--no-verify", "--no-edit", "refs/remotes/origin/"+c.DefaultBranch)
	if err == nil {
		merge, err := local.HeadSHA(ctx, checkout)
		if err != nil {
			return PreMerge{}, err
		}
		// The checkout is the platform's own, before any turn: the run repository may read it.
		if _, err := local.git(ctx, runRepo, "--git-dir="+runRepo, "fetch", "--quiet", "--no-tags", "--update-shallow", checkout, "+HEAD:"+MergeRef(visitID)); err != nil {
			return PreMerge{}, err
		}
		return PreMerge{Merged: f.Default, Commit: merge}, nil
	}
	// A conflict leaves the merge in progress; any other failure merged nothing.
	if _, verr := local.git(ctx, checkout, "rev-parse", "--quiet", "--verify", "MERGE_HEAD"); verr != nil {
		return PreMerge{}, err
	}
	out, uerr := local.git(ctx, checkout, "diff", "--name-only", "-z", "--diff-filter=U")
	if uerr != nil {
		return PreMerge{}, uerr
	}
	var conflicts []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && !slices.Contains(conflicts, p) {
			conflicts = append(conflicts, p)
		}
	}
	slices.Sort(conflicts)
	return PreMerge{Merged: f.Default, Conflicts: conflicts}, nil
}

// markerPattern finds a conflict marker line: seven < or > alone or before a space.
const markerPattern = `^(<{7}|>{7})( |$)`

// ConflictMarkers lists the files of commit in the run repository that the run changed since
// its clone and that hold a conflict marker line: a merge of the default branch (DESIGN 5.22
// rule 12) not resolved yet. Sorted; none when there is nothing to resolve.
func ConflictMarkers(ctx context.Context, runRepo, commit string) ([]string, error) {
	base, _, err := runBase(ctx, runRepo, commit)
	if err != nil {
		return nil, err
	}
	changes, err := (Config{}).treeChanges(ctx, runRepo, base, commit)
	if err != nil {
		return nil, err
	}
	return markerFiles(ctx, runRepo, []string{"--git-dir=" + runRepo}, commit, changes)
}

// markerFiles lists the files of treeish in repo, among those changes leave in place, that
// hold a conflict marker line, sorted. global goes before git's command (--git-dir for a
// bare repository).
func markerFiles(ctx context.Context, repo string, global []string, treeish string, changes []treeChange) ([]string, error) {
	changed := map[string]bool{}
	for _, ch := range changes {
		if !ch.gone() {
			changed[ch.path] = true
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	out, err := (Config{}).git(ctx, repo, append(global, "grep", "-l", "-z", "-I", "-E", markerPattern, treeish)...)
	if err != nil {
		// git grep fails with no output and nothing on stderr when nothing matches.
		if len(bytes.TrimSpace(out)) == 0 && strings.TrimSpace(err.Error()) == "git grep:" {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		p := strings.TrimPrefix(entry, treeish+":")
		if changed[p] && !slices.Contains(files, p) {
			files = append(files, p)
		}
	}
	slices.Sort(files)
	return files, nil
}
