package forgefake

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// The reads Chief platform control's wave 3 adds (DESIGN wave 3 rules 13 and 14):
// pipelines of one commit with their jobs and traces, a branch's commits, a merge
// request's diffs and whether it conflicts with its target.

func findPipeline(rp *repo, id string) *pipeline {
	for _, p := range rp.pipelines {
		if strconv.Itoa(p.id) == id {
			return p
		}
	}
	return nil
}

// jobTrace answers a job's log as plain text, as GitLab's trace endpoint does.
func (s *Server) jobTrace(w http.ResponseWriter, rp *repo, id string) {
	for _, p := range rp.pipelines {
		for _, j := range p.jobs {
			if strconv.Itoa(j.id) == id {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(j.log))
				return
			}
		}
	}
	fail(w, http.StatusNotFound, "404 Not found")
}

// commits lists a branch's commits newest first (ref_name, the default branch when
// absent), at most 100.
func (s *Server) commits(w http.ResponseWriter, r *http.Request, rp *repo) {
	ref := r.URL.Query().Get("ref_name")
	if ref == "" {
		ref = rp.defaultBranch
	}
	list := []map[string]any{}
	if sha := s.rev(rp, ref); sha != "" {
		out, err := s.runGit(rp.dir, "", "rev-list", "--max-count=100", sha)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, id := range strings.Fields(out) {
			list = append(list, map[string]any{"id": id, "short_id": id[:8]})
		}
	}
	lo, hi := page(w, r, len(list))
	writeJSON(w, http.StatusOK, list[lo:hi])
}

// conflicts reports whether the request's source branch conflicts with its target.
func (s *Server) conflicts(rp *repo, mr *mergeRequest) bool {
	src, tgt := s.rev(rp, mr.source), s.rev(rp, mr.target)
	if src == "" || tgt == "" {
		return false
	}
	_, err := s.runGit(rp.dir, "", "merge-tree", "--write-tree", "--name-only", tgt, src)
	var ge *gitError
	return errors.As(err, &ge) && ge.exitCode() == 1
}

// mrDiffs lists the request's changed files, from the merge base of its target and source
// to the source, each with its hunks as GitLab's diff field has them (from the first @@).
func (s *Server) mrDiffs(w http.ResponseWriter, r *http.Request, rp *repo, mr *mergeRequest) {
	src, tgt := s.mrSHA(rp, mr), s.rev(rp, mr.target)
	list := []map[string]any{}
	if src != "" && tgt != "" {
		mb, err := s.runGit(rp.dir, "", "merge-base", tgt, src)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		names, err := s.runGit(rp.dir, "", "diff-tree", "-r", "--no-renames", "--name-status", "-z", mb, src)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		fields := strings.Split(names, "\x00")
		for i := 0; i+1 < len(fields); i += 2 {
			status, path := fields[i], fields[i+1]
			patch, err := s.runGit(rp.dir, "", "diff", "--no-color", "--no-ext-diff", mb, src, "--", path)
			if err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			if at := strings.Index(patch, "\n@@"); at >= 0 {
				patch = patch[at+1:] + "\n"
			}
			list = append(list, map[string]any{"old_path": path, "new_path": path, "new_file": status == "A",
				"deleted_file": status == "D", "renamed_file": false, "diff": patch})
		}
	}
	lo, hi := page(w, r, len(list))
	writeJSON(w, http.StatusOK, list[lo:hi])
}
