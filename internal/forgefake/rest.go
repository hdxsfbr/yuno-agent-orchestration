package forgefake

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// rest serves /api/v4/<segs>. Every handler runs under the forge's mutex.
func (s *Server) rest(w http.ResponseWriter, r *http.Request, segs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(segs) == 1 && segs[0] == "user" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1, "username": "forge", "name": "Forge"})
		return
	}
	if len(segs) < 2 || segs[0] != "projects" {
		fail(w, http.StatusNotFound, "404 Not Found")
		return
	}
	rp := s.lookup(segs[1])
	if rp == nil {
		fail(w, http.StatusNotFound, "404 Project Not Found")
		return
	}
	rest := segs[2:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.projectJSON(r, rp))
	case len(rest) == 0:
		fail(w, http.StatusMethodNotAllowed, "405 Method Not Allowed")
	case rest[0] == "issues":
		s.issues(w, r, rp, rest[1:])
	case rest[0] == "labels":
		s.labels(w, r, rp, rest[1:])
	case rest[0] == "merge_requests":
		s.mergeRequests(w, r, rp, rest[1:])
	case rest[0] == "pipelines":
		s.pipelinesAPI(w, r, rp, rest[1:])
	case rest[0] == "repository" && len(rest) >= 2 && rest[1] == "branches":
		s.branches(w, r, rp, rest[2:])
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func (s *Server) projectJSON(r *http.Request, rp *repo) map[string]any {
	name := rp.path[strings.LastIndex(rp.path, "/")+1:]
	return map[string]any{
		"id": rp.id, "name": name, "path": name, "path_with_namespace": rp.path,
		"default_branch": rp.defaultBranch, "web_url": base(r) + "/" + rp.path,
		"http_url_to_repo": base(r) + "/" + rp.path + ".git",
		"permissions":      map[string]any{"project_access": map[string]any{"access_level": 40}},
	}
}

func noteJSON(n note) map[string]any {
	return map[string]any{"id": n.id, "body": n.body, "system": false, "created_at": stamp(n.createdAt),
		"author": map[string]any{"username": "forge", "name": "Forge"}, "resolvable": false, "resolved": false}
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request, notes *[]note) {
	a, err := readArgs(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	body, _ := a.str("body")
	if strings.TrimSpace(body) == "" {
		fail(w, http.StatusBadRequest, "body is missing")
		return
	}
	s.nextNote++
	n := note{id: s.nextNote, body: body, createdAt: s.now()}
	*notes = append(*notes, n)
	writeJSON(w, http.StatusCreated, noteJSON(n))
}

func listNotes(w http.ResponseWriter, notes []note) {
	out := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		out = append(out, noteJSON(n))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- issues -------------------------------------------------------------------------

func (s *Server) issueJSON(r *http.Request, rp *repo, i *issue) map[string]any {
	labels := i.labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id": i.id, "iid": i.iid, "project_id": rp.id, "title": i.title, "description": i.body,
		"state": i.state, "labels": labels, "confidential": false,
		"created_at": stamp(i.createdAt), "updated_at": stamp(i.updatedAt), "closed_at": stampPtr(i.closedAt),
		"web_url": fmt.Sprintf("%s/%s/-/issues/%d", base(r), rp.path, i.iid),
		"author":  map[string]any{"username": "forge", "name": "Forge"},
	}
}

func findIssue(rp *repo, iid string) *issue {
	n, err := strconv.Atoi(iid)
	if err != nil {
		return nil
	}
	for _, i := range rp.issues {
		if i.iid == n {
			return i
		}
	}
	return nil
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request, rp *repo, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.listIssues(w, r, rp)
	case len(rest) == 0 && r.Method == http.MethodPost:
		a, err := readArgs(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		title, _ := a.str("title")
		if strings.TrimSpace(title) == "" {
			fail(w, http.StatusBadRequest, "title is missing")
			return
		}
		body, _ := a.str("description")
		now := s.now()
		s.nextIssue++
		i := &issue{id: s.nextIssue, iid: len(rp.issues) + 1, title: title, body: body, state: "opened", createdAt: now, updatedAt: now}
		i.labels = s.applyLabels(rp, nil, a)
		rp.issues = append(rp.issues, i)
		writeJSON(w, http.StatusCreated, s.issueJSON(r, rp, i))
	case len(rest) >= 1:
		i := findIssue(rp, rest[0])
		if i == nil {
			fail(w, http.StatusNotFound, "404 Not found")
			return
		}
		s.issue(w, r, rp, i, rest[1:])
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, rp *repo, i *issue, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.issueJSON(r, rp, i))
	case len(rest) == 0 && r.Method == http.MethodPut:
		a, err := readArgs(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if v, ok := a.str("title"); ok && strings.TrimSpace(v) != "" {
			i.title = v
		}
		if v, ok := a.str("description"); ok {
			i.body = v
		}
		i.labels = s.applyLabels(rp, i.labels, a)
		now := s.now()
		switch ev, _ := a.str("state_event"); ev {
		case "close":
			if i.state != "closed" {
				i.state, i.closedAt = "closed", &now
			}
		case "reopen":
			i.state, i.closedAt = "opened", nil
		case "":
		default:
			fail(w, http.StatusBadRequest, "state_event does not have a valid value")
			return
		}
		i.updatedAt = now
		writeJSON(w, http.StatusOK, s.issueJSON(r, rp, i))
	case len(rest) == 1 && rest[0] == "notes" && r.Method == http.MethodPost:
		s.addNote(w, r, &i.notes)
	case len(rest) == 1 && rest[0] == "notes" && r.Method == http.MethodGet:
		listNotes(w, i.notes)
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request, rp *repo) {
	q := r.URL.Query()
	filter := splitLabels(q.Get("labels"))
	state := q.Get("state")
	var list []*issue
	for _, i := range rp.issues {
		if len(filter) > 0 && !labelFilter(i.labels, filter) {
			continue
		}
		if state == "opened" && i.state != "opened" || state == "closed" && i.state != "closed" {
			continue
		}
		if search := q.Get("search"); search != "" && !strings.Contains(strings.ToLower(i.title+"\n"+i.body), strings.ToLower(search)) {
			continue
		}
		list = append(list, i)
	}
	desc := q.Get("sort") != "asc"
	byUpdated := q.Get("order_by") == "updated_at"
	sort.SliceStable(list, func(a, b int) bool {
		less := list[a].iid < list[b].iid
		if byUpdated && !list[a].updatedAt.Equal(list[b].updatedAt) {
			less = list[a].updatedAt.Before(list[b].updatedAt)
		}
		if desc {
			return !less
		}
		return less
	})
	lo, hi := page(w, r, len(list))
	out := make([]map[string]any, 0, hi-lo)
	for _, i := range list[lo:hi] {
		out = append(out, s.issueJSON(r, rp, i))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- labels -------------------------------------------------------------------------

func labelJSON(l *label) map[string]any {
	return map[string]any{"id": l.id, "name": l.name, "color": l.color, "description": l.description, "text_color": "#FFFFFF"}
}

func (s *Server) labels(w http.ResponseWriter, r *http.Request, rp *repo, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		search := strings.ToLower(r.URL.Query().Get("search"))
		var list []*label
		for _, l := range rp.labels {
			if search == "" || strings.Contains(strings.ToLower(l.name), search) {
				list = append(list, l)
			}
		}
		lo, hi := page(w, r, len(list))
		out := make([]map[string]any, 0, hi-lo)
		for _, l := range list[lo:hi] {
			out = append(out, labelJSON(l))
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 0 && r.Method == http.MethodPost:
		a, err := readArgs(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		name, _ := a.str("name")
		if strings.TrimSpace(name) == "" {
			fail(w, http.StatusBadRequest, "name is missing")
			return
		}
		color, _ := a.str("color")
		desc, _ := a.str("description")
		l, created := s.ensureLabel(rp, name, color, desc)
		if !created {
			fail(w, http.StatusConflict, "Label already exists")
			return
		}
		writeJSON(w, http.StatusCreated, labelJSON(l))
	case len(rest) == 1 && r.Method == http.MethodGet:
		for _, l := range rp.labels {
			if strings.EqualFold(l.name, rest[0]) {
				writeJSON(w, http.StatusOK, labelJSON(l))
				return
			}
		}
		fail(w, http.StatusNotFound, "404 Label Not Found")
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// ---- merge requests -----------------------------------------------------------------

// mrSHA is the source branch's head, or the head recorded when the request was merged.
func (s *Server) mrSHA(rp *repo, mr *mergeRequest) string {
	if mr.state == "merged" && mr.headSHA != "" {
		return mr.headSHA
	}
	return s.rev(rp, mr.source)
}

func (s *Server) mrJSON(r *http.Request, rp *repo, mr *mergeRequest) map[string]any {
	labels := mr.labels
	if labels == nil {
		labels = []string{}
	}
	draft := isDraft(mr.title)
	detailed := "mergeable"
	if draft {
		detailed = "draft_status"
	}
	var mergeSHA any
	if mr.mergeSHA != "" {
		mergeSHA = mr.mergeSHA
	}
	return map[string]any{
		"id": mr.id, "iid": mr.iid, "project_id": rp.id, "title": mr.title, "description": mr.body,
		"state": mr.state, "draft": draft, "work_in_progress": draft,
		"source_branch": mr.source, "target_branch": mr.target, "sha": s.mrSHA(rp, mr), "merge_commit_sha": mergeSHA,
		"labels": labels, "has_conflicts": false, "merge_status": "can_be_merged", "detailed_merge_status": detailed,
		"created_at": stamp(mr.createdAt), "updated_at": stamp(mr.updatedAt),
		"merged_at": stampPtr(mr.mergedAt), "closed_at": stampPtr(mr.closedAt),
		"web_url": fmt.Sprintf("%s/%s/-/merge_requests/%d", base(r), rp.path, mr.iid),
		"author":  map[string]any{"username": "forge", "name": "Forge"},
	}
}

func findMR(rp *repo, iid string) *mergeRequest {
	n, err := strconv.Atoi(iid)
	if err != nil {
		return nil
	}
	for _, mr := range rp.mrs {
		if mr.iid == n {
			return mr
		}
	}
	return nil
}

func (s *Server) mergeRequests(w http.ResponseWriter, r *http.Request, rp *repo, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		q := r.URL.Query()
		state := q.Get("state")
		var list []*mergeRequest
		for _, mr := range rp.mrs {
			switch {
			case state != "" && state != "all" && mr.state != state:
			case q.Get("source_branch") != "" && mr.source != q.Get("source_branch"):
			case q.Get("target_branch") != "" && mr.target != q.Get("target_branch"):
			case len(splitLabels(q.Get("labels"))) > 0 && !labelFilter(mr.labels, splitLabels(q.Get("labels"))):
			default:
				list = append(list, mr)
			}
		}
		sort.SliceStable(list, func(a, b int) bool { return list[a].iid > list[b].iid })
		lo, hi := page(w, r, len(list))
		out := make([]map[string]any, 0, hi-lo)
		for _, mr := range list[lo:hi] {
			out = append(out, s.mrJSON(r, rp, mr))
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 0 && r.Method == http.MethodPost:
		s.createMR(w, r, rp)
	case len(rest) >= 1:
		mr := findMR(rp, rest[0])
		if mr == nil {
			fail(w, http.StatusNotFound, "404 Not found")
			return
		}
		s.mergeRequest(w, r, rp, mr, rest[1:])
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

func (s *Server) createMR(w http.ResponseWriter, r *http.Request, rp *repo) {
	a, err := readArgs(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	source, _ := a.str("source_branch")
	target, _ := a.str("target_branch")
	title, _ := a.str("title")
	if target == "" {
		target = rp.defaultBranch
	}
	switch {
	case source == "":
		fail(w, http.StatusBadRequest, "source_branch is missing")
		return
	case strings.TrimSpace(title) == "":
		fail(w, http.StatusBadRequest, "title is missing")
		return
	case s.rev(rp, source) == "":
		fail(w, http.StatusBadRequest, "Source branch does not exist")
		return
	case s.rev(rp, target) == "":
		fail(w, http.StatusBadRequest, "Target branch does not exist")
		return
	case source == target:
		fail(w, http.StatusBadRequest, "Source branch and target branch must differ")
		return
	}
	for _, o := range rp.mrs {
		if o.state == "opened" && o.source == source && o.target == target {
			fail(w, http.StatusConflict, fmt.Sprintf("Another open merge request already exists for this source branch: !%d", o.iid))
			return
		}
	}
	body, _ := a.str("description")
	now := s.now()
	s.nextMR++
	mr := &mergeRequest{id: s.nextMR, iid: len(rp.mrs) + 1, title: title, body: body, state: "opened", source: source, target: target,
		removeSrc: a.boolean("remove_source_branch"), createdAt: now, updatedAt: now}
	mr.labels = s.applyLabels(rp, nil, a)
	rp.mrs = append(rp.mrs, mr)
	writeJSON(w, http.StatusCreated, s.mrJSON(r, rp, mr))
}

func (s *Server) mergeRequest(w http.ResponseWriter, r *http.Request, rp *repo, mr *mergeRequest, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.mrJSON(r, rp, mr))
	case len(rest) == 0 && r.Method == http.MethodPut:
		a, err := readArgs(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if v, ok := a.str("title"); ok && strings.TrimSpace(v) != "" {
			mr.title = v
		}
		if v, ok := a.str("description"); ok {
			mr.body = v
		}
		mr.labels = s.applyLabels(rp, mr.labels, a)
		now := s.now()
		switch ev, _ := a.str("state_event"); ev {
		case "close":
			if mr.state == "opened" {
				mr.state, mr.closedAt = "closed", &now
			}
		case "reopen":
			if mr.state == "closed" {
				mr.state, mr.closedAt = "opened", nil
			}
		case "":
		default:
			fail(w, http.StatusBadRequest, "state_event does not have a valid value")
			return
		}
		mr.updatedAt = now
		writeJSON(w, http.StatusOK, s.mrJSON(r, rp, mr))
	case len(rest) == 1 && rest[0] == "merge" && r.Method == http.MethodPut:
		a, err := readArgs(r)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		switch err := s.mergeMR(rp, mr, a.boolean("should_remove_source_branch")); {
		case err == nil:
			writeJSON(w, http.StatusOK, s.mrJSON(r, rp, mr))
		case errors.Is(err, errNotOpen), errors.Is(err, errDraft):
			fail(w, http.StatusMethodNotAllowed, "405 Method Not Allowed: "+err.Error())
		case errors.Is(err, errConflict), errors.Is(err, errNoBranch), errors.Is(err, errNoTarget):
			fail(w, http.StatusNotAcceptable, "Branch cannot be merged: "+err.Error())
		default:
			fail(w, http.StatusInternalServerError, err.Error())
		}
	case len(rest) == 1 && rest[0] == "notes" && r.Method == http.MethodPost:
		s.addNote(w, r, &mr.notes)
	case len(rest) == 1 && rest[0] == "notes" && r.Method == http.MethodGet:
		listNotes(w, mr.notes)
	case len(rest) == 1 && rest[0] == "discussions" && r.Method == http.MethodGet:
		// Every note is its own discussion, none of them resolvable.
		out := make([]map[string]any, 0, len(mr.notes))
		for _, n := range mr.notes {
			out = append(out, map[string]any{"id": fmt.Sprintf("d%d", n.id), "individual_note": true, "notes": []map[string]any{noteJSON(n)}})
		}
		lo, hi := page(w, r, len(out))
		writeJSON(w, http.StatusOK, out[lo:hi])
	case len(rest) == 1 && rest[0] == "pipelines" && r.Method == http.MethodGet:
		s.listPipelines(w, r, rp, func(p *pipeline) bool { return p.ref == mr.source })
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// ---- pipelines ----------------------------------------------------------------------

// gitlabStatus maps the control endpoint's states to GitLab's pipeline statuses.
var gitlabStatus = map[string]string{
	"passed": "success", "success": "success", "failed": "failed", "running": "running",
	"pending": "pending", "canceled": "canceled",
}

func (s *Server) pipelineJSON(r *http.Request, rp *repo, p *pipeline) map[string]any {
	return map[string]any{
		"id": p.id, "iid": p.id, "project_id": rp.id, "sha": p.sha, "ref": p.ref, "status": p.status,
		"web_url":    fmt.Sprintf("%s/%s/-/pipelines/%d", base(r), rp.path, p.id),
		"created_at": stamp(p.createdAt), "updated_at": stamp(p.createdAt),
	}
}

func (s *Server) pipelinesAPI(w http.ResponseWriter, r *http.Request, rp *repo, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		ref := r.URL.Query().Get("ref")
		s.listPipelines(w, r, rp, func(p *pipeline) bool { return ref == "" || p.ref == ref })
	case len(rest) == 1 && r.Method == http.MethodGet:
		for _, p := range rp.pipelines {
			if strconv.Itoa(p.id) == rest[0] {
				writeJSON(w, http.StatusOK, s.pipelineJSON(r, rp, p))
				return
			}
		}
		fail(w, http.StatusNotFound, "404 Not found")
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// listPipelines lists the project's pipelines that keep passes, newest first unless
// sort=asc (order_by=id is the only order), filtered by status.
func (s *Server) listPipelines(w http.ResponseWriter, r *http.Request, rp *repo, keep func(*pipeline) bool) {
	q := r.URL.Query()
	var list []*pipeline
	for _, p := range rp.pipelines {
		if keep(p) && (q.Get("status") == "" || q.Get("status") == p.status) {
			list = append(list, p)
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		if q.Get("sort") == "asc" {
			return list[a].id < list[b].id
		}
		return list[a].id > list[b].id
	})
	lo, hi := page(w, r, len(list))
	out := make([]map[string]any, 0, hi-lo)
	for _, p := range list[lo:hi] {
		out = append(out, s.pipelineJSON(r, rp, p))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- branches -----------------------------------------------------------------------

func (s *Server) branchJSON(rp *repo, name, sha string) map[string]any {
	short := sha
	if len(short) > 8 {
		short = short[:8]
	}
	return map[string]any{"name": name, "default": name == rp.defaultBranch, "protected": false, "can_push": true,
		"commit": map[string]any{"id": sha, "short_id": short}}
}

func (s *Server) branches(w http.ResponseWriter, r *http.Request, rp *repo, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		out, err := s.runGit(rp.dir, "", "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		search := strings.ToLower(r.URL.Query().Get("search"))
		list := []map[string]any{}
		for _, line := range strings.Split(out, "\n") {
			name, sha, ok := strings.Cut(line, " ")
			if ok && (search == "" || strings.Contains(strings.ToLower(name), search)) {
				list = append(list, s.branchJSON(rp, name, sha))
			}
		}
		lo, hi := page(w, r, len(list))
		writeJSON(w, http.StatusOK, list[lo:hi])
	case len(rest) >= 1 && r.Method == http.MethodGet:
		name := strings.Join(rest, "/")
		if sha := s.rev(rp, name); sha != "" {
			writeJSON(w, http.StatusOK, s.branchJSON(rp, name, sha))
			return
		}
		fail(w, http.StatusNotFound, "404 Branch Not Found")
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}
