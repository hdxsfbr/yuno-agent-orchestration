package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// forgeClient is the runner's view of the fake forge (DESIGN 5.29 rule 17): the control
// endpoints under /_forge/ and the issue endpoint of the GitLab subset, with the forge's
// one token.
type forgeClient struct {
	base  string
	token string
	http  *http.Client
}

func newForgeClient(base, token string) *forgeClient {
	return &forgeClient{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// forgeIssue is an issue as the forge lists it.
type forgeIssue struct {
	IID    int      `json:"iid"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

// forgeMR is a merge request the forge merged.
type forgeMR struct {
	IID      int     `json:"iid"`
	Title    string  `json:"title"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
}

// CreateRepo makes the repository with its first commit.
func (f *forgeClient) CreateRepo(ctx context.Context, path, branch string) error {
	return f.do(ctx, http.MethodPost, "/_forge/repos", map[string]string{"path": path, "default_branch": branch}, nil)
}

// CreateIssue opens an issue through the GitLab subset.
func (f *forgeClient) CreateIssue(ctx context.Context, path string, is IssueSeed) error {
	body := map[string]any{"title": is.Title, "description": is.Body}
	if len(is.Labels) > 0 {
		body["labels"] = strings.Join(is.Labels, ",")
	}
	return f.do(ctx, http.MethodPost, "/api/v4/projects/"+url.PathEscape(path)+"/issues", body, nil)
}

// Issues lists every issue with its labels and state.
func (f *forgeClient) Issues(ctx context.Context, path string) ([]forgeIssue, error) {
	var out []forgeIssue
	err := f.do(ctx, http.MethodGet, "/_forge/repos/"+path+"/issues", nil, &out)
	return out, err
}

// MergeOpen merges the open, non-draft merge requests and returns the ones it merged.
func (f *forgeClient) MergeOpen(ctx context.Context, path string) ([]forgeMR, error) {
	var out []forgeMR
	err := f.do(ctx, http.MethodPost, "/_forge/repos/"+path+"/merge-open", nil, &out)
	return out, err
}

// Pipeline records a pipeline on the default branch's head.
func (f *forgeClient) Pipeline(ctx context.Context, path, state string) error {
	return f.do(ctx, http.MethodPost, "/_forge/repos/"+path+"/pipeline", map[string]string{"state": state}, nil)
}

func (f *forgeClient) do(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.base+p, body)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", f.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("forge %s %s: %w", method, p, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("forge %s %s: %w", method, p, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message any `json:"message"`
		}
		_ = json.Unmarshal(raw, &msg)
		return fmt.Errorf("forge %s %s: %d %v", method, p, resp.StatusCode, msg.Message)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("forge %s %s: decode: %w", method, p, err)
		}
	}
	return nil
}
