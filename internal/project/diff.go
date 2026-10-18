package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"yuno/internal/obs"
)

// The checkout's diff for the run's artifacts (DESIGN 5.25 rule 3). Like Propose it never
// runs git inside the agent's checkout: a hook or a configuration entry an agent wrote
// there would run on the api side (5.16 rule 10's reason). It works in a private clone of
// its own and copies the checkout's working tree over it.

// MaxDiffBytes is the largest patch the artifact store keeps (DESIGN 5.25 units): the
// collector cuts a longer one and ends it with a marker line. Diff itself never holds
// more than MaxDiffBytes+1 bytes of git's output, so a checkout of large binaries cannot
// exhaust the api's memory.
const MaxDiffBytes = 10 << 20

// diffOutputLimit is how much of git's output Diff reads: one byte past MaxDiffBytes, so
// the caller can tell a patch that fits from one that was cut. A variable so a test can
// lower it.
var diffOutputLimit int64 = MaxDiffBytes + 1

// ErrNoCheckout is Diff's answer for a directory that holds no .git directory: there is
// no checkout to diff.
var ErrNoCheckout = errors.New("the checkout has no .git directory")

// Diff returns the patch that turns the commit a run cloned into the checkout's working
// tree: what the agents changed, as `git diff --cached --binary --no-color --no-ext-diff`
// prints it, empty when nothing differs.
//
// branch is the branch the run cloned (the request's source branch for a 5.22 run, else
// the default one) and base the commit the clone was at (the run's checkout_sha; "" skips
// the base and diffs against the branch as it is now). The patch is produced in a private
// temporary directory, removed before Diff returns: it clones branch at depth 1; when
// base is not the clone's head (the branch moved since), it fetches base at depth 1 and
// checks it out detached; it empties the working tree and copies the checkout's tree
// over it with the copy rule of 5.16 rule 10 (regular files only, links skipped, .git and
// the Excluded directories left out); it stages everything and diffs the index.
//
// checkout must hold a .git directory (ErrNoCheckout otherwise). A clone or fetch that
// fails is the returned error, redacted like every git error; the caller skips the diff.
// The copy of the working tree honours ctx and stops at MaxCheckoutBytes
// (ErrCheckoutTooLarge), so a huge checkout cannot run past the caller's deadline.
// The patch is at most MaxDiffBytes+1 bytes: a longer one is cut at that length.
func (c Config) Diff(ctx context.Context, checkout, branch, base string) ([]byte, error) {
	if err := requireGitDir(checkout); err != nil {
		return nil, err
	}
	if base != "" && !commitIDPattern.MatchString(base) {
		return nil, fmt.Errorf("the base %q is not a full commit id", tail(base, 80))
	}
	if branch == "" {
		branch = c.DefaultBranch
	}
	private, err := os.MkdirTemp("", "yuno-diff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(private)
	dest := filepath.Join(private, "repo")
	if err := c.CloneBranch(ctx, dest, branch); err != nil {
		return nil, err
	}
	if base != "" {
		head, err := c.HeadSHA(ctx, dest)
		if err != nil {
			return nil, err
		}
		if head != base {
			// The branch moved since the run cloned it: its commit is the right base.
			if _, err := c.git(ctx, dest, "fetch", "--quiet", "--depth", "1", "origin", base); err != nil {
				return nil, err
			}
			if _, err := c.git(ctx, dest, "checkout", "--quiet", "--detach", base); err != nil {
				return nil, err
			}
		}
	}
	if err := clearWorkingTree(dest); err != nil {
		return nil, err
	}
	if _, err := copyTreeCtx(ctx, checkout, dest, MaxCheckoutBytes); err != nil {
		if errors.Is(err, ErrCheckoutTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("copy the checkout: %w", err)
	}
	if _, err := c.git(ctx, dest, "add", "--all"); err != nil {
		return nil, err
	}
	return c.gitCapped(ctx, dest, diffOutputLimit, "diff", "--cached", "--binary", "--no-color", "--no-ext-diff")
}

// requireGitDir checks that dir holds a .git directory, looking at it without following
// a link (the .git of a checkout is never read or run, only its presence matters).
func requireGitDir(dir string) error {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoCheckout
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(".git")
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		return ErrNoCheckout
	}
	return err
}

// errOutputFull stops the copy of git's output once the limit is reached.
var errOutputFull = errors.New("output limit reached")

// capWriter keeps the first limit bytes written to it and refuses the rest, which ends
// the copy from git's pipe; git then stops on a broken pipe instead of filling memory.
type capWriter struct {
	buf   bytes.Buffer
	limit int64
	full  bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.limit - int64(w.buf.Len())
	if int64(len(p)) > room {
		w.buf.Write(p[:room])
		w.full = true
		return int(room), errOutputFull
	}
	return w.buf.Write(p)
}

// gitCapped is git for a command whose output can be large: at most limit bytes of its
// standard output are read, the rest is dropped and the command ended. The error is
// redacted like git's.
func (c Config) gitCapped(ctx context.Context, dir string, limit int64, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, GitTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), c.authEnv()...)
	cmd.Stdin = nil
	out := &capWriter{limit: limit}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	if out.full {
		// Ended on purpose: the caller gets the first limit bytes.
		return out.buf.Bytes(), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return nil, fmt.Errorf("git %s: %s", args[0], obs.Redact(tail(stderr.String(), 400)))
	}
	return out.buf.Bytes(), nil
}
