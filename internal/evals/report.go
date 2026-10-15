package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// The public pass rate must be at least passGateNum/passGateDen (0.9) or Run returns an
// error and the process exits 1. Integer arithmetic keeps 9 of 10 exactly on the line.
const (
	passGateNum = 9
	passGateDen = 10
)

// maxCellRunes bounds a table cell in the markdown report.
const maxCellRunes = 140

// CaseResult is everything the report records about one case.
type CaseResult struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Split       string `json:"split"`
	Description string `json:"description,omitempty"`

	RunID    string `json:"run_id,omitempty"`
	ScriptID string `json:"script_id,omitempty"`
	// AgentID is the temporary agent of a quick run; a workflow case has WorkflowID
	// and one agent id per agent of the case instead.
	AgentID    string   `json:"agent_id,omitempty"`
	AgentIDs   []string `json:"agent_ids,omitempty"`
	WorkflowID string   `json:"workflow_id,omitempty"`

	// Status and FailureReason are the run's final state as the api reports it.
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	// Error is a problem on the runner's side (timeout, api error, panic). A case with
	// an Error never passes.
	Error string `json:"error,omitempty"`

	StepCount     int   `json:"step_count"`
	ModelCalls    int   `json:"model_calls"`
	ToolCalls     int   `json:"tool_calls"`
	CostUSDMicros int64 `json:"cost_usd_micros"`
	DurationMs    int64 `json:"duration_ms"`

	// E2E is true when the run ended with the status the case expects.
	E2E        bool         `json:"e2e"`
	Evaluators []EvalResult `json:"evaluators"`
	Rubric     RubricResult `json:"rubric"`
}

// Passed reports whether the case had no runner error and every deterministic
// evaluator passed.
func (r CaseResult) Passed() bool {
	if r.Error != "" || len(r.Evaluators) == 0 {
		return false
	}
	for _, e := range r.Evaluators {
		if !e.Passed {
			return false
		}
	}
	return true
}

// Reason is the runner error when there is one, otherwise the run's failure reason.
func (r CaseResult) Reason() string {
	if r.Error != "" {
		return r.Error
	}
	return r.FailureReason
}

func (r CaseResult) evaluatorCounts() (passed, total int) {
	for _, e := range r.Evaluators {
		total++
		if e.Passed {
			passed++
		}
	}
	return passed, total
}

// EvaluatorRate is the pass rate of one evaluator across all cases.
type EvaluatorRate struct {
	Name   string  `json:"name"`
	Passed int     `json:"passed"`
	Total  int     `json:"total"`
	Rate   float64 `json:"rate"`
}

// SplitSummary is the pass and success counts of one split.
type SplitSummary struct {
	Cases             int     `json:"cases"`
	Passed            int     `json:"passed"`
	E2EPassed         int     `json:"e2e_passed"`
	DeterministicPass float64 `json:"deterministic_pass"`
	E2ESuccess        float64 `json:"e2e_success"`
}

// Summary holds the aggregates of a report.
type Summary struct {
	Cases             int             `json:"cases"`
	Passed            int             `json:"passed"`
	E2EPassed         int             `json:"e2e_passed"`
	DeterministicPass float64         `json:"deterministic_pass"`
	E2ESuccess        float64         `json:"e2e_success"`
	Evaluators        []EvaluatorRate `json:"evaluators"`
	MeanCostUSD       float64         `json:"mean_cost_usd"`
	MeanDurationMs    float64         `json:"mean_duration_ms"`
	Public            SplitSummary    `json:"public"`
	SummaryLine       string          `json:"summary_line"`
}

// ReportMeta describes the invocation a report belongs to.
type ReportMeta struct {
	GeneratedAt time.Time `json:"generated_at"`
	Split       string    `json:"split"`
	Provider    string    `json:"provider"`
	APIURL      string    `json:"api_url"`
	CasesDir    string    `json:"cases_dir"`
}

// Report is the full result of one eval run.
type Report struct {
	ReportMeta
	Summary Summary      `json:"summary"`
	Cases   []CaseResult `json:"cases"`
}

// SummaryLine formats the one-line public result:
// "public split: N cases, deterministic pass <x>, e2e success <y>".
func SummaryLine(pub SplitSummary) string {
	return fmt.Sprintf("public split: %d cases, deterministic pass %.3f, e2e success %.3f",
		pub.Cases, pub.DeterministicPass, pub.E2ESuccess)
}

// BuildReport aggregates case results into a report.
func BuildReport(meta ReportMeta, results []CaseResult) *Report {
	rep := &Report{ReportMeta: meta, Cases: results}
	s := &rep.Summary
	s.Cases = len(results)

	counts := map[string]*EvaluatorRate{}
	for _, name := range EvaluatorNames() {
		counts[name] = &EvaluatorRate{Name: name}
	}
	var costMicros, durationMs int64
	var pub SplitSummary
	for _, r := range results {
		if r.Passed() {
			s.Passed++
		}
		if r.E2E {
			s.E2EPassed++
		}
		for _, e := range r.Evaluators {
			c, ok := counts[e.Name]
			if !ok {
				c = &EvaluatorRate{Name: e.Name}
				counts[e.Name] = c
			}
			c.Total++
			if e.Passed {
				c.Passed++
			}
		}
		costMicros += r.CostUSDMicros
		durationMs += r.DurationMs
		if r.Split == SplitPublic {
			pub.Cases++
			if r.Passed() {
				pub.Passed++
			}
			if r.E2E {
				pub.E2EPassed++
			}
		}
	}

	s.DeterministicPass = ratio(s.Passed, s.Cases)
	s.E2ESuccess = ratio(s.E2EPassed, s.Cases)
	if s.Cases > 0 {
		s.MeanCostUSD = float64(costMicros) / 1e6 / float64(s.Cases)
		s.MeanDurationMs = float64(durationMs) / float64(s.Cases)
	}
	// Report evaluators in their canonical order, then any unknown ones by name.
	for _, name := range EvaluatorNames() {
		c := counts[name]
		c.Rate = ratio(c.Passed, c.Total)
		s.Evaluators = append(s.Evaluators, *c)
		delete(counts, name)
	}
	for _, c := range counts {
		c.Rate = ratio(c.Passed, c.Total)
		s.Evaluators = append(s.Evaluators, *c)
	}

	pub.DeterministicPass = ratio(pub.Passed, pub.Cases)
	pub.E2ESuccess = ratio(pub.E2EPassed, pub.Cases)
	s.Public = pub
	s.SummaryLine = SummaryLine(pub)
	return rep
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// PublicGate returns an error when the deterministic pass rate of the public split is
// below 0.9. A run with no public cases is not gated.
func (r *Report) PublicGate() error {
	pub := r.Summary.Public
	if pub.Cases == 0 {
		return nil
	}
	if pub.Passed*passGateDen < pub.Cases*passGateNum {
		return fmt.Errorf("public pass rate %.3f is below %.1f (%d of %d public cases passed)",
			pub.DeterministicPass, float64(passGateNum)/float64(passGateDen), pub.Passed, pub.Cases)
	}
	return nil
}

// Markdown renders the report for people.
func (r *Report) Markdown() string {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Eval report %s\n\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "%s\n\n", s.SummaryLine)
	fmt.Fprintf(&b, "- Split run: %s\n- Provider: %s\n- API: %s\n- Cases: %s\n\n", r.Split, r.Provider, r.APIURL, r.CasesDir)

	b.WriteString("## Overall\n\n| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Cases | %d |\n", s.Cases)
	fmt.Fprintf(&b, "| Overall pass rate (every evaluator passed) | %.3f (%d of %d) |\n", s.DeterministicPass, s.Passed, s.Cases)
	fmt.Fprintf(&b, "| End-to-end success (expected status reached) | %.3f (%d of %d) |\n", s.E2ESuccess, s.E2EPassed, s.Cases)
	fmt.Fprintf(&b, "| Mean cost (USD) | %.6f |\n", s.MeanCostUSD)
	fmt.Fprintf(&b, "| Mean duration | %s |\n\n", formatDuration(int64(s.MeanDurationMs)))

	b.WriteString("## Evaluators\n\n| Evaluator | Passed | Total | Pass rate |\n|---|---|---|---|\n")
	for _, e := range s.Evaluators {
		fmt.Fprintf(&b, "| %s | %d | %d | %.3f |\n", e.Name, e.Passed, e.Total, e.Rate)
	}

	b.WriteString("\n## Cases\n\n| Case | Split | Status | Reason | Evaluators | Cost (USD) | Duration |\n|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %.6f | %s |\n",
			cell(c.ID), c.Split, cell(c.Status), cell(c.Reason()), evaluatorsCell(c),
			float64(c.CostUSDMicros)/1e6, formatDuration(c.DurationMs))
	}

	r.writeEvaluatorMatrix(&b)

	var failures []CaseResult
	for _, c := range r.Cases {
		if !c.Passed() {
			failures = append(failures, c)
		}
	}
	if len(failures) > 0 {
		b.WriteString("\n## Failures\n\n")
		for _, c := range failures {
			fmt.Fprintf(&b, "- %s", c.ID)
			if c.RunID != "" {
				fmt.Fprintf(&b, " (run %s)", c.RunID)
			}
			b.WriteString("\n")
			if c.Error != "" {
				fmt.Fprintf(&b, "  - runner: %s\n", oneLine(c.Error))
			}
			for _, e := range c.Evaluators {
				if !e.Passed {
					fmt.Fprintf(&b, "  - %s: %s\n", e.Name, oneLine(e.Detail))
				}
			}
		}
	}
	return b.String()
}

// writeEvaluatorMatrix renders one row per case and one column per evaluator, so a
// reader sees which evaluator judged which case ("pass", "FAIL", or "-" when the
// evaluator did not run for the case).
func (r *Report) writeEvaluatorMatrix(b *strings.Builder) {
	names := make([]string, 0, len(r.Summary.Evaluators))
	for _, e := range r.Summary.Evaluators {
		names = append(names, e.Name)
	}
	if len(names) == 0 || len(r.Cases) == 0 {
		return
	}
	b.WriteString("\n## Evaluators by case\n\n| Case |")
	for _, n := range names {
		fmt.Fprintf(b, " %s |", n)
	}
	b.WriteString("\n|---|")
	b.WriteString(strings.Repeat("---|", len(names)))
	b.WriteString("\n")
	for _, c := range r.Cases {
		got := map[string]EvalResult{}
		for _, e := range c.Evaluators {
			got[e.Name] = e
		}
		fmt.Fprintf(b, "| %s |", cell(c.ID))
		for _, n := range names {
			mark := "-"
			if e, ok := got[n]; ok {
				mark = map[bool]string{true: "pass", false: "FAIL"}[e.Passed]
			}
			fmt.Fprintf(b, " %s |", mark)
		}
		b.WriteString("\n")
	}
}

// evaluatorsCell renders "3/3" or "2/3 (expectation)" naming the failed evaluators.
func evaluatorsCell(c CaseResult) string {
	passed, total := c.evaluatorCounts()
	out := fmt.Sprintf("%d/%d", passed, total)
	var failed []string
	for _, e := range c.Evaluators {
		if !e.Passed {
			failed = append(failed, e.Name)
		}
	}
	if len(failed) > 0 {
		out += " (" + strings.Join(failed, ", ") + ")"
	}
	return out
}

func formatDuration(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cell makes text safe for one markdown table cell.
func cell(s string) string {
	s = oneLine(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	if utf8.RuneCountInString(s) > maxCellRunes {
		r := []rune(s)
		s = string(r[:maxCellRunes-3]) + "..."
	}
	return s
}

// WriteReport writes <timestamp>.md and <timestamp>.json into dir and returns their
// paths. An existing report from the same second is never overwritten.
func WriteReport(dir string, r *Report) (mdPath, jsonPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create reports dir: %w", err)
	}
	base := r.GeneratedAt.UTC().Format("20060102T150405Z")
	var mdFile *os.File
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		mdPath = filepath.Join(dir, name+".md")
		jsonPath = filepath.Join(dir, name+".json")
		f, err := os.OpenFile(mdPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			mdFile = f
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("create report: %w", err)
		}
	}
	if mdFile == nil {
		return "", "", fmt.Errorf("create report: too many reports named %s in %s", base, dir)
	}
	if _, err := mdFile.WriteString(r.Markdown()); err != nil {
		mdFile.Close()
		return "", "", fmt.Errorf("write %s: %w", mdPath, err)
	}
	if err := mdFile.Close(); err != nil {
		return "", "", fmt.Errorf("close %s: %w", mdPath, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", jsonPath, err)
	}
	return mdPath, jsonPath, nil
}
