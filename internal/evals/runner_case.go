package evals

import (
	"context"
	"fmt"

	"yuno/internal/store"
)

// caseResources are the api objects one case created. release deletes them, newest
// kind first (the workflow before the agents it names), and runs even when the case's
// context has ended.
type caseResources struct {
	r          *Runner
	caseID     string
	workflowID string
	agentIDs   []string
}

func (cr *caseResources) release() {
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

	req := RunRequest{Model: ScriptedModel, Limits: c.RunLimits()}
	if c.IsWorkflow() {
		if err := r.createWorkflowFixtures(ctx, c, suffix, cr, res, &req); err != nil {
			return nil, err
		}
	} else {
		agent, err := r.api.CreateAgent(ctx, AgentRequest{
			Name:             "eval-" + c.ID + "-" + suffix,
			Role:             c.Agent.Name,
			SystemPrompt:     c.Agent.SystemPrompt,
			Model:            r.cfg.DefaultModel,
			ToolAllowlist:    c.Agent.ToolAllowlist,
			InteractionRules: c.Agent.InteractionRules,
			Limits:           c.Agent.Limits.Store(),
		})
		if err != nil {
			return nil, fmt.Errorf("create agent: %w", err)
		}
		cr.agentIDs = append(cr.agentIDs, agent.ID)
		res.AgentID = agent.ID
		req.AgentID, req.Task = agent.ID, c.Task
	}

	scriptID := c.ID + "-" + suffix
	res.ScriptID = scriptID
	if err := r.api.RegisterScript(ctx, scriptID, steps); err != nil {
		return nil, fmt.Errorf("register script: %w", err)
	}
	req.ScriptID = scriptID

	run, err := r.api.CreateRun(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
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

	out, err := r.await(ctx, c, run)
	if err != nil {
		res.Status = out.last.Status
		return nil, err
	}
	finished = !out.timedOut && out.stalled == ""

	// On a timeout the trace is taken before the cancel so it shows the run as it was
	// when the runner gave up.
	trace, traceErr := r.api.GetTrace(ctx, run.ID)
	switch {
	case out.timedOut:
		res.Error = timeoutReason
	case out.stalled != "":
		res.Error = out.stalled
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
// workflow with every agent_name replaced by the matching agent's id, and points the run
// request at the workflow.
func (r *Runner) createWorkflowFixtures(ctx context.Context, c Case, suffix string, cr *caseResources, res *CaseResult, req *RunRequest) error {
	ids := map[string]string{}
	for _, key := range c.AgentKeys() {
		spec := c.Agents[key]
		role := spec.Name
		if role == "" {
			role = key
		}
		agent, err := r.api.CreateAgent(ctx, AgentRequest{
			Name:             "eval-" + c.ID + "-" + key + "-" + suffix,
			Role:             role,
			SystemPrompt:     spec.SystemPrompt,
			Model:            r.cfg.DefaultModel,
			ToolAllowlist:    spec.ToolAllowlist,
			InteractionRules: spec.InteractionRules,
			Limits:           spec.Limits.Store(),
		})
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

// deleteWorkflow removes a case's temporary workflow. It runs even when ctx has ended.
func (r *Runner) deleteWorkflow(caseID, workflowID string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	if err := r.api.DeleteWorkflow(ctx, workflowID); err != nil {
		r.log.Warn("eval workflow cleanup failed", "case", caseID, "workflow", workflowID, "error", err)
	}
}
