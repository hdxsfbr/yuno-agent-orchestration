package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Update from the default branch (Chief platform control, DESIGN wave 3 rules 9 and 10):
// the platform alone, no agent and no checkout, merges the project's default branch into
// a merge request's branch and pushes the merge, never forced.

// The outcomes of UpdateFromDefault (rule 9).
const (
	UpdateUpToDate  = "up_to_date"
	UpdateUpdated   = "updated"
	UpdateConflicts = "conflicts"
)

// BranchUpdate is what UpdateFromDefault did.
type BranchUpdate struct {
	// Outcome is UpdateUpToDate (the branch has the default branch's commit already),
	// UpdateUpdated (a merge commit was pushed) or UpdateConflicts (nothing was pushed).
	Outcome string
	// Head is the branch's commit as fetched, the one the caller expected.
	Head string
	// Default is the default branch's commit that was merged, or found contained.
	Default string
	// Commit is the merge commit pushed, the branch's new head; "" unless UpdateUpdated.
	Commit string
	// Conflicts are the files the merge could not settle, sorted; only with
	// UpdateConflicts.
	Conflicts []string
}

// ErrHeadMoved is UpdateFromDefault's answer for a branch that is not at the commit the
// caller expected, before the fetch or by the push (rules 9 and 10). Current is where the
// branch is now, "" when it could not be read. Nothing was pushed.
type ErrHeadMoved struct{ Current string }

func (e *ErrHeadMoved) Error() string {
	if e.Current == "" {
		return "the branch moved since it was read; nothing was pushed"
	}
	return "the branch moved to " + e.Current + " since it was read; nothing was pushed"
}

// IsCommitID reports whether s is a full commit id, 40 lowercase hex digits (DESIGN wave 3
// units: SHAs are compared exactly).
func IsCommitID(s string) bool { return commitIDPattern.MatchString(s) }

// beforeUpdatePush runs between the merge and its push; a test moves the branch there.
var beforeUpdatePush func()

// UpdateFromDefault merges the default branch into branch and pushes the merge (DESIGN
// wave 3 rules 9 and 10):
//
//  1. branch and the default branch are fetched into a throwaway bare repository with the
//     project's token, deep enough to hold their merge base (FetchRun).
//  2. A branch head that is not expectedHead is *ErrHeadMoved.
//  3. A branch that has the default branch's commit is UpdateUpToDate.
//  4. Otherwise git merge-tree merges the two without a working tree. Conflicts are
//     UpdateConflicts with the files, and nothing is pushed.
//  5. A clean merge is a two-parent commit (the branch's head, then the default branch's)
//     as Yuno, pushed as <commit>:refs/heads/<branch> without force, so a branch that moved
//     since the fetch is refused by the provider: *ErrHeadMoved with where it is now.
//
// ErrNoMergeBase when the two share no history within the fetched commits. Git's errors
// are redacted, as everywhere (5.16 rule 4).
func (c Config) UpdateFromDefault(ctx context.Context, branch, expectedHead string) (BranchUpdate, error) {
	if !commitIDPattern.MatchString(expectedHead) {
		return BranchUpdate{}, fmt.Errorf("the expected head %q is not a full commit id", tail(expectedHead, 80))
	}
	if branch == "" || branch == c.DefaultBranch {
		return BranchUpdate{}, errors.New("the branch is the default branch; there is nothing to update it from")
	}
	private, err := os.MkdirTemp("", "yuno-update-")
	if err != nil {
		return BranchUpdate{}, err
	}
	defer os.RemoveAll(private)
	repo := filepath.Join(private, "repo.git")
	if err := InitRunRepo(ctx, repo); err != nil {
		return BranchUpdate{}, err
	}
	f, err := c.FetchRun(ctx, repo, "update", branch)
	if err != nil {
		return BranchUpdate{}, err
	}
	if f.Base != expectedHead {
		return BranchUpdate{}, &ErrHeadMoved{Current: f.Base}
	}
	u := BranchUpdate{Head: f.Base, Default: f.Default}
	if f.Default == "" || f.Default == f.Base || f.MergeBase == f.Default {
		u.Outcome = UpdateUpToDate
		return u, nil
	}
	if f.MergeBase == "" {
		return BranchUpdate{}, ErrNoMergeBase
	}
	bare := []string{"--git-dir=" + repo}
	out, err := c.git(ctx, private, append(bare, "merge-tree", "--write-tree", "--name-only", "-z", "--no-messages", f.Base, f.Default)...)
	tree, conflicts := parseMergeTree(out)
	switch {
	case !commitIDPattern.MatchString(tree):
		if err == nil {
			err = fmt.Errorf("git merge-tree: %q is not a tree id", tail(tree, 80))
		}
		return BranchUpdate{}, err
	case err != nil && len(conflicts) > 0:
		u.Outcome, u.Conflicts = UpdateConflicts, conflicts
		return u, nil
	case err != nil:
		return BranchUpdate{}, err
	}
	msg := "Merge branch '" + c.DefaultBranch + "' into " + branch
	commit, err := c.commitTree(ctx, repo, tree, msg, f.Base, f.Default)
	if err != nil {
		return BranchUpdate{}, err
	}
	if beforeUpdatePush != nil {
		beforeUpdatePush()
	}
	if _, perr := c.git(ctx, private, append(bare, "push", "--quiet", "--no-verify", "--", c.RepoURL, commit+":refs/heads/"+branch)...); perr != nil {
		// A refusal of a push that is not forced is most often a branch that moved since
		// the fetch: the branch's head now says which.
		if now, lerr := c.remoteHead(ctx, private, branch); lerr == nil && now != f.Base {
			return BranchUpdate{}, &ErrHeadMoved{Current: now}
		}
		return BranchUpdate{}, perr
	}
	u.Outcome, u.Commit = UpdateUpdated, commit
	return u, nil
}

// parseMergeTree reads `git merge-tree --write-tree --name-only -z` output: the tree id,
// then the conflicted files, each ended by NUL, then an empty entry. It returns the tree
// and the files, sorted and without repeats.
func parseMergeTree(out []byte) (string, []string) {
	fields := strings.Split(string(out), "\x00")
	tree := strings.TrimSpace(fields[0])
	var files []string
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		if !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	return tree, files
}

// remoteHead is the commit branch names on the project now (git ls-remote), "" when the
// branch is gone.
func (c Config) remoteHead(ctx context.Context, dir, branch string) (string, error) {
	out, err := c.git(ctx, dir, "ls-remote", "--", c.RepoURL, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	sha, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if sha != "" && !commitIDPattern.MatchString(sha) {
		return "", fmt.Errorf("git ls-remote: %q is not a full commit id", tail(sha, 80))
	}
	return sha, nil
}
