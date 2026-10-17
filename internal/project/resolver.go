package project

import (
	"context"
	"errors"
	"fmt"

	"yuno/internal/store"
)

// Resolver gives the engine and the platform tools the project configuration of a run
// (DESIGN 5.17 rule 6): the run's snapshot of the settings plus the token read from the
// project's row, deleted or not, and opened with the cipher at that moment, so a token
// replaced on the Projects page applies to runs in flight.
type Resolver struct {
	DB     store.Querier
	Cipher *Cipher
}

// ErrNoProject is the answer for a run that names no project.
var ErrNoProject = errors.New("this run has no project")

// ForRun returns the run's project configuration; ErrNoProject when it names none.
func (r *Resolver) ForRun(ctx context.Context, run *store.Run) (*Config, error) {
	if run == nil || run.ProjectID == nil || *run.ProjectID == "" || run.Project == nil {
		return nil, ErrNoProject
	}
	token, err := r.Token(ctx, *run.ProjectID)
	if err != nil {
		return nil, err
	}
	c := ConfigOf(*run.Project, token)
	return &c, nil
}

// ForRunID is ForRun for a run id (the tools have only the id).
func (r *Resolver) ForRunID(ctx context.Context, runID string) (*Config, error) {
	if r == nil || r.DB == nil {
		return nil, errors.New("project: no database")
	}
	run, err := store.GetRun(ctx, r.DB, runID)
	if err != nil {
		return nil, fmt.Errorf("project: run %s: %w", runID, err)
	}
	return r.ForRun(ctx, run)
}

// Token reads and opens a project's token. The errors are the reasons a run or a check
// reports: no row, no token stored, or a blob that does not open (rule 2).
func (r *Resolver) Token(ctx context.Context, projectID string) (string, error) {
	if r == nil || r.DB == nil {
		return "", errors.New("project: no database")
	}
	blob, err := store.GetProjectTokenEnc(ctx, r.DB, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("project: %s no longer exists", projectID)
	}
	if err != nil {
		return "", fmt.Errorf("project: read the token: %w", err)
	}
	if len(blob) == 0 {
		return "", errors.New("project: no token is stored; enter one on the Projects page")
	}
	token, err := r.Cipher.Open(projectID, blob)
	if err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	return token, nil
}
