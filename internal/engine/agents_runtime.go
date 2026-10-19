package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// The agents OpenClaw holds follow the platform's agents: a deleted agent leaves the
// gateway, an agent the gateway lacks or holds out of date is materialised again. Left
// alone, every agent the eval runner and the UI ever deleted stayed configured in the
// gateway, whose heap grew with each one until it crashed.

// RemoveAgent takes a soft-deleted agent out of OpenClaw: agents.delete and its
// workspace directory. An agent with a node visit in flight stays until the next sync
// (a run that started before the deletion continues, DESIGN 5.6 edge cases), and its
// materialised hash is cleared either way, so a later visit of such a run materialises
// it again rather than calling an agent the gateway no longer has.
func (e *Engine) RemoveAgent(ctx context.Context, agentID string) error {
	a, err := store.GetAgentForRun(ctx, e.deps.Pool, agentID)
	if err != nil {
		return fmt.Errorf("agent %s: %w", agentID, err)
	}
	if a.DeletedAt == nil {
		return fmt.Errorf("agent %s is not deleted", agentID)
	}
	activity, err := store.ListAgentActivity(ctx, e.deps.Pool)
	if err != nil {
		return fmt.Errorf("agent %s: activity: %w", agentID, err)
	}
	if err := e.forgetMaterialized(ctx, a); err != nil {
		return err
	}
	if act := activity[a.ID]; act.Busy() {
		e.log.Info("deleted agent kept in OpenClaw while its runs finish", "agent_id", a.ID,
			"running", act.Running, "waiting_approval", act.WaitingApproval)
		return nil
	}
	gw := e.deps.Gateway
	if gw == nil {
		return fmt.Errorf("agent %s: %w", agentID, openclaw.ErrDisconnected)
	}
	list, err := openclaw.AgentsList(ctx, gw)
	if err != nil {
		return fmt.Errorf("agent %s: agents.list: %w", agentID, err)
	}
	listed := map[string]bool{}
	for _, ag := range list.Agents {
		listed[ag.ID] = true
	}
	var errs []error
	for _, id := range runtimeIDs(a) {
		if !listed[id] {
			// Never created, or deleted before: only a workspace may be left.
			errs = append(errs, openclaw.RemoveWorkspace(e.deps.StateDir, id))
			continue
		}
		if err := openclaw.DeleteAgent(ctx, gw, e.deps.StateDir, id); err != nil {
			errs = append(errs, err)
			continue
		}
		e.log.Info("deleted agent removed from OpenClaw", "agent_id", a.ID, "openclaw_agent_id", id)
	}
	return errors.Join(errs...)
}

// runtimeIDs are the OpenClaw ids an agent may hold: the recorded one and the one its
// materialisation derives, which a materialisation that failed after agents.create
// leaves behind without recording it.
func runtimeIDs(a *store.Agent) []string {
	ids := []string{openclaw.DeriveAgentID(a.ID)}
	if a.OpenClawAgentID != nil && *a.OpenClawAgentID != "" && *a.OpenClawAgentID != ids[0] {
		ids = append([]string{*a.OpenClawAgentID}, ids...)
	}
	return ids
}

// forgetMaterialized clears the stored hash of an agent whose runtime copy is going
// away, keeping its OpenClaw id.
func (e *Engine) forgetMaterialized(ctx context.Context, a *store.Agent) error {
	if a.OpenClawAgentID == nil || *a.OpenClawAgentID == "" || a.MaterializedHash == "" {
		return nil
	}
	if err := store.SetAgentMaterialized(ctx, e.deps.Pool, a.ID, *a.OpenClawAgentID, ""); err != nil {
		return fmt.Errorf("agent %s: clear materialisation: %w", a.ID, err)
	}
	a.MaterializedHash = ""
	return nil
}

// AgentSync counts what one SyncAgents pass did.
type AgentSync struct {
	Deleted           int // gateway agents removed: their platform agent is deleted, or there is none
	DeleteFailed      int
	KeptBusy          int // deleted agents kept while a visit of theirs is in flight
	Materialized      int // platform agents pushed because they were missing or out of date
	MaterializeFailed int
	SandboxesCleared  int // per-visit sandbox blocks a dead process left behind (DESIGN 5.16 rule 7)
}

// SyncAgents brings the gateway's agents in line with the platform's. Every agent the
// gateway lists that belongs to a soft-deleted platform agent, or to none, is deleted
// with its workspace; the gateway's default agent is kept. Then every platform agent
// whose materialised hash differs from its configuration, or that the gateway does not
// list, is materialised. A failure for one agent is logged and the pass goes on; the
// error reports only failures that stop the whole pass.
func (e *Engine) SyncAgents(ctx context.Context) (AgentSync, error) {
	var res AgentSync
	gw := e.deps.Gateway
	if gw == nil {
		return res, openclaw.ErrDisconnected
	}
	// The roster is read before the agents: an agent row always exists before its
	// gateway entry (create stores the row, then materialises), so every entry read
	// here has its row in the read below and a new agent is never taken for a stray.
	list, err := openclaw.AgentsList(ctx, gw)
	if err != nil {
		return res, fmt.Errorf("agents.list: %w", err)
	}
	rows, err := store.ListAgentsWithDeleted(ctx, e.deps.Pool)
	if err != nil {
		return res, fmt.Errorf("list agents: %w", err)
	}
	activity, err := store.ListAgentActivity(ctx, e.deps.Pool)
	if err != nil {
		return res, fmt.Errorf("agent activity: %w", err)
	}
	owner := map[string]*store.Agent{}
	for i := range rows {
		a := &rows[i]
		for _, id := range runtimeIDs(a) {
			if prev, ok := owner[id]; !ok || prev.DeletedAt != nil {
				owner[id] = a // a live owner wins over a deleted one
			}
		}
	}
	listed := map[string]bool{}
	for _, ag := range list.Agents {
		listed[ag.ID] = true
		a := owner[ag.ID]
		switch {
		case ag.ID == openclaw.MainAgentID || ag.ID == list.DefaultID:
			continue
		case a != nil && a.DeletedAt == nil:
			continue
		case a != nil && activity[a.ID].Busy():
			res.KeptBusy++
			continue
		case !openclaw.Removable(ag.ID):
			e.log.Warn("gateway agent not removed: unexpected id", "openclaw_agent_id", ag.ID)
			continue
		}
		if a != nil {
			if err := e.forgetMaterialized(ctx, a); err != nil {
				e.log.Warn("stale gateway agent not removed", "openclaw_agent_id", ag.ID, "error", err)
				res.DeleteFailed++
				continue
			}
		}
		if err := openclaw.DeleteAgent(ctx, gw, e.deps.StateDir, ag.ID); err != nil {
			e.log.Warn("stale gateway agent not removed", "openclaw_agent_id", ag.ID, "error", err)
			res.DeleteFailed++
			continue
		}
		res.Deleted++
	}
	for i := range rows {
		if rows[i].DeletedAt == nil {
			e.syncAgent(ctx, rows[i].ID, listed, &res)
		}
	}
	res.SandboxesCleared = e.clearStaleSandboxes(ctx, listed)
	return res, nil
}

// clearStaleSandboxes removes the per-visit sandbox block (DESIGN 5.16 rule 7) from
// every listed agent that no project visit of this process holds: what a process that
// died mid-visit left behind. It returns how many it cleared; failures are logged.
func (e *Engine) clearStaleSandboxes(ctx context.Context, listed map[string]bool) int {
	cfg, err := openclaw.ConfigGet(ctx, e.deps.Gateway)
	if err != nil {
		e.log.Warn("stale sandbox blocks not checked", "error", err)
		return 0
	}
	holds := e.sandboxHolds()
	n := 0
	for id, raw := range openclaw.ConfigEntries(cfg.Config) {
		if !listed[id] || holds[id] > 0 || id == openclaw.MainAgentID {
			continue
		}
		var entry struct {
			Sandbox json.RawMessage `json:"sandbox"`
		}
		if json.Unmarshal(raw, &entry) != nil || len(entry.Sandbox) == 0 || string(entry.Sandbox) == "null" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, e.setupTimeout)
		err := openclaw.ClearAgentSandbox(cctx, e.deps.Gateway, id)
		cancel()
		if err != nil {
			e.log.Warn("stale sandbox block not cleared", "openclaw_agent_id", id, "error", err)
			continue
		}
		e.log.Info("stale sandbox block cleared", "openclaw_agent_id", id)
		n++
	}
	return n
}

// MaterializeAgents is SyncAgents without the removals: every live agent the gateway
// lacks or holds out of date is materialised. KeepAgents runs it between full syncs, so
// an agent created outside the api (yuno seed, run by make run once the stack is up) is
// materialised without waiting for a restart. Removals stay with the full sync: other
// users of the gateway, such as the integration tests, meet them only at an api start
// or reconnect.
func (e *Engine) MaterializeAgents(ctx context.Context) (AgentSync, error) {
	var res AgentSync
	gw := e.deps.Gateway
	if gw == nil {
		return res, openclaw.ErrDisconnected
	}
	list, err := openclaw.AgentsList(ctx, gw)
	if err != nil {
		return res, fmt.Errorf("agents.list: %w", err)
	}
	listed := map[string]bool{}
	for _, ag := range list.Agents {
		listed[ag.ID] = true
	}
	agents, err := store.ListAgents(ctx, e.deps.Pool)
	if err != nil {
		return res, fmt.Errorf("list agents: %w", err)
	}
	for i := range agents {
		e.syncAgent(ctx, agents[i].ID, listed, &res)
	}
	return res, nil
}

// syncAgent materialises one live agent when the gateway lacks it or holds an older
// configuration.
func (e *Engine) syncAgent(ctx context.Context, agentID string, listed map[string]bool, res *AgentSync) {
	a, skills, err := e.loadAgent(ctx, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return // deleted since the list was read
	}
	if err != nil {
		e.log.Warn("agent not materialised at sync", "agent_id", agentID, "error", err)
		res.MaterializeFailed++
		return
	}
	missing := a.OpenClawAgentID == nil || !listed[*a.OpenClawAgentID]
	if want, ex, known := e.desired(ctx, a, skills); !missing && known && a.MaterializedHash == want {
		e.refreshWorkspace(a, skills, ex)
		return
	}
	if missing {
		a.MaterializedHash = "" // the stored hash describes a copy the gateway no longer has
	}
	sctx, cancel := context.WithTimeout(ctx, e.setupTimeout)
	err = e.materialize(sctx, a, skills)
	cancel()
	if err != nil {
		e.log.Warn("agent not materialised at sync", "agent_id", agentID, "error", err)
		res.MaterializeFailed++
		return
	}
	res.Materialized++
}

// KeepAgents runs SyncAgents at start and after every gateway reconnect, and
// MaterializeAgents every agentSync in between, until ctx ends. A full sync that
// cannot run (the gateway or the database refusing the reads) is retried with backoff
// from 1 s doubling to 30 s.
func (e *Engine) KeepAgents(ctx context.Context) {
	gw := e.deps.Gateway
	if gw == nil {
		return
	}
	events, unsub := gw.Subscribe(64)
	defer unsub()
	const base, maxWait = time.Second, 30 * time.Second
	wait := base
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	tick := time.NewTicker(e.agentSync)
	defer tick.Stop()
	due := true
	for {
		if due {
			due = false
			res, err := e.SyncAgents(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				e.log.Warn("openclaw agent sync failed", "error", err, "retry_in", wait.String())
				retry.Reset(wait)
				wait = min(wait*2, maxWait)
			case err == nil:
				wait = base
				e.log.Info("openclaw agents synced", "deleted", res.Deleted, "delete_failed", res.DeleteFailed,
					"kept_busy", res.KeptBusy, "materialized", res.Materialized, "materialize_failed", res.MaterializeFailed)
			}
		}
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Name == openclaw.EventReconnected {
				due = true
			}
		case <-retry.C:
			due = true
		case <-tick.C:
			if _, up := gw.Info(); !up {
				continue // the reconnect brings a full sync
			}
			res, err := e.MaterializeAgents(ctx)
			if err != nil && ctx.Err() == nil {
				e.log.Warn("openclaw agent materialisation pass failed", "error", err)
			} else if res.Materialized+res.MaterializeFailed > 0 {
				e.log.Info("openclaw agents materialised", "materialized", res.Materialized, "materialize_failed", res.MaterializeFailed)
			}
		}
	}
}
