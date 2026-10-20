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
// it with replayed, and with updated when it pushed a commit onto its branch.
type mergeRequestOutput struct {
	Number   int64  `json:"number"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Provider string `json:"provider"`
	Replayed bool   `json:"replayed,omitempty"`
	Updated  bool   `json:"updated,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
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
			"call of the same run that names draft also replaces the request's title and description. Needs a human " +
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
				return m.updateRecorded(ctx, call, pc, provider, named, in, checkout, run, run)
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
			return m.updateRecorded(ctx, call, pc, provider, rec, in, checkout, nil, run)
		}
	}
	exists, err := provider.BranchExists(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		if _, err := pc.ProposePaths(ctx, checkout, branch, in.Title, in.Paths); err != nil {
			var missing *project.ErrPathMissing
			if errors.Is(err, project.ErrNoChanges) || errors.As(err, &missing) {
				return Result{}, &ModelError{Message: err.Error()}
			}
			// A push the provider refused (a token without write access, a protected
			// branch) or a network failure: the text names the git step, the token
			// never appears in it (project.Config redacts git's output).
			return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
		}
	}
	draft := in.Draft != nil && *in.Draft
	out := mergeRequestOutput{Branch: branch, Provider: string(pc.Provider), Draft: draft}
	open, err := provider.FindOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		out.Number, out.URL, out.Replayed = open.Number, open.URL, true
	} else {
		ref, err := provider.Create(ctx, branch, pc.DefaultBranch, in.Title, withIssueCloses(in.Description, run), draft)
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

// updateRecorded answers a later call of a run that has a request to push onto (DESIGN
// 5.20 rule 2, 5.22 rule 4): the working tree goes onto the record's source branch as a new
// commit with the title as its message, and the answer is the record's, replayed, with
// updated set when a commit was pushed. A tree equal to the branch pushes nothing. The
// request's title and description stay as they were.
//
// updating is the run that names the request (runs.merge_request_id), nil on the same-run
// path of 5.20 rule 2 (a run that opened the request it pushes to; L4 stays recorded for
// it). The run's checkout_sha is the branch guard's base: the branch must still be at the
// commit the run cloned it at, else the push is refused with the text of rule 4 and
// nothing is pushed (D-050), and after a push the new commit becomes the run's
// checkout_sha so that a later call passes the guard. A null checkout_sha (a run cloned
// before migration 0013) skips the guard.
func (m *mergeRequest) updateRecorded(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, rec *store.MergeRequest, in mergeRequestInput, checkout string, updating, run *store.Run) (Result, error) {
	if rec.State != store.MergeRequestOpen {
		// DESIGN 5.20 edge cases, 5.22 rule 3: nothing more can be pushed to a request that
		// was merged or closed; the model is told instead of git failing on the branch.
		return Result{}, &ModelError{Message: fmt.Sprintf("the run's %s is %s; nothing more can be pushed to it", rec.Label(), rec.State)}
	}
	out := mergeRequestOutput{Number: rec.Number, URL: rec.URL, Branch: rec.SourceBranch, Provider: rec.Provider, Replayed: true, Draft: rec.Draft}
	base := ""
	if updating != nil && updating.CheckoutSHA != nil {
		base = *updating.CheckoutSHA
	}
	proposal, err := pc.ProposeOntoPaths(ctx, checkout, rec.SourceBranch, base, in.Title, in.Paths)
	var moved *project.ErrBranchMoved
	var missing *project.ErrPathMissing
	switch {
	case errors.As(err, &missing):
		return Result{}, &ModelError{Message: missing.Error()}
	case err == nil:
		out.Updated = true
	case errors.Is(err, project.ErrNoChanges):
		// Nothing to push; a title, description or draft change still goes to the provider.
	case errors.As(err, &moved):
		return Result{}, &ModelError{Message: moved.Error()}
	default:
		return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
	}
	if out.Updated && updating != nil && call.Tx != nil {
		if err := store.SetRunCheckoutSHA(ctx, call.Tx, call.RunID, proposal.Commit); err != nil {
			return Result{IsError: true, Text: "record the new checkout commit after the push: " + obs.Redact(err.Error()) +
				"; the commit was pushed to " + rec.SourceBranch}, nil
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
		if err := m.updateMeta(ctx, call, provider, rec, in, run, &out); err != nil {
			return Result{}, err
		}
	}
	return JSONResult(out), nil
}

// updateMeta is the title, description and draft half of a later call (DESIGN 5.29 rule
// 4) for the run's own request. A call that names draft (true or false) is part of the
// draft protocol: the provider gets its title and description (with the "Closes #n" line
// a created request has) and, when the state differs from the record's, the move between
// draft and ready; then the record follows. A call without draft keeps the earlier
// behaviour: the title is the commit message only and the request's text stays.
func (m *mergeRequest) updateMeta(ctx context.Context, call Call, provider RequestProvider, rec *store.MergeRequest, in mergeRequestInput, run *store.Run, out *mergeRequestOutput) error {
	if in.Draft == nil {
		return nil
	}
	moves := *in.Draft != rec.Draft
	desc := withIssueCloses(in.Description, run)
	u := RequestUpdate{Title: &in.Title, Description: &desc}
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
		if err := store.SetMergeRequestDraft(ctx, call.Tx, rec.ID, in.Title, draft); err != nil {
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
