package evals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"yuno/internal/model"
	"yuno/internal/store"
)

// Environment the runner reads for a project case (DESIGN 5.29 rule 17): where the fake
// forge is reachable from the api and from the runner, and its one token. Both are set by
// compose for the eval profile only.
const (
	envForgeURL   = "EVAL_FORGE_URL"
	envForgeToken = "FORGEFAKE_TOKEN"
)

// forgeFromEnv is the runner's forge client, nil when EVAL_FORGE_URL is not set.
func forgeFromEnv() (*forgeClient, string) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv(envForgeURL)), "/")
	if base == "" {
		return nil, ""
	}
	return newForgeClient(base, os.Getenv(envForgeToken)), base
}

// projectFixture is the project one case created: its repository path on the forge and
// its id in the api.
type projectFixture struct {
	id   string
	name string
	path string
}

// scriptSteps converts a script to the API's registration shape.
func scriptSteps(script []ScriptStep) ([]model.ScriptStep, error) {
	return Case{Script: script}.ModelSteps()
}

// timeoutFor is how long the case's run may take: the project block's timeout when it
// sets one, else the runner's.
func (r *Runner) timeoutFor(c Case) time.Duration {
	if c.Project != nil && c.Project.TimeoutSeconds > 0 {
		return time.Duration(c.Project.TimeoutSeconds) * time.Second
	}
	return r.caseTimeout
}

// hasApproveStep reports a case whose steps decide approvals, which the run's own approval
// actions then leave alone when they run out.
func (c Case) hasApproveStep() bool {
	return slices.ContainsFunc(c.Steps, func(s CaseStep) bool { return s.Approve != nil })
}

// setupProject makes the case's repository on the forge, opens its seed issues and
// registers the project in the api (provider gitlab, the forge as its base URL, the forge's
// token), with the intake settings the case asks for.
func (r *Runner) setupProject(ctx context.Context, c Case, suffix string, cr *caseResources) (*projectFixture, error) {
	if r.forge == nil {
		return nil, fmt.Errorf("the case has a project and %s is not set: the fake forge is not configured", envForgeURL)
	}
	spec := c.Project
	branch := spec.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	pf := &projectFixture{path: "eval/" + c.ID + "-" + suffix, name: "eval-" + c.ID + "-" + suffix}
	if err := r.forge.CreateRepo(ctx, pf.path, branch); err != nil {
		return nil, fmt.Errorf("create the forge repository: %w", err)
	}
	for i, is := range spec.Issues {
		if err := r.forge.CreateIssue(ctx, pf.path, is); err != nil {
			return nil, fmt.Errorf("open project.issues[%d] on the forge: %w", i, err)
		}
	}
	req := ProjectRequest{
		Name: pf.name, RepoURL: r.forgeURL + "/" + pf.path + ".git", Provider: "gitlab", DefaultBranch: branch,
		Token: r.forge.token, Description: "Created by the eval runner for case " + c.ID + ".",
	}
	if spec.MaxCostUSDPerDay > 0 {
		req.MaxCostUSDPerDay = &spec.MaxCostUSDPerDay
	}
	if in := spec.Intake; in != nil {
		req.Intake = &IntakeRequest{Enabled: in.Enabled, Label: in.Label}
		if in.Workflow != "" {
			id, err := r.api.FindWorkflowID(ctx, in.Workflow)
			if err != nil {
				return nil, fmt.Errorf("project.intake.workflow: %w", err)
			}
			req.Intake.WorkflowID = id
		}
	}
	p, err := r.api.CreateProject(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create the project: %w", err)
	}
	pf.id = p.ID
	cr.projectID = p.ID
	for i, pr := range spec.PlatformRuns {
		steps, err := scriptSteps(pr.Script)
		if err != nil {
			return nil, fmt.Errorf("project.platform_runs[%d]: %w", i, err)
		}
		id := fmt.Sprintf("%s-%s-%s-%d", c.ID, suffix, pr.Trigger, i+1)
		if err := r.api.RegisterBoundScript(ctx, id, steps, p.ID, pr.Trigger); err != nil {
			return nil, fmt.Errorf("register the script of project.platform_runs[%d]: %w", i, err)
		}
	}
	r.log.Info("eval project created", "case", c.ID, "project", p.ID, "repo", pf.path)
	return pf, nil
}

// deleteProject ends a case's project: its runs that have not ended are cancelled, its
// open plans cancelled, and the project deleted, so nothing of it keeps polling or
// working once the case is over. It runs even when ctx has ended.
func (r *Runner) deleteProject(caseID, projectID string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	if runs, err := r.api.ListProjectRuns(ctx, projectID, ""); err != nil {
		r.log.Warn("eval project runs cleanup failed", "case", caseID, "project", projectID, "error", err)
	} else {
		for _, run := range runs {
			if !store.IsTerminal(run.Status) {
				r.cancelRun(ctx, caseID, run.ID)
			}
		}
	}
	if plans, err := r.api.ListPlans(ctx, projectID); err != nil {
		r.log.Warn("eval project plans cleanup failed", "case", caseID, "project", projectID, "error", err)
	} else {
		for _, p := range plans {
			if p.Status == store.PlanDone || p.Status == store.PlanCancelled {
				continue
			}
			if err := r.api.CancelPlan(ctx, p.ID, "the eval case ended"); err != nil {
				r.log.Warn("eval plan cleanup failed", "case", caseID, "plan", p.ID, "error", err)
			}
		}
	}
	if err := r.api.DeleteProject(ctx, projectID); err != nil {
		r.log.Warn("eval project cleanup failed", "case", caseID, "project", projectID, "error", err)
	}
}

// stepsRun is the steps of a case running beside its run.
type stepsRun struct {
	stop context.CancelFunc
	done chan error
}

// startSteps runs the case's steps in order in the background. A failed step cancels the
// case's run, so the wait for it ends, and is the case's error.
func (r *Runner) startSteps(ctx context.Context, c Case, pf *projectFixture, runID string) *stepsRun {
	sctx, stop := context.WithCancel(ctx)
	sr := &stepsRun{stop: stop, done: make(chan error, 1)}
	go func() {
		err := r.runSteps(sctx, c, pf)
		if err != nil && sctx.Err() == nil {
			r.log.Warn("eval step failed", "case", c.ID, "error", err)
			r.cancelRun(ctx, c.ID, runID)
		}
		sr.done <- err
	}()
	return sr
}

// finish waits for the steps to end, or stops them now when the run did not finish; it
// returns the failure of a step, "" when they all passed (or were stopped).
func (sr *stepsRun) finish(stopNow bool) error {
	if stopNow {
		sr.stop()
	}
	err := <-sr.done
	sr.stop()
	if errors.Is(err, context.Canceled) && stopNow {
		return nil
	}
	return err
}

// runSteps runs the case's steps in order and stops at the first failure.
func (r *Runner) runSteps(ctx context.Context, c Case, pf *projectFixture) error {
	st := &stepState{decided: map[string]bool{}}
	for i, s := range c.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.runStep(ctx, c, pf, s, st); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, s.describe(), err)
		}
		r.log.Info("eval step done", "case", c.ID, "step", i+1, "what", s.describe())
	}
	return nil
}

// stepState is what the steps share: the approvals they decided already.
type stepState struct {
	decided map[string]bool
}

// describe names a step for messages.
func (s CaseStep) describe() string {
	switch {
	case s.Forge != nil:
		switch f := s.Forge; {
		case f.MergeOpen != nil:
			return "forge merge_open"
		case f.Pipeline != "":
			return "forge pipeline " + f.Pipeline
		case f.CreateIssue != nil:
			return "forge create_issue"
		default:
			return "forge expect_issues"
		}
	case s.Wait != nil:
		switch {
		case s.Wait.PlanStatus != "":
			return "wait plan_status " + s.Wait.PlanStatus
		case s.Wait.TicketStatus != "":
			return fmt.Sprintf("wait wave %d tickets %s", s.Wait.Wave, s.Wait.TicketStatus)
		}
		return "wait plan_event " + s.Wait.PlanEvent
	case s.Approve != nil:
		return "approve " + s.Approve.Decision
	case s.Hold != nil:
		return fmt.Sprintf("hold no ship run for wave %d", s.Hold.NoShipRun)
	case s.PauseWaves != "":
		return "pause_waves"
	case s.ResumeWaves:
		return "resume_waves"
	}
	return "poll_intake"
}

func (r *Runner) runStep(ctx context.Context, c Case, pf *projectFixture, s CaseStep, st *stepState) error {
	switch {
	case s.PollIntake:
		return r.api.PollIntake(ctx, pf.id)
	case s.Forge != nil:
		return r.forgeStep(ctx, pf, s.Forge)
	case s.Wait != nil:
		return r.waitStep(ctx, pf, s.Wait)
	case s.Hold != nil:
		return r.holdStep(ctx, pf, s.Hold)
	case s.PauseWaves != "":
		return r.api.PauseWaves(ctx, pf.id, s.PauseWaves)
	case s.ResumeWaves:
		return r.api.ResumeWaves(ctx, pf.id)
	default:
		return r.approveStep(ctx, c, pf, s.Approve, st)
	}
}

func (r *Runner) forgeStep(ctx context.Context, pf *projectFixture, f *ForgeStep) error {
	switch {
	case f.Pipeline != "":
		return r.forge.Pipeline(ctx, pf.path, f.Pipeline)
	case f.CreateIssue != nil:
		return r.forge.CreateIssue(ctx, pf.path, *f.CreateIssue)
	case f.MergeOpen != nil:
		want, merged := max(f.MergeOpen.Count, 1), 0
		return r.until(ctx, f.TimeoutSeconds, func(ctx context.Context) (bool, string, error) {
			mrs, err := r.forge.MergeOpen(ctx, pf.path)
			if err != nil {
				return false, "", err
			}
			merged += len(mrs)
			return merged >= want, fmt.Sprintf("%d of %d merge requests were open to merge", merged, want), nil
		})
	default:
		return r.until(ctx, f.TimeoutSeconds, func(ctx context.Context) (bool, string, error) {
			issues, err := r.forge.Issues(ctx, pf.path)
			if err != nil {
				return false, "", err
			}
			for _, e := range f.ExpectIssues {
				if !slices.ContainsFunc(issues, e.metBy) {
					return false, "no issue is " + e.describe() + "; the forge has " + summarizeIssues(issues), nil
				}
			}
			return true, "", nil
		})
	}
}

// metBy reports whether the issue meets the expectation.
func (e IssueExpect) metBy(i forgeIssue) bool {
	if e.TitleContains != "" && !containsFold(i.Title, e.TitleContains) {
		return false
	}
	if e.State != "" && i.State != e.State {
		return false
	}
	for _, l := range e.Labels {
		if !slices.ContainsFunc(i.Labels, func(have string) bool { return strings.EqualFold(have, l) }) {
			return false
		}
	}
	return true
}

func (e IssueExpect) describe() string {
	var parts []string
	if e.TitleContains != "" {
		parts = append(parts, fmt.Sprintf("titled like %q", e.TitleContains))
	}
	if len(e.Labels) > 0 {
		parts = append(parts, "labelled "+strings.Join(e.Labels, ","))
	}
	if e.State != "" {
		parts = append(parts, e.State)
	}
	return strings.Join(parts, ", ")
}

func summarizeIssues(issues []forgeIssue) string {
	if len(issues) == 0 {
		return "no issues"
	}
	parts := make([]string, len(issues))
	for i, is := range issues {
		parts[i] = fmt.Sprintf("#%d %s [%s]", is.IID, is.State, strings.Join(is.Labels, ","))
	}
	return strings.Join(parts, "; ")
}

// waitStep waits for a plan of the project to have the status, or an event of the kind.
func (r *Runner) waitStep(ctx context.Context, pf *projectFixture, w *WaitStep) error {
	return r.until(ctx, w.TimeoutSeconds, func(ctx context.Context) (bool, string, error) {
		plans, err := r.api.ListPlans(ctx, pf.id)
		if err != nil {
			return false, "", err
		}
		if w.PlanStatus != "" {
			seen := make([]string, len(plans))
			for i, p := range plans {
				if p.Status == w.PlanStatus {
					return true, "", nil
				}
				seen[i] = p.Status
			}
			return false, "the project's plans are " + listOrNone(seen), nil
		}
		var kinds []string
		for _, p := range plans {
			view, err := r.api.GetPlan(ctx, p.ID)
			if err != nil {
				return false, "", err
			}
			if w.TicketStatus != "" {
				n, matching := 0, 0
				for _, t := range view.Tickets {
					if t.Wave == w.Wave {
						n++
						if t.Status == w.TicketStatus {
							matching++
						}
					}
				}
				if n > 0 && n == matching {
					return true, "", nil
				}
				kinds = append(kinds, fmt.Sprintf("%d of %d tickets of wave %d are %s", matching, n, w.Wave, w.TicketStatus))
				continue
			}
			for _, ev := range view.Events {
				if ev.Kind == w.PlanEvent && (w.Wave == 0 || (ev.Wave != nil && *ev.Wave == w.Wave)) {
					return true, "", nil
				}
				kinds = append(kinds, ev.Kind)
			}
		}
		if w.TicketStatus != "" {
			return false, "the plans' tickets: " + listOrNone(kinds), nil
		}
		return false, "the plans' events are " + listOrNone(kinds), nil
	})
}

// holdStep fails the case if a plan of the project starts a ship run for the wave within
// the step's seconds.
func (r *Runner) holdStep(ctx context.Context, pf *projectFixture, h *HoldStep) error {
	deadline := time.NewTimer(time.Duration(h.Seconds) * time.Second)
	defer deadline.Stop()
	for {
		plans, err := r.api.ListPlans(ctx, pf.id)
		if err != nil {
			return err
		}
		for _, p := range plans {
			view, err := r.api.GetPlan(ctx, p.ID)
			if err != nil {
				return err
			}
			for _, sr := range view.ShipRuns {
				if sr.Wave == h.NoShipRun {
					return fmt.Errorf("a ship run (%s, %s) started for wave %d", sr.RunID, sr.Status, sr.Wave)
				}
			}
		}
		wake := time.NewTimer(max(r.poll, 100*time.Millisecond))
		select {
		case <-ctx.Done():
			wake.Stop()
			return ctx.Err()
		case <-deadline.C:
			wake.Stop()
			return nil
		case <-wake.C:
		}
	}
}

func listOrNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

// approveStep decides pending approvals of the project's runs, oldest first.
func (r *Runner) approveStep(ctx context.Context, c Case, pf *projectFixture, a *ApproveStep, st *stepState) error {
	want, done := max(a.Count, 1), 0
	return r.until(ctx, a.TimeoutSeconds, func(ctx context.Context) (bool, string, error) {
		runs, err := r.api.ListProjectRuns(ctx, pf.id, store.RunWaitingApproval)
		if err != nil {
			return false, "", err
		}
		for _, run := range runs {
			pending, err := r.api.ListApprovals(ctx, run.ID, store.ApprovalPending)
			if err != nil {
				return false, "", err
			}
			slices.SortStableFunc(pending, func(x, y ApprovalItem) int { return x.RequestedAt.Compare(y.RequestedAt) })
			for _, ap := range pending {
				if st.decided[ap.ID] || ap.Kind == store.ApprovalKindQuestion || (a.Node != "" && ap.NodeID != a.Node) {
					continue
				}
				_, err := r.api.DecideApproval(ctx, ap.ID, a.Decision, a.Reason)
				if err != nil && statusOf(err) != 409 {
					return false, "", err
				}
				st.decided[ap.ID] = true
				done++
				r.log.Info("eval approval decided by a step", "case", c.ID, "run", run.ID, "approval", ap.ID, "node", ap.NodeID, "decision", a.Decision)
				if done >= want {
					return true, "", nil
				}
			}
		}
		return false, fmt.Sprintf("%d of %d approvals were pending", done, want), nil
	})
}

// until polls check every runner poll until it reports done, ctx ends, or the step's
// timeout passes; a check that keeps failing ends it too. detail says what was missing.
func (r *Runner) until(ctx context.Context, seconds int, check func(context.Context) (bool, string, error)) error {
	limit := time.Duration(stepTimeoutSeconds(seconds)) * time.Second
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	failures, last := 0, ""
	for {
		done, detail, err := check(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			last = err.Error()
			if failures >= maxPollFailures {
				return err
			}
		case done:
			return nil
		default:
			failures, last = 0, detail
		}
		wake := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			wake.Stop()
			return ctx.Err()
		case <-deadline.C:
			wake.Stop()
			return fmt.Errorf("not met within %s: %s", limit, last)
		case <-wake.C:
		}
	}
}
