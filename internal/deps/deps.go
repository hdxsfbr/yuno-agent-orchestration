// Package deps judges whether the prerequisites of dependent work are satisfied (Chief
// platform control, DESIGN wave 3 rules 4 and 5) and records the judgement on the edges.
// The watchers act on it: mergereq for a dependent merge request (rule 8a), waves for a
// dependent ticket (rules 6 and 8b).
package deps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const (
	// NoCIWindow is how long after the merge a prerequisite of a project without CI waits for
	// a pipeline on its merge commit before it counts as satisfied (rule 4).
	NoCIWindow = 15 * time.Minute
	// NoteNoCI is the note a prerequisite satisfied without any CI carries (rule 4).
	NoteNoCI = "no CI configured"
)

// ProviderError is a failure to read a prerequisite from its provider (the project's
// access, the provider's API): the dependent stays held and the next pass asks again.
type ProviderError struct{ Err error }

func (e *ProviderError) Error() string { return e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

// ProviderFunc returns the project configuration and the provider client a merge request
// record is read through.
type ProviderFunc func(ctx context.Context, mr *store.MergeRequest) (*project.Config, tools.RequestProvider, error)

// Gate judges prerequisites. Pool reads and writes the edges; Provider reads the CI of a
// merge commit; Now is the clock (nil: the system's, in UTC).
type Gate struct {
	Pool     store.Querier
	Provider ProviderFunc
	Now      func() time.Time
}

// Blocker is one prerequisite that holds its dependent (rule 5): Prerequisite and Kind name
// it, Label is how a person reads it ("!17", "T9"), Reason the rule-5 reason.
type Blocker struct {
	Prerequisite string `json:"prerequisite"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	Reason       string `json:"reason"`
}

// State is a dependent's prerequisites as judged. Open are the edges not cleared yet, the
// ones that hold it; Satisfied says each of them is satisfied; Blocked are the ones that are
// not, oldest edge first; Closed are those whose prerequisite was closed without merging,
// which the operator is told about. Labels name the open edges' prerequisites in order.
type State struct {
	Open      []store.Dependency
	Satisfied bool
	Blocked   []Blocker
	Closed    []Blocker
	Labels    []string
}

// Reason is the first blocker's reason, "" when nothing blocks.
func (s State) Reason() string {
	if len(s.Blocked) == 0 {
		return ""
	}
	return s.Blocked[0].Reason
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

// MergeRequest judges the prerequisites of a merge request: its own edges and those of the
// plan ticket it was built for.
func (g *Gate) MergeRequest(ctx context.Context, mrID string) (State, error) {
	edges, err := store.MergeRequestDependencies(ctx, g.Pool, mrID)
	if err != nil {
		return State{}, err
	}
	return g.Judge(ctx, edges)
}

// Ticket judges the prerequisites of a plan ticket.
func (g *Gate) Ticket(ctx context.Context, planID, ticketID string) (State, error) {
	edges, err := store.DependenciesOf(ctx, g.Pool, store.DependencyTicket, store.TicketNode(planID, ticketID))
	if err != nil {
		return State{}, err
	}
	return g.Judge(ctx, edges)
}

// PrerequisitesSatisfied reports whether every prerequisite holding a merge request is
// satisfied and, when one is not, its reason (rule 5). It is the check a run that would work
// on the request makes (rule 7). A request no edge holds is satisfied.
func (g *Gate) PrerequisitesSatisfied(ctx context.Context, mrID string) (bool, string, error) {
	st, err := g.MergeRequest(ctx, mrID)
	if err != nil {
		return false, "", err
	}
	return st.Satisfied, st.Reason(), nil
}

// Judge evaluates the open edges among edges whose prerequisite is not satisfied yet
// (rules 4 and 5), records each judgement on its edge, and returns the state. A satisfied
// prerequisite stays satisfied and is not asked about again. A provider's error
// (*ProviderError) or the database's ends the judgement: the edges judged before it keep their record, and
// the dependent stays held.
func (g *Gate) Judge(ctx context.Context, edges []store.Dependency) (State, error) {
	var st State
	var nodes []store.DependencyNode
	for _, e := range edges {
		if e.ClearedAt == nil {
			st.Open = append(st.Open, e)
			nodes = append(nodes, store.DependencyNode{Kind: e.PrerequisiteKind, ID: e.PrerequisiteID})
		}
	}
	labels, err := store.DependencyLabels(ctx, g.Pool, nodes)
	if err != nil {
		return st, err
	}
	now := g.now()
	for i := range st.Open {
		e := &st.Open[i]
		label := labels[nodes[i]]
		st.Labels = append(st.Labels, label)
		if e.SatisfiedAt != nil {
			continue
		}
		v, err := g.judge(ctx, e, label, now)
		if err != nil {
			return st, err
		}
		reason := v.reason
		var r *string
		if reason != "" {
			r = &reason
		}
		if err := store.SetDependencyEvaluation(ctx, g.Pool, e.ID, v.satisfied, r, now); err != nil {
			return st, err
		}
		e.Reason, e.EvaluatedAt = r, &now
		if v.satisfied {
			e.SatisfiedAt = &now
			continue
		}
		b := Blocker{Prerequisite: e.PrerequisiteID, Kind: e.PrerequisiteKind, Label: label, Reason: reason}
		st.Blocked = append(st.Blocked, b)
		if v.closed {
			st.Closed = append(st.Closed, b)
		}
	}
	st.Satisfied = len(st.Blocked) == 0
	return st, nil
}

// Stored is the state as the watcher last recorded it, without asking any provider: the
// record reads show it. A prerequisite not judged yet is waiting for the watcher.
func Stored(ctx context.Context, q store.Querier, edges []store.Dependency) (State, error) {
	var st State
	var nodes []store.DependencyNode
	for _, e := range edges {
		if e.ClearedAt == nil {
			st.Open = append(st.Open, e)
			nodes = append(nodes, store.DependencyNode{Kind: e.PrerequisiteKind, ID: e.PrerequisiteID})
		}
	}
	labels, err := store.DependencyLabels(ctx, q, nodes)
	if err != nil {
		return st, err
	}
	for i, e := range st.Open {
		label := labels[nodes[i]]
		st.Labels = append(st.Labels, label)
		if e.SatisfiedAt != nil {
			continue
		}
		reason := label + " has not been checked yet"
		if e.Reason != nil && *e.Reason != "" {
			reason = *e.Reason
		}
		st.Blocked = append(st.Blocked, Blocker{Prerequisite: e.PrerequisiteID, Kind: e.PrerequisiteKind, Label: label, Reason: reason})
	}
	st.Satisfied = len(st.Blocked) == 0
	return st, nil
}

// verdict is one prerequisite judged: satisfied, or the reason it is not; closed when it can
// never be (closed without merging). A satisfied one may carry a note as its reason.
type verdict struct {
	satisfied bool
	reason    string
	closed    bool
}

func (g *Gate) judge(ctx context.Context, e *store.Dependency, label string, now time.Time) (verdict, error) {
	switch e.PrerequisiteKind {
	case store.DependencyMergeRequest:
		mr, err := store.GetMergeRequest(ctx, g.Pool, e.PrerequisiteID)
		if errors.Is(err, store.ErrNotFound) {
			return verdict{reason: "prerequisite " + label + " no longer exists"}, nil
		}
		if err != nil {
			return verdict{}, err
		}
		return g.judgeRequest(ctx, mr, label, now)
	case store.DependencyTicket:
		return g.judgeTicket(ctx, e.PrerequisiteID, label, now)
	}
	return verdict{}, fmt.Errorf("unknown prerequisite kind %q", e.PrerequisiteKind)
}

// judgeTicket is rule 4 for a ticket: satisfied when its merge request is; never when it was
// closed without a merge.
func (g *Gate) judgeTicket(ctx context.Context, node, label string, now time.Time) (verdict, error) {
	planID, ticketID, _ := store.SplitTicketNode(node)
	t, err := store.GetPlanTicket(ctx, g.Pool, planID, ticketID)
	if errors.Is(err, store.ErrNotFound) {
		return verdict{reason: "prerequisite " + label + " is not a ticket of the plan any more"}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	var mr *store.MergeRequest
	if t.MergeRequestID != nil {
		if mr, err = store.GetMergeRequest(ctx, g.Pool, *t.MergeRequestID); errors.Is(err, store.ErrNotFound) {
			mr = nil
		} else if err != nil {
			return verdict{}, err
		}
	}
	switch {
	case mr != nil && mr.State == store.MergeRequestMerged:
		return g.judgeRequest(ctx, mr, store.RequestLabel(mr.Provider, mr.Number), now)
	case t.Status == store.TicketClosed:
		return verdict{reason: "prerequisite " + label + " was closed without merging", closed: true}, nil
	case mr != nil && mr.State == store.MergeRequestOpen:
		return verdict{reason: store.RequestLabel(mr.Provider, mr.Number) + " is not merged"}, nil
	}
	return verdict{reason: label + " is not merged"}, nil
}

// judgeRequest is rule 4 for a merge request: merged, its merge commit known, and the
// default branch's CI of exactly that commit passed; or, in a project without CI, no
// pipeline on the merge commit within NoCIWindow of the merge. The reasons are rule 5's.
func (g *Gate) judgeRequest(ctx context.Context, mr *store.MergeRequest, label string, now time.Time) (verdict, error) {
	switch mr.State {
	case store.MergeRequestOpen:
		return verdict{reason: label + " is not merged"}, nil
	case store.MergeRequestClosed:
		return verdict{reason: "prerequisite " + label + " was closed without merging", closed: true}, nil
	}
	if mr.MergeSHA == nil || *mr.MergeSHA == "" {
		return verdict{reason: label + " merged; the merge commit is not known yet"}, nil
	}
	sha := *mr.MergeSHA
	if g.Provider == nil {
		return verdict{}, errors.New("no provider to read the CI of a merge commit")
	}
	pc, rp, err := g.Provider(ctx, mr)
	if err != nil {
		return verdict{}, &ProviderError{Err: err}
	}
	ci, err := rp.CommitCI(ctx, pc.DefaultBranch, sha)
	if err != nil {
		return verdict{}, &ProviderError{Err: err}
	}
	pending := func(state string) verdict {
		return verdict{reason: fmt.Sprintf("%s merged; CI on %s is %s", label, Short(sha), state)}
	}
	switch ci.State {
	case tools.BranchPassed:
		return verdict{satisfied: true}, nil
	case tools.BranchFailed, tools.BranchRunning, tools.BranchPending:
		return pending(ci.State), nil
	}
	merged := mr.UpdatedAt
	if mr.ClosedAt != nil {
		merged = *mr.ClosedAt
	}
	if now.Sub(merged) < NoCIWindow {
		return pending(tools.BranchPending), nil
	}
	configured, err := rp.CIConfigured(ctx, pc.DefaultBranch)
	if err != nil {
		return verdict{}, &ProviderError{Err: err}
	}
	if configured {
		return pending(tools.BranchPending), nil
	}
	return verdict{satisfied: true, reason: NoteNoCI}, nil
}

// Short is a commit as a message names it: its first seven characters.
func Short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
