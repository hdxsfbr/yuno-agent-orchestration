package skillbundle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"yuno/internal/pathcheck"
)

// GitTimeout bounds one whole import (preflight, clone, checkout), DESIGN 5.27 rule 7.
const GitTimeout = 60 * time.Second

// GitRunner runs one git command in dir ("" for the current directory) and returns its
// standard output. A failure is an error whose message is git's first stderr line. The
// default runs the git binary; tests inject a fake.
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// GitImporter imports a skill from a Git repository (DESIGN 5.27 rule 7).
type GitImporter struct {
	// Hosts are the hosts a repository may be on, and a redirect may lead to.
	Hosts []string
	// Run runs git; nil runs the git binary.
	Run GitRunner
	// Client does the preflight request that follows redirects under the allowlist; nil
	// uses a plain client. Its CheckRedirect is replaced.
	Client *http.Client
	// Timeout overrides GitTimeout; MaxCloneBytes overrides MaxCloneBytes; TempDir is where
	// the clone is made (os.TempDir when empty).
	Timeout       time.Duration
	MaxCloneBytes int64
	TempDir       string

	// Test seams: rewrite maps the validated URL to what git is given, extraConfig adds
	// -c settings, skipPreflight skips the HTTP request.
	rewrite       func(string) string
	extraConfig   []string
	skipPreflight bool
}

// GitRequest is the body of POST /api/skills/import-git.
type GitRequest struct{ URL, Ref, Path string }

// GitResult is an imported skill and where it came from: URL is the repository, Commit the
// resolved commit sha.
type GitResult struct {
	Bundle *Bundle
	URL    string
	Commit string
}

var (
	refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+@-]{0,199}$`)
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ValidateURL checks a repository URL against rule 7: https, no credentials, no query or
// fragment, and a host in the allowlist. It returns the URL without a trailing slash.
func (g *GitImporter) ValidateURL(raw string) (*url.URL, error) {
	bad := func(format string, a ...any) error { return &InputError{Path: "url", Msg: fmt.Sprintf(format, a...)} }
	if raw == "" || strings.ContainsAny(raw, " \t\r\n\\") {
		return nil, bad("is not a valid URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, bad("is not a valid URL")
	}
	if u.Scheme != "https" {
		return nil, bad("must use https")
	}
	if u.User != nil {
		return nil, bad("must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, bad("must not have a query or fragment")
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, bad("must use the default https port")
	}
	if err := g.checkHost(u.Hostname()); err != nil {
		return nil, bad("%v", err)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if u.Path == "" || slices.Contains(strings.Split(u.Path, "/"), "..") || strings.Contains(u.Path, "//") {
		return nil, bad("must name a repository")
	}
	return u, nil
}

func (g *GitImporter) checkHost(host string) error {
	host = strings.ToLower(host)
	for _, h := range g.Hosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return nil
		}
	}
	return fmt.Errorf("host %q is not allowed (allowed: %s)", host, strings.Join(g.Hosts, ", "))
}

// Import clones the repository shallowly without a checkout, checks out only req.Path,
// reads the skill there and removes the clone on every path.
func (g *GitImporter) Import(ctx context.Context, req GitRequest) (*GitResult, error) {
	u, err := g.ValidateURL(req.URL)
	if err != nil {
		return nil, err
	}
	if req.Ref != "" && (!refPattern.MatchString(req.Ref) || strings.Contains(req.Ref, "..")) {
		return nil, &InputError{Path: "ref", Msg: "is not a valid branch, tag or commit"}
	}
	sub := ""
	if req.Path != "" {
		sub = strings.TrimRight(req.Path, "/")
		if sub == "" || sub == "." {
			sub = ""
		} else if err := pathcheck.Valid(sub); err != nil || path.Clean(sub) != sub || strings.HasPrefix(sub, ".") || strings.HasPrefix(sub, "-") {
			return nil, &InputError{Path: "path", Msg: "must be a relative directory inside the repository"}
		}
	}
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = GitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	repoURL := u.String()
	if !g.skipPreflight {
		if repoURL, err = g.preflight(ctx, u); err != nil {
			return nil, err
		}
	}
	cloneURL := repoURL
	if g.rewrite != nil {
		cloneURL = g.rewrite(repoURL)
	}

	tmp, err := os.MkdirTemp(g.TempDir, "skill-import-")
	if err != nil {
		return nil, fmt.Errorf("temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	repo := filepath.Join(tmp, "repo")

	max := g.MaxCloneBytes
	if max <= 0 {
		max = MaxCloneBytes
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go watchSize(ctx, repo, max, stop)

	commit, err := g.fetch(ctx, cloneURL, req.Ref, sub, repo)
	if err != nil {
		if c := context.Cause(ctx); c != nil {
			var ce *CapError
			if errors.As(c, &ce) {
				return nil, c
			}
			if errors.Is(c, context.DeadlineExceeded) {
				return nil, fmt.Errorf("git did not finish within %s", timeout)
			}
		}
		return nil, err
	}
	if n := dirSize(repo); n > max {
		return nil, &CapError{Msg: fmt.Sprintf("the repository is over the %d MiB clone cap", max>>20)}
	}

	dir, err := skillDir(repo, sub)
	if err != nil {
		return nil, err
	}
	fallback := path.Base(sub)
	if sub == "" {
		fallback = strings.TrimSuffix(path.Base(u.Path), ".git")
	}
	b, err := ReadDir(dir, fallback, false)
	if err != nil {
		return nil, err
	}
	return &GitResult{Bundle: b, URL: u.String(), Commit: commit}, nil
}

// fetch clones into repo and returns the resolved commit.
func (g *GitImporter) fetch(ctx context.Context, cloneURL, ref, sub, repo string) (string, error) {
	isSHA := shaPattern.MatchString(ref)
	clone := func(filter bool) error {
		args := []string{"clone", "--quiet", "--depth", "1", "--no-checkout"}
		if filter {
			args = append(args, "--filter=blob:limit=2m")
		}
		if ref != "" && !isSHA {
			args = append(args, "--branch", ref)
		}
		_, err := g.git(ctx, "", append(args, "--", cloneURL, repo)...)
		return err
	}
	err := clone(true)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "filter") && ctx.Err() == nil {
		_ = os.RemoveAll(repo)
		err = clone(false)
	}
	if err != nil {
		return "", err
	}
	target := "HEAD"
	if isSHA {
		if _, err := g.git(ctx, repo, "fetch", "--depth", "1", "origin", ref); err != nil {
			return "", err
		}
		target = "FETCH_HEAD"
	}
	out, err := g.git(ctx, repo, "rev-parse", "--verify", target+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if sub != "" {
		if _, err := g.git(ctx, repo, "sparse-checkout", "set", "--cone", "--", sub); err != nil {
			return "", err
		}
	}
	if _, err := g.git(ctx, repo, "checkout", "-q", "--detach", commit); err != nil {
		return "", err
	}
	return commit, nil
}

// git runs one git command with the hardening of rule 7: no prompts, no system or global
// configuration, no protocol but https, no redirects, no submodules.
func (g *GitImporter) git(ctx context.Context, dir string, args ...string) (string, error) {
	pre := []string{
		"-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"-c", "http.followRedirects=false", "-c", "submodule.recurse=false",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=", "-c", "advice.detachedHead=false",
	}
	for _, c := range g.extraConfig {
		pre = append(pre, "-c", c)
	}
	run := g.Run
	if run == nil {
		run = execGit
	}
	return run(ctx, dir, append(pre, args...)...)
}

func execGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &out, max: 1 << 20}
	cmd.Stderr = &limitedBuffer{b: &errb, max: 64 << 10}
	if err := cmd.Run(); err != nil {
		// git's first error line: "fatal:" or "error:" if there is one, else the first line.
		first := ""
		for _, l := range strings.Split(errb.String(), "\n") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			if first == "" {
				first = l
			}
			if strings.HasPrefix(l, "fatal:") || strings.HasPrefix(l, "error:") {
				return "", errors.New(l)
			}
		}
		if first != "" {
			return "", errors.New(first)
		}
		return "", fmt.Errorf("git %s: %v", subcommand(args), err)
	}
	return out.String(), nil
}

// subcommand is the first argument that is not part of the "-c key=value" preamble.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return ""
}

func gitEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.TempDir(),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ASKPASS=true",
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// preflight asks the repository's info/refs with redirects followed under the allowlist
// (https only, hosts in Hosts) and returns the URL the request ended on, which git is then
// given with redirects off. A redirect off the allowlist is refused here.
func (g *GitImporter) preflight(ctx context.Context, u *url.URL) (string, error) {
	var c http.Client
	if g.Client != nil {
		c = *g.Client
	}
	if c.Timeout == 0 {
		c.Timeout = 20 * time.Second
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return &InputError{Path: "url", Msg: "redirects too many times"}
		}
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return &InputError{Path: "url", Msg: "redirects to something other than a plain https URL"}
		}
		if err := g.checkHost(req.URL.Hostname()); err != nil {
			return &InputError{Path: "url", Msg: "redirects off the allowed hosts: " + err.Error()}
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return "", &InputError{Path: "url", Msg: "is not a valid URL"}
	}
	req.Header.Set("User-Agent", "git/2.43")
	resp, err := c.Do(req)
	if err != nil {
		var ie *InputError
		if errors.As(err, &ie) {
			return "", ie
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("cannot reach the repository: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("repository not found or not public (HTTP %d)", resp.StatusCode)
	}
	final := *resp.Request.URL
	final.RawQuery = ""
	final.Path = strings.TrimSuffix(final.Path, "/info/refs")
	final.RawPath = ""
	return final.String(), nil
}

// skillDir is the directory of repo that holds the skill, refusing a path that goes
// through a symlink.
func skillDir(repo, sub string) (string, error) {
	if sub == "" {
		return repo, nil
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return "", &FormatError{Msg: "cannot read the repository"}
	}
	defer root.Close()
	cur := ""
	for _, seg := range strings.Split(sub, "/") {
		cur = path.Join(cur, seg)
		fi, err := root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return "", &FormatError{Msg: fmt.Sprintf("path %q does not exist in the repository", sub)}
		}
		if err != nil {
			return "", &FormatError{Msg: "cannot read the repository"}
		}
		if fi.Mode()&fs.ModeSymlink != 0 || (cur == sub && !fi.IsDir()) {
			return "", &FormatError{Msg: fmt.Sprintf("path %q is not a plain directory", sub)}
		}
	}
	return filepath.Join(repo, filepath.FromSlash(sub)), nil
}

// watchSize cancels ctx with a CapError once dir grows past max.
func watchSize(ctx context.Context, dir string, max int64, stop context.CancelCauseFunc) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if dirSize(dir) > max {
				stop(&CapError{Msg: fmt.Sprintf("the repository is over the %d MiB clone cap", max>>20)})
				return
			}
		}
	}
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
