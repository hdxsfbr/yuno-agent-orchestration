package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"yuno/internal/store"
)

// Shared is the part of the API the seed reuses (DESIGN 5.15 rules 13 and 14): the
// template instantiation behind POST /api/workflow-templates/{key}/instantiate and the
// bind code behind PUT /api/channels/telegram/binding, without routes, platform tools or
// an event recorder.
type Shared struct{ s *Server }

// NewShared builds Shared from the server's dependencies; Engine and Gateway are only
// needed to bind.
func NewShared(d Deps) *Shared {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Shared{s: &Server{cfg: d.Cfg, log: d.Log, pool: d.Pool, engine: d.Engine, gateway: d.Gateway}}
}

// InstantiateTemplate creates a workflow from the template with key, named name (the
// template's name when empty), exactly as the API's "Create copy" does. Problems with
// the template, such as an agent it names that does not exist, are one error listing
// them.
func (sh *Shared) InstantiateTemplate(ctx context.Context, key, name string) (*store.Workflow, error) {
	wf, fe, err := sh.s.instantiate(ctx, key, name)
	if err != nil {
		return nil, err
	}
	if len(fe) > 0 {
		msgs := make([]string, len(fe))
		for i, f := range fe {
			msgs[i] = f.Path + ": " + f.Message
		}
		return nil, fmt.Errorf("template %s cannot be instantiated: %s", key, strings.Join(msgs, "; "))
	}
	return wf, nil
}

// ResolvedTemplate returns the definition ResetFromTemplate would store for the
// workflow: its template's definition with the agent names resolved and checked.
func (sh *Shared) ResolvedTemplate(ctx context.Context, workflowID string) (json.RawMessage, error) {
	wf, err := store.GetWorkflow(ctx, sh.s.pool, workflowID)
	if err != nil {
		return nil, err
	}
	return sh.resolvedTemplate(ctx, wf)
}

func (sh *Shared) resolvedTemplate(ctx context.Context, wf *store.Workflow) (json.RawMessage, error) {
	if wf.FromTemplate == nil {
		return nil, fmt.Errorf("workflow %s was not made from a template", wf.ID)
	}
	tpl, err := store.GetWorkflowByTemplateKey(ctx, sh.s.pool, *wf.FromTemplate)
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", *wf.FromTemplate, err)
	}
	def, fe, err := sh.s.resolveAgentNames(ctx, tpl.Definition)
	if err != nil {
		return nil, err
	}
	if len(fe) == 0 {
		fe, err = sh.s.checkDefinition(ctx, def, definitionRules{agents: true})
		if err != nil {
			return nil, err
		}
	}
	if len(fe) > 0 {
		msgs := make([]string, len(fe))
		for i, f := range fe {
			msgs[i] = f.Path + ": " + f.Message
		}
		return nil, fmt.Errorf("template %s cannot be applied: %s", *wf.FromTemplate, strings.Join(msgs, "; "))
	}
	return def, nil
}

// ResetFromTemplate rewrites an instantiated workflow's definition from the template
// it came from, resolving agent names as "Create copy" does, keeping the workflow's id,
// name and description (DESIGN 5.16 rule 14).
func (sh *Shared) ResetFromTemplate(ctx context.Context, workflowID string) error {
	wf, err := store.GetWorkflow(ctx, sh.s.pool, workflowID)
	if err != nil {
		return err
	}
	if wf.FromTemplate == nil {
		return fmt.Errorf("workflow %s was not made from a template", workflowID)
	}
	def, err := sh.resolvedTemplate(ctx, wf)
	if err != nil {
		return err
	}
	wf.Definition = def
	return store.UpdateWorkflow(ctx, sh.s.pool, wf)
}

// BindTelegram binds the agent to Telegram as the Channels page does: refused while
// another agent is bound, materialised first when needed, then stored and applied on
// the gateway, which restarts to take it.
func (sh *Shared) BindTelegram(ctx context.Context, agentID string) error {
	_, err := sh.s.bindAgent(ctx, agentID, "")
	return err
}
