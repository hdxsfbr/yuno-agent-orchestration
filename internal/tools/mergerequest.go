package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
)

// OpenMergeRequestName proposes a run's project checkout as a merge request (DESIGN
// 5.16 rule 10, D-037): GitLab or GitHub behind one interface.
const OpenMergeRequestName = "open_merge_request"

// ProjectResolver gives the tool the project of the run that calls it (DESIGN 5.17
// rule 6); the project package's Resolver implements it.
type ProjectResolver interface {
	ForRunID(ctx context.Context, runID string) (*project.Config, error)
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
    "description": {"type": "string", "maxLength": 20000, "description": "The detail: what changed and why, how it was tested, anything the reviewer should know."}
  },
  "required": ["run_token", "title", "description"],
  "additionalProperties": false
}`)

type mergeRequestInput struct {
	Title       string `json:"title"`
	Description string `json:"description"`
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
		Description: "Propose the changes in the project checkout (runs/<run_id>/repo/) as a merge request (GitLab) " +
			"or pull request (GitHub) against the project's default branch: the platform commits the working " +
			"tree, pushes a branch and opens the request with your title and description. The title is a short " +
			"summary of the change, like a commit subject (at most 72 characters), never the task text; put " +
			"what changed and why and how it was tested in the description. A run has one request: calling " +
			"this again in the same run adds the working tree to that request as a new commit (the answer then " +
			"has replayed and updated true) and never opens a second one. Needs a human approval unless " +
			"the operator allowed it to run automatically. Returns the request's number and URL; if a human " +
			"has not decided yet it returns status pending_approval, and if they refused it returns status " +
			"rejected with a reason.",
		InputSchema: mergeRequestSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     2 * time.Minute,
		Describe: func(input json.RawMessage) string {
			var in mergeRequestInput
			_ = json.Unmarshal(input, &in)
			return fmt.Sprintf("Open merge request %q (a pull request on GitHub) on the run's project", in.Title)
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

// run is open_merge_request after the checks (DESIGN 5.16 rule 10, 5.20 rule 2). A run
// that has a request already pushes onto its branch and opens no second one; otherwise
// the branch is pushed, the request found or opened, and the record written in the
// side effect's transaction right after the provider answered, so a failed insert rolls
// the result back and releases the key. Without a transaction (the tool called on its
// own, as the unit tests of the provider flow do) no record is read or written.
func (m *mergeRequest) run(ctx context.Context, call Call, pc *project.Config, provider RequestProvider, in mergeRequestInput, checkout, branch string) (Result, error) {
	if call.Tx != nil {
		// Two calls of one run with different keys must not both find no record and open
		// two requests: the second waits for the first's transaction, then finds the record.
		if err := store.LockRunMergeRequest(ctx, call.Tx, call.RunID); err != nil {
			return Result{}, err
		}
		rec, err := store.MergeRequestByRun(ctx, call.Tx, call.RunID)
		if err != nil {
			return Result{}, err
		}
		if rec != nil {
			return m.updateRecorded(ctx, pc, rec, in, checkout)
		}
	}
	exists, err := provider.BranchExists(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if !exists {
		if _, err := pc.Propose(ctx, checkout, branch, in.Title); err != nil {
			if errors.Is(err, project.ErrNoChanges) {
				return Result{}, &ModelError{Message: err.Error()}
			}
			// A push the provider refused (a token without write access, a protected
			// branch) or a network failure: the text names the git step, the token
			// never appears in it (project.Config redacts git's output).
			return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
		}
	}
	out := mergeRequestOutput{Branch: branch, Provider: string(pc.Provider)}
	open, err := provider.FindOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		out.Number, out.URL, out.Replayed = open.Number, open.URL, true
	} else {
		ref, err := provider.Create(ctx, branch, pc.DefaultBranch, in.Title, in.Description)
		if err != nil {
			return Result{}, err
		}
		out.Number, out.URL = ref.Number, ref.URL
	}
	if call.Tx != nil {
		if err := m.record(ctx, call, pc, out, in.Title); err != nil {
			return Result{}, err
		}
	}
	return JSONResult(out), nil
}

// updateRecorded answers a later call of a run that has its request (DESIGN 5.20 rule
// 2): the working tree goes onto the record's source branch as a new commit with the
// title as its message, and the answer is the record's, replayed, with updated set when
// a commit was pushed. A tree equal to the branch pushes nothing. The request's title
// and description stay as they were.
func (m *mergeRequest) updateRecorded(ctx context.Context, pc *project.Config, rec *store.MergeRequest, in mergeRequestInput, checkout string) (Result, error) {
	if rec.State != store.MergeRequestOpen {
		// DESIGN 5.20 edge cases: nothing more can be pushed to a request that was
		// merged or closed; the model is told instead of git failing on the branch.
		return Result{}, &ModelError{Message: fmt.Sprintf("the run's %s %s%d is %s; nothing more can be pushed to it",
			requestNoun(rec.Provider), requestSigil(rec.Provider), rec.Number, rec.State)}
	}
	out := mergeRequestOutput{Number: rec.Number, URL: rec.URL, Branch: rec.SourceBranch, Provider: rec.Provider, Replayed: true}
	// No branch guard here yet: "" is the same-run path of DESIGN 5.20 rule 2 (5.22 rule 4
	// passes the run's checkout_sha for a run that cloned the branch it pushes to).
	_, err := pc.ProposeOnto(ctx, checkout, rec.SourceBranch, "", in.Title)
	switch {
	case err == nil:
		out.Updated = true
	case errors.Is(err, project.ErrNoChanges):
	default:
		return Result{IsError: true, Text: "propose the changes: " + err.Error()}, nil
	}
	return JSONResult(out), nil
}

// record writes the run's merge request record in the call's transaction (DESIGN 5.20
// rule 2): state open, ci_status none, never synced.
func (m *mergeRequest) record(ctx context.Context, call Call, pc *project.Config, out mergeRequestOutput, title string) error {
	run, err := store.GetRun(ctx, call.Tx, call.RunID)
	if err != nil {
		return fmt.Errorf("record the merge request: %w", err)
	}
	if run.ProjectID == nil {
		return errors.New("record the merge request: the run names no project")
	}
	inserted, err := store.InsertMergeRequest(ctx, call.Tx, &store.MergeRequest{
		ProjectID: *run.ProjectID, RunID: call.RunID, Provider: string(pc.Provider), Number: out.Number, URL: out.URL,
		SourceBranch: out.Branch, TargetBranch: pc.DefaultBranch, Title: title,
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

// requestNoun and requestSigil are what the provider calls a request and how it numbers
// it: "merge request !12" on GitLab, "pull request #12" on GitHub.
func requestNoun(provider string) string {
	if provider == "github" {
		return "pull request"
	}
	return "merge request"
}

func requestSigil(provider string) string {
	if provider == "github" {
		return "#"
	}
	return "!"
}
