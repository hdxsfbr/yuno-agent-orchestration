package evals

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// The rubric evaluator (DESIGN 5.13 rule 4). A case lists three to six criteria; each
// scores 0 or 1 and the case's rubric score is the share met. A case passes the rubric
// at 0.75 or more. Two graders produce the scores: the heuristic mode below runs each
// criterion's local check and needs no network, the model mode (rubric_model.go) asks
// claude-opus-5-5. Every score carries the mode that produced it.

// Rubric modes, as the report labels them.
const (
	RubricModeHeuristic = "heuristic"
	RubricModeModel     = "model"
)

// Rubric bounds and the pass mark.
const (
	MinRubricCriteria = 3
	MaxRubricCriteria = 6
	// RubricPassScore is the lowest rubric score that passes: 3 of 4 criteria.
	RubricPassScore = 0.75
)

// RubricCriterion is one thing the case's outcome must show. The text is what the model
// grader judges; the check is what the heuristic grader runs instead.
type RubricCriterion struct {
	Criterion string      `yaml:"criterion"`
	Check     RubricCheck `yaml:"check"`
}

// RubricCheck is a local test of the run's final state. Exactly one of Contains, Regex
// or JSONPathEq is set. Path is a JSON pointer into the final state; for Contains and
// Regex it defaults to the whole state, and the text tested is the value at Path when it
// is a string, else the value's compact JSON. JSONPathEq compares the value at Path
// (required) with the given value.
type RubricCheck struct {
	Path       string    `yaml:"path"`
	Contains   string    `yaml:"contains"`
	Regex      string    `yaml:"regex"`
	JSONPathEq yaml.Node `yaml:"json_path_eq"`
}

// kinds names the checks that are set.
func (k RubricCheck) kinds() []string {
	var out []string
	if k.Contains != "" {
		out = append(out, "contains")
	}
	if k.Regex != "" {
		out = append(out, "regex")
	}
	if k.JSONPathEq.Kind != 0 {
		out = append(out, "json_path_eq")
	}
	return out
}

// expected decodes the json_path_eq value.
func (k RubricCheck) expected() (any, error) {
	var v any
	if err := k.JSONPathEq.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// RubricItem is the verdict on one criterion.
type RubricItem struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason"`
}

// RubricResult is the outcome of the rubric evaluator for one case.
type RubricResult struct {
	// Mode is the grader that produced the score, "heuristic" or "model"; empty when the
	// case has no rubric.
	Mode string `json:"mode,omitempty"`
	// Model is the model that graded, in model mode.
	Model    string  `json:"model,omitempty"`
	Score    float64 `json:"score"`
	Criteria int     `json:"criteria"`
	// Detail explains a rubric that could not be scored (a failed model call).
	Detail string       `json:"detail,omitempty"`
	Items  []RubricItem `json:"items,omitempty"`
}

// OK reports whether the rubric passes: no criteria, or a score of at least 0.75.
func (r RubricResult) OK() bool { return r.Criteria == 0 || r.Score >= RubricPassScore }

// Met is how many criteria were met.
func (r RubricResult) Met() int {
	n := 0
	for _, it := range r.Items {
		if it.Passed {
			n++
		}
	}
	return n
}

// newRubricResult builds a result from per-criterion verdicts.
func newRubricResult(mode, model string, items []RubricItem) RubricResult {
	res := RubricResult{Mode: mode, Model: model, Criteria: len(items), Items: items}
	if len(items) > 0 {
		res.Score = float64(res.Met()) / float64(len(items))
	}
	return res
}

// ScoreRubric is the heuristic grader: each criterion's check runs against the run's
// final state. A case with no criteria scores 1.
func ScoreRubric(c Case, t store.Trace) RubricResult {
	if len(c.Rubric) == 0 {
		return RubricResult{Score: 1}
	}
	state, err := finalState(t)
	items := make([]RubricItem, len(c.Rubric))
	for i, cr := range c.Rubric {
		items[i] = RubricItem{Criterion: cr.Criterion}
		if err != nil {
			items[i].Reason = err.Error()
			continue
		}
		items[i].Passed, items[i].Reason = cr.Check.run(state)
	}
	return newRubricResult(RubricModeHeuristic, "", items)
}

// finalState decodes the run's final state; a run without one has an empty state.
func finalState(t store.Trace) (any, error) {
	if t.Run == nil || len(t.Run.State) == 0 {
		return map[string]any{}, nil
	}
	var state any
	if err := json.Unmarshal(t.Run.State, &state); err != nil {
		return nil, fmt.Errorf("run state is not valid JSON: %v", err)
	}
	return state, nil
}

// run applies the check to the decoded final state and says why it passed or failed.
func (k RubricCheck) run(state any) (bool, string) {
	where := "state"
	if k.Path != "" {
		where = "state " + k.Path
	}
	val, ok := workflow.Lookup(state, k.Path)
	if !ok {
		return false, where + " is missing"
	}
	switch kinds := k.kinds(); {
	case len(kinds) != 1:
		return false, "the check must set exactly one of contains, regex or json_path_eq"
	case k.Contains != "":
		text := checkText(val)
		if strings.Contains(strings.ToLower(text), strings.ToLower(k.Contains)) {
			return true, fmt.Sprintf("%s contains %q", where, k.Contains)
		}
		return false, fmt.Sprintf("%s does not contain %q (it is %s)", where, k.Contains, snippet(text))
	case k.Regex != "":
		re, err := regexp.Compile(k.Regex)
		if err != nil {
			return false, "invalid regex: " + err.Error()
		}
		text := checkText(val)
		if re.MatchString(text) {
			return true, fmt.Sprintf("%s matches /%s/", where, k.Regex)
		}
		return false, fmt.Sprintf("%s does not match /%s/ (it is %s)", where, k.Regex, snippet(text))
	default:
		want, err := k.expected()
		if err != nil {
			return false, "json_path_eq value: " + err.Error()
		}
		if jsonEqual(want, val) {
			return true, fmt.Sprintf("%s is %s", where, compactJSON(want))
		}
		return false, fmt.Sprintf("%s is %s, want %s", where, snippet(compactJSON(val)), compactJSON(want))
	}
}

// checkText is the text a contains or regex check looks at: a string as it is, anything
// else as compact JSON.
func checkText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return compactJSON(v)
}

// maxSnippetRunes bounds the excerpt of a value in a reason.
const maxSnippetRunes = 80

// snippet shortens text for a one-line reason.
func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxSnippetRunes {
		s = string([]rune(s)[:maxSnippetRunes-3]) + "..."
	}
	return fmt.Sprintf("%q", s)
}
