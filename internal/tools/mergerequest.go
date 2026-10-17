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

// mergeRequestOutput is the tool's result: {number, url, branch, provider, replayed?}.
type mergeRequestOutput struct {
	Number   int64  `json:"number"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Provider string `json:"provider"`
	Replayed bool   `json:"replayed,omitempty"`
}

// requestRef is an open merge or pull request as the provider reports it.
type requestRef struct {
	Number int64
	URL    string
}

// requestProvider is the slice of a provider's API the tool uses (DESIGN 5.16 rule
// 11). A non-2xx answer is an *errAPIStatus carrying the result for the model.
type requestProvider interface {
	branchExists(ctx context.Context, branch string) (bool, error)
	findOpen(ctx context.Context, branch string) (*requestRef, error)
	create(ctx context.Context, branch, base, title, description string) (*requestRef, error)
}

// NewOpenMergeRequest builds open_merge_request: commit the run's checkout on the key's
// branch and push it from the api, then open the request against the default branch
// of the run's project (DESIGN 5.17 rule 6). A second call with the same key finds the
// branch (no second push) and the open request (no second one) and reports replayed.
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
			"what changed and why and how it was tested in the description. Needs a human approval unless " +
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
func (m *mergeRequest) providerFor(pc *project.Config) requestProvider {
	base, path := m.cfg.APIBase, m.cfg.RepoPath
	if base == "" {
		base = pc.APIBase()
	}
	if path == "" {
		path = pc.Path()
	}
	switch pc.Provider {
	case project.ProviderGitHub:
		return &githubProvider{api: &apiClient{what: "github", client: m.cfg.Client, authorize: bearerAuth(pc.Token)}, base: base, owner: ownerOf(path)}
	default:
		return &gitlabProvider{api: &apiClient{what: "gitlab", client: m.cfg.Client, authorize: privateTokenAuth(pc.Token)}, base: base}
	}
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
	res, err := m.run(ctx, pc, m.providerFor(pc), in, checkout, branch)
	var se *errAPIStatus
	if errors.As(err, &se) {
		return se.res, nil
	}
	return res, err
}

func (m *mergeRequest) run(ctx context.Context, pc *project.Config, provider requestProvider, in mergeRequestInput, checkout, branch string) (Result, error) {
	exists, err := provider.branchExists(ctx, branch)
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
	open, err := provider.findOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		return JSONResult(mergeRequestOutput{Number: open.Number, URL: open.URL, Branch: branch, Provider: string(pc.Provider), Replayed: true}), nil
	}
	ref, err := provider.create(ctx, branch, pc.DefaultBranch, in.Title, in.Description)
	if err != nil {
		return Result{}, err
	}
	return JSONResult(mergeRequestOutput{Number: ref.Number, URL: ref.URL, Branch: branch, Provider: string(pc.Provider)}), nil
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
