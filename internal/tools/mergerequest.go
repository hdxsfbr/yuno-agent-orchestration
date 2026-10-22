package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"yuno/internal/obs"
	"yuno/internal/pathcheck"
	"yuno/internal/project"
	"yuno/internal/store"
)

// OpenMergeRequestName proposes a run's project checkout as a merge request (DESIGN
// 5.16 rule 10, D-037): GitLab or GitHub behind one interface.
const OpenMergeRequestName = "open_merge_request"

// ProjectResolver gives the tool the project of the run that calls it (DESIGN 5.17
// rule 6) and opens the token of a project a conversation names (read_merge_request,
// 5.22 rule 6); the project package's Resolver implements it.
type ProjectResolver interface {
	ForRunID(ctx context.Context, runID string) (*project.Config, error)
	Token(ctx context.Context, projectID string) (string, error)
}

// MergeRequestConfig is what open_merge_request needs. The tool is registered always:
// the project comes from the calling run.
type MergeRequestConfig struct {
	Projects ProjectResolver
	// APIBase and RepoPath override the project's APIBase() and Path() (tests point
	// them at a fake while git talks to a local repository).
	APIBase  string
	RepoPath string
	Client   *http.Client
	// Sleep waits between the looks for a pushed branch (create); nil is a real,
	// cancellable wait.
	Sleep func(ctx context.Context, d time.Duration) error
}

// The title is capped at 72 characters, a commit subject (DESIGN 5.17 rule 11).
var mergeRequestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "title": {"type": "string", "minLength": 1, "maxLength": 72, "description": "A short summary of the change, like a commit subject (at most 72 characters), never the task text; it is also the commit message."},
    "description": {"type": "string", "maxLength": 20000, "description": "The detail: what changed and why, how it was tested, anything the reviewer should know."},
    "draft": {"type": "boolean", "description": "Optional. true opens the request as a draft (GitLab: the title starts with \"Draft: \"); a later call of the same run with draft false marks it ready for review; omit it to leave the state as it is (a new request is not a draft)."},
    "paths": {"type": "array", "minItems": 1, "maxItems": 100, "items": {"type": "string", "minLength": 1, "maxLength": 200}, "description": "Optional. Files or directories of the checkout, relative to its root (docs/plans/x/PRD.md, docs/plans/x). When present only these are committed, and everything else left in the checkout stays out; a path deleted in the checkout is deleted in the commit. Omit to propose the whole working tree."}
  },
  "required": ["run_token", "title", "description"],
  "additionalProperties": false
}`)

type mergeRequestInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Paths       []string `json:"paths,omitempty"`
	// Draft is nil when the call did not say: a new request is then ready and a recorded
	// one keeps its state (DESIGN 5.29 rule 4).
	Draft *bool `json:"draft,omitempty"`
}

// mergeRequestOutput is the tool's result: {number, url, branch, provider, replayed?,
// updated?}. A run has one request (DESIGN 5.20 rule 2): a later successful call answers
// it with replayed, and with updated when it pushed a commit onto its branch. Since DESIGN
// 5.29 rule 4 a request merged before the run's last push is answered by its follow-up
// (follow_up), or, when nothing was left to push, by the merged request itself (merged).
// Since DESIGN 5.28 rule 9 (D-089) a ticket run's answer names the plan documents its push
// left out (left_out), with the notice that says where such notes go.
type mergeRequestOutput struct {
	Number   int64    `json:"number"`
	URL      string   `json:"url"`
	Branch   string   `json:"branch"`
	Provider string   `json:"provider"`
	Replayed bool     `json:"replayed,omitempty"`
	Updated  bool     `json:"updated,omitempty"`
	Draft    bool     `json:"draft,omitempty"`
	FollowUp bool     `json:"follow_up,omitempty"`
	Merged   bool     `json:"merged,omitempty"`
	LeftOut  []string `json:"left_out,omitempty"`
	Notice   string   `json:"notice,omitempty"`
}

// leftOut records on out the plan documents a ticket run's push left out, with the notice.
func (out *mergeRequestOutput) leftOut(base project.Base, paths []string) {
	if base.Ticket == nil || len(paths) == 0 {
		return
	}
	out.LeftOut, out.Notice = paths, leftOutNotice(*base.Ticket, paths)
}

// leftOutNotice tells a ticket run which plan documents its push left out and where their
// notes go instead (DESIGN 5.28 rule 9, D-089).
func leftOutNotice(t project.TicketDocs, paths []string) string {
	return "Left out of the merge request: " + strings.Join(paths, ", ") + ". A ticket's merge request carries no plan " +
		"document of the feature but the ticket's own file, " + t.File() + ", because the tickets of a wave run in parallel. " +
		"Write what the other documents need in that file under the heading \"## Notes for the plan documents\", naming the " +
		"document and section of each note; the wave's ship run applies them."
}

// runBase is the base of the run's next proposal (project.RunBase) with the plan ticket the
// run builds, when it builds one, so the proposal carries none of the feature's plan
// documents but the ticket's own file (DESIGN 5.28 rule 9, D-089). Without a transaction no
// ticket is read.
func runBase(ctx context.Context, call Call, run *store.Run) (project.Base, error) {
	base := project.RunBase(run)
	if call.Tx == nil {
		return base, nil
	}
	feature, ticket, err := store.TicketDocsOfRun(ctx, call.Tx, call.RunID)
	if err != nil {
		return base, fmt.Errorf("read the run's ticket: %w", err)
	}
	if ticket != "" {
		base.Ticket = &project.TicketDocs{Feature: feature, Ticket: ticket}
	}
	return base, nil
}

// NewOpenMergeRequest builds open_merge_request: commit the run's checkout on the key's
// branch and push it from the api, then open the request against the default branch
// of the run's project (DESIGN 5.17 rule 6) and record it (5.20 rule 2). A second call
// with the same key finds the branch (no second push) and the open request (no second
// one) and reports replayed; a later call of the same run with another key pushes onto
// the recorded request's branch instead of opening a second one.
func NewOpenMergeRequest(cfg MergeRequestConfig) *Tool {
	if cfg.Client == nil {
		cfg.Client = ProviderClient(nil)
	}
	m := &mergeRequest{cfg: cfg}
	return &Tool{
		Name: OpenMergeRequestName,
		Description: "Open or update a merge request: propose the changes in the project checkout (runs/<run_id>/repo/) " +
			"as a merge request (GitLab) or pull request (GitHub) against the project's default branch. The " +
			"platform commits the working tree, pushes a branch and opens the request with your title and " +
			"description. The title is a short summary of the change, like a commit subject (at most 72 " +
			"characters), never the task text; put what changed and why and how it was tested in the " +
			"description. A run has one request: calling this again in the same run adds the working tree to " +
			"that request as a new commit (the answer then has replayed and updated true) and never opens a " +
			"second one. A run that updates an existing merge request pushes your working tree onto that " +
			"request's branch as a new commit, with the title as its message, instead of opening a new " +
			"request; when the answer has replayed without updated, nothing new was pushed. Pass draft true to " +
			"open the request as a draft and draft false in a later call to mark it ready for review; a later " +
			"call of the same run that names draft also replaces the request's title and description. If the run's " +
			"request was merged before this call, the platform opens a small follow-up request with what is left " +
			"(the answer has follow_up true and the follow-up's number and URL), or, when nothing is left, answers " +
			"with the merged request and merged true. In the run of a plan ticket the request carries none of the " +
			"feature's plan documents but the ticket's own docs/plans/<feature>/tickets/<ticket id>.md: changes to " +
			"PRD.md, ARCHITECTURE.md, TASKS.md, WAVES.md or another ticket's file are left out of the push, and the " +
			"answer lists them in left_out with a notice saying where such notes go. Needs a human " +
			"approval unless the operator allowed it to run automatically. Returns the request's number and " +
			"URL; pass paths (files or directories relative to the checkout root) to commit only those and leave " +
			"the rest of the checkout out of the request; if a human has not decided yet it returns status " +
			"pending_approval: then end your turn without finishing, and the platform resumes you with the " +
			"outcome; if they refused it returns status rejected with a reason.",
		InputSchema: mergeRequestSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     2 * time.Minute,
		Describe: func(input json.RawMessage) string {
			var in mergeRequestInput
			_ = json.Unmarshal(input, &in)
			return fmt.Sprintf("Open merge request %q on the run's project", in.Title)
		},
		Exec: m.exec,
	}
}

type mergeRequest struct {
	cfg MergeRequestConfig
}

// providerFor builds the provider client of one project (DESIGN 5.16 rule 11).
func (m *mergeRequest) providerFor(pc *project.Config) RequestProvider {
	base, path := m.cfg.APIBase, m.cfg.RepoPath
	if base == "" {
		base = pc.APIBase()
	}
	if path == "" {
		path = pc.Path()
	}
	return newRequestProvider(pc, m.cfg.Client, base, path)
}

func (m *mergeRequest) exec(ctx context.Context, call Call) (Result, error) {
	var in mergeRequestInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	if call.IdempotencyKey == "" {
		return Result{}, errors.New("open_merge_request: no idempotency key")
	}
	paths, err := cleanPaths(in.Paths)
	if err != nil {
		return Result{}, &ModelError{Message: err.Error()}
	}
	in.Paths = paths
	if m.cfg.Projects == nil {
		return Result{}, &ModelError{Message: project.ErrNoProject.Error()}
	}
	pc, err := m.cfg.Projects.ForRunID(ctx, call.RunID)
	if errors.Is(err, project.ErrNoProject) {
		return Result{}, &ModelError{Message: err.Error()}
	}
	if err != nil {
		// A token that does not open, a project row gone: the operator fixes it on the
		// Projects page; the text carries no secret (the resolver's errors never do).
		return Result{IsError: true, Text: err.Error()}, nil
	}
	checkout, err := checkoutDir(call.Workspace, call.RunID)
	if err != nil {
		return Result{}, err
	}
	branch := GitLabBranch(call.IdempotencyKey)
	res, err := m.run(ctx, call, pc, m.providerFor(pc), in, checkout, branch)
	var se *errAPIStatus
	if errors.As(err, &se) {
		return se.res, nil
	}
	return res, err
}

// run is open_merge_request after the checks (DESIGN 5.16 rule 10, 5.20 rule 2, 5.22 rule
// 4). A run that names a request (runs.merge_request_id) pushes onto that record's branch
// under the branch guard; a run that has a request of its own already pushes onto its
// branch and opens no second one; otherwise the branch is pushed, the request found or
// opened, and the record written in the side effect's transaction right after the provider
// answered, so a failed insert rolls the result back and releases the key. Without a
// transaction (the tool called on its own, as the unit tests of the provider flow do) no
// record is read or written.
func (m *mergeRequest) run(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, in mergeRequestInput, checkout, branch string) (Result, error) {
	var run *store.Run
	if call.Tx != nil {
		// Two calls of one run with different keys must not both find no record and open
		// two requests: the second waits for the first's transaction, then finds the record.
		if err := store.LockRunMergeRequest(ctx, call.Tx, call.RunID); err != nil {
			return Result{}, err
		}
		var err error
		if run, err = store.GetRun(ctx, call.Tx, call.RunID); err != nil {
			return Result{}, fmt.Errorf("read the run: %w", err)
		}
		// The lookup is the record the run names first, then the run's own (5.22 rule 4).
		if run.MergeRequestID != nil && *run.MergeRequestID != "" {
			named, err := store.GetMergeRequest(ctx, call.Tx, *run.MergeRequestID)
			switch {
			case err == nil:
				return m.updateRecorded(ctx, call, pc, provider, named, in, checkout, run, run, nil)
			case !errors.Is(err, store.ErrNotFound):
				return Result{}, err
			}
		}
		rec, err := store.MergeRequestByRun(ctx, call.Tx, call.RunID)
		if err != nil {
			return Result{}, err
		}
		if rec != nil {
			// The same-run path of 5.20 rule 2: no guard (L4 stays recorded for it).
			return m.updateOwn(ctx, call, pc, provider, rec, in, checkout, run)
		}
	}
	exists, err := provider.BranchExists(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	var leftOut []string
	base, err := runBase(ctx, call, run)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		proposal, err := pc.ProposePaths(ctx, checkout, branch, base, in.Title, in.Paths)
		if err != nil {
			var missing *project.ErrPathMissing
			var conflict *project.ErrConflict
			if errors.Is(err, project.ErrNoChanges) && len(proposal.LeftOut) > 0 && base.Ticket != nil {
				return Result{}, &ModelError{Message: "the checkout has no changes to propose besides plan documents a ticket run " +
					"does not change; nothing was pushed. " + leftOutNotice(*base.Ticket, proposal.LeftOut)}
			}
			if errors.Is(err, project.ErrNoChanges) || errors.As(err, &missing) || errors.As(err, &conflict) {
				return Result{}, &ModelError{Message: err.Error()}
			}
			// A push the provider refused (a token without write access, a protected
			// branch) or a network failure: the text names the git step, the token
			// never appears in it (project.Config redacts git's output).
			return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
		}
		// The run's own commit is the base of its next push (DESIGN 5.16 rule 10, D-087).
		if call.Tx != nil {
			if err := store.SetRunCheckoutSHA(ctx, call.Tx, call.RunID, proposal.Base); err != nil {
				return Result{}, fmt.Errorf("record the run's commit: %w", err)
			}
		}
		leftOut = proposal.LeftOut
	}
	draft := in.Draft != nil && *in.Draft
	out := mergeRequestOutput{Branch: branch, Provider: string(pc.Provider), Draft: draft}
	out.leftOut(base, leftOut)
	open, err := provider.FindOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		out.Number, out.URL, out.Replayed = open.Number, open.URL, true
	} else {
		ref, err := m.create(ctx, pc, provider, branch, in.Title, withDraftNotice(pc, withIssueCloses(in.Description, run), draft), draft)
		if err != nil {
			return Result{}, err
		}
		out.Number, out.URL = ref.Number, ref.URL
	}
	if call.Tx != nil {
		if err := m.record(ctx, call, pc, out, in.Title, draft); err != nil {
			return Result{}, err
		}
	}
	return JSONResult(out), nil
}

// branchLooks is how many times create looks for the pushed branch on the provider before
// it gives up; the waits between the looks are Backoff's 1, 2, 4 and 8 s.
const branchLooks = 5

// create opens the request for a branch the call pushed (or found pushed) once the
// provider shows that branch (DESIGN 5.16 rule 10). A push that git reports done can still
// be missing from the provider's API for a moment: GitLab refreshes the branch list that
// its merge request API checks after the push rather than with it, and answers a create in
// between with HTTP 400 "source_branch does not exist". So the branch is looked up through
// the API first, and a create refused for a missing source branch is tried again after the
// next wait. A branch the provider still does not show is a model error that names it, with
// nothing opened; the call's key is then free, and the same call again finds the branch
// and does not push it twice.
func (m *mergeRequest) create(ctx context.Context, pc *project.Config, provider RequestProvider, branch, title, description string, draft bool) (*RequestRef, error) {
	for n := 1; ; n++ {
		exists, err := provider.BranchExists(ctx, branch)
		if err != nil {
			return nil, err
		}
		if exists {
			ref, err := provider.Create(ctx, branch, pc.DefaultBranch, title, description, draft)
			if !missingSourceBranch(err) {
				return ref, err
			}
		}
		if n >= branchLooks {
			return nil, &ModelError{Message: fmt.Sprintf("the branch %s was pushed, but %s does not show it, so no %s was opened. "+
				"Call open_merge_request again with the same input; if it fails the same way, tell the operator that %s does not show "+
				"the branches pushed to the project's repository", branch, providerName(pc), pc.RequestName(), providerName(pc))}
		}
		if err := m.sleep(ctx, Backoff(n)); err != nil {
			return nil, err
		}
	}
}

// missingSourceBranch reports whether a create was refused because the provider does not
// know the source branch: GitLab's 400 (or 422) naming source_branch with "does not
// exist", GitHub's 422 whose errors name the head field as invalid.
func missingSourceBranch(err error) bool {
	var se *errAPIStatus
	if !errors.As(err, &se) {
		return false
	}
	switch se.status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	text := strings.ToLower(se.plain)
	gitlab := (strings.Contains(text, "source_branch") || strings.Contains(text, "source branch")) && strings.Contains(text, "does not exist")
	github := strings.Contains(text, `"field":"head"`) && strings.Contains(text, `"code":"invalid"`)
	return gitlab || github
}

// providerName is the provider as the texts the model and the operator read name it.
func providerName(pc *project.Config) string {
	if pc.Provider == project.ProviderGitHub {
		return "GitHub"
	}
	return "GitLab"
}

func (m *mergeRequest) sleep(ctx context.Context, d time.Duration) error {
	if m.cfg.Sleep != nil {
		return m.cfg.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// cleanPaths validates the paths of a call (pathcheck.Valid), cleans them to slash form
// without a trailing slash, drops repeats, and refuses the checkout's root and anything in
// .git. nil when there are none, which proposes the whole working tree.
func cleanPaths(in []string) ([]string, error) {
	if in != nil && len(in) == 0 {
		return nil, errors.New("paths: empty; omit paths to propose the whole working tree")
	}
	var out []string
	for _, raw := range in {
		if err := pathcheck.Valid(raw); err != nil {
			return nil, fmt.Errorf("paths: %w", err)
		}
		p := path.Clean(raw)
		if p == "." {
			return nil, fmt.Errorf("paths: %q is the checkout itself; omit paths to propose all of it", raw)
		}
		if slices.Contains(strings.Split(p, "/"), ".git") {
			return nil, fmt.Errorf("paths: %q is inside .git", raw)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// withIssueCloses is the description of a request being created (DESIGN 5.23 rule 7): for
// a run that came from an issue, "\n\nCloses #<n>" is appended so that merging the request
// closes the issue through the provider's keywords, unless the description already says
// "closes #<n>" (any case). It is for creation only: a later update never rewrites the
// description (5.20 rule 2), and any other run's description is returned as it is.
func withIssueCloses(description string, run *store.Run) string {
	if run == nil || run.Trigger != store.TriggerIssue || run.TriggerRef == nil {
		return description
	}
	i := strings.LastIndexByte(*run.TriggerRef, '#')
	if i < 0 {
		return description
	}
	n := (*run.TriggerRef)[i+1:]
	if n == "" || strings.Trim(n, "0123456789") != "" {
		return description
	}
	// "closes #12" inside "closes #123" does not close issue 12.
	if regexp.MustCompile(`(?i)closes #` + n + `(?:\D|$)`).MatchString(description) {
		return description
	}
	return description + "\n\nCloses #" + n
}

// updateOwn answers a later call of the run that opened rec (DESIGN 5.20 rule 2, 5.29 rule
// 4). The operator may have merged the request since the last sync, so its state is read
// from the provider first (liveState). An open request gets the push (updateRecorded) and a
// closed one its refusal. A merged one leaves the rest to its follow-up: the follow-up the
// run opened already, read the same way, or a new one (followUp).
func (m *mergeRequest) updateOwn(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, rec *store.MergeRequest, in mergeRequestInput, checkout string, run *store.Run) (Result, error) {
	cur, followed := rec, (*store.MergeRequest)(nil)
	for {
		state, err := liveState(ctx, provider, cur)
		if err != nil {
			return Result{}, err
		}
		if state != store.MergeRequestMerged {
			now := *cur
			now.State = state
			return m.updateRecorded(ctx, call, pc, provider, &now, in, checkout, nil, run, followed)
		}
		next, err := store.FollowUpMergeRequest(ctx, call.Tx, cur.ID)
		if err != nil {
			return Result{}, fmt.Errorf("read the follow-up merge request: %w", err)
		}
		if next == nil {
			return m.followUp(ctx, call, pc, provider, rec, cur, in, checkout, run)
		}
		cur, followed = next, cur
	}
}

// liveState is the state of a request of the run now: the record's when it is final, else
// the provider's, since the record is only as fresh as the last sync (DESIGN 5.20 rule 3).
func liveState(ctx context.Context, provider RequestProvider, rec *store.MergeRequest) (string, error) {
	if rec.State != store.MergeRequestOpen {
		return rec.State, nil
	}
	d, err := provider.Details(ctx, rec.Number)
	if errors.Is(err, ErrRequestNotFound) {
		return "", &ModelError{Message: fmt.Sprintf("the run's %s is gone from the provider", rec.Label())}
	}
	if err != nil {
		return "", err
	}
	return d.State, nil
}

// followUp opens the follow-up of a request the operator merged before the run's last
// push (DESIGN 5.29 rule 4). What the call proposes, under the same paths rule, goes onto
// the call's own branch cut from the default branch, which holds the merged request now, so
// the follow-up carries only what the merge left out. It is titled "Follow-up: <the run's
// title>", its description says which request it follows, and it is recorded as the
// follow-up of merged. When the default branch holds all of it already there is nothing
// left: the merged request is the answer, with merged set, and nothing is pushed or opened.
// own is the run's own record, whose title the follow-up's is made of.
func (m *mergeRequest) followUp(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, own, merged *store.MergeRequest, in mergeRequestInput, checkout string, run *store.Run) (Result, error) {
	branch := GitLabBranch(call.IdempotencyKey)
	exists, err := provider.BranchExists(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	base, err := runBase(ctx, call, run)
	if err != nil {
		return Result{}, err
	}
	var leftOut []string
	if !exists {
		// Only the run's own changes, counted from its base, go onto the default branch (D-087):
		// what the merge already carried is no change, and nothing else is touched.
		proposal, err := pc.ProposePaths(ctx, checkout, branch, base, in.Title, in.Paths)
		if err != nil {
			var missing *project.ErrPathMissing
			switch {
			case errors.Is(err, project.ErrNoChanges):
				out := mergeRequestOutput{Number: merged.Number, URL: merged.URL, Branch: merged.SourceBranch,
					Provider: merged.Provider, Replayed: true, Merged: true}
				out.leftOut(base, proposal.LeftOut)
				return JSONResult(out), nil
			case errors.As(err, &missing):
				return Result{}, &ModelError{Message: err.Error()}
			}
			return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
		}
		// The run's own commit is the base of its next push, onto the follow-up (D-087).
		if err := store.SetRunCheckoutSHA(ctx, call.Tx, call.RunID, proposal.Base); err != nil {
			return Result{}, fmt.Errorf("record the run's commit: %w", err)
		}
		leftOut = proposal.LeftOut
	}
	draft := in.Draft != nil && *in.Draft
	title := followUpTitle(own.Title)
	out := mergeRequestOutput{Branch: branch, Provider: string(pc.Provider), Draft: draft, FollowUp: true}
	out.leftOut(base, leftOut)
	open, err := provider.FindOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		out.Number, out.URL, out.Replayed = open.Number, open.URL, true
	} else {
		desc := withDraftNotice(pc, followUpDescription(pc, merged, in.Description), draft)
		ref, err := m.create(ctx, pc, provider, branch, title, desc, draft)
		if err != nil {
			return Result{}, err
		}
		out.Number, out.URL = ref.Number, ref.URL
	}
	inserted, err := store.InsertFollowUpMergeRequest(ctx, call.Tx, &store.MergeRequest{ProjectID: merged.ProjectID, RunID: call.RunID,
		Provider: string(pc.Provider), Number: out.Number, URL: out.URL, SourceBranch: branch, TargetBranch: pc.DefaultBranch,
		Title: title, Draft: draft}, merged.ID)
	if err != nil {
		return Result{}, fmt.Errorf("record the follow-up merge request: %w", err)
	}
	if !inserted {
		return Result{}, fmt.Errorf("record the follow-up merge request: %d is already recorded for the project", out.Number)
	}
	return JSONResult(out), nil
}

// followUpPrefix starts the title of a follow-up request (DESIGN 5.29 rule 4).
const followUpPrefix = "Follow-up: "

// followUpTitle is the title of a follow-up of a request titled title.
func followUpTitle(title string) string {
	if strings.HasPrefix(title, followUpPrefix) {
		return title
	}
	return followUpPrefix + title
}

// followUpDescription is a follow-up's description: a first paragraph that says which
// request it follows and why, then the call's description.
func followUpDescription(pc *project.Config, followed *store.MergeRequest, description string) string {
	return "Follow-up to " + followed.Label() + " (" + followed.URL + "), which was merged before Yuno had pushed all of " +
		"its changes. This " + pc.RequestName() + " holds the rest.\n\n" + description
}

// withDraftNotice puts the notice of DESIGN 5.29 rule 4 first in the description of a
// request that is a draft: the operator must not merge it until Yuno marks it ready, which
// Yuno does after the run's last push. A ready request's description has no notice.
func withDraftNotice(pc *project.Config, description string, draft bool) string {
	if !draft {
		return description
	}
	return "**Do not merge this " + pc.RequestName() + " yet.** Yuno is still working on it and will push more changes. " +
		"It marks the " + pc.RequestName() + " ready for review once the last of them is pushed.\n\n" + description
}

// updateRecorded answers a later call of a run that has a request to push onto (DESIGN
// 5.20 rule 2, 5.22 rule 4): the working tree goes onto the record's source branch as a new
// commit with the title as its message, and the answer is the record's, replayed, with
// updated set when a commit was pushed. A tree equal to the branch pushes nothing. The
// request's title and description stay as they were.
//
// updating is the run that names the request (runs.merge_request_id), nil on the same-run
// path of 5.20 rule 2 (a run that opened the request it pushes to). The run's checkout_sha
// is the base its changes are counted from (5.16 rule 10, D-087). For a run that names the
// request it is also the branch guard's: the branch must still be there, else the push is
// refused with the text of rule 4 and nothing is pushed (D-050); on the same-run path a
// branch that moved gets the run's changes per path. After a push the run's own commit
// becomes its checkout_sha, the base of a later call. A null checkout_sha (a run cloned
// before migration 0013) skips the guard.
//
// followed is the record rec is the follow-up of (DESIGN 5.29 rule 4), nil when rec is the
// run's own request or the one it names.
func (m *mergeRequest) updateRecorded(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, rec *store.MergeRequest, in mergeRequestInput, checkout string, updating, run *store.Run, followed *store.MergeRequest) (Result, error) {
	if rec.State != store.MergeRequestOpen {
		// DESIGN 5.20 edge cases, 5.22 rule 3: nothing more can be pushed to a request that
		// was merged or closed; the model is told instead of git failing on the branch.
		return Result{}, &ModelError{Message: fmt.Sprintf("the run's %s is %s; nothing more can be pushed to it", rec.Label(), rec.State)}
	}
	out := mergeRequestOutput{Number: rec.Number, URL: rec.URL, Branch: rec.SourceBranch, Provider: rec.Provider, Replayed: true, Draft: rec.Draft,
		FollowUp: followed != nil}
	base, err := runBase(ctx, call, run)
	if err != nil {
		return Result{}, err
	}
	base.Guard = updating != nil
	proposal, err := pc.ProposeOntoPaths(ctx, checkout, rec.SourceBranch, base, in.Title, in.Paths)
	var moved *project.ErrBranchMoved
	var missing *project.ErrPathMissing
	var conflict *project.ErrConflict
	switch {
	case errors.As(err, &missing):
		return Result{}, &ModelError{Message: missing.Error()}
	case err == nil:
		out.Updated = true
		out.leftOut(base, proposal.LeftOut)
	case errors.Is(err, project.ErrNoChanges):
		// Nothing to push; a title, description or draft change still goes to the provider.
		out.leftOut(base, proposal.LeftOut)
	case errors.As(err, &moved):
		return Result{}, &ModelError{Message: moved.Error()}
	case errors.As(err, &conflict):
		return Result{}, &ModelError{Message: conflict.Error()}
	default:
		return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
	}
	if out.Updated && call.Tx != nil {
		if err := store.SetRunCheckoutSHA(ctx, call.Tx, call.RunID, proposal.Base); err != nil {
			return Result{IsError: true, Text: "record the new checkout commit after the push: " + obs.Redact(err.Error()) +
				"; the commit was pushed to " + rec.SourceBranch}, nil
		}
		// The check of the old head no longer speaks for the branch (Chief platform control,
		// DESIGN 5.34 rule 11): the record moves to the pushed commit with its CI pending.
		if _, err := store.MergeRequestPushed(ctx, call.Tx, rec.ID, proposal.Commit, time.Now(), false); err != nil {
			return Result{}, fmt.Errorf("record the push on the merge request: %w", err)
		}
	}
	if updating != nil && call.Tx != nil {
		// An iteration on a ticket's request: the request is linked already, so the ticket
		// goes back to review here (LinkTicketMergeRequest only runs for a new record).
		if _, err := store.ReturnTicketToReview(ctx, call.Tx, call.RunID); err != nil {
			return Result{}, fmt.Errorf("return the ticket to review: %w", err)
		}
	}
	if updating == nil {
		if err := m.updateMeta(ctx, call, pc, provider, rec, in, run, followed, &out); err != nil {
			return Result{}, err
		}
	}
	return JSONResult(out), nil
}

// updateMeta is the title, description and draft half of a later call (DESIGN 5.29 rule
// 4) for the run's own request. A call that names draft (true or false) is part of the
// draft protocol: the provider gets its title and description (with the "Closes #n" line
// a created request has, or the follow-up's own title and first paragraph for a follow-up,
// and the do-not-merge notice while the request stays a draft) and, when the state differs
// from the record's, the move between draft and ready; then the record follows. A call
// without draft keeps the earlier behaviour: the title is the commit message only and the
// request's text stays.
func (m *mergeRequest) updateMeta(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, rec *store.MergeRequest, in mergeRequestInput, run *store.Run, followed *store.MergeRequest, out *mergeRequestOutput) error {
	if in.Draft == nil {
		return nil
	}
	moves := *in.Draft != rec.Draft
	title, desc := in.Title, withIssueCloses(in.Description, run)
	if followed != nil {
		title, desc = followUpTitle(in.Title), followUpDescription(pc, followed, in.Description)
	}
	desc = withDraftNotice(pc, desc, *in.Draft)
	u := RequestUpdate{Title: &title, Description: &desc}
	if moves {
		u.Draft = in.Draft
	}
	if err := provider.Update(ctx, rec.Number, u); err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			return &ModelError{Message: fmt.Sprintf("the run's %s is gone from the provider", rec.Label())}
		}
		// The provider's refusal (its text carries no token) is the model's to read, not an
		// internal error (review L1).
		what := "updating"
		if moves {
			what = "marking as draft"
			if !*in.Draft {
				what = "marking as ready"
			}
		}
		return &ModelError{Message: fmt.Sprintf("%s the run's %s failed: %s", what, rec.Label(), err)}
	}
	draft := rec.Draft
	if moves {
		draft = *in.Draft
	}
	out.Draft = draft
	if call.Tx != nil {
		if err := store.SetMergeRequestDraft(ctx, call.Tx, rec.ID, title, draft); err != nil {
			return fmt.Errorf("record the merge request update: %w", err)
		}
	}
	return nil
}

// record writes the run's merge request record in the call's transaction (DESIGN 5.20
// rule 2): state open, ci_status none, never synced.
func (m *mergeRequest) record(ctx context.Context, call Call, pc *project.Config, out mergeRequestOutput, title string, draft bool) error {
	run, err := store.GetRun(ctx, call.Tx, call.RunID)
	if err != nil {
		return fmt.Errorf("record the merge request: %w", err)
	}
	if run.ProjectID == nil {
		return errors.New("record the merge request: the run names no project")
	}
	inserted, err := store.InsertMergeRequest(ctx, call.Tx, &store.MergeRequest{
		ProjectID: *run.ProjectID, RunID: call.RunID, Provider: string(pc.Provider), Number: out.Number, URL: out.URL,
		SourceBranch: out.Branch, TargetBranch: pc.DefaultBranch, Title: title, Draft: draft,
	})
	if err != nil {
		return fmt.Errorf("record the merge request: %w", err)
	}
	if !inserted {
		return fmt.Errorf("record the merge request: %d is already recorded for the project", out.Number)
	}
	return nil
}

// checkoutDir is <workspace>/runs/<run id>/repo, reached through an os.Root of the
// workspace with links refused on the way, holding a .git directory; problems the
// model can fix are *ModelError.
func checkoutDir(workspace, runID string) (string, error) {
	if workspace == "" || runID == "" {
		return "", errors.New("no workspace for this call")
	}
	rel := project.RepoRel(runID)
	ws, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer ws.Close()
	info, err := statNoLinks(ws, rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", &ModelError{Message: "this run has no project checkout at " + rel + "/"}
	case errors.Is(err, errLink):
		return "", &ModelError{Message: "the checkout at " + rel + "/ is a link; the platform's checkout is a directory"}
	case err != nil:
		return "", err
	case !info.IsDir():
		return "", &ModelError{Message: rel + " is not a directory"}
	}
	git, err := ws.Lstat(filepath.Join(rel, ".git"))
	if err != nil || !git.IsDir() {
		return "", &ModelError{Message: "this run has no project checkout at " + rel + "/: no .git directory"}
	}
	return filepath.Join(workspace, rel), nil
}
