package forgefake

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// repo is one project: its bare repository and everything the REST subset keeps about it.
type repo struct {
	id            int
	path          string // path_with_namespace
	defaultBranch string
	dir           string // the bare repository

	issues    []*issue
	mrs       []*mergeRequest
	pipelines []*pipeline
	labels    []*label
}

type label struct {
	id          int
	name        string
	color       string
	description string
}

type note struct {
	id        int
	body      string
	createdAt time.Time
}

type issue struct {
	id, iid     int
	title, body string
	state       string // opened | closed
	labels      []string
	notes       []note
	createdAt   time.Time
	updatedAt   time.Time
	closedAt    *time.Time
}

type mergeRequest struct {
	id, iid     int
	title, body string
	state       string // opened | merged | closed
	source      string
	target      string
	labels      []string
	notes       []note
	removeSrc   bool
	createdAt   time.Time
	updatedAt   time.Time
	mergedAt    *time.Time
	closedAt    *time.Time
	headSHA     string // the source head when merged (the branch may be gone)
	mergeSHA    string
}

type pipeline struct {
	id        int
	ref       string
	sha       string
	status    string
	createdAt time.Time
	jobs      []*job
}

// job is one job of a pipeline, with the log its trace answers.
type job struct {
	id     int
	name   string
	stage  string
	status string
	log    string
}

var (
	// pathPattern keeps a repository path to what a directory name and a URL both carry.
	pathPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)+$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// errExists is createRepo's answer for a path that is taken.
var errExists = errors.New("the repository exists")

// createRepo makes a bare repository with an initial commit (a README) on branch. The
// caller holds s.mu.
func (s *Server) createRepo(path, branch string) (*repo, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !pathPattern.MatchString(path) || strings.Contains(path, "..") {
		return nil, fmt.Errorf("path %q is not a namespace/name path", path)
	}
	if branch == "" {
		branch = "main"
	}
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "..") {
		return nil, fmt.Errorf("default_branch %q is not a branch name", branch)
	}
	if _, ok := s.repos[path]; ok {
		return nil, errExists
	}
	dir := filepath.Join(s.dir, filepath.FromSlash(path)+".git")
	if _, err := s.runGit(s.dir, "", "init", "--quiet", "--bare", "--initial-branch="+branch, dir); err != nil {
		return nil, err
	}
	blob, err := s.runGit(dir, "# "+path+"\n", "hash-object", "-w", "--stdin")
	if err != nil {
		return nil, err
	}
	tree, err := s.runGit(dir, "100644 blob "+blob+"\tREADME.md\n", "mktree")
	if err != nil {
		return nil, err
	}
	commit, err := s.runGit(dir, "", "commit-tree", tree, "-m", "Initial commit")
	if err != nil {
		return nil, err
	}
	if _, err := s.runGit(dir, "", "update-ref", "refs/heads/"+branch, commit); err != nil {
		return nil, err
	}
	s.nextRepo++
	rp := &repo{id: s.nextRepo, path: path, defaultBranch: branch, dir: dir}
	s.repos[path] = rp
	s.byID[rp.id] = rp
	return rp, nil
}

// rev is the commit a branch names, "" when there is no such branch.
func (s *Server) rev(rp *repo, branch string) string {
	out, err := s.runGit(rp.dir, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}

// isAncestor reports whether commit a is an ancestor of (or is) b.
func (s *Server) isAncestor(rp *repo, a, b string) (bool, error) {
	_, err := s.runGit(rp.dir, "", "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var ge *gitError
	if errors.As(err, &ge) && ge.exitCode() == 1 {
		return false, nil
	}
	return false, err
}

// Merge refusals, mapped to GitLab's statuses by the REST handler.
var (
	errNotOpen  = errors.New("the merge request is not open")
	errDraft    = errors.New("draft merge requests cannot be merged")
	errConflict = errors.New("the branches conflict")
	errNoBranch = errors.New("the source branch no longer exists")
	errNoTarget = errors.New("the target branch no longer exists")
)

// mergeMR merges the request's source into its target in the bare repository: a
// fast-forward when the target is behind the source, else a merge commit (git merge-tree
// builds the tree, so no working tree is needed). The request becomes merged with
// merged_at; the source branch is deleted when it asks to be removed or remove is set.
// The caller holds s.mu.
func (s *Server) mergeMR(rp *repo, mr *mergeRequest, remove bool) error {
	if mr.state != "opened" {
		return errNotOpen
	}
	if isDraft(mr.title) {
		return errDraft
	}
	src, tgt := s.rev(rp, mr.source), s.rev(rp, mr.target)
	if src == "" {
		return errNoBranch
	}
	if tgt == "" {
		return errNoTarget
	}
	merged := tgt
	switch already, err := s.isAncestor(rp, src, tgt); {
	case err != nil:
		return err
	case already:
		// Nothing to add: the target has the source's commits.
	default:
		ff, err := s.isAncestor(rp, tgt, src)
		if err != nil {
			return err
		}
		if ff {
			merged = src
		} else {
			tree, err := s.runGit(rp.dir, "", "merge-tree", "--write-tree", tgt, src)
			if err != nil {
				var ge *gitError
				if errors.As(err, &ge) && ge.exitCode() == 1 {
					return errConflict
				}
				return err
			}
			tree = strings.Fields(tree)[0]
			msg := fmt.Sprintf("Merge branch '%s' into '%s'\n\nSee merge request !%d", mr.source, mr.target, mr.iid)
			merged, err = s.runGit(rp.dir, "", "commit-tree", tree, "-p", tgt, "-p", src, "-m", msg)
			if err != nil {
				return err
			}
		}
		if _, err := s.runGit(rp.dir, "", "update-ref", "refs/heads/"+mr.target, merged, tgt); err != nil {
			return err
		}
	}
	now := s.now()
	mr.state, mr.mergedAt, mr.updatedAt = "merged", &now, now
	mr.headSHA, mr.mergeSHA = src, merged
	if (remove || mr.removeSrc) && mr.source != rp.defaultBranch {
		if _, err := s.runGit(rp.dir, "", "update-ref", "-d", "refs/heads/"+mr.source); err != nil {
			return err
		}
	}
	return nil
}

// isDraft reports a title GitLab reads as a draft.
func isDraft(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	for _, p := range []string{"draft:", "[draft]", "(draft)", "wip:", "[wip]"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// lookup finds a project by its encoded path or its numeric id.
func (s *Server) lookup(id string) *repo {
	if rp, ok := s.repos[id]; ok {
		return rp
	}
	var n int
	if _, err := fmt.Sscanf(id, "%d", &n); err == nil && fmt.Sprint(n) == id {
		return s.byID[n]
	}
	return nil
}

// ensureLabel creates a project label that does not exist yet (GitLab does the same for
// the labels of a new issue).
func (s *Server) ensureLabel(rp *repo, name, color, desc string) (*label, bool) {
	for _, l := range rp.labels {
		if strings.EqualFold(l.name, name) {
			return l, false
		}
	}
	if color == "" {
		color = "#428BCA"
	}
	s.nextLabel++
	l := &label{id: s.nextLabel, name: name, color: color, description: desc}
	rp.labels = append(rp.labels, l)
	return l, true
}

// applyLabels computes a label list: replaced by set when given, then extended by add and
// reduced by remove (case-insensitive, as GitLab compares them), every label ensured.
func (s *Server) applyLabels(rp *repo, cur []string, a args) []string {
	if set, ok := a.labels("labels"); ok {
		cur = nil
		for _, l := range set {
			cur = withLabel(cur, l)
		}
	}
	if add, ok := a.labels("add_labels"); ok {
		for _, l := range add {
			cur = withLabel(cur, l)
		}
	}
	if rm, ok := a.labels("remove_labels"); ok {
		for _, l := range rm {
			cur = withoutLabel(cur, l)
		}
	}
	for _, l := range cur {
		s.ensureLabel(rp, l, "", "")
	}
	return cur
}

func withLabel(list []string, l string) []string {
	for _, e := range list {
		if strings.EqualFold(e, l) {
			return list
		}
	}
	return append(list, l)
}

func withoutLabel(list []string, l string) []string {
	out := list[:0:0]
	for _, e := range list {
		if !strings.EqualFold(e, l) {
			out = append(out, e)
		}
	}
	return out
}

func hasLabel(list []string, l string) bool {
	for _, e := range list {
		if strings.EqualFold(e, l) {
			return true
		}
	}
	return false
}

// labelFilter is GitLab's labels= filter: every named label, or None (no label) or Any.
func labelFilter(have []string, filter []string) bool {
	if len(filter) == 1 {
		switch strings.ToLower(filter[0]) {
		case "none":
			return len(have) == 0
		case "any":
			return len(have) > 0
		}
	}
	for _, f := range filter {
		if !hasLabel(have, f) {
			return false
		}
	}
	return true
}
