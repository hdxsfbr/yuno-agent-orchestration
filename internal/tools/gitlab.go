package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitLabCreateMRName opens a merge request with run artifacts (DESIGN 5.7 table, D-010).
const GitLabCreateMRName = "gitlab_create_mr"

// GitLabConfig is what gitlab_create_mr needs; the tool is registered only when all
// three of GITLAB_BASE_URL, GITLAB_PROJECT_ID and GITLAB_TOKEN are set.
type GitLabConfig struct {
	BaseURL   string
	ProjectID string
	Token     string
	MaxBytes  int64
	Client    *http.Client
}

var gitlabSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "title": {"type": "string", "minLength": 1, "maxLength": 255, "description": "Merge request title."},
    "description": {"type": "string", "maxLength": 20000, "description": "Merge request description."},
    "paths": {"type": "array", "minItems": 1, "maxItems": 200,
      "items": {"type": "string", "minLength": 1, "maxLength": 200},
      "description": "Files or directories to commit, relative to runs/<run_id>/ in your workspace; each lands at the same path in the repository."}
  },
  "required": ["run_token", "title", "description", "paths"],
  "additionalProperties": false
}`)

type gitlabInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
}

// gitlabOutput is the tool's result: {merge_request_iid, url, branch, replayed?}.
type gitlabOutput struct {
	IID      int64  `json:"merge_request_iid"`
	URL      string `json:"url"`
	Branch   string `json:"branch"`
	Replayed bool   `json:"replayed,omitempty"`
}

// GitLabBranch is the branch a call commits to: yuno/ plus the first 12 hex characters
// of its idempotency key, so a repeated call finds its own branch (DESIGN 5.4 rule 3).
func GitLabBranch(key string) string {
	if len(key) > 12 {
		key = key[:12]
	}
	return "yuno/" + key
}

// NewGitLabCreateMR builds gitlab_create_mr: commit the files to the key's branch
// through the repository commits API, then open a merge request into the default
// branch. A second call with the same key finds the branch (no second commit) and the
// open merge request (no second MR) and reports replayed.
func NewGitLabCreateMR(cfg GitLabConfig) *Tool {
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	g := &gitlab{cfg: cfg, api: &apiClient{what: "gitlab", client: client, authorize: privateTokenAuth(cfg.Token)}}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(base, "/api/v4") {
		base += "/api/v4"
	}
	g.project = base + "/projects/" + url.PathEscape(cfg.ProjectID)
	return &Tool{
		Name: GitLabCreateMRName,
		Description: "Commit files from your run directory to a new branch and open a GitLab merge request. Needs " +
			"a human approval unless the operator allowed it to run automatically. Returns the merge request number " +
			"and URL; if a human has not decided yet it returns status pending_approval: then end your turn " +
			"without finishing, and the platform resumes you with the outcome; if they refused it returns " +
			"status rejected with a reason.",
		InputSchema: gitlabSchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsExternal,
		Timeout:     30 * time.Second,
		Describe: func(input json.RawMessage) string {
			var in gitlabInput
			_ = json.Unmarshal(input, &in)
			return fmt.Sprintf("Open merge request %q (%s)", in.Title, summarizePaths(in.Paths))
		},
		Exec: g.exec,
	}
}

type gitlab struct {
	cfg     GitLabConfig
	api     *apiClient
	project string
}

func (g *gitlab) exec(ctx context.Context, call Call) (Result, error) {
	var in gitlabInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	if call.IdempotencyKey == "" {
		return Result{}, errors.New("gitlab_create_mr: no idempotency key")
	}
	files, err := packageRunFiles(call.Workspace, call.RunID, in.Paths, g.cfg.MaxBytes)
	if err != nil {
		return Result{}, err
	}
	branch := GitLabBranch(call.IdempotencyKey)
	res, err := g.run(ctx, in, files, branch)
	var se *errAPIStatus
	if errors.As(err, &se) {
		return se.res, nil
	}
	return res, err
}

func (g *gitlab) run(ctx context.Context, in gitlabInput, files []packagedFile, branch string) (Result, error) {
	var project struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, g.project, nil, &project); err != nil {
		return Result{}, err
	}
	if project.DefaultBranch == "" {
		return Result{IsError: true, Text: "gitlab: the project has no default branch"}, nil
	}
	status, err := g.api.do(ctx, http.MethodGet, g.project+"/repository/branches/"+url.PathEscape(branch), nil, nil)
	if err != nil && status != http.StatusNotFound {
		return Result{}, err
	}
	if status == http.StatusNotFound {
		if err := g.commit(ctx, in, files, branch, project.DefaultBranch); err != nil {
			return Result{}, err
		}
	}
	var open []struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	q := url.Values{"source_branch": {branch}, "state": {"opened"}}
	if _, err := g.api.do(ctx, http.MethodGet, g.project+"/merge_requests?"+q.Encode(), nil, &open); err != nil {
		return Result{}, err
	}
	if len(open) > 0 {
		return JSONResult(gitlabOutput{IID: open[0].IID, URL: open[0].WebURL, Branch: branch, Replayed: true}), nil
	}
	var mr struct {
		IID    int64  `json:"iid"`
		WebURL string `json:"web_url"`
	}
	body := map[string]any{"source_branch": branch, "target_branch": project.DefaultBranch, "title": in.Title,
		"description": in.Description, "remove_source_branch": true}
	if _, err := g.api.do(ctx, http.MethodPost, g.project+"/merge_requests", body, &mr); err != nil {
		return Result{}, err
	}
	return JSONResult(gitlabOutput{IID: mr.IID, URL: mr.WebURL, Branch: branch}), nil
}

// commit creates the branch from the default branch with one commit holding every
// file, created or updated depending on whether the default branch has it.
func (g *gitlab) commit(ctx context.Context, in gitlabInput, files []packagedFile, branch, defaultBranch string) error {
	actions := make([]map[string]string, len(files))
	for i, f := range files {
		action := "create"
		u := g.project + "/repository/files/" + url.PathEscape(f.Path) + "?ref=" + url.QueryEscape(defaultBranch)
		status, err := g.api.do(ctx, http.MethodHead, u, nil, nil)
		switch {
		case err == nil:
			action = "update"
		case status != http.StatusNotFound:
			return err
		}
		actions[i] = map[string]string{"action": action, "file_path": f.Path, "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString(f.Content)}
	}
	body := map[string]any{"branch": branch, "start_branch": defaultBranch, "commit_message": in.Title, "actions": actions}
	_, err := g.api.do(ctx, http.MethodPost, g.project+"/repository/commits", body, nil)
	return err
}
