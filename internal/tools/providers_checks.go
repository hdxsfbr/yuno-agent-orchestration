package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"yuno/internal/store"
)

// The merge request reads of Chief platform control (DESIGN 5.34 rules 4, 13 and 14):
// the CI of exactly one commit with its jobs, the request's files and patches, a job's
// log, and whether a branch has CI at all. The callers redact and cap what reaches a
// person or an agent (mergereq); these return what the provider said.

// Job statuses of a Checks' jobs: the providers' words, folded into one small set.
const (
	JobPassed   = "passed"
	JobFailed   = "failed"
	JobRunning  = "running"
	JobPending  = "pending"
	JobCanceled = "canceled"
	JobSkipped  = "skipped"
	JobManual   = "manual"
)

// Checks is the CI of one commit (rule 14). Status is one of the store's ci_status values
// by the mapping tables of Status. PipelineID and URL name GitLab's pipeline; on GitHub
// PipelineID is 0 and URL is the check that decides Status (the first failing one, else
// one still going, else the first).
type Checks struct {
	SHA        string     `json:"sha"`
	Status     string     `json:"status"`
	PipelineID int64      `json:"pipeline_id,omitempty"`
	URL        string     `json:"url,omitempty"`
	Jobs       []CheckJob `json:"jobs"`
}

// CheckJob is one job of a pipeline (GitLab) or one check run or commit status (GitHub).
// ID is what the job log read takes: GitLab's job id, GitHub's check run id (an Actions
// job's id), or "status-<id>" for a GitHub commit status, which has no log. Stage is
// GitLab's stage, "" on GitHub.
type CheckJob struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Stage  string `json:"stage"`
	Status string `json:"status"`
	URL    string `json:"url"`
}

// Diff is a request's changed files (rule 14). More says the provider has files past the
// ones read, which the limit stopped.
type Diff struct {
	Files []DiffFile
	More  bool
}

// DiffFile is one changed file: Status is added, deleted, modified or renamed; OldPath is
// set for a rename; Patch is the provider's unified diff hunks, "" when it gave none (a
// binary file, or one too large for it).
type DiffFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
	Patch   string `json:"patch"`
}

// JobLog is the tail of a job's log (rule 14): Text is its last bytes as read, Size how
// many bytes the log had, Truncated whether Text is less than the whole.
type JobLog struct {
	Text      []byte
	Size      int64
	Truncated bool
}

// ErrJobNotFound is the answer for a job the provider has no log for: an unknown id, a
// GitHub commit status, or a log the provider no longer keeps.
var ErrJobNotFound = errors.New("the provider has no log for this job")

// jobIDPattern is a provider's job id: digits only, since it goes into a URL path.
var jobIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

const (
	// diffPageSize and diffMaxPages bound how much of a request's file list is read.
	diffPageSize = 100
	diffMaxPages = 30
	// ciConfiguredCommits is how many of a branch's newest commits CIConfigured looks at
	// (rule 4: "the default branch in its last 20 commits").
	ciConfiguredCommits = 20
)

// ---- GitLab -------------------------------------------------------------------------

// gitlabJobStatuses folds a GitLab job status; a status not listed is pending.
var gitlabJobStatuses = map[string]string{
	"success": JobPassed, "failed": JobFailed, "canceled": JobCanceled, "canceling": JobCanceled,
	"running": JobRunning, "skipped": JobSkipped, "manual": JobManual,
}

// Checks is rule 14 for GitLab: the newest pipeline of sha (as Status reads it), then GET
// pipelines/<id>/jobs?per_page=100. No pipeline is none with no jobs.
func (g *gitlabProvider) Checks(ctx context.Context, _ int64, sha string) (Checks, error) {
	out := Checks{SHA: sha, Status: store.CIStatusNone, Jobs: []CheckJob{}}
	pl, err := g.headPipeline(ctx, sha)
	if err != nil || pl.ID == 0 {
		return out, err
	}
	out.Status, out.PipelineID, out.URL = pl.ciStatus(), pl.ID, pl.WebURL
	var jobs []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Stage  string `json:"stage"`
		Status string `json:"status"`
		WebURL string `json:"web_url"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("%s/pipelines/%d/jobs?per_page=100", g.base, pl.ID), nil, &jobs); err != nil {
		return Checks{}, err
	}
	for _, j := range jobs {
		st, ok := gitlabJobStatuses[j.Status]
		if !ok {
			st = JobPending
		}
		out.Jobs = append(out.Jobs, CheckJob{ID: strconv.FormatInt(j.ID, 10), Name: j.Name, Stage: j.Stage, Status: st, URL: j.WebURL})
	}
	return out, nil
}

// Diff is rule 14 for GitLab: GET merge_requests/<iid>/diffs, a page of 100 at a time.
func (g *gitlabProvider) Diff(ctx context.Context, number int64, limit int) (Diff, error) {
	var out Diff
	read := 0
	for page := 1; page <= diffMaxPages; page++ {
		var files []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			NewFile     bool   `json:"new_file"`
			RenamedFile bool   `json:"renamed_file"`
			DeletedFile bool   `json:"deleted_file"`
			Diff        string `json:"diff"`
		}
		u := fmt.Sprintf("%s/merge_requests/%d/diffs?page=%d&per_page=%d", g.base, number, page, diffPageSize)
		if status, err := g.api.do(ctx, http.MethodGet, u, nil, &files); err != nil {
			return Diff{}, notFoundOr(status, err)
		}
		for _, f := range files {
			d := DiffFile{Path: f.NewPath, Status: "modified", Patch: f.Diff}
			switch {
			case f.NewFile:
				d.Status = "added"
			case f.DeletedFile:
				d.Status, d.Path = "deleted", f.OldPath
			case f.RenamedFile:
				d.Status, d.OldPath = "renamed", f.OldPath
			}
			out.Files = append(out.Files, d)
			read += len(d.Path) + len(d.Patch)
		}
		if len(files) < diffPageSize {
			return out, nil
		}
		if read >= limit {
			out.More = true
			return out, nil
		}
	}
	out.More = true
	return out, nil
}

// JobLog is rule 14 for GitLab: GET jobs/<id>/trace, the log as plain text.
func (g *gitlabProvider) JobLog(ctx context.Context, job string, tail int) (JobLog, error) {
	if !jobIDPattern.MatchString(job) {
		return JobLog{}, ErrJobNotFound
	}
	return g.api.tailText(ctx, g.base+"/jobs/"+job+"/trace", tail, false)
}

// CIConfigured is rule 4's test for GitLab: GET repository/commits?ref_name=<branch>&
// per_page=20, then GET pipelines?ref=<branch>&order_by=id&sort=desc&per_page=100; a
// pipeline of any of those commits is CI.
func (g *gitlabProvider) CIConfigured(ctx context.Context, branch string) (bool, error) {
	var commits []struct {
		ID string `json:"id"`
	}
	q := url.Values{"ref_name": {branch}, "per_page": {strconv.Itoa(ciConfiguredCommits)}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/repository/commits?"+q.Encode(), nil, &commits); err != nil {
		return false, err
	}
	if len(commits) == 0 {
		return false, nil
	}
	recent := make(map[string]bool, len(commits))
	for _, c := range commits {
		recent[c.ID] = true
	}
	var pipelines []gitlabPipeline
	q = url.Values{"ref": {branch}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"100"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/pipelines?"+q.Encode(), nil, &pipelines); err != nil {
		return false, err
	}
	for _, p := range pipelines {
		if recent[p.SHA] {
			return true, nil
		}
	}
	return false, nil
}

// CommitCI is rule 4's check of a merge commit for GitLab: GET pipelines?sha=<sha>&
// ref=<ref>&order_by=id&sort=desc&per_page=1, the variant of headPipeline that counts only
// the default branch's own pipeline. No pipeline (or a skipped one) is none.
func (g *gitlabProvider) CommitCI(ctx context.Context, ref, sha string) (BranchStatus, error) {
	if sha == "" {
		return BranchStatus{State: BranchNone}, nil
	}
	var pipelines []struct {
		Status    string  `json:"status"`
		SHA       string  `json:"sha"`
		WebURL    string  `json:"web_url"`
		CreatedAt *string `json:"created_at"`
	}
	q := url.Values{"sha": {sha}, "ref": {ref}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/pipelines?"+q.Encode(), nil, &pipelines); err != nil {
		return BranchStatus{}, err
	}
	if len(pipelines) == 0 {
		return BranchStatus{State: BranchNone}, nil
	}
	pl := pipelines[0]
	state, ok := gitlabBranchStates[pl.Status]
	if !ok {
		state = BranchPending
	}
	if state == BranchNone {
		return BranchStatus{State: BranchNone}, nil
	}
	st := BranchStatus{State: state, SHA: sha, URL: pl.WebURL}
	if t := parseProviderTime(pl.CreatedAt); t != nil {
		st.At = *t
	}
	return st, nil
}

// ---- GitHub -------------------------------------------------------------------------

// CommitCI is rule 4's check of a merge commit for GitHub: every check run and the combined
// status of the commit (checks belong to a commit, not to a ref, so ref is not read).
// Success, neutral and skipped pass; nothing at all is none.
func (g *githubProvider) CommitCI(ctx context.Context, _ string, sha string) (BranchStatus, error) {
	c, err := g.checks(ctx, sha)
	if err != nil {
		return BranchStatus{}, err
	}
	st := BranchStatus{SHA: sha, URL: c.URL}
	switch c.Status {
	case store.CIStatusPassed:
		st.State = BranchPassed
	case store.CIStatusFailed:
		st.State = BranchFailed
	case store.CIStatusPending:
		st.State = BranchPending
	default:
		return BranchStatus{State: BranchNone}, nil
	}
	return st, nil
}

// githubCheckRun is a check run as GitHub lists it.
type githubCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// jobStatus folds a check run's status and conclusion (rule 14).
func (c githubCheckRun) jobStatus() string {
	switch c.Status {
	case "", "completed":
	case "in_progress":
		return JobRunning
	default:
		return JobPending
	}
	switch c.Conclusion {
	case "success", "neutral":
		return JobPassed
	case "skipped":
		return JobSkipped
	case "cancelled":
		return JobCanceled
	case "", "stale":
		return JobPending
	}
	return JobFailed
}

// checks reads the commit's check runs (the first 100) and combined status and folds them
// into one pipeline status (DESIGN 5.20 rule 3): any failing check or a failed combined
// status is failed; else any check not completed, or a pending combined status that has
// statuses, is pending; else none when there is nothing, else passed. Status, CIStatus and
// Checks share it; an empty sha is none.
func (g *githubProvider) checks(ctx context.Context, sha string) (Checks, error) {
	out := Checks{SHA: sha, Status: store.CIStatusNone, Jobs: []CheckJob{}}
	if sha == "" {
		return out, nil
	}
	var runs struct {
		CheckRuns []githubCheckRun `json:"check_runs"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(sha)+"/check-runs?per_page=100", nil, &runs); err != nil {
		return Checks{}, err
	}
	var combined struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
		Statuses   []struct {
			ID        int64  `json:"id"`
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(sha)+"/status", nil, &combined); err != nil {
		return Checks{}, err
	}
	statuses := max(combined.TotalCount, len(combined.Statuses))

	failed, pending := combined.State == "failure" || combined.State == "error", combined.State == "pending" && statuses > 0
	var failedURL, pendingURL string
	for _, c := range runs.CheckRuns {
		switch c.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			failed = true
			failedURL = cmp.Or(failedURL, c.HTMLURL)
		}
		if c.Status != "" && c.Status != "completed" {
			pending = true
			pendingURL = cmp.Or(pendingURL, c.HTMLURL)
		}
		out.URL = cmp.Or(out.URL, c.HTMLURL)
		out.Jobs = append(out.Jobs, CheckJob{ID: strconv.FormatInt(c.ID, 10), Name: c.Name, Status: c.jobStatus(), URL: c.HTMLURL})
	}
	for _, s := range combined.Statuses {
		job := CheckJob{ID: "status-" + strconv.FormatInt(s.ID, 10), Name: s.Context, URL: s.TargetURL}
		switch s.State {
		case "success":
			job.Status = JobPassed
		case "failure", "error":
			job.Status = JobFailed
			failedURL = cmp.Or(failedURL, s.TargetURL)
		default:
			job.Status = JobPending
			pendingURL = cmp.Or(pendingURL, s.TargetURL)
		}
		out.URL = cmp.Or(out.URL, s.TargetURL)
		out.Jobs = append(out.Jobs, job)
	}
	switch {
	case failed:
		out.Status, out.URL = store.CIStatusFailed, cmp.Or(failedURL, out.URL)
	case pending:
		out.Status, out.URL = store.CIStatusPending, cmp.Or(pendingURL, out.URL)
	case len(runs.CheckRuns) == 0 && statuses == 0:
		out.Status = store.CIStatusNone
	default:
		out.Status = store.CIStatusPassed
	}
	return out, nil
}

// Checks is rule 14 for GitHub: the commit's check runs and commit statuses as jobs.
func (g *githubProvider) Checks(ctx context.Context, _ int64, sha string) (Checks, error) {
	return g.checks(ctx, sha)
}

// githubFileStatuses maps a pull request file's status to a DiffFile's.
var githubFileStatuses = map[string]string{
	"added": "added", "copied": "added", "removed": "deleted", "renamed": "renamed",
}

// Diff is rule 14 for GitHub: GET pulls/<n>/files, a page of 100 at a time.
func (g *githubProvider) Diff(ctx context.Context, number int64, limit int) (Diff, error) {
	var out Diff
	read := 0
	for page := 1; page <= diffMaxPages; page++ {
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			Patch            string `json:"patch"`
		}
		u := fmt.Sprintf("%s/pulls/%d/files?page=%d&per_page=%d", g.base, number, page, diffPageSize)
		if status, err := g.api.do(ctx, http.MethodGet, u, nil, &files); err != nil {
			return Diff{}, notFoundOr(status, err)
		}
		for _, f := range files {
			d := DiffFile{Path: f.Filename, Status: "modified", Patch: f.Patch}
			if st, ok := githubFileStatuses[f.Status]; ok {
				d.Status = st
			}
			if d.Status == "renamed" {
				d.OldPath = f.PreviousFilename
			}
			out.Files = append(out.Files, d)
			read += len(d.Path) + len(d.Patch)
		}
		if len(files) < diffPageSize {
			return out, nil
		}
		if read >= limit {
			out.More = true
			return out, nil
		}
	}
	out.More = true
	return out, nil
}

// JobLog is rule 14 for GitHub: GET actions/jobs/<id>/logs, which answers a redirect to
// the log's own short-lived address. That address is read without the token (a token
// header never follows a redirect, ProviderClient).
func (g *githubProvider) JobLog(ctx context.Context, job string, tail int) (JobLog, error) {
	if !jobIDPattern.MatchString(job) {
		return JobLog{}, ErrJobNotFound
	}
	return g.api.tailText(ctx, g.base+"/actions/jobs/"+job+"/logs", tail, true)
}

// CIConfigured is rule 4's test for GitHub: GET commits?sha=<branch>&per_page=20, then,
// newest first, each commit's check runs and combined status until one has any.
func (g *githubProvider) CIConfigured(ctx context.Context, branch string) (bool, error) {
	var commits []struct {
		SHA string `json:"sha"`
	}
	q := url.Values{"sha": {branch}, "per_page": {strconv.Itoa(ciConfiguredCommits)}}
	if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits?"+q.Encode(), nil, &commits); err != nil {
		return false, err
	}
	for _, c := range commits {
		if c.SHA == "" {
			continue
		}
		var runs struct {
			TotalCount int              `json:"total_count"`
			CheckRuns  []githubCheckRun `json:"check_runs"`
		}
		if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(c.SHA)+"/check-runs?per_page=1", nil, &runs); err != nil {
			return false, err
		}
		if runs.TotalCount > 0 || len(runs.CheckRuns) > 0 {
			return true, nil
		}
		var combined struct {
			TotalCount int   `json:"total_count"`
			Statuses   []any `json:"statuses"`
		}
		if _, err := g.api.do(ctx, http.MethodGet, g.base+"/commits/"+url.PathEscape(c.SHA)+"/status", nil, &combined); err != nil {
			return false, err
		}
		if combined.TotalCount > 0 || len(combined.Statuses) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// ---- plain-text reads ---------------------------------------------------------------

// tailText GETs a plain-text answer (a job log) and keeps its last tail bytes, reading the
// whole body through so Size is the log's. With follow, one redirect is followed to the
// address it names, without the token. A 404 or 410 is ErrJobNotFound; any other non-2xx
// answer is an *errAPIStatus as do gives.
func (a *apiClient) tailText(ctx context.Context, u string, tail int, follow bool) (JobLog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return JobLog{}, err
	}
	if a.authorize != nil {
		a.authorize(req)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return JobLog{}, &errAPIStatus{res: transportFailure(a.what, err)}
	}
	if follow && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc, lerr := resp.Location()
		resp.Body.Close()
		if lerr != nil {
			return JobLog{}, fmt.Errorf("%s: a redirect without a location", a.what)
		}
		next, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
		if err != nil {
			return JobLog{}, err
		}
		if resp, err = a.client.Do(next); err != nil {
			return JobLog{}, &errAPIStatus{res: transportFailure(a.what, err)}
		}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return JobLog{}, ErrJobNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		raw, _ := readBody(resp, 64<<10)
		return JobLog{}, &errAPIStatus{res: statusFailure(a.what, resp, raw), plain: statusPlain(a.what, resp, raw), status: resp.StatusCode,
			message: providerMessage(raw)}
	}
	tail = max(tail, 1)
	var out JobLog
	// The buffer holds up to twice the tail before it drops the older half, so a long log
	// is not moved once per chunk.
	buf := make([]byte, 0, 2*tail)
	chunk := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			out.Size += int64(n)
			buf = append(buf, chunk[:n]...)
			if len(buf) > 2*tail {
				buf = append(buf[:0], buf[len(buf)-tail:]...)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return JobLog{}, &errAPIStatus{res: transportFailure(a.what, rerr)}
		}
	}
	if len(buf) > tail {
		buf = buf[len(buf)-tail:]
	}
	out.Text, out.Truncated = buf, out.Size > int64(len(buf))
	return out, nil
}
