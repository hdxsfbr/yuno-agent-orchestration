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

// MergeRequestConfig is what open_merge_request needs; the tool is registered only when
// the project is configured.
type MergeRequestConfig struct {
	Project project.Config
	// APIBase and RepoPath override Project.APIBase() and Project.Path() (tests point
	// them at a fake while git talks to a local repository).
	APIBase  string
	RepoPath string
	Client   *http.Client
}

var mergeRequestSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "title": {"type": "string", "minLength": 1, "maxLength": 255, "description": "Title of the merge request; also the commit message."},
    "description": {"type": "string", "maxLength": 20000, "description": "Description of the merge request: what changed and why, how it was tested."}
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
// branch and push it from the api, then open the request against the default branch.
// A second call with the same key finds the branch (no second push) and the open
// request (no second one) and reports replayed.
func NewOpenMergeRequest(cfg MergeRequestConfig) *Tool {
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	base, path := cfg.APIBase, cfg.RepoPath
	if base == "" {
		base = cfg.Project.APIBase()
	}
	if path == "" {
		path = cfg.Project.Path()
	}
	var p requestProvider
	switch cfg.Project.Provider {
	case project.ProviderGitHub:
		p = &githubProvider{api: &apiClient{what: "github", client: client, authorize: bearerAuth(cfg.Project.Token)}, base: base, owner: ownerOf(path)}
	default:
		p = &gitlabProvider{api: &apiClient{what: "gitlab", client: client, authorize: privateTokenAuth(cfg.Project.Token)}, base: base}
	}
	m := &mergeRequest{cfg: cfg, provider: p}
	what := cfg.Project.RequestName()
	return &Tool{
		Name: OpenMergeRequestName,
		Description: "Propose the changes in the project checkout (runs/<run_id>/repo/) as a " + what + " against the " +
			"project's default branch: the platform commits the working tree, pushes a branch and opens the " + what +
			" with your title and description. Needs a human approval unless the operator allowed it to run " +
			"automatically. Returns the " + what + " number and URL; if a human has not decided yet it returns status " +
			"pending_approval, and if they refused it returns status rejected with a reason.",
		InputSchema: mergeRequestSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     2 * time.Minute,
		Describe: func(input json.RawMessage) string {
			var in mergeRequestInput
			_ = json.Unmarshal(input, &in)
			return fmt.Sprintf("Open %s %q in %s", what, in.Title, path)
		},
		Exec: m.exec,
	}
}

type mergeRequest struct {
	cfg      MergeRequestConfig
	provider requestProvider
}

func (m *mergeRequest) exec(ctx context.Context, call Call) (Result, error) {
	var in mergeRequestInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	if call.IdempotencyKey == "" {
		return Result{}, errors.New("open_merge_request: no idempotency key")
	}
	checkout, err := checkoutDir(call.Workspace, call.RunID)
	if err != nil {
		return Result{}, err
	}
	branch := GitLabBranch(call.IdempotencyKey)
	res, err := m.run(ctx, in, checkout, branch)
	var se *errAPIStatus
	if errors.As(err, &se) {
		return se.res, nil
	}
	return res, err
}

func (m *mergeRequest) run(ctx context.Context, in mergeRequestInput, checkout, branch string) (Result, error) {
	pc := m.cfg.Project
	exists, err := m.provider.branchExists(ctx, branch)
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
	open, err := m.provider.findOpen(ctx, branch)
	if err != nil {
		return Result{}, err
	}
	if open != nil {
		return JSONResult(mergeRequestOutput{Number: open.Number, URL: open.URL, Branch: branch, Provider: string(pc.Provider), Replayed: true}), nil
	}
	ref, err := m.provider.create(ctx, branch, pc.DefaultBranch, in.Title, in.Description)
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
