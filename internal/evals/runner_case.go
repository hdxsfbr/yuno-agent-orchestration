package evals

import (
	"context"
	"fmt"

	"yuno/internal/store"
)

// caseResources are the api objects one case created. release deletes them, newest
// kind first (the schedule and the workflow before the agents they name), and runs even
// when the case's context has ended.
type caseResources struct {
	r          *Runner
	caseID     string
	scheduleID string
	workflowID string
	projectID  string
	agentIDs   []string
}

func (cr *caseResources) release() {
	if cr.projectID != "" {
		cr.r.deleteProject(cr.caseID, cr.projectID)
	}
	if cr.scheduleID != "" {
		cr.r.deleteSchedule(cr.caseID, cr.scheduleID)
	}
	if cr.workflowID != "" {
		cr.r.deleteWorkflow(cr.caseID, cr.workflowID)
	}
	for _, id := range cr.agentIDs {
		cr.r.deleteAgent(cr.caseID, id)
	}
}

// execCase drives one case against the api: agents (and the workflow), script, run,
// wait, trace. Everything it created is deleted before it returns. It returns the
// fetched trace when there is one; a timeout or a stalled approval is recorded in
// res.Error and still returns the trace taken at that moment.
func (r *Runner) execCase(ctx context.Context, c Case, res *CaseResult) (*store.Trace, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	steps, err := c.ModelSteps()
	if err != nil {
		return nil, err
	}

	cr := &caseResources{r: r, caseID: c.ID}
	defer cr.release()

	req := RunRequest{Limits: c.RunLimits()}
	var pf *projectFixture
	if c.Project != nil {
		if pf, err = r.setupProject(ctx, c, suffix, cr); err != nil {
			return nil, err
		}
		req.ProjectID = pf.id
	}
	if c.RunsModel() {
		req.Model = ScriptedModel
	}
	if c.IsWorkflow() {
		if err := r.createWorkflowFixtures(ctx, c, suffix, cr, res, &req); err != nil {
			return nil, err
		}
	} else {
		agent, err := r.api.CreateAgent(ctx, c.Agent.request("eval-"+c.ID+"-"+suffix, c.Agent.Name, r.cfg.DefaultModel))
		if err != nil {
			return nil, fmt.Errorf("create agent: %w", err)
		}
		cr.agentIDs = append(cr.agentIDs, agent.ID)
		res.AgentID = agent.ID
		req.AgentID, req.Task = agent.ID, c.Task
	}

	// A workflow without agent nodes plays no script: there is no model call to feed.
	if len(steps) > 0 {
		scriptID := c.ID + "-" + suffix
		res.ScriptID = scriptID
		if err := r.api.RegisterScript(ctx, scriptID, steps); err != nil {
			return nil, fmt.Errorf("register script: %w", err)
		}
		req.ScriptID = scriptID
	}

	obs := &Observed{}
	run, err := r.startRun(ctx, c, req, cr, obs)
	res.Observed = obs
	if err != nil {
		return nil, err
	}
	res.RunID = run.ID
	// Every exit that did not see the run finish cancels it (a timeout, an interrupted
	// runner, a poll that kept failing, an approval nobody will decide), with a context
	// of its own, so the api does not keep working on a case nobody waits for. It runs
	// after the trace is taken below.
	finished := false
	defer func() {
		if !finished {
			r.cancelRun(ctx, c.ID, run.ID)
		}
	}()

	var bg *stepsRun
	if pf != nil && len(c.Steps) > 0 {
		bg = r.startSteps(ctx, c, pf, run.ID)
	}
	out, err := r.await(ctx, c, run)
	var stepErr error
	if bg != nil {
		// Steps still waiting once the run is over are given their own timeouts, unless the
		// run did not finish: then nothing they wait for can come.
		stepErr = bg.finish(err != nil || out.timedOut || out.stalled != "")
	}
	if err != nil {
		res.Status = out.last.Status
		return nil, err
	}
	finished = !out.timedOut && out.stalled == ""
	if finished {
		r.observe(ctx, c, req, obs, res)
	}

	// On a timeout the trace is taken before the cancel so it shows the run as it was
	// when the runner gave up.
	trace, traceErr := r.api.GetTrace(ctx, run.ID)
	switch {
	case out.timedOut:
		res.Error = timeoutReason
	case out.stalled != "":
		res.Error = out.stalled
	case stepErr != nil:
		res.Error = stepErr.Error()
	}
	if traceErr != nil {
		res.Status = out.last.Status
		if res.Error == "" {
			res.Error = "fetch trace: " + traceErr.Error()
		}
		return nil, fmt.Errorf("fetch trace: %w", traceErr)
	}
	if trace.Run == nil {
		trace.Run = out.last
	}
	return trace, nil
}

// createWorkflowFixtures creates the case's agents, one per entry of agents, then the
// workflow with every agent_name replaced by the id of the matching agent, and points the
// run request at the workflow.
func (r *Runner) createWorkflowFixtures(ctx context.Context, c Case, suffix string, cr *caseResources, res *CaseResult, req *RunRequest) error {
	ids := map[string]string{}
	for _, key := range c.AgentKeys() {
		spec := c.Agents[key]
		role := spec.Name
		if role == "" {
			role = key
		}
		agent, err := r.api.CreateAgent(ctx, spec.request("eval-"+c.ID+"-"+key+"-"+suffix, role, r.cfg.DefaultModel))
		if err != nil {
			return fmt.Errorf("create agent %s: %w", key, err)
		}
		ids[key] = agent.ID
		cr.agentIDs = append(cr.agentIDs, agent.ID)
		res.AgentIDs = append(res.AgentIDs, agent.ID)
	}

	def, err := c.WorkflowBody(ids)
	if err != nil {
		return err
	}
	input, err := c.InputJSON()
	if err != nil {
		return err
	}
	wf, err := r.api.CreateWorkflow(ctx, WorkflowRequest{
		Name:        "eval-" + c.ID + "-" + suffix,
		Description: "Created by the eval runner for case " + c.ID + ".",
		Definition:  def,
	})
	if err != nil {
		return fmt.Errorf("create workflow: %w", err)
	}
	cr.workflowID = wf.ID
	res.WorkflowID = wf.ID
	req.WorkflowID, req.Input = wf.ID, input
	return nil
}

// request builds the agent the api is asked to create for this spec.
func (a AgentSpec) request(name, role, defaultModel string) AgentRequest {
	return AgentRequest{
		Name:             name,
		Role:             role,
		SystemPrompt:     a.SystemPrompt,
		Model:            defaultModel,
		ToolAllowlist:    a.ToolAllowlist,
		InteractionRules: a.InteractionRules,
		Limits:           a.Limits.Store(),
		Guardrails:       a.Guardrails.Store(),
		MemoryPolicy:     a.MemoryPolicy.Store(),
	}
}

// deleteWorkflow removes a case's temporary workflow. It runs even when ctx has ended.
func (r *Runner) deleteWorkflow(caseID, workflowID string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	if err := r.api.DeleteWorkflow(ctx, workflowID); err != nil {
		r.log.Warn("eval workflow cleanup failed", "case", caseID, "workflow", workflowID, "error", err)
	}
}

// deleteSchedule removes a case's temporary schedule. It runs even when ctx has ended.
func (r *Runner) deleteSchedule(caseID, scheduleID string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	if err := r.api.DeleteSchedule(ctx, scheduleID); err != nil {
		r.log.Warn("eval schedule cleanup failed", "case", caseID, "schedule", scheduleID, "error", err)
	}
}
