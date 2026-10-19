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
	E2E bool `json:"e2e"`
	// ExpectedStatus is the status the case expects, so the report can tell the cases
	// that expect success from those that expect a failure.
	ExpectedStatus string       `json:"expected_status,omitempty"`
	Evaluators     []EvalResult `json:"evaluators"`
	Rubric         RubricResult `json:"rubric"`
	// Observed is what the runner saw outside the run's trace (see Observed).
	Observed *Observed `json:"observed,omitempty"`
}

// DeterministicPassed reports whether the case had no runner error and every
// deterministic evaluator passed.
func (r CaseResult) DeterministicPassed() bool {
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

// Passed reports whether the case passes (DESIGN 5.13 rule 5): every deterministic
// evaluator passed and the rubric, when the case has one, scored at least 0.75.
func (r CaseResult) Passed() bool { return r.DeterministicPassed() && r.Rubric.OK() }

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

// SplitSummary is the pass and success counts of one split. Passed counts the cases
// that pass outright (evaluators and rubric); DeterministicPassed those whose
// deterministic evaluators all passed.
type SplitSummary struct {
	Cases               int     `json:"cases"`
	Passed              int     `json:"passed"`
	DeterministicPassed int     `json:"deterministic_passed"`
	E2EPassed           int     `json:"e2e_passed"`
	OverallPass         float64 `json:"overall_pass"`
	DeterministicPass   float64 `json:"deterministic_pass"`
	E2ESuccess          float64 `json:"e2e_success"`
	// ExpectSucceeded cases expect a succeeded run; SucceededPassed of them got one.
	ExpectSucceeded int     `json:"expect_succeeded"`
	SucceededPassed int     `json:"succeeded_passed"`
	SucceededRate   float64 `json:"succeeded_rate"`
}

// add counts one case result into the split.
func (p *SplitSummary) add(r CaseResult) {
	p.Cases++
	if r.Passed() {
		p.Passed++
	}
	if r.DeterministicPassed() {
		p.DeterministicPassed++
	}
	if r.E2E {
		p.E2EPassed++
	}
	if r.ExpectedStatus == "succeeded" {
		p.ExpectSucceeded++
		if r.Status == "succeeded" && r.Error == "" {
			p.SucceededPassed++
		}
	}
}

// finish computes the rates once every case is counted.
func (p *SplitSummary) finish() {
	p.OverallPass = ratio(p.Passed, p.Cases)
	p.DeterministicPass = ratio(p.DeterministicPassed, p.Cases)
	p.E2ESuccess = ratio(p.E2EPassed, p.Cases)
	p.SucceededRate = ratio(p.SucceededPassed, p.ExpectSucceeded)
}

// RubricSummary aggregates the rubric evaluator over the cases that have a rubric.
type RubricSummary struct {
	Cases     int     `json:"cases"`
	Passed    int     `json:"passed"`
	MeanScore float64 `json:"mean_score"`
	// Heuristic and Model count the cases by the mode that graded them; a case that
	// could not be scored (it never ran) is in neither.
	Heuristic int `json:"heuristic"`
	Model     int `json:"model"`
}

// Summary holds the aggregates of a report.
type Summary struct {
	Cases               int             `json:"cases"`
	Passed              int             `json:"passed"`
	DeterministicPassed int             `json:"deterministic_passed"`
	E2EPassed           int             `json:"e2e_passed"`
	OverallPass         float64         `json:"overall_pass"`
	DeterministicPass   float64         `json:"deterministic_pass"`
	E2ESuccess          float64         `json:"e2e_success"`
	ExpectSucceeded     int             `json:"expect_succeeded"`
	SucceededPassed     int             `json:"succeeded_passed"`
	SucceededRate       float64         `json:"succeeded_rate"`
	Evaluators          []EvaluatorRate `json:"evaluators"`
	Rubric              RubricSummary   `json:"rubric"`
	MeanCostUSD         float64         `json:"mean_cost_usd"`
	MeanDurationMs      float64         `json:"mean_duration_ms"`
	Public              SplitSummary    `json:"public"`
	Private             SplitSummary    `json:"private"`
	SummaryLine         string          `json:"summary_line"`
}

// ReportMeta describes the invocation a report belongs to.
type ReportMeta struct {
	GeneratedAt time.Time `json:"generated_at"`
	Split       string    `json:"split"`
	// Provider is EVAL_PROVIDER: the runs are always replayed from the cases' scripts,
	// the provider says how rubrics are graded (scripted: local checks, anthropic: the
	// model).
	Provider string `json:"provider"`
	APIURL   string `json:"api_url"`
	CasesDir string `json:"cases_dir"`
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
	var rubricSum float64
	var pub, priv SplitSummary
	for _, r := range results {
		if r.ExpectedStatus == "succeeded" {
			s.ExpectSucceeded++
			if r.Status == "succeeded" && r.Error == "" {
				s.SucceededPassed++
			}
		}
		if r.Passed() {
			s.Passed++
		}
		if r.DeterministicPassed() {
			s.DeterministicPassed++
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
		if r.Rubric.Criteria > 0 {
			s.Rubric.Cases++
			rubricSum += r.Rubric.Score
			if r.Rubric.OK() {
				s.Rubric.Passed++
			}
			switch r.Rubric.Mode {
			case RubricModeHeuristic:
				s.Rubric.Heuristic++
			case RubricModeModel:
				s.Rubric.Model++
			}
		}
		costMicros += r.CostUSDMicros
		durationMs += r.DurationMs
		switch r.Split {
		case SplitPublic:
			pub.add(r)
		case SplitPrivate:
			priv.add(r)
		}
	}

	s.OverallPass = ratio(s.Passed, s.Cases)
	s.DeterministicPass = ratio(s.DeterministicPassed, s.Cases)
	s.E2ESuccess = ratio(s.E2EPassed, s.Cases)
	s.SucceededRate = ratio(s.SucceededPassed, s.ExpectSucceeded)
	if s.Rubric.Cases > 0 {
		s.Rubric.MeanScore = rubricSum / float64(s.Rubric.Cases)
	}
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

	pub.finish()
	priv.finish()
	s.Public = pub
	s.Private = priv
	s.SummaryLine = SummaryLine(pub)
	return rep
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// PublicGate returns an error when the pass rate of the public split, evaluators and
// rubric together, is below 0.9. A run with no public cases is not gated.
func (r *Report) PublicGate() error {
	pub := r.Summary.Public
	if pub.Cases == 0 {
		return nil
	}
	if pub.Passed*passGateDen < pub.Cases*passGateNum {
		return fmt.Errorf("public pass rate %.3f is below %.1f (%d of %d public cases passed)",
			pub.OverallPass, float64(passGateNum)/float64(passGateDen), pub.Passed, pub.Cases)
	}
	return nil
}

// Markdown renders the report for people.
func (r *Report) Markdown() string {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Eval report %s\n\n", r.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "%s\n\n", s.SummaryLine)
	fmt.Fprintf(&b, "- Split run: %s\n- Provider: %s\n- API: %s\n- Cases: %s\n\n", r.Split, providerLabel(r.Provider), r.APIURL, r.CasesDir)

	b.WriteString("## Overall\n\n| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Cases | %d |\n", s.Cases)
	fmt.Fprintf(&b, "| Overall pass rate (every evaluator passed and the rubric scored %.2f or more) | %.3f (%d of %d) |\n",
		RubricPassScore, s.OverallPass, s.Passed, s.Cases)
	fmt.Fprintf(&b, "| Deterministic pass rate (every evaluator passed) | %.3f (%d of %d) |\n", s.DeterministicPass, s.DeterministicPassed, s.Cases)
	fmt.Fprintf(&b, "| Expected outcome reached (the run ended with the status the case expected, a failure included) | %.3f (%d of %d) |\n", s.E2ESuccess, s.E2EPassed, s.Cases)
	fmt.Fprintf(&b, "| Succeeded (cases expecting success) | %.3f (%d of %d) |\n", s.SucceededRate, s.SucceededPassed, s.ExpectSucceeded)
	fmt.Fprintf(&b, "| Rubric (cases with a rubric that scored %.2f or more) | %s |\n", RubricPassScore, rubricSummaryCell(s.Rubric))
	fmt.Fprintf(&b, "| Mean cost (USD) | %.6f |\n", s.MeanCostUSD)
	fmt.Fprintf(&b, "| Mean duration | %s |\n\n", formatDuration(int64(s.MeanDurationMs)))

	b.WriteString("## By split\n\n| Split | Cases | Overall pass | Deterministic pass | Expected outcome reached | Succeeded (cases expecting success) |\n|---|---|---|---|---|---|\n")
	for _, sp := range []struct {
		name string
		s    SplitSummary
	}{{SplitPublic, s.Public}, {SplitPrivate, s.Private}} {
		fmt.Fprintf(&b, "| %s | %d | %.3f (%d) | %.3f (%d) | %.3f (%d) | %.3f (%d of %d) |\n", sp.name, sp.s.Cases,
			sp.s.OverallPass, sp.s.Passed, sp.s.DeterministicPass, sp.s.DeterministicPassed,
			sp.s.E2ESuccess, sp.s.E2EPassed, sp.s.SucceededRate, sp.s.SucceededPassed, sp.s.ExpectSucceeded)
	}
	b.WriteString("\n## Evaluators\n\n| Evaluator | Passed | Total | Pass rate |\n|---|---|---|---|\n")
	for _, e := range s.Evaluators {
		fmt.Fprintf(&b, "| %s | %d | %d | %.3f |\n", e.Name, e.Passed, e.Total, e.Rate)
	}

	b.WriteString("\n## Cases\n\n| Case | Split | Status | Reason | Evaluators | Rubric | Cost (USD) | Duration |\n|---|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cases {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %.6f | %s |\n",
			cell(c.ID), c.Split, cell(c.Status), cell(c.Reason()), evaluatorsCell(c), rubricCell(c.Rubric),
			float64(c.CostUSDMicros)/1e6, formatDuration(c.DurationMs))
	}

	r.writeEvaluatorMatrix(&b)
	r.writeRubricTable(&b)

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
			writeRubricFailures(&b, c.Rubric)
		}
	}
	return b.String()
}

// providerLabel says how the provider setting is used.
func providerLabel(p string) string {
	switch p {
	case ProviderAnthropic:
		return "anthropic (runs replayed from their scripts, rubrics graded by " + RubricModel + ")"
	case ProviderScripted:
		return "scripted (runs replayed from their scripts, rubrics graded by their local checks)"
	}
	return p
}

// rubricCell renders a case's rubric for the case table: "-" without one, otherwise the
// score, the criteria met and the grader that produced it.
func rubricCell(r RubricResult) string {
	switch {
	case r.Criteria == 0:
		return "-"
	case r.Mode == "":
		return fmt.Sprintf("not scored (%d criteria)", r.Criteria)
	}
	return fmt.Sprintf("%.2f (%d/%d, %s)", r.Score, r.Met(), r.Criteria, r.Mode)
}

func rubricSummaryCell(s RubricSummary) string {
	if s.Cases == 0 {
		return "none of the cases has a rubric"
	}
	return fmt.Sprintf("%d of %d, mean score %.3f (%d heuristic, %d model)", s.Passed, s.Cases, s.MeanScore, s.Heuristic, s.Model)
}

// writeRubricTable lists every criterion of every case with a rubric, with the mode that
// graded it and the grader's reason, so a reader can see what each score rests on.
func (r *Report) writeRubricTable(b *strings.Builder) {
	var rows []CaseResult
	for _, c := range r.Cases {
		if c.Rubric.Criteria > 0 && len(c.Rubric.Items) > 0 {
			rows = append(rows, c)
		}
	}
	if len(rows) == 0 {
		return
	}
	b.WriteString("\n## Rubric by criterion\n\n| Case | Mode | Criterion | Met | Reason |\n|---|---|---|---|---|\n")
	for _, c := range rows {
		for _, it := range c.Rubric.Items {
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", cell(c.ID), c.Rubric.Mode, cell(it.Criterion),
				map[bool]string{true: "yes", false: "NO"}[it.Passed], cell(it.Reason))
		}
	}
}

// writeRubricFailures lists what kept a rubric from passing: the criteria not met, or
// why the rubric could not be scored.
func writeRubricFailures(b *strings.Builder, r RubricResult) {
	if r.Criteria == 0 || r.OK() {
		return
	}
	fmt.Fprintf(b, "  - rubric: %s\n", oneLine(rubricCell(r)))
	if r.Detail != "" {
		fmt.Fprintf(b, "    - %s\n", oneLine(r.Detail))
	}
	for _, it := range r.Items {
		if !it.Passed {
			fmt.Fprintf(b, "    - not met: %s (%s)\n", oneLine(it.Criterion), oneLine(it.Reason))
		}
	}
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
