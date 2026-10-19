package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
)

const maxAgentBody = 1 << 20

// Enumerations from api/openapi.yaml (AgentInput, ExecPolicy).
var (
	effortLevels     = []string{"low", "medium", "high", "xhigh", "max"}
	thinkingLevels   = []string{"off", "minimal", "low", "medium", "high", "xhigh", "adaptive", "max"}
	interactionModes = []string{"auto", "require_approval", "blocked"}
	execSecurity     = []string{"deny", "allowlist", "full"}
	execAsk          = []string{"off", "on-miss", "always"}
)

// defaultMaxTokens mirrors the agents.max_tokens column default.
const defaultMaxTokens = 8192

// agentInput is AgentInput. The Agent read-only fields are accepted and ignored so a
// client can PUT back what it fetched.
type agentInput struct {
	Name             string             `json:"name"`
	Role             string             `json:"role"`
	SystemPrompt     string             `json:"system_prompt"`
	Model            string             `json:"model"`
	Effort           string             `json:"effort"`
	Thinking         string             `json:"thinking"`
	MaxTokens        *int               `json:"max_tokens"`
	ToolAllowlist    []string           `json:"tool_allowlist"`
	InteractionRules map[string]string  `json:"interaction_rules"`
	Limits           store.Limits       `json:"limits"`
	Guardrails       store.Guardrails   `json:"guardrails"`
	MemoryPolicy     store.MemoryPolicy `json:"memory_policy"`
	ExecPolicy       store.ExecPolicy   `json:"exec_policy"`
	Skills           []string           `json:"skills"`
	// Channels is read-only (bindings are made on the Channels page): accepted so a client
	// that echoes a whole agent back is not refused, never validated or stored.
	Channels json.RawMessage `json:"channels"`

	ID               json.RawMessage `json:"id"`
	OpenClawAgentID  json.RawMessage `json:"openclaw_agent_id"`
	MaterializedHash json.RawMessage `json:"materialized_hash"`
	CreatedAt        json.RawMessage `json:"created_at"`
	UpdatedAt        json.RawMessage `json:"updated_at"`
}

// validate checks the input against the contract and the tool registry.
func (in *agentInput) validate(registry *tools.Registry) fieldErrors {
	var fe fieldErrors
	switch n := utf8.RuneCountInString(in.Name); {
	case strings.TrimSpace(in.Name) == "":
		fe.add("name", "is required")
	case n > 80:
		fe.add("name", "must be at most 80 characters")
	}
	if in.Model == "" {
		fe.add("model", "is required")
	} else if models := openclaw.ManagedModels; !slices.Contains(models, in.Model) {
		fe.add("model", "must be one of %s", strings.Join(models, ", "))
	}
	checkEnum(&fe, "effort", in.Effort, effortLevels)
	checkEnum(&fe, "thinking", in.Thinking, thinkingLevels)
	if in.MaxTokens != nil && *in.MaxTokens < 1 {
		fe.add("max_tokens", "must be a positive integer")
	}
	// Zero means "use the default"; a negative limit is an error (DESIGN 5.2), here and
	// again when a run inherits the agent's limits.
	for _, l := range []struct {
		name string
		v    float64
	}{
		{"max_steps", float64(in.Limits.MaxSteps)},
		{"max_duration_seconds", float64(in.Limits.MaxDurationSeconds)},
		{"max_node_iterations", float64(in.Limits.MaxNodeIterations)},
		{"max_loop_visits", float64(in.Limits.MaxLoopVisits)},
		{"max_cost_usd", in.Limits.MaxCostUSD},
		{"max_tool_calls", float64(in.Limits.MaxToolCalls)},
		{"approval_timeout_seconds", float64(in.Limits.ApprovalTimeoutSeconds)},
	} {
		if l.v < 0 {
			fe.add("limits."+l.name, "must not be negative")
		}
	}
	known := registry.Names()
	for i, name := range in.ToolAllowlist {
		if !known[name] && !tools.IsOpenClawBuiltin(name) {
			fe.add(fmt.Sprintf("tool_allowlist[%d]", i), "unknown tool %q", name)
		}
	}
	for _, key := range sortedKeys(in.InteractionRules) {
		checkInteractionRule(&fe, registry, key, in.InteractionRules[key])
	}
	checkEnum(&fe, "exec_policy.security", in.ExecPolicy.Security, execSecurity)
	checkEnum(&fe, "exec_policy.ask", in.ExecPolicy.Ask, execAsk)
	checkEnum(&fe, "exec_policy.ask_fallback", in.ExecPolicy.AskFallback, execSecurity)
	return fe
}

// apply copies the configuration onto a (new or stored) agent row.
func (in *agentInput) apply(a *store.Agent) {
	a.Name = strings.TrimSpace(in.Name)
	a.Role = in.Role
	a.SystemPrompt = in.SystemPrompt
	a.Model = in.Model
	a.Effort = in.Effort
	a.Thinking = in.Thinking
	a.MaxTokens = defaultMaxTokens
	if in.MaxTokens != nil {
		a.MaxTokens = *in.MaxTokens
	}
	a.ToolAllowlist = in.ToolAllowlist
	a.InteractionRules = in.InteractionRules
	a.Limits = in.Limits
	a.Guardrails = in.Guardrails
	a.MemoryPolicy = in.MemoryPolicy
	a.ExecPolicy = in.ExecPolicy
	a.Skills = in.Skills
	if a.Channels == nil {
		a.Channels = []string{} // never stored from input; the bindings decide what is shown
	}
}

// withBindings sets the agent's channels to the ones it is bound to now.
func (s *Server) withBindings(ctx context.Context, w http.ResponseWriter, r *http.Request, a *store.Agent) bool {
	bound, err := store.ListAgentChannels(ctx, s.pool)
	if err != nil {
		s.internalError(w, r, "list agent channels", err)
		return false
	}
	a.Channels = bound[a.ID]
	if a.Channels == nil {
		a.Channels = []string{}
	}
	return true
}

// checkInteractionRule validates one interaction rule (DESIGN 5.3 rules 6 and 7):
// the key names a known tool, the value is auto, require_approval or blocked (a
// side-effecting tool may be auto, D-009), require_approval only gates platform tools
// (built-ins follow the exec policy), finish, the run protocol itself, stays auto, and
// ask_operator is auto or blocked (DESIGN 5.24 rule 1).
func checkInteractionRule(fe *fieldErrors, registry *tools.Registry, key, value string) {
	path := "interaction_rules." + key
	name := tools.PlainName(key)
	_, platform := registry.Get(name)
	switch {
	case name == tools.AskOperatorName && value == tools.PolicyRequireApproval:
		// A gated question would wait twice and outlive OpenClaw's request timeout
		// (DESIGN 5.24 rule 1).
		fe.add(path, "must be auto or blocked")
	case !platform && !tools.IsOpenClawBuiltin(name):
		fe.add(path, "unknown tool %q", key)
	case !slices.Contains(interactionModes, value):
		fe.add(path, "must be one of %s", strings.Join(interactionModes, ", "))
	case name == tools.FinishName && value != tools.PolicyAuto:
		fe.add(path, "finish is how a node reports its result and is always auto")
	case !platform && value == tools.PolicyRequireApproval:
		fe.add(path, "require_approval applies to platform tools; built-in tools follow the exec policy")
	}
}

func checkEnum(fe *fieldErrors, path, v string, allowed []string) {
	if v != "" && !slices.Contains(allowed, v) {
		fe.add(path, "must be one of %s", strings.Join(allowed, ", "))
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// agentListItem is an agent in GET /api/agents (AgentListItem): channels are the ones it
// answers, from the channel bindings rather than the form's setting, and activity counts
// its node visits in flight (audit #17, #18).
type agentListItem struct {
	store.Agent
	Activity store.AgentActivity `json:"activity"`
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents, err := store.ListAgents(ctx, s.pool)
	if err != nil {
		s.internalError(w, r, "list agents", err)
		return
	}
	bound, err := store.ListAgentChannels(ctx, s.pool)
	if err != nil {
		s.internalError(w, r, "list agent channels", err)
		return
	}
	activity, err := store.ListAgentActivity(ctx, s.pool)
	if err != nil {
		s.internalError(w, r, "list agent activity", err)
		return
	}
	items := make([]agentListItem, len(agents))
	for i, a := range agents {
		a.Channels = bound[a.ID]
		if a.Channels == nil {
			a.Channels = []string{}
		}
		items[i] = agentListItem{Agent: a, Activity: activity[a.ID]}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := store.GetAgent(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if !s.withBindings(r.Context(), w, r, a) {
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// createAgent writes the row, then materialises it in OpenClaw. A materialisation
// failure leaves the row in place and answers 502 with its id.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var in agentInput
	if !decodeBody(w, r, &in, maxAgentBody) {
		return
	}
	fe := in.validate(s.tools)
	known := func(n string) bool { _, ok := s.tools.Get(n); return ok || tools.IsOpenClawBuiltin(n) }
	for _, p := range guardrails.Validate(in.Guardrails, known) {
		fe.add(p.Path, "%s", p.Message)
	}
	skillErrs, err := s.checkAgentSkills(r.Context(), in.Skills)
	if err != nil {
		s.internalError(w, r, "check skills", err)
		return
	}
	if fe = append(fe, skillErrs...); len(fe) > 0 {
		writeValidation(w, "invalid agent", fe)
		return
	}
	a := &store.Agent{}
	in.apply(a)
	if err := store.CreateAgent(r.Context(), s.pool, a); err != nil {
		s.internalError(w, r, "create agent", err)
		return
	}
	saved, ok := s.materializeSaved(w, r, a.ID)
	if !ok {
		return
	}
	if !s.withBindings(r.Context(), w, r, saved) {
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// updateAgent replaces the configuration and re-materialises (same 502 rule as create).
func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	a, err := store.GetAgent(r.Context(), s.pool, r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	var in agentInput
	if !decodeBody(w, r, &in, maxAgentBody) {
		return
	}
	fe := in.validate(s.tools)
	known := func(n string) bool { _, ok := s.tools.Get(n); return ok || tools.IsOpenClawBuiltin(n) }
	for _, p := range guardrails.Validate(in.Guardrails, known) {
		fe.add(p.Path, "%s", p.Message)
	}
	skillErrs, err := s.checkAgentSkills(r.Context(), in.Skills)
	if err != nil {
		s.internalError(w, r, "check skills", err)
		return
	}
	if fe = append(fe, skillErrs...); len(fe) > 0 {
		writeValidation(w, "invalid agent", fe)
		return
	}
	in.apply(a)
	if err := store.UpdateAgent(r.Context(), s.pool, a); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	saved, ok := s.materializeSaved(w, r, a.ID)
	if !ok {
		return
	}
	if !s.withBindings(r.Context(), w, r, saved) {
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// materializeSaved pushes a just-saved agent to OpenClaw and re-reads the row, which
// the materialiser updates. On failure it answers 502 with the agent id in details.
func (s *Server) materializeSaved(w http.ResponseWriter, r *http.Request, id string) (*store.Agent, bool) {
	if _, err := s.materialize(r.Context(), id); err != nil {
		s.log.Warn("agent saved but not materialised", "agent_id", id, "error", err)
		writeError(w, http.StatusBadGateway, "materialize_failed",
			"the agent was saved but could not be pushed to OpenClaw: "+obs.Redact(err.Error()), map[string]string{"id": id})
		return nil, false
	}
	saved, err := store.GetAgent(r.Context(), s.pool, id)
	if err != nil {
		s.storeError(w, r, "agent", err)
		return nil, false
	}
	return saved, true
}

func (s *Server) materialize(ctx context.Context, id string) (*engine.RuntimeView, error) {
	if s.engine == nil {
		return nil, errEngineUnavailable
	}
	return s.engine.MaterializeAgent(ctx, id)
}

// agentRemoveTimeout bounds taking a deleted agent out of OpenClaw within the request.
const agentRemoveTimeout = 15 * time.Second

// deleteAgent soft-deletes the agent, closes Telegram if it was the bound agent, and
// removes it from OpenClaw. A runtime that refuses is logged, not answered: the
// deletion stands and the sync at the next api start removes what is left.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := store.DeleteAgent(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	s.unbindAgent(r.Context(), id)
	if s.engine != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentRemoveTimeout)
		err := s.engine.RemoveAgent(ctx, id)
		cancel()
		if err != nil {
			s.log.Warn("deleted agent not removed from OpenClaw", "agent_id", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) materializeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := store.GetAgent(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	view, err := s.materialize(r.Context(), id)
	if err != nil {
		s.runtimeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) agentRuntime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := store.GetAgent(r.Context(), s.pool, id); err != nil {
		s.storeError(w, r, "agent", err)
		return
	}
	if s.engine == nil {
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "the run engine is not available", nil)
		return
	}
	view, err := s.engine.AgentRuntime(r.Context(), id)
	if err != nil {
		s.runtimeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// runtimeError maps a materialiser failure: unknown agent 404, anything else is the
// gateway's and answers 502.
func (s *Server) runtimeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "agent not found", nil)
		return
	}
	s.log.Warn("openclaw runtime call failed", "request_path", r.URL.Path, "error", err)
	writeError(w, http.StatusBadGateway, "openclaw_error", obs.Redact(err.Error()), nil)
}
