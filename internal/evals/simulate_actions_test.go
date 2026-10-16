package evals

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// The parts of the simulator that model wave 3 features: agent memory (DESIGN 5.10),
// the rate limit (5.11 rule 2), the tool list and the schedule (5.9). They are the
// written-down assumption about the backend, like the rest of the model; the real
// behaviour is checked by `make eval` against a running api.

// memory plays memory_save and memory_recall for the agent the node runs. Facts belong
// to the agent, not to the run or the node, so a later node of the same agent sees them.
func (s *simulator) memory(row *store.ToolCall, n *workflow.Node, spec AgentSpec, name string, args map[string]any) {
	if !spec.MemoryPolicy.Enabled {
		s.problem("node %s: %s is called but the agent's memory_policy is not enabled, so the platform does not offer the tool", n.ID, name)
		row.Status, row.IsError = store.ToolFailed, true
		row.Output = json.RawMessage(`{"error":"memory is not enabled for this agent"}`)
		return
	}
	label := s.label(n)
	if s.facts[label] == nil {
		s.facts[label] = map[string]string{}
	}
	str := func(key string) string { v, _ := args[key].(string); return v }
	switch name {
	case "memory_save":
		if str("key") == "" || str("value") == "" {
			s.problem("node %s: memory_save needs a key and a value, got %v", n.ID, args)
		}
		s.facts[label][str("key")] = str("value")
		row.Output, _ = json.Marshal(map[string]string{"status": "saved", "key": str("key")})
	case "memory_recall":
		type fact struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		out := []fact{}
		query := strings.ToLower(str("query"))
		for _, k := range sortedKeys(s.facts[label]) {
			v := s.facts[label][k]
			if strings.Contains(strings.ToLower(k), query) || strings.Contains(strings.ToLower(v), query) {
				out = append(out, fact{Key: k, Value: v})
			}
		}
		row.Output, _ = json.Marshal(map[string]any{"facts": out})
	}
}

// observe builds what the runner would see outside the trace for the case's actions.
func (s *simulator) observe() {
	if s.c.Actions.Schedule != nil {
		runID := simRunID
		s.obs.Schedule = &ScheduleView{ID: "sch-sim", WorkflowID: "wf-sim", Kind: ScheduleKindDelay, Enabled: false, LastRunID: &runID}
	}
	if s.c.Actions.Retrigger {
		s.obs.Retrigger = s.retrigger()
	}
	if len(s.c.Expect.ToolsPresent) > 0 || len(s.c.Expect.ToolsAbsent) > 0 {
		s.obs.Tools, s.obs.ToolsListed = s.toolList(), true
	}
}

// retrigger plays the extra trigger: the agents' runs in the last hour are counted,
// the first run included, and one at its max_runs_per_hour refuses the trigger with 429
// and the message of DESIGN 5.11 rule 2.
func (s *simulator) retrigger() *TriggerAttempt {
	for _, key := range s.c.AgentKeys() {
		spec := s.c.Agents[key]
		if limit := spec.Guardrails.MaxRunsPerHour; limit > 0 && limit <= 1 {
			return &TriggerAttempt{Status: 429, Message: fmt.Sprintf("rate limit: %d runs/hour for agent eval-%s-%s-sim", limit, s.c.ID, key)}
		}
	}
	return &TriggerAttempt{Status: 201, Message: "the api created a run"}
}

// toolList is GET /api/tools of a deployment without GitLab: the platform tools, then
// the OpenClaw built-ins.
func (s *simulator) toolList() []string {
	var names []string
	for name := range platformTools {
		if name != "gitlab_create_mr" {
			names = append(names, name)
		}
	}
	names = append(names, "read", "write", "edit", "apply_patch", "ls", "exec", "process")
	slices.Sort(names)
	return names
}
