package skillbundle

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	g := &GitImporter{Hosts: []string{"github.com", "gitlab.com"}}
	for _, ok := range []string{"https://github.com/o/r", "https://github.com/o/r.git", "https://GitLab.com/g/sub/r/", "https://github.com:443/o/r"} {
		if _, err := g.ValidateURL(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "github.com/o/r", "http://github.com/o/r", "git://github.com/o/r", "ssh://git@github.com/o/r", "file:///etc",
		"https://user@github.com/o/r", "https://user:pw@github.com/o/r", "https://evil.example/o/r",
		"https://github.com.evil.example/o/r", "https://github.com./o/r", "https://evil.example@github.com/o/r",
		"https://github.com:8443/o/r", "https://github.com/o/r?x=1", "https://github.com/o/r#frag", "https://github.com", "https://github.com/",
		"https://github.com/o/../r", "https://github.com/o r", "https://127.0.0.1/o/r", "https://github.com\\@evil.example/o/r",
	} {
		_, err := g.ValidateURL(bad)
		if !isInput(err, "url") {
			t.Errorf("%q: err = %v, want an InputError on url", bad, err)
		}
	}
}

func TestImportArgumentValidation(t *testing.T) {
	g := &GitImporter{Hosts: []string{"github.com"}, skipPreflight: true, Run: func(context.Context, string, ...string) (string, error) {
		t.Fatal("git must not run")
		return "", nil
	}}
	for _, r := range []GitRequest{
		{URL: "https://github.com/o/r", Ref: "--upload-pack=x"},
		{URL: "https://github.com/o/r", Ref: "a..b"},
		{URL: "https://github.com/o/r", Ref: "a b"},
	} {
		if _, err := g.Import(context.Background(), r); !isInput(err, "ref") {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for _, p := range []string{"../x", "/abs", "-flag", "a/../b", ".git/x", "a b"} {
		if _, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r", Path: p}); !isInput(err, "path") {
			t.Errorf("path %q: %v", p, err)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(r *http.Request, code int, loc string) *http.Response {
	h := http.Header{}
	if loc != "" {
		h.Set("Location", loc)
	}
	return &http.Response{StatusCode: code, Header: h, Body: http.NoBody, Request: r}
}

// T5.27.8: a redirect off the allowlist is refused before git runs; one within it is
// followed, and git is given the final URL.
func TestPreflightRedirects(t *testing.T) {
	ran := 0
	var cloned string
	g := &GitImporter{
		Hosts: []string{"github.com"},
		Run: func(ctx context.Context, dir string, args ...string) (string, error) {
			ran++
			for i, a := range args {
				if a == "clone" {
					cloned = args[len(args)-2]
					_ = os.MkdirAll(args[len(args)-1], 0o755)
					_ = i
				}
			}
			return strings.Repeat("a", 40) + "\n", nil
		},
	}
	redirect := func(to string) *http.Client {
		return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "github.com" && r.URL.Path == "/o/r/info/refs" {
				return resp(r, http.StatusFound, to), nil
			}
			return resp(r, http.StatusOK, ""), nil
		})}
	}
	for _, to := range []string{
		"https://evil.example/o/r/info/refs?service=git-upload-pack",
		"http://github.com/o/r/info/refs?service=git-upload-pack",
		"https://user:pw@github.com/o/r/info/refs",
		"https://github.com.evil.example/o/r/info/refs",
	} {
		g.Client = redirect(to)
		_, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"})
		if !isInput(err, "url") {
			t.Errorf("redirect to %s: err = %v", to, err)
		}
	}
	if ran != 0 {
		t.Fatalf("git ran %d times after refused redirects", ran)
	}
	// Within the allowlist: followed, and git gets the final URL.
	g.Client = redirect("https://github.com/o/renamed/info/refs?service=git-upload-pack")
	_, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"})
	if cloned != "https://github.com/o/renamed" {
		t.Fatalf("git was given %q (err %v)", cloned, err)
	}
	// An unreachable or private repository is a plain error, not a prompt.
	g.Client = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) { return resp(r, http.StatusUnauthorized, ""), nil })}
	if _, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("401: %v", err)
	}
}

// The first --filter failure retries without the filter.
func TestImportFilterFallback(t *testing.T) {
	var clones [][]string
	g := &GitImporter{
		Hosts: []string{"github.com"}, skipPreflight: true, TempDir: t.TempDir(),
		Run: func(ctx context.Context, dir string, args ...string) (string, error) {
			sub := subcommand(args)
			switch sub {
			case "clone":
				clones = append(clones, args)
				if strings.Contains(strings.Join(args, " "), "--filter") {
					return "", errors.New("fatal: filtering not recognized by server")
				}
				dest := args[len(args)-1]
				_ = os.MkdirAll(dest, 0o755)
				_ = os.WriteFile(filepath.Join(dest, "SKILL.md"), []byte(goodMD), 0o644)
			case "rev-parse":
				return strings.Repeat("c", 40) + "\n", nil
			}
			return "", nil
		},
	}
	res, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"})
	if err != nil || len(clones) != 2 || res.Commit != strings.Repeat("c", 40) || res.Bundle.Name != "pdf-reports" {
		t.Fatalf("res %+v err %v clones %d", res, err, len(clones))
	}
	if res.URL != "https://github.com/o/r" {
		t.Fatalf("url = %s", res.URL)
	}
}

func TestImportHardeningFlags(t *testing.T) {
	var seen []string
	g := &GitImporter{Hosts: []string{"github.com"}, skipPreflight: true, TempDir: t.TempDir(),
		Run: func(ctx context.Context, dir string, args ...string) (string, error) {
			if subcommand(args) == "clone" {
				seen = args
				return "", errors.New("fatal: stop here")
			}
			return "", nil
		}}
	_, _ = g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r", Ref: "v1"})
	joined := " " + strings.Join(seen, " ") + " "
	for _, want := range []string{" protocol.allow=never ", " protocol.https.allow=always ", " http.followRedirects=false ", " --depth 1 ", " --no-checkout ", " --branch v1 ", " -- https://github.com/o/r "} {
		if !strings.Contains(joined, want) {
			t.Errorf("clone args lack %q: %s", want, joined)
		}
	}
}

func TestImportTimeoutAndCloneCap(t *testing.T) {
	block := func(ctx context.Context, dir string, args ...string) (string, error) {
		if subcommand(args) == "clone" {
			dest := args[len(args)-1]
			_ = os.MkdirAll(dest, 0o755)
			_ = os.WriteFile(filepath.Join(dest, "big.pack"), make([]byte, 3<<20), 0o644)
		}
		<-ctx.Done()
		return "", errors.New("signal: killed")
	}
	g := &GitImporter{Hosts: []string{"github.com"}, skipPreflight: true, TempDir: t.TempDir(), Run: block, Timeout: 100 * time.Millisecond}
	if _, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"}); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("timeout: %v", err)
	}
	g = &GitImporter{Hosts: []string{"github.com"}, skipPreflight: true, TempDir: t.TempDir(), Run: block, MaxCloneBytes: 1 << 20, Timeout: 5 * time.Second}
	_, err := g.Import(context.Background(), GitRequest{URL: "https://github.com/o/r"})
	if !isCap(err) {
		t.Errorf("clone cap: %v", err)
	}
	if left, _ := os.ReadDir(g.TempDir); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
}

func gitOK(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed in this environment; the real-git import test is skipped")
	}
}

// A real git against a local bare repository: sparse checkout of a sub-path, modes, the
// resolved commit, a commit as ref, and the temp directory removed.
func TestImportRealGit(t *testing.T) {
	gitOK(t)
	work := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(work, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
	}
	run(work, "init", "-q", "-b", "main")
	write("README.md", "root", 0o644)
	write("skills/demo/SKILL.md", goodMD, 0o644)
	write("skills/demo/scripts/run.sh", "#!/bin/sh\n", 0o755)
	write("skills/other/SKILL.md", "---\nname: other\n---\nx", 0o644)
	write("skills/linked/SKILL.md", goodMD, 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(work, "skills/linked/passwd")); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "one")
	first := run(work, "rev-parse", "HEAD")
	write("skills/demo/NEW.txt", "new", 0o644)
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "two")
	head := run(work, "rev-parse", "HEAD")
	run(work, "branch", "feature", first)
	bare := filepath.Join(t.TempDir(), "r.git")
	run(work, "clone", "-q", "--bare", work, bare)

	tmp := t.TempDir()
	g := &GitImporter{Hosts: []string{"github.com"}, skipPreflight: true, TempDir: tmp,
		rewrite:     func(string) string { return "file://" + bare },
		extraConfig: []string{"protocol.file.allow=always"}}
	ctx := context.Background()

	res, err := g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Path: "skills/demo"})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]int{}
	for _, f := range res.Bundle.Files {
		paths[f.Path] = f.Mode
	}
	if res.Commit != head || res.Bundle.Name != "pdf-reports" || len(paths) != 2 || paths["scripts/run.sh"] != 0o755 || paths["NEW.txt"] != 0o644 {
		t.Fatalf("commit %s (want %s), bundle %+v", res.Commit, head, paths)
	}

	// A branch, and a commit sha, as ref.
	res, err = g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Ref: "feature", Path: "skills/demo"})
	if err != nil || res.Commit != first || len(res.Bundle.Files) != 1 {
		t.Fatalf("branch: %v %+v", err, res)
	}
	res, err = g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Ref: first, Path: "skills/other"})
	if err != nil || res.Commit != first || res.Bundle.Name != "other" {
		t.Fatalf("sha: %v %+v", err, res)
	}

	// Errors: a path that is not in the repository, a symlink in the skill, an unknown ref.
	if _, err := g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Path: "skills/missing"}); !isFormat(err) {
		t.Errorf("missing path: %v", err)
	}
	if _, err := g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Path: "skills/linked"}); !isFormat(err) {
		t.Errorf("symlink: %v", err)
	}
	_, err = g.Import(ctx, GitRequest{URL: "https://github.com/o/r", Ref: "nope"})
	if err == nil || isFormat(err) || strings.Contains(err.Error(), "\n") {
		t.Errorf("unknown ref: %v", err)
	}
	// The root of the repository holds no SKILL.md.
	if _, err := g.Import(ctx, GitRequest{URL: "https://github.com/o/r"}); !isFormat(err) {
		t.Errorf("root: %v", err)
	}

	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
}
