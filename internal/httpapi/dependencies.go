package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/actor"
	"yuno/internal/deps"
	"yuno/internal/store"
)

// The prerequisites of a merge request (Chief platform control, DESIGN wave 3 rules 1, 3
// and 5): the operator and the Chief set them; the Planner's edges of the request's ticket
// are listed and are not editable here. The state shown is what the dependency watcher
// last recorded; no provider is asked.

// maxPrerequisites bounds one PUT.
const maxPrerequisites = 50

// dependencyOut is one prerequisite of a merge request as the routes answer it. Reason is
// the rule-5 reason while it is not satisfied, or the note of a satisfied one ("no CI
// configured").
type dependencyOut struct {
	ID           string          `json:"id"`
	Prerequisite string          `json:"prerequisite"`
	Kind         string          `json:"kind"`
	Label        string          `json:"label"`
	Source       string          `json:"source"`
	Editable     bool            `json:"editable"`
	CreatedBy    json.RawMessage `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	Satisfied    bool            `json:"satisfied"`
	SatisfiedAt  *time.Time      `json:"satisfied_at,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ClearedAt    *time.Time      `json:"cleared_at,omitempty"`
}

// dependenciesOut is GET and PUT /api/merge-requests/{id}/dependencies.
type dependenciesOut struct {
	MergeRequestID string          `json:"merge_request_id"`
	Prerequisites  []dependencyOut `json:"prerequisites"`
	BlockedBy      []deps.Blocker  `json:"blocked_by"`
}

// mergeRequestRead is a record as GET /api/merge-requests/{id} answers it: with what holds
// it, when something does.
type mergeRequestRead struct {
	*store.MergeRequest
	BlockedBy []deps.Blocker `json:"blocked_by,omitempty"`
}

// blockedBy is what holds an open merge request (rule 5), as last recorded: each
// prerequisite not satisfied with its reason; once all are satisfied and the request is not
// let go yet, each with the watcher's reason (an update from the default branch to come,
// conflicts, CI on the new head). A request that is not open is held by nothing.
func blockedBy(ctx context.Context, q store.Querier, mr *store.MergeRequest, edges []store.Dependency) ([]deps.Blocker, error) {
	if mr.State != store.MergeRequestOpen {
		return []deps.Blocker{}, nil
	}
	st, err := deps.Stored(ctx, q, edges)
	if err != nil {
		return nil, err
	}
	if len(st.Blocked) > 0 || len(st.Open) == 0 {
		if st.Blocked == nil {
			st.Blocked = []deps.Blocker{}
		}
		return st.Blocked, nil
	}
	g, err := store.GetDependencyGate(ctx, q, mr.ID)
	if err != nil {
		return nil, err
	}
	reason := "the platform has not updated the branch from the default branch yet"
	if g != nil && g.Reason != nil && *g.Reason != "" {
		reason = *g.Reason
	}
	out := make([]deps.Blocker, len(st.Open))
	for i, e := range st.Open {
		out[i] = deps.Blocker{Prerequisite: e.PrerequisiteID, Kind: e.PrerequisiteKind, Label: st.Labels[i], Reason: reason}
	}
	return out, nil
}

// dependencies reads the prerequisites of a merge request for the routes.
func (s *Server) dependencies(ctx context.Context, mr *store.MergeRequest) (dependenciesOut, error) {
	edges, err := store.MergeRequestDependencies(ctx, s.pool, mr.ID)
	if err != nil {
		return dependenciesOut{}, err
	}
	nodes := make([]store.DependencyNode, len(edges))
	for i, e := range edges {
		nodes[i] = store.DependencyNode{Kind: e.PrerequisiteKind, ID: e.PrerequisiteID}
	}
	labels, err := store.DependencyLabels(ctx, s.pool, nodes)
	if err != nil {
		return dependenciesOut{}, err
	}
	out := dependenciesOut{MergeRequestID: mr.ID, Prerequisites: []dependencyOut{}}
	for i, e := range edges {
		d := dependencyOut{ID: e.ID, Prerequisite: e.PrerequisiteID, Kind: e.PrerequisiteKind, Label: labels[nodes[i]], Source: e.Source,
			Editable: e.Source != store.DependencyPlanner, CreatedBy: e.CreatedBy, CreatedAt: e.CreatedAt,
			Satisfied: e.SatisfiedAt != nil, SatisfiedAt: e.SatisfiedAt, ClearedAt: e.ClearedAt}
		if e.Reason != nil {
			d.Reason = *e.Reason
		}
		out.Prerequisites = append(out.Prerequisites, d)
	}
	if out.BlockedBy, err = blockedBy(ctx, s.pool, mr, edges); err != nil {
		return dependenciesOut{}, err
	}
	return out, nil
}

// getMergeRequestDependencies lists a merge request's prerequisites and what holds it
// (DESIGN wave 3 rule 3).
func (s *Server) getMergeRequestDependencies(w http.ResponseWriter, r *http.Request) {
	mr, err := store.GetMergeRequest(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	out, err := s.dependencies(r.Context(), mr)
	if err != nil {
		s.internalError(w, r, "read the merge request's dependencies", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// putMergeRequestDependencies replaces the operator and Chief prerequisites of a merge
// request with the listed merge requests of the same project (DESIGN wave 3 rules 1 and 3).
// A self-edge is a validation error; an edge that would close a cycle is 409
// dependency_cycle, "would make a cycle: !24 -> !17 -> !24". The Planner's edges stay.
func (s *Server) putMergeRequestDependencies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		Prerequisites []string `json:"prerequisites"`
	}
	if !decodeBody(w, r, &in, 16<<10) {
		return
	}
	mr, err := store.GetMergeRequest(ctx, s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "merge request", err)
		return
	}
	var fe fieldErrors
	if in.Prerequisites == nil {
		fe.add("prerequisites", "is required (an empty list removes them all)")
	}
	if len(in.Prerequisites) > maxPrerequisites {
		fe.add("prerequisites", "at most %d", maxPrerequisites)
	}
	seen := map[string]bool{}
	var list []string
	for i, id := range in.Prerequisites {
		path := fmt.Sprintf("prerequisites[%d]", i)
		switch {
		case seen[id]:
			continue
		case id == mr.ID:
			fe.add(path, "a merge request cannot depend on itself")
			continue
		}
		p, err := store.GetMergeRequest(ctx, s.pool, id)
		switch {
		case errors.Is(err, store.ErrNotFound) || (err == nil && p.ProjectID != mr.ProjectID):
			fe.add(path, "is not a merge request of this project")
			continue
		case err != nil:
			s.internalError(w, r, "read a prerequisite", err)
			return
		}
		seen[id] = true
		list = append(list, id)
	}
	if len(fe) > 0 {
		writeValidation(w, "invalid request body", fe)
		return
	}
	source, by := store.DependencyOperator, json.RawMessage(`{"kind":"operator"}`)
	if a, ok := actor.From(ctx); ok {
		if a.Kind == actor.Agent {
			source = store.DependencyChief
		}
		if raw, err := json.Marshal(a); err == nil {
			by = raw
		}
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := store.ReplaceMergeRequestPrerequisites(ctx, tx, mr.ProjectID, mr.ID, list, source, by)
		return err
	})
	var cycle *store.DependencyCycleError
	switch {
	case errors.As(err, &cycle):
		writeError(w, http.StatusConflict, "dependency_cycle", cycle.Error(), map[string]any{"path": cycle.Path})
		return
	case errors.Is(err, store.ErrSelfDependency):
		writeValidation(w, "invalid request body", fieldErrors{{Path: "prerequisites", Message: store.ErrSelfDependency.Error()}})
		return
	case err != nil:
		s.internalError(w, r, "set the merge request's dependencies", err)
		return
	}
	out, err := s.dependencies(ctx, mr)
	if err != nil {
		s.internalError(w, r, "read the merge request's dependencies", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
