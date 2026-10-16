package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// materialized creates an agent and pushes it to the fake gateway, returning the agent
// as stored and its OpenClaw id.
func (env *testEnv) materialized(name string) (*store.Agent, string) {
	env.t.Helper()
	a := env.agent(func(a *store.Agent) { a.Name = name })
	if _, err := env.e.MaterializeAgent(context.Background(), a.ID); err != nil {
		env.t.Fatalf("materialise %s: %v", name, err)
	}
	got, err := store.GetAgent(context.Background(), env.pool, a.ID)
	if err != nil || got.OpenClawAgentID == nil {
		env.t.Fatalf("read %s: %+v %v", name, got, err)
	}
	return got, *got.OpenClawAgentID
}

func (env *testEnv) gatewayAgents() []string {
	env.fc.mu.Lock()
	defer env.fc.mu.Unlock()
	return slices.Clone(env.fc.agents)
}

func (env *testEnv) deletedIDs() []string {
	var out []string
	for _, raw := range env.fc.callsFor("agents.delete") {
		var p struct {
			AgentID string `json:"agentId"`
		}
		_ = json.Unmarshal(raw, &p)
		out = append(out, p.AgentID)
	}
	slices.Sort(out)
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// busy gives the agent a node visit in flight: a claimed run with a running visit.
func (env *testEnv) busy(a *store.Agent) {
	env.t.Helper()
	ctx := context.Background()
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "work"})
	if err != nil {
		env.t.Fatal(err)
	}
	if _, err := store.ClaimQueuedRun(ctx, env.pool); err != nil {
		env.t.Fatal(err)
	}
	now := time.Now()
	id := a.ID
	if err := store.CreateRunNode(ctx, env.pool, &store.RunNode{RunID: run.ID, NodeID: "agent", AgentID: &id, Visit: 1,
		Status: store.NodeRunning, StartedAt: &now}); err != nil {
		env.t.Fatal(err)
	}
}

// Deleting an agent reaches the runtime: agents.delete for its OpenClaw id, its
// workspace removed, its materialised hash cleared (audit #5).
func TestRemoveAgentDeletesRuntime(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a, ocID := env.materialized("Doomed")
	keep, keepID := env.materialized("Kept")
	ws := openclaw.WorkspaceDir(env.dir, ocID)
	if !exists(filepath.Join(ws, "AGENTS.md")) {
		t.Fatalf("materialisation wrote no workspace at %s", ws)
	}
	if err := env.e.RemoveAgent(ctx, a.ID); err == nil {
		t.Fatal("RemoveAgent on a live agent succeeded")
	}
	if err := store.DeleteAgent(ctx, env.pool, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.e.RemoveAgent(ctx, a.ID); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if got := env.deletedIDs(); !slices.Equal(got, []string{ocID}) {
		t.Fatalf("agents.delete calls = %v, want [%s]", got, ocID)
	}
	if exists(ws) {
		t.Fatalf("workspace %s still exists", ws)
	}
	if got := env.gatewayAgents(); !slices.Equal(got, []string{keepID}) {
		t.Fatalf("gateway agents = %v, want only %s", got, keepID)
	}
	if !exists(openclaw.WorkspaceDir(env.dir, keepID)) {
		t.Fatal("another agent's workspace was removed")
	}
	row, err := store.GetAgentForRun(ctx, env.pool, a.ID)
	if err != nil || row.MaterializedHash != "" || row.OpenClawAgentID == nil || *row.OpenClawAgentID != ocID {
		t.Fatalf("deleted row = %+v, %v; want the hash cleared and the id kept", row, err)
	}
	_ = keep
}

// A gateway that refuses leaves the workspace for the next attempt and says so.
func TestRemoveAgentGatewayRefuses(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a, ocID := env.materialized("Refused")
	if err := store.DeleteAgent(ctx, env.pool, a.ID); err != nil {
		t.Fatal(err)
	}
	env.fc.fail["agents.delete"] = &openclaw.RPCError{Code: "UNAVAILABLE", Message: "busy"}
	err := env.e.RemoveAgent(ctx, a.ID)
	var rpc *openclaw.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("RemoveAgent = %v, want the gateway's refusal", err)
	}
	if !exists(openclaw.WorkspaceDir(env.dir, ocID)) {
		t.Fatal("workspace removed although the gateway kept the agent")
	}
}

// An agent deleted while a visit of its runs is in flight stays in the gateway until
// the run lets it go (DESIGN 5.6: a run whose agent was deleted continues).
func TestRemoveAgentKeepsBusyAgent(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a, ocID := env.materialized("Busy")
	env.busy(a)
	if err := store.DeleteAgent(ctx, env.pool, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.e.RemoveAgent(ctx, a.ID); err != nil {
		t.Fatalf("RemoveAgent: %v", err)
	}
	if got := env.deletedIDs(); len(got) != 0 {
		t.Fatalf("agents.delete called for a busy agent: %v", got)
	}
	if !exists(openclaw.WorkspaceDir(env.dir, ocID)) {
		t.Fatal("busy agent's workspace removed")
	}
	row, _ := store.GetAgentForRun(ctx, env.pool, a.ID)
	if row.MaterializedHash != "" {
		t.Fatal("hash kept: the run's next visit would call an agent about to disappear")
	}
}

// The start-up sync removes what belongs to deleted agents or to no agent, keeps live
// agents, busy deleted ones and the gateway's default, and materialises every live
// agent that is missing or out of date (audit #5, demo pre-flight).
func TestSyncAgents(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	_, liveID := env.materialized("Live")
	edited, editedID := env.materialized("Edited")
	edited.Role = "a role the gateway has not seen"
	if err := store.UpdateAgent(ctx, env.pool, edited); err != nil {
		t.Fatal(err)
	}
	env.fc.defaultID = "gw-default" // the gateway's own default is never removed, whatever its id
	gone, goneID := env.materialized("Gone")
	busy, busyID := env.materialized("Busy")
	env.busy(busy)
	for _, a := range []*store.Agent{gone, busy} {
		if err := store.DeleteAgent(ctx, env.pool, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	// A stray from an earlier install, and a seeded agent never materialised.
	const strayID = "zz-stray-agent"
	env.fc.mu.Lock()
	env.fc.agents = append(env.fc.agents, strayID)
	env.fc.mu.Unlock()
	if err := os.MkdirAll(openclaw.WorkspaceDir(env.dir, strayID), 0o755); err != nil {
		t.Fatal(err)
	}
	// The broken agent comes first: its failure must not stop the seeded one.
	broken := env.agent(func(a *store.Agent) { a.Name = "Broken" })
	seeded := env.agent(func(a *store.Agent) { a.Name = "Seeded" })
	env.fc.createErr = map[string]error{openclaw.DeriveAgentID(broken.ID): &openclaw.RPCError{Code: "INVALID_REQUEST", Message: "nope"}}

	res, err := env.e.SyncAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := AgentSync{Deleted: 2, KeptBusy: 1, Materialized: 2, MaterializeFailed: 1}
	if res != want {
		t.Fatalf("sync = %+v, want %+v", res, want)
	}
	wantDeleted := []string{goneID, strayID}
	slices.Sort(wantDeleted)
	if got := env.deletedIDs(); !slices.Equal(got, wantDeleted) {
		t.Fatalf("agents.delete calls = %v, want %v (never main, live or busy)", got, wantDeleted)
	}
	for _, id := range []string{goneID, strayID} {
		if exists(openclaw.WorkspaceDir(env.dir, id)) {
			t.Errorf("workspace of %s kept", id)
		}
	}
	for _, id := range []string{liveID, editedID, busyID} {
		if !exists(openclaw.WorkspaceDir(env.dir, id)) {
			t.Errorf("workspace of %s removed", id)
		}
	}
	if e, _ := store.GetAgent(ctx, env.pool, edited.ID); e.MaterializedHash == edited.MaterializedHash {
		t.Fatal("the edited agent was not materialised again")
	}
	s, err := store.GetAgent(ctx, env.pool, seeded.ID)
	if err != nil || s.OpenClawAgentID == nil || s.MaterializedHash == "" {
		t.Fatalf("seeded agent not materialised: %+v %v", s, err)
	}
	if got := env.gatewayAgents(); !slices.Contains(got, *s.OpenClawAgentID) || !slices.Contains(got, liveID) {
		t.Fatalf("gateway agents = %v", got)
	}

	// A second pass has nothing to do: no RPC writes, the busy agent still kept.
	before := len(env.fc.callsFor("agents.create")) + len(env.fc.callsFor("config.patch"))
	env.fc.createErr = nil
	res, err = env.e.SyncAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 0 || res.KeptBusy != 1 || res.Materialized != 1 || res.MaterializeFailed != 0 {
		t.Fatalf("second sync = %+v, want only the broken agent materialised", res)
	}
	if after := len(env.fc.callsFor("agents.create")) + len(env.fc.callsFor("config.patch")); after != before+2 {
		t.Fatalf("second sync wrote %d times, want one create and one patch", after-before)
	}
}

// An agent the gateway lost (its state reset) is materialised again although its stored
// hash matches.
func TestSyncAgentsRestoresMissing(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	_, ocID := env.materialized("Lost")
	env.fc.mu.Lock()
	env.fc.agents = nil
	env.fc.mu.Unlock()
	res, err := env.e.SyncAgents(ctx)
	if err != nil || res.Materialized != 1 {
		t.Fatalf("sync = %+v %v, want one materialised", res, err)
	}
	if got := env.gatewayAgents(); !slices.Equal(got, []string{ocID}) {
		t.Fatalf("gateway agents = %v, want [%s]", got, ocID)
	}
}

// The keeper syncs at start and again after every gateway reconnect.
func TestKeepAgentsSyncsOnReconnect(t *testing.T) {
	env := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	stray := func(id string) {
		env.fc.mu.Lock()
		env.fc.agents = append(env.fc.agents, id)
		env.fc.mu.Unlock()
	}
	stray("zz-stray-one")
	go func() { env.e.KeepAgents(ctx); close(stopped) }()
	eventually(t, "the start-up sync", func() bool { return len(env.gatewayAgents()) == 0 })
	stray("zz-stray-two")
	env.fc.push(openclaw.EventReconnected, map[string]any{})
	eventually(t, "the sync after a reconnect", func() bool { return len(env.gatewayAgents()) == 0 })
}

// Between full syncs the keeper materialises agents created outside the api (yuno seed
// after make run) without removing anything: removals wait for a start or a reconnect.
func TestKeepAgentsMaterializesBetweenSyncs(t *testing.T) {
	env := newEnv(t)
	env.e.agentSync = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	go func() { env.e.KeepAgents(ctx); close(stopped) }()
	eventually(t, "the start-up sync", func() bool { return len(env.fc.callsFor("agents.list")) > 0 })

	env.fc.mu.Lock()
	env.fc.agents = append(env.fc.agents, "zz-stray-later")
	env.fc.mu.Unlock()
	seeded := env.agent(func(a *store.Agent) { a.Name = "Seeded later" })
	eventually(t, "the seeded agent materialised", func() bool {
		a, err := store.GetAgent(context.Background(), env.pool, seeded.ID)
		return err == nil && a.MaterializedHash != ""
	})
	if got := env.deletedIDs(); len(got) != 0 {
		t.Fatalf("the periodic pass removed %v", got)
	}
}
