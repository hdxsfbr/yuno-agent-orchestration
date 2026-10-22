// Package forgefake is the fake forge of the evals (DESIGN 5.29 rule 17): an in-memory
// subset of GitLab's REST v4 (projects, issues, labels, merge requests with their diffs,
// pipelines with their jobs and traces, branches, commits) plus Git smart HTTP over bare
// repositories in a temporary directory, so the pushes of open_merge_request are real. It
// accepts one fixed token and has control endpoints under /_forge/ that only the eval
// runner calls. It is started only under the compose profile eval and is never part of
// make run or the public deployment.
package forgefake

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAddr is where the role listens when FORGEFAKE_ADDR is not set.
const DefaultAddr = ":8090"

// Server is the forge. Its state is in memory behind one mutex; git runs on bare
// repositories under dir.
type Server struct {
	token string
	dir   string
	owned bool // dir was created here and Close removes it
	now   func() time.Time

	mu         sync.Mutex
	repos      map[string]*repo // by path_with_namespace
	byID       map[int]*repo
	nextRepo   int
	nextIssue  int
	nextMR     int
	nextPipe   int
	nextJob    int
	nextNote   int
	nextLabel  int
	gitTimeout time.Duration
}

// New returns a forge that accepts token. dir is where the bare repositories live; ""
// makes a temporary directory that Close removes.
func New(token, dir string) (*Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("forgefake: a token is required")
	}
	s := &Server{token: token, dir: dir, now: func() time.Time { return time.Now().UTC() },
		repos: map[string]*repo{}, byID: map[int]*repo{}, gitTimeout: time.Minute}
	if dir == "" {
		d, err := os.MkdirTemp("", "forgefake-")
		if err != nil {
			return nil, err
		}
		s.dir, s.owned = d, true
	}
	return s, nil
}

// Close removes the temporary directory New made.
func (s *Server) Close() error {
	if s.owned {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// Run serves the forge on addr until ctx ends. The token comes from FORGEFAKE_TOKEN.
func Run(ctx context.Context, addr string, log *slog.Logger) error {
	s, err := New(os.Getenv("FORGEFAKE_TOKEN"), "")
	if err != nil {
		return fmt.Errorf("%w (set FORGEFAKE_TOKEN)", err)
	}
	defer s.Close()
	if addr == "" {
		addr = DefaultAddr
	}
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("forgefake listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// ServeHTTP routes a request: /healthz (open), /api/v4/ (the REST subset), /_forge/ (the
// control endpoints) and <path>.git/ (Git smart HTTP). Everything but /healthz needs the
// token.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	switch {
	case p == "/healthz":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case strings.HasPrefix(p, "/api/v4/"):
		if !s.apiAuthorized(r) {
			fail(w, http.StatusUnauthorized, "401 Unauthorized")
			return
		}
		s.rest(w, r, splitPath(strings.TrimPrefix(p, "/api/v4/")))
	case strings.HasPrefix(p, "/_forge/"):
		if !s.apiAuthorized(r) {
			fail(w, http.StatusUnauthorized, "401 Unauthorized")
			return
		}
		s.control(w, r, splitPath(strings.TrimPrefix(p, "/_forge/")))
	case strings.Contains(p, ".git/"):
		s.gitHTTP(w, r)
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// apiAuthorized accepts the token as PRIVATE-TOKEN or as a bearer token.
func (s *Server) apiAuthorized(r *http.Request) bool {
	if s.tokenOK(r.Header.Get("PRIVATE-TOKEN")) {
		return true
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return s.tokenOK(v)
	}
	return false
}

func (s *Server) tokenOK(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// splitPath splits an escaped path into unescaped segments, so a project path sent as one
// encoded segment (group%2Fname) arrives whole.
func splitPath(escaped string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(escaped, "/"), "/") {
		if seg == "" {
			continue
		}
		if u, err := url.PathUnescape(seg); err == nil {
			seg = u
		}
		out = append(out, seg)
	}
	return out
}

// base is the forge's own URL as the caller reached it, for web_url fields.
func base(r *http.Request) string {
	return "http://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail writes GitLab's error shape: {"message": text}.
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message})
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func stampPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := stamp(*t)
	return &s
}

// args are the parameters of a write: the JSON body, with the query string filling what
// the body does not name.
type args map[string]any

func readArgs(r *http.Request) (args, error) {
	a := args{}
	if r.Body != nil && r.ContentLength != 0 {
		raw := http.MaxBytesReader(nil, r.Body, 4<<20)
		defer raw.Close()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			dec := json.NewDecoder(raw)
			if err := dec.Decode(&a); err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("invalid JSON body: %w", err)
			}
		} else if err := r.ParseForm(); err == nil {
			for k, v := range r.PostForm {
				a[k] = v[0]
			}
		}
	}
	for k, v := range r.URL.Query() {
		if _, ok := a[k]; !ok && len(v) > 0 {
			a[k] = v[0]
		}
	}
	return a, nil
}

func (a args) str(k string) (string, bool) {
	v, ok := a[k]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func (a args) boolean(k string) bool {
	v, _ := a.str(k)
	return v == "true"
}

// labels reads a label list sent as "a,b" or as ["a","b"].
func (a args) labels(k string) ([]string, bool) {
	v, ok := a[k]
	if !ok {
		return nil, false
	}
	var out []string
	switch t := v.(type) {
	case string:
		out = splitLabels(t)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, splitLabels(s)...)
			}
		}
	}
	return out, true
}

func splitLabels(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// page applies GitLab's pagination (page, per_page) to a list of n entries and writes the
// X-* headers; it returns the slice bounds.
func page(w http.ResponseWriter, r *http.Request, n int) (lo, hi int) {
	per, pg := 20, 1
	if v, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && v > 0 {
		per = min(v, 100)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		pg = v
	}
	pages := (n + per - 1) / per
	h := w.Header()
	h.Set("X-Total", strconv.Itoa(n))
	h.Set("X-Total-Pages", strconv.Itoa(pages))
	h.Set("X-Per-Page", strconv.Itoa(per))
	h.Set("X-Page", strconv.Itoa(pg))
	if pg < pages {
		h.Set("X-Next-Page", strconv.Itoa(pg+1))
	} else {
		h.Set("X-Next-Page", "")
	}
	if pg > 1 {
		h.Set("X-Prev-Page", strconv.Itoa(pg-1))
	} else {
		h.Set("X-Prev-Page", "")
	}
	lo = min((pg-1)*per, n)
	hi = min(lo+per, n)
	return lo, hi
}

// runGit runs git with a fixed identity in dir and returns its stdout. A failure carries
// the tail of stderr.
func (s *Server) runGit(dir string, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = s.gitEnv()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), &gitError{args: args, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return strings.TrimSpace(out.String()), nil
}

func (s *Server) gitEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.dir, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Forge", "GIT_AUTHOR_EMAIL=forge@localhost", "GIT_COMMITTER_NAME=Forge", "GIT_COMMITTER_EMAIL=forge@localhost"}
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *gitError) exitCode() int {
	var ee *exec.ExitError
	if errors.As(e.err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
