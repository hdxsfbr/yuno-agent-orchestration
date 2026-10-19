package evals

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"yuno/internal/store"
)

// ProjectRequest is the body of POST /api/projects the runner sends (DESIGN 5.17): a
// project on the fake forge, with the intake settings of the case.
type ProjectRequest struct {
	Name             string         `json:"name"`
	RepoURL          string         `json:"repo_url"`
	Provider         string         `json:"provider"`
	DefaultBranch    string         `json:"default_branch"`
	Token            string         `json:"token"`
	MaxCostUSDPerDay *float64       `json:"max_cost_usd_per_day,omitempty"`
	Intake           *IntakeRequest `json:"intake,omitempty"`
	Description      string         `json:"description,omitempty"`
}

// IntakeRequest is the intake member of a ProjectRequest.
type IntakeRequest struct {
	Enabled    bool   `json:"enabled"`
	Label      string `json:"label"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

// CreateProject registers a project.
func (c *Client) CreateProject(ctx context.Context, req ProjectRequest) (*store.Project, error) {
	var out store.Project
	if err := c.do(ctx, http.MethodPost, "/api/projects", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("POST /api/projects: response has no id")
	}
	return &out, nil
}

// DeleteProject deletes a project. One that is already gone is not an error.
func (c *Client) DeleteProject(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/projects/"+url.PathEscape(id), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// PollIntake asks the api to poll the project's issues now.
func (c *Client) PollIntake(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(id)+"/intake/poll", nil, nil)
}

// FindWorkflowID is the id of the workflow with this name.
func (c *Client) FindWorkflowID(ctx context.Context, name string) (string, error) {
	var list []store.Workflow
	if err := c.do(ctx, http.MethodGet, "/api/workflows", nil, &list); err != nil {
		return "", err
	}
	for _, w := range list {
		if w.Name == name {
			return w.ID, nil
		}
	}
	return "", errors.New("no workflow is named " + name)
}

// ListProjectRuns lists the project's runs with the status ("" for any).
func (c *Client) ListProjectRuns(ctx context.Context, projectID, status string) ([]store.Run, error) {
	q := url.Values{"project_id": {projectID}, "limit": {"200"}}
	if status != "" {
		q.Set("status", status)
	}
	var out []store.Run
	if err := c.do(ctx, http.MethodGet, "/api/runs?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PlanView is a plan with its events, as GET /api/plans/{id} shows it.
type PlanView struct {
	store.Plan
	Tickets  []store.PlanTicket `json:"tickets"`
	Events   []store.PlanEvent  `json:"events"`
	ShipRuns []store.ShipRun    `json:"ship_runs"`
}

// PauseWaves pauses the project's waves; ResumeWaves resumes them (DESIGN 5.29 rule 9).
func (c *Client) PauseWaves(ctx context.Context, projectID, reason string) error {
	return c.do(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(projectID)+"/waves/pause", map[string]string{"reason": reason}, nil)
}

// ResumeWaves resumes the project's paused waves.
func (c *Client) ResumeWaves(ctx context.Context, projectID string) error {
	return c.do(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(projectID)+"/waves/resume", nil, nil)
}

// ListPlans lists the project's plans, newest first.
func (c *Client) ListPlans(ctx context.Context, projectID string) ([]store.Plan, error) {
	var out []store.Plan
	if err := c.do(ctx, http.MethodGet, "/api/projects/"+url.PathEscape(projectID)+"/plans", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPlan reads a plan with its events.
func (c *Client) GetPlan(ctx context.Context, id string) (*PlanView, error) {
	var out PlanView
	if err := c.do(ctx, http.MethodGet, "/api/plans/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelPlan cancels an open plan; one that is finished already (409) is not an error.
func (c *Client) CancelPlan(ctx context.Context, id, reason string) error {
	err := c.do(ctx, http.MethodPost, "/api/plans/"+url.PathEscape(id)+"/cancel", map[string]string{"reason": reason}, nil)
	if statusOf(err) == http.StatusConflict {
		return nil
	}
	return err
}
