package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DeployReleaseName publishes run artifacts to the deploy target (DESIGN 5.7 table, D-010).
const DeployReleaseName = "deploy_release"

// DeployConfig is what deploy_release needs.
type DeployConfig struct {
	// TargetURL is the deploy target's base URL (DEPLOYTARGET_URL).
	TargetURL string
	// MaxBytes caps the packaged files (DEPLOYTARGET_MAX_MB).
	MaxBytes int64
	// MaxRequestBytes is what the target accepts in one request body; 0 skips the check.
	MaxRequestBytes int64
	// PublicBaseURL prefixes the release URL the model reports; the web server proxies
	// /deploy/ to the target.
	PublicBaseURL string
	Client        *http.Client
}

var deploySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "version": {"type": "string", "minLength": 1, "maxLength": 64, "description": "Release version, e.g. 1.0.0."},
    "notes": {"type": "string", "maxLength": 4000, "description": "Release notes."},
    "paths": {"type": "array", "minItems": 1, "maxItems": 200,
      "items": {"type": "string", "minLength": 1, "maxLength": 200},
      "description": "Files or directories to publish, relative to runs/<run_id>/ in your workspace."}
  },
  "required": ["run_token", "version", "notes", "paths"],
  "additionalProperties": false
}`)

type deployInput struct {
	Version string   `json:"version"`
	Notes   string   `json:"notes"`
	Paths   []string `json:"paths"`
}

type deployRequest struct {
	Version string       `json:"version"`
	Notes   string       `json:"notes"`
	Files   []deployFile `json:"files"`
}

type deployFile struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

// deployOutput is the tool's result: {release_id, version, url, replayed?}.
type deployOutput struct {
	ReleaseID string `json:"release_id"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	Replayed  bool   `json:"replayed,omitempty"`
}

// NewDeployRelease builds deploy_release: it packages files from the run directory and
// POSTs them to <target>/releases with the call's idempotency key, which the target
// enforces too (DESIGN 5.4 rules 3 and 4).
func NewDeployRelease(cfg DeployConfig) *Tool {
	client := cfg.Client
	if client == nil {
		client = &http.Client{}
	}
	return &Tool{
		Name: DeployReleaseName,
		Description: "Publish files from your run directory as a release on the deploy target. Needs a human " +
			"approval unless the operator allowed it to run automatically. Returns the release id, version and URL; " +
			"if a human has not decided yet it returns status pending_approval, and if they refused it returns " +
			"status rejected with a reason.",
		InputSchema: deploySchema,
		Risk:        RiskSideEffecting,
		Hits:        HitsLocalService,
		Timeout:     30 * time.Second,
		Describe: func(input json.RawMessage) string {
			var in deployInput
			_ = json.Unmarshal(input, &in)
			return fmt.Sprintf("Deploy release %s (%s)", in.Version, summarizePaths(in.Paths))
		},
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in deployInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			if call.IdempotencyKey == "" {
				return Result{}, errors.New("deploy_release: no idempotency key")
			}
			files, err := packageRunFiles(call.Workspace, call.RunID, in.Paths, cfg.MaxBytes)
			if err != nil {
				return Result{}, err
			}
			req := deployRequest{Version: in.Version, Notes: in.Notes, Files: make([]deployFile, len(files))}
			for i, f := range files {
				req.Files[i] = deployFile{Path: f.Path, ContentB64: base64.StdEncoding.EncodeToString(f.Content)}
			}
			body, err := json.Marshal(req)
			if err != nil {
				return Result{}, err
			}
			if cfg.MaxRequestBytes > 0 && int64(len(body)) > cfg.MaxRequestBytes {
				return Result{}, &ModelError{Message: fmt.Sprintf("the release is too large for the deploy target (%d bytes encoded, at most %d)",
					len(body), cfg.MaxRequestBytes)}
			}
			hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.TargetURL, "/")+"/releases", bytes.NewReader(body))
			if err != nil {
				return Result{}, err
			}
			hreq.Header.Set("Content-Type", "application/json")
			hreq.Header.Set("Idempotency-Key", call.IdempotencyKey)
			resp, err := client.Do(hreq)
			if err != nil {
				return transportFailure("deploy target", err), nil
			}
			defer resp.Body.Close()
			raw, err := readBody(resp, 1<<20)
			if err != nil {
				return transportFailure("deploy target", err), nil
			}
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return statusFailure("deploy target", resp, raw), nil
			}
			var rel struct {
				ID      string `json:"release_id"`
				Version string `json:"version"`
				URL     string `json:"url"`
			}
			if r := decodeJSONBody("deploy target", raw, &rel); r != nil {
				return *r, nil
			}
			if rel.ID == "" {
				return Result{IsError: true, Text: "deploy target: the response has no release_id"}, nil
			}
			return JSONResult(deployOutput{
				ReleaseID: rel.ID,
				Version:   rel.Version,
				URL:       releaseURL(cfg.PublicBaseURL, rel.URL),
				// 200 is the target replaying an earlier release for this key (DESIGN 5.4 rule 4).
				Replayed: resp.StatusCode == http.StatusOK,
			}), nil
		},
	}
}

// releaseURL is where a person opens the release: the web server serves the deploy
// target under /deploy/.
func releaseURL(publicBase, targetPath string) string {
	if targetPath == "" || strings.HasPrefix(targetPath, "http://") || strings.HasPrefix(targetPath, "https://") {
		return targetPath
	}
	return strings.TrimRight(publicBase, "/") + "/deploy/" + strings.TrimLeft(targetPath, "/")
}

// summarizePaths names up to three paths for an approval message.
func summarizePaths(paths []string) string {
	switch n := len(paths); {
	case n == 0:
		return "no files"
	case n <= 3:
		return strings.Join(paths, ", ")
	default:
		return fmt.Sprintf("%s and %d more", strings.Join(paths[:3], ", "), n-3)
	}
}
