package project

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"yuno/internal/obs"
	"yuno/internal/store"
)

// Git operations the api runs on the platform side (DESIGN 5.16 rules 3, 4 and 10).
// The token reaches git only as an http.extraheader configuration entry through the
// environment, so nothing on disk carries it, and every git error is redacted before
// it is returned.

// GitTimeout bounds one git command when the caller's context has no deadline.
const GitTimeout = 2 * time.Minute

// Excluded are the directories the local exclude file lists (rule 3) and the working
// tree copy skips (rule 10): dependencies and caches that must never be committed.
var Excluded = []string{"node_modules/", ".venv/", "__pycache__/", ".pytest_cache/", ".mypy_cache/", ".ruff_cache/", ".cache/", ".home/"}

// CommitIdentity is who the platform's commits are from.
const (
	CommitName  = "Yuno"
	CommitEmail = "yuno@localhost"
)

// authEnv is the git configuration that authenticates to the provider, passed through
// the environment of one command (GIT_CONFIG_COUNT and friends) rather than written
// anywhere. GIT_TERMINAL_PROMPT=0 keeps a wrong token from hanging on a prompt.
func (c Config) authEnv() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if c.Token != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(c.authUser() + ":" + c.Token))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraheader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+cred)
	}
	return env
}

// git runs one command in dir with the project's auth environment and returns its
// output. The error carries the redacted tail of stderr.
func (c Config) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return c.gitEnv(ctx, dir, nil, args...)
}

// gitEnv is git with env added to the command's environment (GIT_DIR, GIT_WORK_TREE and
// GIT_INDEX_FILE for the run repository, DESIGN 5.30).
func (c Config) gitEnv(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	return c.gitInput(ctx, dir, env, nil, args...)
}

// gitInput is gitEnv with stdin as the command's standard input (nil: none).
func (c Config) gitInput(ctx context.Context, dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, GitTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(baseEnv(), c.authEnv()...), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", subcommand(args), ctx.Err())
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", subcommand(args), obs.Redact(tail(stderr.String(), 400)))
	}
	return stdout.Bytes(), nil
}

// subcommand is the git command args run, past the global options before it (--git-dir,
// --work-tree, -c and its value), for the error text.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return "command"
}

// baseEnv is the environment every git command gets: the path and home of the api's
// own user, nothing else from the process.
func baseEnv() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	return env
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}

// Clone clones the default branch at depth 1 into dest (rule 3), which must not exist,
// and writes the local exclude file. dest's parent must exist.
func (c Config) Clone(ctx context.Context, dest string) error {
	return c.CloneBranch(ctx, dest, c.DefaultBranch)
}

// CloneBranch is Clone for any branch of the repository: still one branch at depth 1
// (--single-branch), with the token handled as in rule 4. A run that updates a merge
// request clones the request's source branch with it (DESIGN 5.22 rule 2).
func (c Config) CloneBranch(ctx context.Context, dest, branch string) error {
	if _, err := c.git(ctx, filepath.Dir(dest), "clone", "--quiet", "--depth", "1", "--single-branch",
		"--branch", branch, "--", c.RepoURL, filepath.Base(dest)); err != nil {
		return err
	}
	return WriteExclude(dest)
}

// WriteExclude appends the Excluded directories to <repo>/.git/info/exclude, the
// repository-local ignore file that is never committed (rule 3).
func WriteExclude(repo string) error {
	dir := filepath.Join(repo, ".git", "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "exclude"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("# Yuno: dependency and cache directories are never committed\n" + strings.Join(Excluded, "\n") + "\n")
	return err
}

// commitIDPattern is a full git commit id in SHA-1 form, 40 lowercase hex digits.
var commitIDPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// HeadSHA is the commit id HEAD names in the repository at repo (git rev-parse HEAD),
// trimmed. It is an error for anything but 40 hex digits: a run records the answer as
// its checkout_sha (DESIGN 5.22 rule 2), the base the branch guard compares against, and
// a short or empty id must never be recorded as one.
func (c Config) HeadSHA(ctx context.Context, repo string) (string, error) {
	out, err := c.git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !commitIDPattern.MatchString(sha) {
		return "", fmt.Errorf("git rev-parse HEAD: %q is not a full commit id", tail(sha, 80))
	}
	return sha, nil
}

// ErrBranchMoved is a guarded proposal's answer when the branch it would push to is no
// longer at the run's base (DESIGN 5.22 rule 4, D-050): someone pushed to the branch
// since. Head is the commit the branch is at now, Base the one the run expected. Nothing
// was copied or pushed; the operator starts a new run, which clones the branch as it is.
type ErrBranchMoved struct {
	Branch string
	Head   string
	Base   string
}

func (e *ErrBranchMoved) Error() string {
	return "the branch " + e.Branch + " has new commits since this run cloned it; nothing was pushed. " +
		"Finish and report it; a new run on the request starts from the branch as it is now"
}

// ErrConflict is a proposal's answer when files the run changed also changed on the branch
// it goes onto since the run's base, to other content (rule 10, D-087). Nothing was pushed.
// Branch is the branch they changed on, Paths the files, sorted.
type ErrConflict struct {
	Branch string
	Paths  []string
}

// maxNamedPaths is how many paths an error names before it counts the rest.
const maxNamedPaths = 20

func (e *ErrConflict) Error() string {
	return "these files conflict with changes on " + e.Branch + " made since this run's checkout: " + namePaths(e.Paths) +
		". Nothing was pushed. Ask the operator how to resolve it, or finish and report the conflict"
}

// namePaths lists paths for an error text, the first maxNamedPaths of them.
func namePaths(paths []string) string {
	if len(paths) <= maxNamedPaths {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:maxNamedPaths], ", ") + fmt.Sprintf(" and %d more", len(paths)-maxNamedPaths)
}

// Proposal is what a merge request's branch holds once Propose has pushed it.
type Proposal struct {
	Branch string
	// Commit is the branch's new head.
	Commit string
	// Base is the run's own commit: the base's tree with the run's changes, so the checkout
	// matches it apart from what the run changes later. It is the base of the run's next
	// proposal (runs.checkout_sha). Commit itself when the branch was still at the base,
	// else Commit's second parent (rule 10, D-087).
	Base  string
	Files int // files copied from the checkout
}

// Base is where a run's changes are counted from when they are proposed (rule 10, D-087).
type Base struct {
	// Commit is the project commit the checkout matches apart from what the run changed
	// since: the commit the run cloned, then its own commit after each push
	// (runs.checkout_sha, Proposal.Base). "" when it is not known: the branch's head as it
	// is now stands in for it, and the whole working tree is the run's.
	Commit string
	// Guard refuses with *ErrBranchMoved when the branch is no longer at Commit (DESIGN 5.22
	// rule 4), instead of merging the run's changes onto it per path.
	Guard bool
}

// RunBase is the unguarded base of a run's next proposal: its checkout_sha.
func RunBase(run *store.Run) Base {
	if run == nil || run.CheckoutSHA == nil {
		return Base{}
	}
	return Base{Commit: *run.CheckoutSHA}
}

// ErrNoChanges is Propose's answer when the checkout holds no change to propose.
var ErrNoChanges = errors.New("the checkout has no changes to propose")

// Propose publishes the run's changes in an agent's checkout as a new branch from the
// default branch (rule 10), with the default branch's head as the base: ProposePaths with
// an unknown base and no paths.
func (c Config) Propose(ctx context.Context, checkout, branch, message string) (Proposal, error) {
	return c.propose(ctx, checkout, c.DefaultBranch, branch, Base{}, message, nil)
}

// ProposePaths publishes the run's changes in an agent's checkout (rule 10, D-087) on top of
// the default branch as it is now and pushes them to branch. The api never runs git inside
// the checkout, where a hook or a configuration entry an agent wrote would run on the api
// side: propose works in a private clone. The run's changes are what differs between base
// and the checkout's working tree, copied with the copy rule (regular files only, links
// skipped, .git and the Excluded directories left out). When the default branch is still
// at the base they are one commit on top of it; when it moved they go onto it per path
// (see propose), so a file the run never touched is never deleted or reverted.
//
// With paths (relative files or directories of the checkout, already validated by the
// caller) only those are the run's changes: each replaces its counterpart in the base (a
// path the checkout no longer has but the base does is a deletion), and the rest of the
// checkout stays out. A path in neither is *ErrPathMissing. ErrNoChanges when there is
// nothing to push, *ErrConflict when the default branch changed the same files.
func (c Config) ProposePaths(ctx context.Context, checkout, branch string, base Base, message string, paths []string) (Proposal, error) {
	return c.propose(ctx, checkout, c.DefaultBranch, branch, base, message, paths)
}

// ErrPathMissing is ProposePaths' answer for a path that is neither in the checkout nor
// in the repository.
type ErrPathMissing struct{ Path string }

func (e *ErrPathMissing) Error() string {
	return "the path " + e.Path + " is not in the checkout; nothing was proposed"
}

// ProposeOnto is ProposeOntoPaths with no paths and the branch guard of DESIGN 5.22 rule 4:
// base is the commit the run's checkout was cloned from (or last pushed), and a branch that
// is no longer there answers *ErrBranchMoved without copying or pushing anything (D-050).
// An empty base skips the guard and takes the branch's head as the base.
func (c Config) ProposeOnto(ctx context.Context, checkout, branch, base, message string) (Proposal, error) {
	return c.propose(ctx, checkout, branch, branch, Base{Commit: base, Guard: true}, message, nil)
}

// ProposeOntoPaths is ProposePaths for a run that has a request already (DESIGN 5.20 rule 2,
// 5.22 rule 4): the run's changes go onto the request's own source branch and are pushed
// back to it.
func (c Config) ProposeOntoPaths(ctx context.Context, checkout, branch string, base Base, message string, paths []string) (Proposal, error) {
	return c.propose(ctx, checkout, branch, branch, base, message, paths)
}

// propose clones the branch from into a private directory and pushes the run's changes onto
// it as branch (rule 10, D-087):
//
//  1. The head is the clone's commit. A guarded base that is not the head is
//     *ErrBranchMoved; an empty base is the head. A base that is not the head is fetched
//     and checked out, so the clone holds the tree the run's checkout started from.
//  2. The checkout is copied over it (the whole working tree, or the paths) and staged; the
//     run's changes are the difference between the base and that index, path by path,
//     less what the copy cannot speak for (keepRunChanges). None is ErrNoChanges.
//  3. The run's own commit is the base with those changes. When the head is the base it is
//     what is pushed, as before.
//  4. Otherwise the branch moved since the base, and each changed path is weighed against
//     what the branch did to it since: a path the branch did not change takes the run's
//     version or deletion; a path both changed to the same content needs nothing; a path
//     both changed to different content, or a file the run writes where the branch made a
//     directory (or the reverse), is a conflict. Any conflict is *ErrConflict and nothing
//     is pushed. Otherwise the head with the run's changes is a merge commit of the head and
//     the run's own commit, and that is pushed; files the run did not change are the
//     head's.
//
// Before the push, a commit that would delete or change a path outside the run's changes
// is refused (checkTouched), so a regression shows instead of losing work.
func (c Config) propose(ctx context.Context, checkout, from, branch string, base Base, message string, paths []string) (Proposal, error) {
	if base.Commit != "" && !commitIDPattern.MatchString(base.Commit) {
		return Proposal{}, fmt.Errorf("the base %q is not a full commit id", tail(base.Commit, 80))
	}
	private, err := os.MkdirTemp("", "yuno-propose-")
	if err != nil {
		return Proposal{}, err
	}
	defer os.RemoveAll(private)
	dest := filepath.Join(private, "repo")
	if err := c.CloneBranch(ctx, dest, from); err != nil {
		return Proposal{}, err
	}
	head, err := c.HeadSHA(ctx, dest)
	if err != nil {
		return Proposal{}, err
	}
	basis := base.Commit
	if basis == "" {
		basis = head
	}
	if basis != head {
		if base.Guard {
			return Proposal{}, &ErrBranchMoved{Branch: branch, Head: head, Base: basis}
		}
		if _, err := c.git(ctx, dest, "fetch", "--quiet", "--no-tags", "--depth", "1", "origin", basis); err != nil {
			return Proposal{}, err
		}
		if _, err := c.git(ctx, dest, "checkout", "--quiet", "--detach", basis); err != nil {
			return Proposal{}, err
		}
	}
	var n int
	addArgs := []string{"add", "--all"}
	if len(paths) > 0 {
		if n, err = copyPaths(checkout, dest, paths); err != nil {
			var missing *ErrPathMissing
			if errors.As(err, &missing) {
				return Proposal{}, err
			}
			return Proposal{}, fmt.Errorf("copy the paths: %w", err)
		}
		addArgs = append(addArgs, "--")
		addArgs = append(addArgs, paths...)
	} else {
		if err := clearWorkingTree(dest); err != nil {
			return Proposal{}, err
		}
		if n, err = copyTree(checkout, dest); err != nil {
			return Proposal{}, fmt.Errorf("copy the checkout: %w", err)
		}
	}
	if _, err := c.git(ctx, dest, addArgs...); err != nil {
		return Proposal{}, err
	}
	staged, err := c.git(ctx, dest, "write-tree")
	if err != nil {
		return Proposal{}, err
	}
	changes, err := c.treeChanges(ctx, dest, basis, strings.TrimSpace(string(staged)))
	if err != nil {
		return Proposal{}, err
	}
	if changes, err = keepRunChanges(checkout, changes); err != nil {
		return Proposal{}, err
	}
	if len(changes) == 0 {
		return Proposal{Branch: branch, Files: n}, ErrNoChanges
	}
	onto := changes
	if basis != head {
		theirs, err := c.treeChanges(ctx, dest, basis, head)
		if err != nil {
			return Proposal{}, err
		}
		var conflicts []string
		if onto, conflicts = mergePaths(changes, theirs); len(conflicts) > 0 {
			return Proposal{}, &ErrConflict{Branch: from, Paths: conflicts}
		}
		if len(onto) == 0 {
			// The branch has every change of the run already.
			return Proposal{Branch: branch, Files: n}, ErrNoChanges
		}
	}
	if strings.TrimSpace(message) == "" {
		message = "Changes proposed by Yuno"
	}
	index := filepath.Join(private, "index")
	ownTree, err := c.applyChanges(ctx, dest, index, basis, changes)
	if err != nil {
		return Proposal{}, err
	}
	own, err := c.commitTree(ctx, dest, ownTree, message, basis)
	if err != nil {
		return Proposal{}, err
	}
	if err := c.checkTouched(ctx, dest, basis, own, changes); err != nil {
		return Proposal{}, err
	}
	pushed := own
	if basis != head {
		tree, err := c.applyChanges(ctx, dest, index, head, onto)
		if err != nil {
			return Proposal{}, err
		}
		subject, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
		if pushed, err = c.commitTree(ctx, dest, tree, "Merge \""+subject+"\" with "+from, head, own); err != nil {
			return Proposal{}, err
		}
		if err := c.checkTouched(ctx, dest, head, pushed, onto); err != nil {
			return Proposal{}, err
		}
	}
	if _, err := c.git(ctx, dest, "push", "--quiet", "--no-verify", "--", "origin", pushed+":refs/heads/"+branch); err != nil {
		return Proposal{}, err
	}
	return Proposal{Branch: branch, Commit: pushed, Base: own, Files: n}, nil
}

// treeChange is one path of `git diff-tree -r -z`: its mode and object id before and after,
// mode 000000 for a path that is absent on that side.
type treeChange struct {
	path             string
	oldMode, newMode string
	oldID, newID     string
}

const (
	absentMode  = "000000"
	gitlinkMode = "160000"
	zeroID      = "0000000000000000000000000000000000000000"
)

func (t treeChange) gone() bool { return t.newMode == absentMode }

// sameResult tells whether two changes of a path leave it the same: the same mode and
// content, or both a deletion.
func (t treeChange) sameResult(o treeChange) bool {
	return t.newMode == o.newMode && t.newID == o.newID
}

// treeChanges lists the paths that differ between two trees or commits of repo, renames as
// a deletion and an addition.
func (c Config) treeChanges(ctx context.Context, repo, from, to string) ([]treeChange, error) {
	out, err := c.git(ctx, repo, "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", from, to)
	if err != nil {
		return nil, err
	}
	var changes []treeChange
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 {
			return nil, fmt.Errorf("git diff-tree: unexpected entry %q", tail(fields[i], 80))
		}
		changes = append(changes, treeChange{path: fields[i+1], oldMode: meta[0], newMode: meta[1], oldID: meta[2], newID: meta[3]})
	}
	return changes, nil
}

// keepRunChanges drops the changes the copy of the checkout cannot speak for (rule 10): a
// path inside an Excluded directory, a submodule, and the deletion of a path the checkout
// still has as a link, or under one, or as anything the copy skips. Those keep the base's
// version.
func keepRunChanges(checkout string, changes []treeChange) ([]treeChange, error) {
	root, err := os.OpenRoot(checkout)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	skip := map[string]bool{".git": true}
	for _, d := range Excluded {
		skip[strings.TrimSuffix(d, "/")] = true
	}
	var kept []treeChange
	for _, ch := range changes {
		dirs := strings.Split(ch.path, "/")
		dirs = dirs[:len(dirs)-1]
		if slices.ContainsFunc(dirs, func(d string) bool { return skip[d] }) || ch.oldMode == gitlinkMode || ch.newMode == gitlinkMode {
			continue
		}
		if ch.gone() {
			absent, err := absentFrom(root, ch.path)
			if err != nil {
				return nil, err
			}
			if !absent {
				continue
			}
		}
		kept = append(kept, ch)
	}
	return kept, nil
}

// absentFrom tells whether the checkout lacks p for real: a part of it does not exist, a
// part that should be a directory is not one, or p is now a directory. A link on the way,
// or at p, is not an absence; nor is anything else at p, which the copy skipped.
func absentFrom(root *os.Root, p string) (bool, error) {
	parts := strings.Split(p, "/")
	cur := ""
	for i, part := range parts {
		cur = path.Join(cur, part)
		info, err := root.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return true, nil
		case err != nil:
			return false, err
		case info.Mode()&fs.ModeSymlink != 0:
			return false, nil
		case i == len(parts)-1:
			return info.IsDir(), nil
		case !info.IsDir():
			return true, nil
		}
	}
	return false, nil
}

// mergePaths is the per-path merge of propose's step 4: ours are the run's changes since the
// base, theirs the branch's. It returns what to apply onto the branch and the paths that
// conflict, sorted.
func mergePaths(ours, theirs []treeChange) (onto []treeChange, conflicts []string) {
	byPath := make(map[string]treeChange, len(theirs))
	dirs := map[string]bool{} // directories the branch has files in that the base may not
	for _, t := range theirs {
		byPath[t.path] = t
		if !t.gone() {
			for d := path.Dir(t.path); d != "."; d = path.Dir(d) {
				dirs[d] = true
			}
		}
	}
	for _, o := range ours {
		if t, ok := byPath[o.path]; ok {
			if !o.sameResult(t) {
				conflicts = append(conflicts, o.path)
			}
			continue
		}
		if !o.gone() {
			clash := dirs[o.path]
			for d := path.Dir(o.path); d != "." && !clash; d = path.Dir(d) {
				if t, ok := byPath[d]; ok && !t.gone() {
					clash = true
				}
			}
			if clash {
				conflicts = append(conflicts, o.path)
				continue
			}
		}
		onto = append(onto, o)
	}
	slices.Sort(conflicts)
	return onto, conflicts
}

// applyChanges is the tree of treeish with changes applied, built in the index file index of
// repo: deletions first, so a path can turn from a file into a directory or back.
func (c Config) applyChanges(ctx context.Context, repo, index, treeish string, changes []treeChange) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := c.gitEnv(ctx, repo, env, "read-tree", treeish); err != nil {
		return "", err
	}
	var in bytes.Buffer
	for _, ch := range changes {
		if ch.gone() {
			fmt.Fprintf(&in, "0 %s\t%s\x00", zeroID, ch.path)
		}
	}
	for _, ch := range changes {
		if !ch.gone() {
			fmt.Fprintf(&in, "%s %s\t%s\x00", ch.newMode, ch.newID, ch.path)
		}
	}
	if _, err := c.gitInput(ctx, repo, env, in.Bytes(), "update-index", "--add", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := c.gitEnv(ctx, repo, env, "write-tree")
	return strings.TrimSpace(string(out)), err
}

// commitTree commits tree in repo as Yuno with message and parents, and returns its id.
func (c Config) commitTree(ctx context.Context, repo, tree, message string, parents ...string) (string, error) {
	args := []string{"-c", "user.name=" + CommitName, "-c", "user.email=" + CommitEmail, "-c", "commit.gpgsign=false", "commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := c.git(ctx, repo, append(args, "-m", message)...)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if !commitIDPattern.MatchString(sha) {
		return "", fmt.Errorf("git commit-tree: %q is not a full commit id", tail(sha, 80))
	}
	return sha, nil
}

// checkTouched is the guardrail of rule 10 (D-087): the commit to (on top of from) may
// change only the paths of changes, the run's. propose builds it so; a path outside them
// (a deletion above all) refuses the push and names the paths.
func (c Config) checkTouched(ctx context.Context, repo, from, to string, changes []treeChange) error {
	got, err := c.treeChanges(ctx, repo, from, to)
	if err != nil {
		return err
	}
	ours := make(map[string]bool, len(changes))
	for _, ch := range changes {
		ours[ch.path] = true
	}
	var deleted, changed []string
	for _, g := range got {
		switch {
		case ours[g.path]:
		case g.gone():
			deleted = append(deleted, g.path)
		default:
			changed = append(changed, g.path)
		}
	}
	if len(deleted) == 0 && len(changed) == 0 {
		return nil
	}
	text := "the commit would"
	if len(deleted) > 0 {
		text += " delete " + namePaths(deleted)
		if len(changed) > 0 {
			text += " and"
		}
	}
	if len(changed) > 0 {
		text += " change " + namePaths(changed)
	}
	return errors.New(text + ", which this run did not change; nothing was pushed")
}

// copyPaths replaces each path of dest (a fresh clone) with the same path of src (the
// agent's checkout): a file or a directory (links and the Excluded directories inside it
// skipped, as copyTree does). A path the checkout lacks is removed from dest, a deletion;
// one neither has is *ErrPathMissing. A path that goes through a link, on either side, is
// refused. It returns how many files it copied.
func copyPaths(src, dest string, paths []string) (int, error) {
	from, err := os.OpenRoot(src)
	if err != nil {
		return 0, err
	}
	defer from.Close()
	to, err := os.OpenRoot(dest)
	if err != nil {
		return 0, err
	}
	defer to.Close()
	n := 0
	for _, p := range paths {
		sinfo, serr := plainInfo(from, p)
		_, derr := plainInfo(to, p)
		for _, e := range []error{serr, derr} {
			if e != nil && !errors.Is(e, fs.ErrNotExist) {
				return n, e
			}
		}
		if serr != nil { // not in the checkout
			if derr != nil {
				return n, &ErrPathMissing{Path: p}
			}
			if err := to.RemoveAll(p); err != nil {
				return n, err
			}
			continue
		}
		if derr == nil {
			if err := to.RemoveAll(p); err != nil {
				return n, err
			}
		}
		switch {
		case sinfo.IsDir():
			if err := to.MkdirAll(p, 0o755); err != nil {
				return n, err
			}
			c, err := copyTree(filepath.Join(src, filepath.FromSlash(p)), filepath.Join(dest, filepath.FromSlash(p)))
			n += c
			if err != nil {
				return n, err
			}
		case sinfo.Mode().IsRegular():
			if err := to.MkdirAll(path.Dir(p), 0o755); err != nil {
				return n, err
			}
			in, err := from.Open(p)
			if err != nil {
				return n, err
			}
			out, err := to.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, sinfo.Mode().Perm()|0o600)
			if err != nil {
				in.Close()
				return n, err
			}
			_, err = copyChunks(context.Background(), out, in)
			in.Close()
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, err
			}
			n++
		default:
			return n, fmt.Errorf("path %s is not a regular file or directory", p)
		}
	}
	return n, nil
}

// plainInfo is Lstat of p in root, refusing a path whose parts (the last included) are
// links. A part that does not exist is fs.ErrNotExist.
func plainInfo(root *os.Root, p string) (fs.FileInfo, error) {
	var info fs.FileInfo
	cur := ""
	for _, part := range strings.Split(p, "/") {
		cur = path.Join(cur, part)
		var err error
		if info, err = root.Lstat(cur); err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("path %s goes through a link", p)
		}
	}
	return info, nil
}

// clearWorkingTree removes everything but .git from a checkout, so files the agent
// deleted are deletions in the commit.
func clearWorkingTree(repo string) error {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(repo, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies the regular files of src into dst, creating directories as needed.
// Links and special files are skipped, never followed; .git and the Excluded
// directories are left out at any depth. src is opened as an os.Root so a link cannot
// lead outside it. It returns how many files it copied.
func copyTree(src, dst string) (int, error) {
	return copyTreeCtx(context.Background(), src, dst, 0)
}

// ErrCheckoutTooLarge is Diff's answer for a checkout whose files add up to more than
// MaxCheckoutBytes: the diff is skipped rather than copying them (review L1).
var ErrCheckoutTooLarge = errors.New("the checkout is over 500 MiB")

// MaxCheckoutBytes is how much of a checkout Diff copies into its private clone: the sum of
// the sizes of the files it would copy (DESIGN 5.25 rule 3). A checkout of a repository
// that size is not one an artifact's diff is worth the time and the disk for.
const MaxCheckoutBytes = 500 << 20

// copyTreeCtx is copyTree under a context, with a cap on the bytes copied when limit is
// positive. A cancelled context ends the walk, and a copy of a large file, at the next
// entry or chunk; a tree whose files add up to more than limit is ErrCheckoutTooLarge,
// found from the sizes before a file is copied and again as it is read, since a file can
// grow meanwhile. What was copied before the error stays in dst.
func copyTreeCtx(ctx context.Context, src, dst string, limit int64) (int, error) {
	root, err := os.OpenRoot(src)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	return copyTreeRoot(ctx, root, dst, limit)
}

// copyTreeRoot is copyTreeCtx for a checkout the caller opened as an os.Root.
func copyTreeRoot(ctx context.Context, root *os.Root, dst string, limit int64) (int, error) {
	skip := map[string]bool{".git": true}
	for _, d := range Excluded {
		skip[strings.TrimSuffix(d, "/")] = true
	}
	n := 0
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if skip[d.Name()] {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, p), 0o755)
		}
		if d.Name() == ".git" {
			// A .git file (a gitfile naming another repository) is never content, and git
			// would follow it from the copy (the run repository's commit, DESIGN 5.30).
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := root.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if limit > 0 && total+info.Size() > limit {
			return ErrCheckoutTooLarge
		}
		out, err := os.OpenFile(filepath.Join(dst, p), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		var from io.Reader = in
		if limit > 0 {
			from = io.LimitReader(in, limit-total+1)
		}
		copied, err := copyChunks(ctx, out, from)
		total += copied
		if err != nil {
			out.Close()
			return err
		}
		if limit > 0 && total > limit {
			out.Close()
			return ErrCheckoutTooLarge
		}
		n++
		return out.Close()
	})
	return n, err
}

// copyChunks copies src to dst in chunks, looking at ctx between them, and returns the
// bytes written.
func copyChunks(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 256<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		m, rerr := src.Read(buf)
		if m > 0 {
			w, werr := dst.Write(buf[:m])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}
