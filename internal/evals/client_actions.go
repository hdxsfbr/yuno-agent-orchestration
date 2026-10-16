package evals

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// ScheduleRequest is the body of POST /api/schedules the runner sends (DESIGN 5.9): a
// one-shot schedule of a workflow. RunAt is an absolute UTC timestamp.
type ScheduleRequest struct {
	WorkflowID string          `json:"workflow_id"`
	Kind       string          `json:"kind"`
	RunAt      time.Time       `json:"run_at"`
	Input      json.RawMessage `json:"input,omitempty"`
	Enabled    bool            `json:"enabled"`
}

// ScheduleView is the part of a schedule the runner reads back: whether it is still
// enabled and which run it created (DESIGN 5.9 rule 2).
type ScheduleView struct {
	ID         string     `json:"id"`
	WorkflowID string     `json:"workflow_id"`
	Kind       string     `json:"kind"`
	Enabled    bool       `json:"enabled"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	LastRunID  *string    `json:"last_run_id,omitempty"`
	LastError  *string    `json:"last_error,omitempty"`
}

// RunID is the id of the run the schedule created, "" until it fired.
func (s ScheduleView) RunID() string {
	if s.LastRunID == nil {
		return ""
	}
	return *s.LastRunID
}

// LastErrorText is the schedule's last_error, "" when it has none. It is not named Error so
// that a ScheduleView is never mistaken for an error value.
func (s ScheduleView) LastErrorText() string {
	if s.LastError == nil {
		return ""
	}
	return *s.LastError
}

// CreateSchedule creates a schedule.
func (c *Client) CreateSchedule(ctx context.Context, req ScheduleRequest) (*ScheduleView, error) {
	var out ScheduleView
	if err := c.do(ctx, http.MethodPost, "/api/schedules", req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("POST /api/schedules: response has no id")
	}
	return &out, nil
}

// GetSchedule fetches a schedule.
func (c *Client) GetSchedule(ctx context.Context, id string) (*ScheduleView, error) {
	var out ScheduleView
	if err := c.do(ctx, http.MethodGet, "/api/schedules/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSchedule removes a schedule. One that is already gone is not an error.
func (c *Client) DeleteSchedule(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/schedules/"+url.PathEscape(id), nil, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// ListTools returns the names GET /api/tools lists: the platform tools registered in
// this deployment, then the OpenClaw built-ins.
func (c *Client) ListTools(ctx context.Context) ([]string, error) {
	var out []struct {
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/tools", nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, len(out))
	for i, t := range out {
		names[i] = t.Name
	}
	return names, nil
}
