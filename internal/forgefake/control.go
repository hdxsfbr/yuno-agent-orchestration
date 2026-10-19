package forgefake

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// control serves /_forge/<segs>, the endpoints only the eval runner calls (DESIGN 5.29
// rule 17):
//
//	POST /_forge/repos                          {path, default_branch}  a project with a README commit
//	POST /_forge/repos/<path>/merge-open        {iid?}                  merge the open, non-draft requests
//	POST /_forge/repos/<path>/pipeline          {state, ref?}           record a pipeline now
//	GET  /_forge/repos/<path>/issues                                    every issue with labels and state
func (s *Server) control(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) == 0 || segs[0] != "repos" {
		fail(w, http.StatusNotFound, "404 Not Found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(segs) == 1 {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
			return
		}
		s.controlCreate(w, r)
		return
	}
	action := segs[len(segs)-1]
	rp := s.repos[strings.Join(segs[1:len(segs)-1], "/")]
	if rp == nil {
		fail(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	switch {
	case action == "merge-open" && r.Method == http.MethodPost:
		s.controlMergeOpen(w, r, rp)
	case action == "pipeline" && r.Method == http.MethodPost:
		s.controlPipeline(w, r, rp)
	case action == "issues" && r.Method == http.MethodGet:
		out := make([]map[string]any, 0, len(rp.issues))
		for _, i := range rp.issues {
			out = append(out, s.issueJSON(r, rp, i))
		}
		writeJSON(w, http.StatusOK, out)
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func (s *Server) controlCreate(w http.ResponseWriter, r *http.Request) {
	a, err := readArgs(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	path, _ := a.str("path")
	branch, _ := a.str("default_branch")
	rp, err := s.createRepo(path, branch)
	switch {
	case errors.Is(err, errExists):
		fail(w, http.StatusConflict, "the repository exists")
	case err != nil:
		fail(w, http.StatusBadRequest, err.Error())
	default:
		out := s.projectJSON(r, rp)
		out["head"] = s.rev(rp, rp.defaultBranch)
		writeJSON(w, http.StatusCreated, out)
	}
}

// controlMergeOpen merges every open, non-draft request (or the one named by iid) in
// iid order and returns the merged ones. A request that cannot be merged stops the run
// with 409 naming it; the ones merged before it stay merged.
func (s *Server) controlMergeOpen(w http.ResponseWriter, r *http.Request, rp *repo) {
	a, err := readArgs(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	only := 0
	if v, ok := a.str("iid"); ok {
		if only, err = strconv.Atoi(v); err != nil {
			fail(w, http.StatusBadRequest, "iid is not a number")
			return
		}
	}
	list := append([]*mergeRequest(nil), rp.mrs...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].iid < list[j].iid })
	merged := []map[string]any{}
	for _, mr := range list {
		if mr.state != "opened" || (only != 0 && mr.iid != only) {
			continue
		}
		if isDraft(mr.title) {
			if only != 0 {
				fail(w, http.StatusConflict, fmt.Sprintf("merge request !%d is a draft", mr.iid))
				return
			}
			continue
		}
		if err := s.mergeMR(rp, mr, false); err != nil {
			fail(w, http.StatusConflict, fmt.Sprintf("merge request !%d: %v", mr.iid, err))
			return
		}
		merged = append(merged, s.mrJSON(r, rp, mr))
	}
	if only != 0 && len(merged) == 0 {
		fail(w, http.StatusNotFound, fmt.Sprintf("no open merge request !%d", only))
		return
	}
	writeJSON(w, http.StatusOK, merged)
}

// controlPipeline records a pipeline at the head of ref (the default branch unless ref is
// given) with the state and the time now.
func (s *Server) controlPipeline(w http.ResponseWriter, r *http.Request, rp *repo) {
	a, err := readArgs(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	state, _ := a.str("state")
	status, ok := gitlabStatus[state]
	if !ok {
		fail(w, http.StatusBadRequest, "state must be passed, failed, running, pending or canceled")
		return
	}
	ref, _ := a.str("ref")
	if ref == "" {
		ref = rp.defaultBranch
	}
	sha := s.rev(rp, ref)
	if sha == "" {
		fail(w, http.StatusNotFound, "no branch "+ref)
		return
	}
	s.nextPipe++
	p := &pipeline{id: s.nextPipe, ref: ref, sha: sha, status: status, createdAt: s.now()}
	rp.pipelines = append(rp.pipelines, p)
	writeJSON(w, http.StatusCreated, s.pipelineJSON(r, rp, p))
}

// ---- Git smart HTTP -----------------------------------------------------------------

// gitHTTP serves <path>.git/info/refs, git-upload-pack and git-receive-pack over the bare
// repository, with the token as the basic-auth password (any user name), which is how
// internal/project/git.go authenticates. The two services run in stateless-rpc mode, the
// same commands git http-backend runs, so no CGI binary is needed.
func (s *Server) gitHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pass, ok := r.BasicAuth(); !ok || !s.tokenOK(pass) {
		w.Header().Set("WWW-Authenticate", `Basic realm="forgefake"`)
		fail(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	i := strings.Index(p, ".git/")
	if i < 0 {
		fail(w, http.StatusNotFound, "404 Not Found")
		return
	}
	s.mu.Lock()
	rp := s.repos[p[:i]]
	s.mu.Unlock()
	if rp == nil {
		fail(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	rest := p[i+len(".git/"):]
	v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	env := append(s.gitEnv(), "GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			fail(w, http.StatusForbidden, "only the smart protocol is served")
			return
		}
		w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if !v2 {
			line := "# service=" + service + "\n"
			_, _ = fmt.Fprintf(w, "%04x%s0000", len(line)+4, line)
		}
		s.serveGit(w, nil, env, strings.TrimPrefix(service, "git-"), "--stateless-rpc", "--advertise-refs", rp.dir)
	case (rest == "git-upload-pack" || rest == "git-receive-pack") && r.Method == http.MethodPost:
		if r.Header.Get("Content-Type") != "application/x-"+rest+"-request" {
			fail(w, http.StatusUnsupportedMediaType, "unexpected content type")
			return
		}
		var body io.Reader = http.MaxBytesReader(w, r.Body, 512<<20)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(body)
			if err != nil {
				fail(w, http.StatusBadRequest, "bad gzip body")
				return
			}
			defer zr.Close()
			body = zr
		}
		w.Header().Set("Content-Type", "application/x-"+rest+"-result")
		w.Header().Set("Cache-Control", "no-cache")
		s.serveGit(w, body, env, strings.TrimPrefix(rest, "git-"), "--stateless-rpc", rp.dir)
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// serveGit runs one git service and streams its output into the response. A failure
// after the response has started leaves the client a broken stream, which it reports.
func (s *Server) serveGit(w http.ResponseWriter, stdin io.Reader, env []string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = w
	_ = cmd.Run()
}
