package evals

import (
	"encoding/json"
	"strings"
	"testing"
)

// rubricResult builds a graded rubric with the given verdicts.
func rubricResult(mode string, verdicts ...bool) RubricResult {
	items := make([]RubricItem, len(verdicts))
	for i, v := range verdicts {
		items[i] = RubricItem{Criterion: "criterion " + string(rune('A'+i)), Passed: v, Reason: "because " + string(rune('a'+i))}
	}
	model := ""
	if mode == RubricModeModel {
		model = RubricModel
	}
	return newRubricResult(mode, model, items)
}

func withRubric(r CaseResult, rub RubricResult) CaseResult {
	r.Rubric = rub
	return r
}

func TestCasePassesOnEvaluatorsAndRubricTogether(t *testing.T) {
	tests := []struct {
		name          string
		rubric        RubricResult
		passed        bool
		deterministic bool
	}{
		{"no rubric", RubricResult{Score: 1}, true, true},
		{"rubric at the mark", rubricResult(RubricModeHeuristic, true, true, true, false), true, true},
		{"rubric below the mark", rubricResult(RubricModeHeuristic, true, true, false, false), false, true},
		{"rubric that could not be scored", RubricResult{Criteria: 3, Detail: "not scored: x"}, false, true},
		{"model grading that failed", RubricResult{Mode: RubricModeModel, Criteria: 3, Detail: "model grading failed: x"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := withRubric(passing("a", SplitPublic), tt.rubric)
			if r.Passed() != tt.passed || r.DeterministicPassed() != tt.deterministic {
				t.Fatalf("passed %v deterministic %v, want %v and %v", r.Passed(), r.DeterministicPassed(), tt.passed, tt.deterministic)
			}
		})
	}
	// A rubric never rescues a case whose evaluators failed.
	bad := withRubric(failing("b"), rubricResult(RubricModeHeuristic, true, true, true))
	if bad.Passed() || bad.DeterministicPassed() {
		t.Error("a failed evaluator fails the case whatever the rubric says")
	}
}

func TestReportCountsTheRubricInTheGateButNotInTheSummaryLine(t *testing.T) {
	results := []CaseResult{
		withRubric(passing("a", SplitPublic), rubricResult(RubricModeHeuristic, true, true, true)),
		withRubric(passing("b", SplitPublic), rubricResult(RubricModeModel, true, true, true, false)),
		withRubric(passing("c", SplitPublic), rubricResult(RubricModeHeuristic, true, false, false)),
		passing("p", SplitPrivate),
	}
	rep := BuildReport(ReportMeta{GeneratedAt: t0, Split: "all", Provider: ProviderAnthropic}, results)
	s := rep.Summary

	if s.Passed != 3 || s.DeterministicPassed != 4 || s.OverallPass != 0.75 || s.DeterministicPass != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.Public.Cases != 3 || s.Public.Passed != 2 || s.Public.DeterministicPassed != 3 {
		t.Fatalf("public = %+v", s.Public)
	}
	// The summary line is the one DESIGN 5.13 fixes: deterministic pass, not the rubric.
	if want := "public split: 3 cases, deterministic pass 1.000, e2e success 1.000"; s.SummaryLine != want {
		t.Fatalf("summary line = %q, want %q", s.SummaryLine, want)
	}
	if s.Rubric.Cases != 3 || s.Rubric.Passed != 2 || s.Rubric.Heuristic != 2 || s.Rubric.Model != 1 {
		t.Errorf("rubric summary = %+v", s.Rubric)
	}
	if want := (1.0 + 0.75 + 1.0/3) / 3; s.Rubric.MeanScore < want-1e-9 || s.Rubric.MeanScore > want+1e-9 {
		t.Errorf("mean rubric score = %v, want %v", s.Rubric.MeanScore, want)
	}
	// 2 of 3 public cases pass outright, so the gate (0.9) fails although every evaluator passed.
	if err := rep.PublicGate(); err == nil || !strings.Contains(err.Error(), "public pass rate 0.667 is below 0.9 (2 of 3 public cases passed)") {
		t.Fatalf("gate = %v", err)
	}
}

func TestMarkdownLabelsTheRubricModePerCase(t *testing.T) {
	results := []CaseResult{
		withRubric(passing("a", SplitPublic), rubricResult(RubricModeHeuristic, true, true, true)),
		withRubric(passing("b", SplitPublic), rubricResult(RubricModeModel, true, true, true, false)),
		withRubric(passing("c", SplitPublic), rubricResult(RubricModeHeuristic, true, false, false)),
		withRubric(passing("d", SplitPublic), RubricResult{Mode: RubricModeModel, Model: RubricModel, Criteria: 3, Detail: "model grading failed: model proxy answered 401"}),
		passing("e", SplitPublic),
	}
	md := BuildReport(ReportMeta{GeneratedAt: t0, Split: "public", Provider: ProviderAnthropic, APIURL: "http://x", CasesDir: "evals/cases"}, results).Markdown()

	for _, want := range []string{
		"| Case | Split | Status | Reason | Evaluators | Rubric | Cost (USD) | Duration |",
		"| a | public | succeeded |  | 7/7 | 1.00 (3/3, heuristic) |",
		"| b | public | succeeded |  | 7/7 | 0.75 (3/4, model) |",
		"| c | public | succeeded |  | 7/7 | 0.33 (1/3, heuristic) |",
		"| e | public | succeeded |  | 7/7 | - |",
		"- Provider: anthropic (runs replayed from their scripts, rubrics graded by claude-opus-5-5)",
		"| Rubric (cases with a rubric that scored 0.75 or more) | 2 of 4,",
		"| Deterministic pass rate (every evaluator passed) | 1.000 (5 of 5) |",
		"| Overall pass rate (every evaluator passed and the rubric scored 0.75 or more) | 0.600 (3 of 5) |",
		"## Rubric by criterion",
		"| b | model | criterion D | NO | because d |",
		"| a | heuristic | criterion A | yes | because a |",
		"## Failures",
		"  - rubric: 0.33 (1/3, heuristic)",
		"    - not met: criterion B (because b)",
		"    - model grading failed: model proxy answered 401",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "  - rubric: 0.75") {
		t.Errorf("a rubric that passed is not a failure:\n%s", md)
	}
}

func TestMarkdownWithNoRubricAtAll(t *testing.T) {
	md := BuildReport(ReportMeta{GeneratedAt: t0, Provider: ProviderScripted}, []CaseResult{passing("a", SplitPublic)}).Markdown()
	for _, want := range []string{"none of the cases has a rubric", "scripted (runs replayed from their scripts, rubrics graded by their local checks)"} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Rubric by criterion") {
		t.Error("no criterion table without a rubric")
	}
}

func TestJSONReportKeepsTheRubricAndWhatWasObserved(t *testing.T) {
	r := withRubric(passing("a", SplitPublic), rubricResult(RubricModeModel, true, true, true, false))
	r.Observed = &Observed{Retrigger: &TriggerAttempt{Status: 429, Message: "rate limit: 1 runs/hour for agent x"}}
	rep := BuildReport(ReportMeta{GeneratedAt: t0, Provider: ProviderAnthropic}, []CaseResult{r})
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got := back.Cases[0]
	if got.Rubric.Mode != "model" || got.Rubric.Model != RubricModel || len(got.Rubric.Items) != 4 || got.Rubric.Items[3].Passed ||
		got.Observed == nil || got.Observed.Retrigger.Status != 429 || back.Provider != "anthropic" {
		t.Fatalf("round trip = %+v", got)
	}
	if !strings.Contains(string(raw), `"mode":"model"`) {
		t.Errorf("json = %s", raw)
	}
}
