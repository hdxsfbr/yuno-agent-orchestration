package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"yuno/internal/model"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Case splits. SplitAll is only valid as a filter, never as a case's own split.
const (
	SplitPublic  = "public"
	SplitPrivate = "private"
	SplitAll     = "all"
)

// Approval decisions an action can take, as the approvals endpoints name them.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// caseIDPattern keeps ids safe for agent names, script ids and file names; the longest
// id plus the runner's "-<6 hex>" suffix stays within the 80 characters script ids allow.
var caseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,60}$`)

// Case is one versioned evaluation case. The YAML format is documented in
// evals/README.md. A case is either a quick run (Agent and Task: one temporary agent
// runs one task) or a workflow run (Workflow, Input and Agents: an inline definition
// whose agent nodes name the agents of the Agents map).
type Case struct {
	ID          string `yaml:"id"`
	Version     string `yaml:"version"`
	Split       string `yaml:"split"`
	Description string `yaml:"description"`

	Agent AgentSpec `yaml:"agent"`
	Task  string    `yaml:"task"`

	// Workflow is a workflow.Definition in its JSON shape, held as a generic mapping
	// because the definition's fields carry JSON tags only. Definition decodes it.
	Workflow map[string]any       `yaml:"workflow"`
	Input    map[string]any       `yaml:"input"`
	Agents   map[string]AgentSpec `yaml:"agents"`

	Script  []ScriptStep      `yaml:"script"`
	Actions Actions           `yaml:"actions"`
	Expect  Expect            `yaml:"expect"`
	Rubric  []RubricCriterion `yaml:"rubric"`

	// Path is the file the case was loaded from.
	Path string `yaml:"-"`
}

// IsWorkflow reports whether the case runs an inline workflow rather than one agent.
func (c Case) IsWorkflow() bool { return c.Workflow != nil }

// RunsModel reports whether the case has any agent turn to script: a quick run always
// does, a workflow only when it has an agent node. A workflow without one (start to end,
// DESIGN 5.6) needs no agents and no script.
func (c Case) RunsModel() bool {
	if !c.IsWorkflow() {
		return true
	}
	def, err := c.Definition()
	if err != nil {
		return true
	}
	for _, n := range def.Nodes {
		if n.Kind == workflow.KindAgent {
			return true
		}
	}
	return false
}

// AgentSpec describes a temporary agent created for the case. In a quick run Name is a
// label sent as the agent's role; in a workflow case the key of the agents map is the
// label and Name is optional. Created agents are always named
// "eval-<id>[-<key>]-<random>".
type AgentSpec struct {
	Name          string   `yaml:"name"`
	SystemPrompt  string   `yaml:"system_prompt"`
	ToolAllowlist []string `yaml:"tool_allowlist"`
	Limits        Limits   `yaml:"limits"`
	// InteractionRules maps a tool name to auto, require_approval or blocked
	// (DESIGN 5.3 rule 6). The approval_gate evaluator reads it to know which side
	// effects were allowed to run without a human decision.
	InteractionRules map[string]string `yaml:"interaction_rules"`
	// Guardrails and MemoryPolicy are the agent's DESIGN 5.11 and 5.10 settings, sent
	// to the api as they are.
	Guardrails   Guardrails   `yaml:"guardrails"`
	MemoryPolicy MemoryPolicy `yaml:"memory_policy"`
}

func (a AgentSpec) isZero() bool {
	return a.Name == "" && a.SystemPrompt == "" && len(a.ToolAllowlist) == 0 &&
		a.Limits == (Limits{}) && len(a.InteractionRules) == 0 &&
		a.Guardrails.isZero() && a.MemoryPolicy == (MemoryPolicy{})
}

// Limits mirrors store.Limits with YAML tags. Zero means the platform default.
type Limits struct {
	MaxSteps               int     `yaml:"max_steps"`
	MaxDurationSeconds     int     `yaml:"max_duration_seconds"`
	MaxNodeIterations      int     `yaml:"max_node_iterations"`
	MaxLoopVisits          int     `yaml:"max_loop_visits"`
	MaxCostUSD             float64 `yaml:"max_cost_usd"`
	MaxToolCalls           int     `yaml:"max_tool_calls"`
	ApprovalTimeoutSeconds int     `yaml:"approval_timeout_seconds"`
}

// Store converts the limits to the API's shape.
func (l Limits) Store() store.Limits {
	return store.Limits{
		MaxSteps:               l.MaxSteps,
		MaxDurationSeconds:     l.MaxDurationSeconds,
		MaxNodeIterations:      l.MaxNodeIterations,
		MaxLoopVisits:          l.MaxLoopVisits,
		MaxCostUSD:             l.MaxCostUSD,
		MaxToolCalls:           l.MaxToolCalls,
		ApprovalTimeoutSeconds: l.ApprovalTimeoutSeconds,
	}
}

// RunLimits are the limits the case asks the run to use: the workflow definition's for
// a workflow case, the agent's for a quick run. Evaluators fall back to them when the
// run does not carry its own snapshot.
func (c Case) RunLimits() store.Limits {
	if c.IsWorkflow() {
		if def, err := c.Definition(); err == nil {
			return def.Limits
		}
		return store.Limits{}
	}
	return c.Agent.Limits.Store()
}

// ScriptStep is one scripted model response: exactly one of Text or Tool.
type ScriptStep struct {
	Text  string       `yaml:"text"`
	Tool  *ScriptTool  `yaml:"tool"`
	Usage *ScriptUsage `yaml:"usage"`
}

// ScriptTool is a tool call the scripted model makes. Args is a mapping in YAML.
type ScriptTool struct {
	Name string         `yaml:"name"`
	Args map[string]any `yaml:"args"`
}

// ScriptUsage is the token usage the scripted provider reports for a step.
type ScriptUsage struct {
	Prompt     int `yaml:"prompt"`
	Completion int `yaml:"completion"`
}

// Actions are things the runner does to the run while it executes.
type Actions struct {
	// CancelAfterSteps cancels the run once its step_count reaches this value. Zero
	// means never.
	CancelAfterSteps int `yaml:"cancel_after_steps"`
	// Approvals are applied in order to the run's pending approvals as the runner sees
	// them, whatever their kind. A pending approval with no action left is a case error.
	Approvals []ApprovalAction `yaml:"approvals"`
	// ApprovalWaitSeconds makes the runner leave the first pending approval alone for
	// that long before it applies any action; an expiry case waits past the timeout.
	ApprovalWaitSeconds int `yaml:"approval_wait_seconds"`
	// CancelWhenWaiting cancels the run once it is waiting for approval, instead of
	// deciding the approval (DESIGN 5.3 rule 8).
	CancelWhenWaiting bool `yaml:"cancel_when_waiting"`
	// Retrigger triggers the same workflow again once the run has ended and records the
	// HTTP status of the answer (expect.trigger_status). A rate limit refuses it (429).
	Retrigger bool `yaml:"retrigger"`
	// Schedule starts the run through a schedule instead of POST /api/runs.
	Schedule *ScheduleAction `yaml:"schedule"`
}

// ScheduleAction makes the runner trigger the case through a one-shot schedule
// (DESIGN 5.9): POST /api/schedules with kind delay and run_at set RunInSeconds from
// now, then wait for the run the scheduler creates.
type ScheduleAction struct {
	Kind         string `yaml:"kind"`           // delay (the only kind that fires within a case)
	RunInSeconds int    `yaml:"run_in_seconds"` // run_at = now + this
	WaitSeconds  int    `yaml:"wait_seconds"`   // how long to wait for the run to appear; default 30
}

// ScheduleKindDelay is the one-shot schedule kind of DESIGN 5.9.
const ScheduleKindDelay = "delay"

// Bounds for a schedule action, in seconds.
const (
	maxScheduleRunIn = 30
	maxScheduleWait  = 120
	// defaultScheduleWait covers one scheduler tick (SCHEDULER_TICK, default 10 s) plus
	// the delay and some margin.
	defaultScheduleWait = 30
)

// wait returns the time the runner waits for a scheduled run to appear.
func (s ScheduleAction) wait() int {
	if s.WaitSeconds > 0 {
		return s.WaitSeconds
	}
	return defaultScheduleWait
}

// ApprovalAction is one human decision the runner makes on a pending approval.
type ApprovalAction struct {
	Decision string `yaml:"decision"` // approve | reject
	Reason   string `yaml:"reason"`
}

// Expect is what the expectation evaluator checks. Zero values mean "not checked",
// except Status, which is required.
type Expect struct {
	Status                string `yaml:"status"`
	FailureReasonContains string `yaml:"failure_reason_contains"`
	MinToolCalls          int    `yaml:"min_tool_calls"`
	MinModelCalls         int    `yaml:"min_model_calls"`
	MinAgentMessages      int    `yaml:"min_agent_messages"`
	// State maps a JSON pointer into the run's final state to the value it must have;
	// every entry is checked.
	State map[string]any `yaml:"state"`
	// ToolStatuses maps a tool name to the statuses its tool call rows must show, in
	// order: each listed status must be matched by a distinct row of that tool, earlier
	// rows first, other rows may sit in between. An empty list means the run must have
	// no row for the tool at all.
	ToolStatuses map[string][]string `yaml:"tool_statuses"`
	// NeverToolStatuses maps a tool name to statuses none of its rows may have. It states
	// what must not happen without requiring a row, for a call the runtime may refuse
	// before the platform records it.
	NeverToolStatuses map[string][]string `yaml:"never_tool_statuses"`
	// ApprovalStatuses, when set, is the exact list of the run's approval statuses in
	// the order they were requested. An empty list means the run raised no approval.
	ApprovalStatuses *[]string `yaml:"approval_statuses"`
	// ApprovalReasons, when set, lists in request order a substring each approval's
	// reason must contain (case-insensitive); an empty entry is not checked. A list
	// shorter than the approvals checks the first ones only.
	ApprovalReasons []string `yaml:"approval_reasons"`
	// ToolOutputs maps a tool name to texts that must each appear in the output of at
	// least one row of that tool (case-insensitive, in any string of the output).
	ToolOutputs map[string][]string `yaml:"tool_outputs"`

	// The next fields are checked against what the runner observed outside the run's
	// own trace (see Observed).

	// Trigger is the run's trigger (manual, schedule, channel). With "schedule" the run's
	// trigger_ref must also name the schedule the runner created ("<id>@<occurrence>").
	Trigger string `yaml:"trigger"`
	// ScheduleEnabled is whether the schedule the case created is still enabled once
	// the run has ended; a delay schedule disables itself after it fires.
	ScheduleEnabled *bool `yaml:"schedule_enabled"`
	// TriggerStatus is the HTTP status of the extra trigger of actions.retrigger.
	TriggerStatus int `yaml:"trigger_status"`
	// TriggerErrorContains is a case-insensitive substring of that answer's message.
	TriggerErrorContains string `yaml:"trigger_error_contains"`
	// ToolsPresent and ToolsAbsent are tool names GET /api/tools must, or must not, list.
	ToolsPresent []string `yaml:"tools_present"`
	ToolsAbsent  []string `yaml:"tools_absent"`
}

// ModelSteps converts the script to the API's registration shape.
func (c Case) ModelSteps() ([]model.ScriptStep, error) {
	steps := make([]model.ScriptStep, 0, len(c.Script))
	for i, s := range c.Script {
		var ms model.ScriptStep
		switch {
		case s.Tool != nil:
			args := s.Tool.Args
			if args == nil {
				args = map[string]any{}
			}
			raw, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("script step %d: tool args are not JSON-encodable: %w", i+1, err)
			}
			ms.Tool = &model.ScriptToolCall{Name: s.Tool.Name, Args: raw}
		default:
			ms.Text = s.Text
		}
		if s.Usage != nil {
			ms.Usage = &model.ScriptUsage{Prompt: s.Usage.Prompt, Completion: s.Usage.Completion}
		}
		steps = append(steps, ms)
	}
	return steps, nil
}

// LoadCases reads every *.yaml and *.yml file under dir recursively, validates each
// against the version directory it sits in, and returns the cases of the requested split
// ("public", "private" or "all") sorted by id. Any invalid file fails the load, so a
// broken case is never silently skipped.
func LoadCases(dir, split string) ([]Case, error) {
	split = strings.ToLower(strings.TrimSpace(split))
	if split != SplitPublic && split != SplitPrivate && split != SplitAll {
		return nil, fmt.Errorf("unknown eval split %q (want public, private or all)", split)
	}
	var (
		all  []Case
		errs []error
		seen = map[string]string{}
	)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isYAML(path) {
			return nil
		}
		c, err := loadCaseFile(path, versionDir(dir, path))
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if prev, dup := seen[c.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate case id %q (also in %s)", path, c.ID, prev))
			return nil
		}
		seen[c.ID] = path
		all = append(all, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read eval cases from %s: %w", dir, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid eval cases: %w", errors.Join(errs...))
	}
	var out []Case
	for _, c := range all {
		if split == SplitAll || c.Split == split {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no eval cases for split %q under %s", split, dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

// versionDir names the version directory a case file belongs to: the first directory
// under root. A file directly in root belongs to root itself, so the runner can also be
// pointed at a single version directory.
func versionDir(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) >= 2 {
			return parts[0]
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Base(abs)
	}
	return filepath.Base(root)
}

// loadCaseFile decodes one case file strictly (unknown fields are errors) and validates
// it against the version directory it sits in.
func loadCaseFile(path, dirVersion string) (Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Case
	if err := dec.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("%s: decode: %w", path, err)
	}
	c.Path = path
	if err := c.Validate(dirVersion); err != nil {
		return Case{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
