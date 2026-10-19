package seed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// removeTimeout bounds taking one retired agent out of OpenClaw, as the API's own
// delete does.
const removeTimeout = 15 * time.Second

// Remover takes a soft-deleted agent out of the OpenClaw runtime (engine.Engine in
// production, the same call the API's DELETE /api/agents/{id} makes). A nil Remover
// leaves the runtime to the sync at the next api start.
type Remover interface {
	RemoveAgent(ctx context.Context, agentID string) error
}

// retiredNames are the names the retired templates were shipped under, which a copy
// made by the seed carries.
var retiredNames = map[string]string{
	"dev_pipeline":      "Dev pipeline",
	"research_pipeline": "Research pipeline",
	"review_panel":      "Review panel",
}

// RetireWorkflows deletes the shipped workflows of the retired templates (DESIGN 5.28
// rule 2): the template rows themselves and the untouched copies instantiated from them.
// A copy is untouched when it still has its shipped name and version 1 (every update
// raises the version). A copy the operator renamed or edited, one a project's intake or
// an enabled schedule points at, and one with a run in flight, is kept and logged. It
// returns the names deleted.
func RetireWorkflows(ctx context.Context, q store.Querier, log *slog.Logger) ([]string, error) {
	all, err := store.ListWorkflows(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("seed: list workflows: %w", err)
	}
	var deleted []string
	for _, wf := range all {
		key := retiredKey(wf)
		if key == "" {
			continue
		}
		if wf.TemplateKey == nil { // an instantiated copy, not the platform's template row
			if wf.Version != 1 || wf.Name != retiredNames[key] {
				log.Info("seed: retired workflow kept: the operator changed it", "workflow", wf.Name, "id", wf.ID, "version", wf.Version)
				continue
			}
		}
		ref, err := workflowReference(ctx, q, wf.ID)
		if err != nil {
			return deleted, fmt.Errorf("seed: references of workflow %s: %w", wf.Name, err)
		}
		if ref != "" {
			log.Info("seed: retired workflow kept: referenced by "+ref, "workflow", wf.Name, "id", wf.ID)
			continue
		}
		ok, err := store.DeleteWorkflowIfIdle(ctx, q, wf.ID, wf.Version)
		if err != nil {
			return deleted, fmt.Errorf("seed: delete workflow %s: %w", wf.Name, err)
		}
		if !ok {
			log.Info("seed: retired workflow kept: a run is in flight", "workflow", wf.Name, "id", wf.ID)
			continue
		}
		log.Info("seed: retired workflow deleted", "workflow", wf.Name, "id", wf.ID, "created_at", wf.CreatedAt.Format(time.RFC3339))
		deleted = append(deleted, wf.Name)
	}
	return deleted, nil
}

// workflowReference says what points at the workflow, as "intake on project X" or
// "schedule Y", or "" when nothing does: the intake setting of a live project and an
// enabled schedule keep running the workflow by id.
func workflowReference(ctx context.Context, q store.Querier, id string) (string, error) {
	var project string
	err := q.QueryRow(ctx, `SELECT name FROM projects WHERE intake_workflow_id = $1 AND deleted_at IS NULL ORDER BY created_at LIMIT 1`, id).Scan(&project)
	if err == nil {
		return "intake on project " + project, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var schedule string
	err = q.QueryRow(ctx, `SELECT id FROM schedules WHERE workflow_id = $1 AND enabled ORDER BY created_at LIMIT 1`, id).Scan(&schedule)
	if err == nil {
		return "schedule " + schedule, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return "", nil
}

// retiredKey returns the retired template key a workflow is, or was made from, or "".
func retiredKey(wf store.Workflow) string {
	for _, k := range []*string{wf.FromTemplate, wf.TemplateKey} {
		if k != nil && slices.Contains(RetiredTemplateKeys, *k) {
			return *k
		}
	}
	return ""
}

// RetireAgents deletes the nine retired agents by exact name (DESIGN 5.28 rule 2),
// through the API's delete path: soft delete, unbind, then removal from OpenClaw. It
// keeps (and logs) an agent that a live workflow still references, that has a node
// visit in flight, that is bound to a channel, and never touches the Chief of Staff or any other name. Run RetireWorkflows
// first, so the copies of the retired templates no longer count as references. It
// returns the names deleted.
func RetireAgents(ctx context.Context, q store.Querier, rm Remover, log *slog.Logger) ([]string, error) {
	agents, err := store.ListAgents(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("seed: list agents: %w", err)
	}
	flows, err := store.ListWorkflows(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("seed: list workflows: %w", err)
	}
	bound, err := store.ListAgentChannels(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("seed: list channel bindings: %w", err)
	}
	busy, err := store.ListAgentActivity(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("seed: list agent activity: %w", err)
	}
	var deleted []string
	for _, a := range agents {
		if a.Name == ChiefName || !slices.Contains(RetiredAgentNames, a.Name) {
			continue
		}
		if wf := referencingWorkflow(flows, a); wf != "" {
			log.Info("seed: retired agent kept: a workflow still uses it", "agent", a.Name, "workflow", wf)
			continue
		}
		if len(bound[a.ID]) > 0 {
			log.Info("seed: retired agent kept: it answers a channel", "agent", a.Name, "channels", strings.Join(bound[a.ID], ","))
			continue
		}
		if act := busy[a.ID]; act.Busy() {
			log.Info("seed: retired agent kept: it has a run in flight", "agent", a.Name, "id", a.ID, "running", act.Running, "waiting_approval", act.WaitingApproval)
			continue
		}
		if err := store.DeleteAgent(ctx, q, a.ID); err != nil {
			return deleted, fmt.Errorf("seed: delete agent %s: %w", a.Name, err)
		}
		log.Info("seed: retired agent deleted", "agent", a.Name, "id", a.ID, "created_at", a.CreatedAt.Format(time.RFC3339))
		deleted = append(deleted, a.Name)
		if rm == nil {
			continue
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
		err := rm.RemoveAgent(rctx, a.ID)
		cancel()
		if err != nil {
			// As the API does: the deletion stands, the sync at the next api start removes the rest.
			log.Warn("seed: deleted agent not removed from OpenClaw", "agent", a.Name, "error", err)
		}
	}
	return deleted, nil
}

// referencingWorkflow returns the name of a live workflow with an agent node that runs
// the agent (by id, or by name for a template row), or "". The hidden quick-run wrappers
// do not count, as they follow their agent.
func referencingWorkflow(flows []store.Workflow, a store.Agent) string {
	for _, wf := range flows {
		if wf.TemplateKey != nil && strings.HasPrefix(*wf.TemplateKey, "quickrun:") {
			continue
		}
		def, err := workflow.Parse(wf.Definition)
		if err != nil {
			continue
		}
		for _, n := range def.Nodes {
			if n.Kind == workflow.KindAgent && (n.AgentID == a.ID || strings.EqualFold(n.AgentName, a.Name)) {
				return wf.Name
			}
		}
	}
	return ""
}
