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
)

// Case splits. SplitAll is only valid as a filter, never as a case's own split.
const (
	SplitPublic  = "public"
	SplitPrivate = "private"
	SplitAll     = "all"
)

// caseIDPattern keeps ids safe for agent names, script ids and file names; the longest
// id plus the runner's "-<6 hex>" suffix stays within the 80 characters script ids allow.
var caseIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,60}$`)

// Case is one versioned evaluation case. The YAML format is documented in
// evals/README.md.
type Case struct {
	ID          string           `yaml:"id"`
	Version     string           `yaml:"version"`
	Split       string           `yaml:"split"`
	Description string           `yaml:"description"`
	Agent       AgentSpec        `yaml:"agent"`
	Task        string           `yaml:"task"`
	Script      []ScriptStep     `yaml:"script"`
	Actions     Actions          `yaml:"actions"`
	Expect      Expect           `yaml:"expect"`
	Rubric      []map[string]any `yaml:"rubric"`

	// Path is the file the case was loaded from.
	Path string `yaml:"-"`
}

// AgentSpec describes the temporary agent created for the case. Name is a label for
// the case's agent and is sent as the agent's role; the created agent is always named
// "eval-<id>-<random>".
type AgentSpec struct {
	Name          string   `yaml:"name"`
	SystemPrompt  string   `yaml:"system_prompt"`
	ToolAllowlist []string `yaml:"tool_allowlist"`
	Limits        Limits   `yaml:"limits"`
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
}

// Expect is what the expectation evaluator checks. Zero values mean "not checked",
// except Status, which is required.
type Expect struct {
	Status                string `yaml:"status"`
	FailureReasonContains string `yaml:"failure_reason_contains"`
	MinToolCalls          int    `yaml:"min_tool_calls"`
	MinModelCalls         int    `yaml:"min_model_calls"`
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

// Validate checks the case against the format rules. dirVersion is the name of the
// version directory the file lives in, which must equal the case's version.
func (c Case) Validate(dirVersion string) error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	if !caseIDPattern.MatchString(c.ID) {
		add("id %q must match %s", c.ID, caseIDPattern)
	}
	switch {
	case c.Version == "":
		add("version is required")
	case c.Version != dirVersion:
		add("version %q does not match its directory %q", c.Version, dirVersion)
	}
	if c.Split != SplitPublic && c.Split != SplitPrivate {
		add("split %q must be %q or %q", c.Split, SplitPublic, SplitPrivate)
	}
	if strings.TrimSpace(c.Task) == "" {
		add("task is required")
	}
	if len(c.Script) == 0 {
		add("script is required and must have at least one step")
	}
	for i, s := range c.Script {
		n := i + 1
		hasText := strings.TrimSpace(s.Text) != ""
		switch {
		case s.Tool != nil && hasText:
			add("script step %d has both text and tool", n)
		case s.Tool == nil && !hasText:
			add("script step %d has neither text nor tool", n)
		case s.Tool != nil && strings.TrimSpace(s.Tool.Name) == "":
			add("script step %d: tool name is required", n)
		}
		if s.Tool != nil {
			if _, err := json.Marshal(s.Tool.Args); err != nil {
				add("script step %d: tool args are not JSON-encodable: %v", n, err)
			}
		}
		if s.Usage != nil && (s.Usage.Prompt < 0 || s.Usage.Completion < 0) {
			add("script step %d: usage must not be negative", n)
		}
	}
	if c.Actions.CancelAfterSteps < 0 {
		add("actions.cancel_after_steps must not be negative")
	}
	if !store.IsTerminal(c.Expect.Status) {
		add("expect.status %q must be a terminal run status (succeeded, failed, cancelled, timed_out)", c.Expect.Status)
	}
	if c.Expect.MinToolCalls < 0 || c.Expect.MinModelCalls < 0 {
		add("expect.min_tool_calls and expect.min_model_calls must not be negative")
	}
	if len(p) == 0 {
		return nil
	}
	return errors.New(strings.Join(p, "; "))
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
