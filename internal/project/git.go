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
	"path/filepath"
	"strings"
	"time"

	"yuno/internal/obs"
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
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, GitTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), c.authEnv()...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", args[0], obs.Redact(tail(stderr.String(), 400)))
	}
	return stdout.Bytes(), nil
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
	if _, err := c.git(ctx, filepath.Dir(dest), "clone", "--quiet", "--depth", "1", "--single-branch",
		"--branch", c.DefaultBranch, "--", c.RepoURL, filepath.Base(dest)); err != nil {
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

// Proposal is what a merge request's branch holds once Propose has pushed it.
type Proposal struct {
	Branch string
	Commit string
	Files  int // files copied from the checkout
}

// ErrNoChanges is Propose's answer when the checkout equals the default branch.
var ErrNoChanges = errors.New("the checkout has no changes to propose")

// Propose publishes the working tree of an agent's checkout as one commit on branch
// (rule 10): the api never runs git inside the checkout, where a hook or a
// configuration entry an agent wrote would run on the api side. It clones the default
// branch again into a private directory of its own, copies the checkout's working tree
// over it (regular files only, links skipped, .git and the Excluded directories left
// out), commits what differs as Yuno with message, and pushes HEAD to the branch with
// the token. ErrNoChanges when nothing differs.
func (c Config) Propose(ctx context.Context, checkout, branch, message string) (Proposal, error) {
	private, err := os.MkdirTemp("", "yuno-propose-")
	if err != nil {
		return Proposal{}, err
	}
	defer os.RemoveAll(private)
	dest := filepath.Join(private, "repo")
	if err := c.Clone(ctx, dest); err != nil {
		return Proposal{}, err
	}
	if err := clearWorkingTree(dest); err != nil {
		return Proposal{}, err
	}
	n, err := copyTree(checkout, dest)
	if err != nil {
		return Proposal{}, fmt.Errorf("copy the checkout: %w", err)
	}
	if _, err := c.git(ctx, dest, "add", "--all"); err != nil {
		return Proposal{}, err
	}
	status, err := c.git(ctx, dest, "status", "--porcelain")
	if err != nil {
		return Proposal{}, err
	}
	if len(bytes.TrimSpace(status)) == 0 {
		return Proposal{Branch: branch, Files: n}, ErrNoChanges
	}
	if strings.TrimSpace(message) == "" {
		message = "Changes proposed by Yuno"
	}
	if _, err := c.git(ctx, dest, "-c", "user.name="+CommitName, "-c", "user.email="+CommitEmail, "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--no-verify", "-m", message); err != nil {
		return Proposal{}, err
	}
	sha, err := c.git(ctx, dest, "rev-parse", "HEAD")
	if err != nil {
		return Proposal{}, err
	}
	if _, err := c.git(ctx, dest, "push", "--quiet", "--", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return Proposal{}, err
	}
	return Proposal{Branch: branch, Commit: strings.TrimSpace(string(sha)), Files: n}, nil
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
	root, err := os.OpenRoot(src)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	skip := map[string]bool{".git": true}
	for _, d := range Excluded {
		skip[strings.TrimSuffix(d, "/")] = true
	}
	n := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
		out, err := os.OpenFile(filepath.Join(dst, p), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		n++
		return out.Close()
	})
	return n, err
}
