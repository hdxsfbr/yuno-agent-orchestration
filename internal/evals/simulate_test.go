package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// The cases cannot run end to end while the backend is being built, so this file
// walks each case's script through a small model of the rules in DESIGN 5.3, 5.4, 5.6
// and 5.13 and builds the trace those rules say the run leaves behind. It answers three
// questions about a case without an api: is the script consumed exactly (no step left
// over, none missing), does the run end the way the case expects, and do all the
// evaluators accept the trace of a correct run. The model is deliberately dumb: it is
// the written-down assumption about the engine, nothing more.

const (
	simRunID = "run-sim"

	// sweepSeconds is how often the engine looks for expired approvals (DESIGN 5.2).
	sweepSeconds = 30
)

// platformTools are served over MCP and reach the model as yuno__<name>.
var platformTools = map[string]bool{
	"finish": true, "http_fetch": true, "memory_save": true, "memory_recall": true,
	"deploy_release": true, "gitlab_create_mr": true,
}

type simulator struct {
	c   Case
	def *workflow.Definition

	cursor  int                        // script steps consumed
	action  int                        // approval actions consumed
	written map[string]map[string]bool // agent label -> files it wrote under runs/$RUN_ID/: every agent has a workspace of its own
	clock   time.Duration

	state    map[string]any
	status   string
	reason   string
	problems []string
	messages int
	paused   int64 // ms the run spent waiting for an approval

	tr         store.Trace
	seenInputs map[string]string // visit|tool|input -> idempotency key of the first call
}

func (s *simulator) problem(format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

func (s *simulator) fail(reason string) { s.status, s.reason = store.RunFailed, reason }

func (s *simulator) tick() time.Time {
	s.clock += time.Second
	return t0.Add(s.clock)
}

// simulate runs the case's script through the model.
func simulate(t *testing.T, c Case) *simulator {
	t.Helper()
	s := &simulator{c: c, written: map[string]map[string]bool{}, state: map[string]any{}, seenInputs: map[string]string{}}
	if c.IsWorkflow() {
		def, err := c.Definition()
		if err != nil {
			t.Fatal(err)
		}
		s.def = def
	} else {
		s.def = workflow.SingleAgent("", c.Task, nil, c.Agent.Limits.Store())
	}
	s.walk()

	if s.cursor != len(c.Script) {
		s.problem("the script has %d steps but the run consumed %d", len(c.Script), s.cursor)
	}
	if s.action != len(c.Actions.Approvals) {
		s.problem("actions.approvals has %d actions but the run used %d", len(c.Actions.Approvals), s.action)
	}
	s.finalize()
	return s
}

func (s *simulator) walk() {
	limits := s.def.Limits
	maxVisits := limits.MaxLoopVisits
	if maxVisits == 0 {
		maxVisits = store.DefaultLimits.MaxLoopVisits
	}
	visits := map[string]int{}
	cur, _ := s.def.Start()
	for guard := 0; guard < 200; guard++ {
		n, ok := s.def.Node(cur)
		if !ok {
			s.problem("walk reached unknown node %q", cur)
			return
		}
		switch n.Kind {
		case workflow.KindEnd:
			s.status = store.RunSucceeded
			return
		case workflow.KindApproval:
			s.render(n, n.Message)
			if !s.approvalNode(n) {
				return
			}
		case workflow.KindAgent:
			s.render(n, n.Task)
			visits[n.ID]++
			if visits[n.ID] > maxVisits {
				s.fail(fmt.Sprintf("loop limit %d exceeded for %s", maxVisits, n.ID))
				return
			}
			if !s.visit(n, visits[n.ID]) {
				return
			}
		}
		// The state is compared as decoded JSON, as the engine's is.
		decoded, _ := normalizeJSON(s.state)
		next, found := s.def.Next(cur, decoded)
		if !found {
			s.fail("no matching edge from " + cur)
			return
		}
		if n.Kind == workflow.KindAgent {
			s.messages++
		}
		cur = next
	}
	s.problem("the walk did not end")
}

// render renders a task or message with the engine's renderer over the real input and
// the state so far, as the engine does before the node runs. An error would fail the
// node; "<no value>" would put garbage in front of the agent or the approver.
func (s *simulator) render(n *workflow.Node, text string) {
	input, err := s.c.InputJSON()
	if err != nil {
		s.problem("%v", err)
		return
	}
	state, _ := json.Marshal(s.state)
	out, err := workflow.Render(text, input, state)
	switch {
	case err != nil:
		s.problem("node %s: the template fails to render with the state so far: %v", n.ID, err)
	case strings.Contains(out, "<no value>"):
		s.problem("node %s: the template renders <no value>: %q", n.ID, out)
	case strings.TrimSpace(out) == "":
		s.problem("node %s: the template renders empty", n.ID)
	}
}

// approvalNode handles a workflow approval node (DESIGN 5.6 rule 8). It reports whether
// the run continues past the node.
func (s *simulator) approvalNode(n *workflow.Node) bool {
	timeout := s.def.Limits.ApprovalTimeoutSeconds
	if timeout == 0 {
		timeout = store.DefaultLimits.ApprovalTimeoutSeconds
	}
	rn := s.addNode(n, 1, store.NodeWaitingApproval)
	ap := store.Approval{
		ID: fmt.Sprintf("ap%d", len(s.tr.Approvals)+1), RunID: simRunID, RunNodeID: s.tr.Nodes[rn].ID, Kind: store.ApprovalKindNode,
		NodeID: n.ID, Status: store.ApprovalPending, RequestedAt: s.tick(),
	}
	wait := s.c.Actions.ApprovalWaitSeconds
	switch {
	case wait > timeout:
		// Left alone past its timeout, the approval expires and the run fails.
		if wait < timeout+sweepSeconds+5 {
			s.problem("approval_wait_seconds %d does not outlast the %d s timeout plus the %d s expiry sweep", wait, timeout, sweepSeconds)
		}
		ap.Status = store.ApprovalExpired
		s.finishNode(rn, store.NodeFailed)
		s.fail("approval expired")
		s.tr.Approvals = append(s.tr.Approvals, ap)
		s.paused = int64(timeout+sweepSeconds) * 1000
		return false
	case s.action >= len(s.c.Actions.Approvals):
		s.problem("approval node %s has no action in actions.approvals", n.ID)
		s.status = store.RunWaitingApproval
		return false
	}
	act := s.c.Actions.Approvals[s.action]
	s.action++
	decided := s.tick()
	ap.DecidedAt = &decided
	if act.Decision == DecisionReject {
		ap.Status = store.ApprovalRejected
		s.tr.Approvals = append(s.tr.Approvals, ap)
		s.finishNode(rn, store.NodeFailed)
		if n.OnReject != "" {
			s.problem("on_reject edges are not modelled")
		}
		s.fail("rejected at " + n.ID)
		return false
	}
	ap.Status = store.ApprovalApproved
	s.tr.Approvals = append(s.tr.Approvals, ap)
	s.finishNode(rn, store.NodeSucceeded)
	return true
}

// addNode records a node visit and returns its index in the trace's nodes.
func (s *simulator) addNode(n *workflow.Node, visit int, status string) int {
	started := s.tick()
	s.tr.Nodes = append(s.tr.Nodes, store.RunNode{
		ID: fmt.Sprintf("rn%d", len(s.tr.Nodes)+1), RunID: simRunID, NodeID: n.ID, Visit: visit, Status: status, StartedAt: &started,
	})
	return len(s.tr.Nodes) - 1
}

func (s *simulator) finishNode(i int, status string) {
	done := s.tick()
	s.tr.Nodes[i].Status, s.tr.Nodes[i].FinishedAt = status, &done
}

// agentSpec is the agent a node runs.
func (s *simulator) agentSpec(n *workflow.Node) AgentSpec {
	if !s.c.IsWorkflow() {
		return s.c.Agent
	}
	return s.c.Agents[n.AgentName]
}

// visit plays one agent node visit: model calls until a text reply, with the finish
// protocol of DESIGN 5.6 rules 6 and 7. It reports whether the run continues.
func (s *simulator) visit(n *workflow.Node, visit int) bool {
	spec := s.agentSpec(n)
	raw := n.OutputSchema
	if !hasSchema(raw) {
		raw = json.RawMessage(`{"type":"object"}`) // the engine accepts any object without a schema
	}
	schema, err := compileSchema(raw)
	if err != nil {
		s.problem("node %s: %v", n.ID, err)
		return false
	}
	rn := s.addNode(n, visit, store.NodeRunning)
	accepted, invalid := false, 0
	for {
		if s.cursor >= len(s.c.Script) {
			s.problem("the script ends inside node %s (visit %d)", n.ID, visit)
			s.fail("script exhausted")
			return false
		}
		step := s.c.Script[s.cursor]
		s.cursor++
		s.tr.ModelCalls = append(s.tr.ModelCalls, store.ModelCall{
			ID: fmt.Sprintf("m%d", s.cursor), RunID: simRunID, RunNodeID: s.tr.Nodes[rn].ID, Provider: "scripted", Model: "scripted-1",
			InputTokens: 100, OutputTokens: 10,
		})
		if step.Tool == nil {
			if !accepted {
				s.finishNode(rn, store.NodeFailed)
				s.fail("agent did not call finish")
				return false
			}
			s.finishNode(rn, store.NodeSucceeded)
			return true
		}
		if accepted {
			s.problem("node %s: script step %d is a tool call after finish was accepted; the closing step must be text", n.ID, s.cursor)
		}
		switch s.toolCall(n, s.tr.Nodes[rn].ID, visit, spec, step.Tool, schema) {
		case callAccepted:
			accepted = true
			s.tr.Nodes[rn].Output = s.outputOf(n.ID)
		case callInvalid:
			invalid++
			if invalid >= 3 {
				s.finishNode(rn, store.NodeFailed)
				s.fail("output schema: invalid result")
				return false
			}
		}
	}
}

func (s *simulator) outputOf(nodeID string) json.RawMessage {
	raw, _ := json.Marshal(s.state[nodeID])
	return raw
}

type callResult int

const (
	callDone callResult = iota
	callAccepted
	callInvalid
)

// toolCall plays one scripted tool call and records the row it leaves.
func (s *simulator) toolCall(n *workflow.Node, runNodeID string, visit int, spec AgentSpec, tool *ScriptTool, schema *jsonschema.Schema) callResult {
	name := toolBase(tool.Name)
	platform := strings.HasPrefix(tool.Name, platformPrefix)
	input, _ := json.Marshal(tool.Args)
	started := s.tick()
	row := store.ToolCall{
		ID: fmt.Sprintf("tc%d", len(s.tr.ToolCalls)+1), RunID: simRunID, RunNodeID: runNodeID, ToolName: name,
		Input: input, Status: store.ToolExecuted, StartedAt: &started,
	}
	result := callDone
	if platform && tool.Args["run_token"] != "$RUN_TOKEN" {
		s.problem("node %s: %s must carry run_token \"$RUN_TOKEN\"", n.ID, tool.Name)
	}
	if platform != platformTools[name] {
		s.problem("node %s: %s: platform tools carry the %s prefix and built-ins do not", n.ID, tool.Name, platformPrefix)
	}

	rule := spec.InteractionRules[name]
	allowlisted := name == finishTool || allowed(spec.ToolAllowlist, name)
	switch {
	case rule == "blocked":
		// OpenClaw refuses a denied tool itself and records no assistant message with
		// usage for the attempt (first live run of case 011, 2026-10-15), so the model
		// call this step produced is not part of the trace the platform records.
		s.tr.ModelCalls = s.tr.ModelCalls[:len(s.tr.ModelCalls)-1]
		row.Status, row.IsError = store.ToolBlocked, true
		row.Output = json.RawMessage(`{"error":"tool is blocked for this agent"}`)
	case !allowlisted:
		s.problem("node %s: tool %s is not in the allowlist of its agent", n.ID, name)
		row.Status = store.ToolBlocked
	case name == finishTool:
		res, _ := json.Marshal(tool.Args["result"])
		if msg := validateDoc(schema, res); msg != "" {
			row.Status, row.IsError = store.ToolFailed, true
			row.Output, _ = json.Marshal(map[string]string{"error": msg})
			result = callInvalid
		} else {
			s.state[n.ID] = tool.Args["result"]
			row.Output = json.RawMessage(`{"status":"accepted"}`)
			result = callAccepted
		}
	case sideEffectingTools[name]:
		s.sideEffect(&row, n, visit, rule, tool)
	default:
		s.builtin(n, name, tool.Args)
	}
	done := s.tick()
	row.FinishedAt = &done
	s.tr.ToolCalls = append(s.tr.ToolCalls, row)
	return result
}

// builtin checks the arguments of an OpenClaw built-in and tracks the files written.
func (s *simulator) builtin(n *workflow.Node, name string, args map[string]any) {
	str := func(key string) string { v, _ := args[key].(string); return v }
	switch name {
	case "write":
		const prefix = "runs/$RUN_ID/"
		path := str("path")
		if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) || str("content") == "" {
			s.problem("node %s: write needs a path under %s and content, got %v", n.ID, prefix, args)
			return
		}
		label := s.label(n)
		if s.written[label] == nil {
			s.written[label] = map[string]bool{}
		}
		s.written[label][strings.TrimPrefix(path, prefix)] = true
	case "read", "ls":
		if str("path") == "" {
			s.problem("node %s: %s needs a path", n.ID, name)
		}
	case "exec":
		if str("command") == "" {
			s.problem("node %s: exec needs a command", n.ID)
		}
	}
}

// sideEffect plays a side-effecting call: the approval gate of DESIGN 5.3 rule 5 unless
// the rule is auto, then the idempotent execution of DESIGN 5.4.
func (s *simulator) sideEffect(row *store.ToolCall, n *workflow.Node, visit int, rule string, tool *ScriptTool) {
	paths, _ := tool.Args["paths"].([]any)
	if len(paths) == 0 {
		s.problem("node %s: %s needs paths", n.ID, tool.Name)
	}
	for _, p := range paths {
		if rel, _ := p.(string); !s.written[s.label(n)][rel] {
			s.problem("node %s: %s names %v, which this agent has not written under runs/$RUN_ID/ (each agent has a workspace of its own)", n.ID, tool.Name, p)
		}
	}
	in := canonicalJSON(row.Input)
	id := fmt.Sprintf("%s|%s|%s", row.RunNodeID, row.ToolName, in)
	sum := sha256.Sum256([]byte(simRunID + id))
	key := hex.EncodeToString(sum[:])
	row.IdempotencyKey = &key

	if rule != "auto" {
		if s.action >= len(s.c.Actions.Approvals) {
			s.problem("node %s: %s needs an approval and the case has no action left", n.ID, row.ToolName)
			row.Status = store.ToolAwaitingApproval
			return
		}
		act := s.c.Actions.Approvals[s.action]
		s.action++
		requested := s.tick()
		decided := s.tick()
		ap := store.Approval{
			ID: fmt.Sprintf("ap%d", len(s.tr.Approvals)+1), RunID: simRunID, RunNodeID: row.RunNodeID, ToolCallID: &row.ID,
			Kind: store.ApprovalKindTool, NodeID: n.ID, Status: store.ApprovalApproved, RequestedAt: requested, DecidedAt: &decided,
		}
		if act.Decision == DecisionReject {
			ap.Status = store.ApprovalRejected
			row.Status = store.ToolRejected
			s.tr.Approvals = append(s.tr.Approvals, ap)
			return
		}
		s.tr.Approvals = append(s.tr.Approvals, ap)
	}

	if _, replay := s.seenInputs[id]; replay {
		row.Output = json.RawMessage(`{"release_id":"rel-1","replayed":true}`)
		return
	}
	s.seenInputs[id] = key
	row.Output = json.RawMessage(`{"release_id":"rel-1"}`)
	s.tr.SideEffects = append(s.tr.SideEffects, store.SideEffect{
		IdempotencyKey: key, ToolName: row.ToolName, RunID: simRunID, ToolCallID: row.ID, Result: row.Output, CreatedAt: t0,
	})
}

// finalize fills in the run of the trace.
func (s *simulator) finalize() {
	started, finished := t0, t0.Add(s.clock+time.Duration(s.paused)*time.Millisecond)
	run := &store.Run{
		ID: simRunID, Status: s.status, StepCount: s.cursor, Limits: s.def.Limits,
		StartedAt: &started, FinishedAt: &finished, PausedTotalMs: s.paused,
	}
	if s.reason != "" {
		run.FailureReason = &s.reason
	}
	run.State, _ = json.Marshal(s.state)
	s.tr.Run = run
	s.tr.Spans = []store.Span{{ID: "s1", RunID: simRunID, Kind: "run", Name: "run"}}
	s.tr.AgentMessages = make([]store.AgentMessage, s.messages)
}

// label names the agent that runs a node: its key in the case's agents, or "agent" for
// a quick run.
func (s *simulator) label(n *workflow.Node) string {
	if s.c.IsWorkflow() {
		return n.AgentName
	}
	return "agent"
}
