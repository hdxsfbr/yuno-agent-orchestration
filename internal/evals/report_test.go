package evals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// allPassing is a passing result for every evaluator, in report order.
func allPassing() []EvalResult {
	var out []EvalResult
	for _, name := range EvaluatorNames() {
		out = append(out, EvalResult{Name: name, Passed: true})
	}
	return out
}

func passing(id, split string) CaseResult {
	return CaseResult{
		ID: id, Version: "v1", Split: split, Status: "succeeded", E2E: true,
		StepCount: 3, CostUSDMicros: 2000, DurationMs: 3000,
		Evaluators: allPassing(),
		Rubric:     RubricResult{Score: 1},
	}
}

// failing is a public case whose run ended in the wrong status.
func failing(id string) CaseResult {
	r := passing(id, SplitPublic)
	r.Status, r.E2E = "failed", false
	r.FailureReason = "agent did not call finish"
	r.Evaluators[2] = EvalResult{Name: EvalExpectation, Detail: `status "failed", want "succeeded"`}
	return r
}

func TestSummaryLine(t *testing.T) {
	tests := []struct {
		name string
		in   SplitSummary
		want string
	}{
		{"all pass", SplitSummary{Cases: 3, DeterministicPass: 1, E2ESuccess: 1},
			"public split: 3 cases, deterministic pass 1.000, e2e success 1.000"},
		{"thirds round to three decimals", SplitSummary{Cases: 3, DeterministicPass: 2.0 / 3, E2ESuccess: 1.0 / 3},
			"public split: 3 cases, deterministic pass 0.667, e2e success 0.333"},
		{"no public cases", SplitSummary{},
			"public split: 0 cases, deterministic pass 0.000, e2e success 0.000"},
		{"twenty cases", SplitSummary{Cases: 20, DeterministicPass: 0.95, E2ESuccess: 0.9},
			"public split: 20 cases, deterministic pass 0.950, e2e success 0.900"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SummaryLine(tt.in); got != tt.want {
				t.Fatalf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestBuildReportAggregates(t *testing.T) {
	results := []CaseResult{
		passing("a", SplitPublic),
		passing("b", SplitPublic),
		failing("c"),
		passing("p", SplitPrivate),
	}
	rep := BuildReport(ReportMeta{GeneratedAt: t0, Split: "all"}, results)
	s := rep.Summary

	if s.Cases != 4 || s.Passed != 3 || s.E2EPassed != 3 {
		t.Fatalf("totals = %d cases, %d passed, %d e2e", s.Cases, s.Passed, s.E2EPassed)
	}
	if s.DeterministicPass != 0.75 || s.E2ESuccess != 0.75 {
		t.Fatalf("rates = %v, %v", s.DeterministicPass, s.E2ESuccess)
	}
	if len(s.Evaluators) != len(EvaluatorNames()) {
		t.Fatalf("evaluators = %+v", s.Evaluators)
	}
	for i, name := range EvaluatorNames() {
		want := [2]int{4, 4}
		if name == EvalExpectation {
			want = [2]int{3, 4}
		}
		e := s.Evaluators[i]
		if e.Name != name || e.Passed != want[0] || e.Total != want[1] {
			t.Errorf("evaluator %d = %+v, want %s %v", i, e, name, want)
		}
	}
	if s.Evaluators[2].Rate != 0.75 {
		t.Errorf("expectation rate = %v", s.Evaluators[2].Rate)
	}
	if s.MeanCostUSD != 0.002 {
		t.Errorf("mean cost = %v", s.MeanCostUSD)
	}
	if s.MeanDurationMs != 3000 {
		t.Errorf("mean duration = %v", s.MeanDurationMs)
	}
	if s.Public.Cases != 3 || s.Public.Passed != 2 || s.Public.E2EPassed != 2 {
		t.Fatalf("public = %+v", s.Public)
	}
	if want := "public split: 3 cases, deterministic pass 0.667, e2e success 0.667"; s.SummaryLine != want {
		t.Fatalf("summary line = %q, want %q", s.SummaryLine, want)
	}
}

func TestBuildReportEmpty(t *testing.T) {
	rep := BuildReport(ReportMeta{GeneratedAt: t0}, nil)
	if rep.Summary.Cases != 0 || rep.Summary.DeterministicPass != 0 || rep.Summary.MeanCostUSD != 0 {
		t.Fatalf("summary = %+v", rep.Summary)
	}
	if err := rep.PublicGate(); err != nil {
		t.Fatalf("gate on an empty report: %v", err)
	}
}

func TestPublicGate(t *testing.T) {
	mk := func(pass, fail int, extra ...CaseResult) *Report {
		var rs []CaseResult
		for i := 0; i < pass; i++ {
			rs = append(rs, passing("p", SplitPublic))
		}
		for i := 0; i < fail; i++ {
			rs = append(rs, failing("f"))
		}
		return BuildReport(ReportMeta{GeneratedAt: t0}, append(rs, extra...))
	}
	timedOut := passing("t", SplitPublic)
	timedOut.Error = "runner timeout"

	tests := []struct {
		name    string
		rep     *Report
		wantErr string
	}{
		{"all pass", mk(3, 0), ""},
		{"exactly 0.9 passes", mk(9, 1), ""},
		{"18 of 20 passes", mk(18, 2), ""},
		{"17 of 20 fails", mk(17, 3), "0.850"},
		{"8 of 10 fails", mk(8, 2), "below 0.9"},
		{"two of three fails", mk(2, 1), "0.667"},
		{"one of one passes", mk(1, 0), ""},
		{"nothing passes", mk(0, 2), "0.000"},
		{"a runner error counts as a failure", mk(0, 0, timedOut), "below 0.9"},
		{"private-only report is not gated", BuildReport(ReportMeta{}, []CaseResult{func() CaseResult {
			r := failing("x")
			r.Split = SplitPrivate
			return r
		}()}), ""},
		{"private failures do not count against the public rate", mk(3, 0, func() CaseResult {
			r := failing("x")
			r.Split = SplitPrivate
			return r
		}()), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rep.PublicGate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestCaseResultPassed(t *testing.T) {
	ok := passing("a", SplitPublic)
	if !ok.Passed() {
		t.Fatal("a fully passing result should pass")
	}
	withErr := ok
	withErr.Error = "runner timeout"
	if withErr.Passed() {
		t.Error("a runner error must fail the case even when evaluators passed")
	}
	if withErr.Reason() != "runner timeout" {
		t.Errorf("reason = %q", withErr.Reason())
	}
	none := ok
	none.Evaluators = nil
	if none.Passed() {
		t.Error("a case with no evaluator results must not pass")
	}
	if got := failing("x").Reason(); got != "agent did not call finish" {
		t.Errorf("reason = %q", got)
	}
}

func TestMarkdown(t *testing.T) {
	bad := failing("c|pipe")
	bad.RunID = "run-9"
	bad.FailureReason = "line one\nline two | with a pipe"
	rep := BuildReport(ReportMeta{
		GeneratedAt: t0, Split: "public", Provider: "scripted", APIURL: "http://127.0.0.1:8080", CasesDir: "evals/cases",
	}, []CaseResult{passing("a", SplitPublic), bad})
	md := rep.Markdown()

	line := "public split: 2 cases, deterministic pass 0.500, e2e success 0.500"
	found := false
	for _, l := range strings.Split(md, "\n") {
		if l == line {
			found = true
		}
	}
	if !found {
		t.Errorf("markdown has no line exactly equal to %q:\n%s", line, md)
	}
	for _, want := range []string{
		"# Eval report 2026-10-15T12:00:00Z",
		"| termination | 2 | 2 | 1.000 |",
		"| expectation | 1 | 2 | 0.500 |",
		"| a | public | succeeded |  | 7/7 | 0.002000 | 3.0s |",
		`| c\|pipe | public | failed | line one line two \| with a pipe | 6/7 (expectation) |`,
		"## Failures",
		"- c|pipe (run run-9)",
		`  - expectation: status "failed", want "succeeded"`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestMarkdownShowsEveryEvaluatorPerCase(t *testing.T) {
	bad := failing("b")
	bad.Evaluators = append(bad.Evaluators[:3:3],
		EvalResult{Name: EvalApprovalGate, Detail: "tool call t2 (deploy_release) executed without an approval"},
		EvalResult{Name: EvalIdempotency, Passed: true},
		EvalResult{Name: EvalSchema, Passed: true},
		EvalResult{Name: EvalAllowlist, Detail: "agent coder executed exec"})
	old := passing("old", SplitPublic)
	old.Evaluators = old.Evaluators[:3] // a result recorded before the newer evaluators existed
	md := BuildReport(ReportMeta{GeneratedAt: t0}, []CaseResult{passing("a", SplitPublic), bad, old}).Markdown()

	for _, want := range []string{
		"## Evaluators by case",
		"| Case | termination | trace_integrity | expectation | approval_gate | idempotency | schema | allowlist |",
		"| a | pass | pass | pass | pass | pass | pass | pass |",
		"| b | pass | pass | FAIL | FAIL | pass | pass | FAIL |",
		"| old | pass | pass | pass | - | - | - | - |",
		"| approval_gate | 1 | 2 | 0.500 |",
		"  - approval_gate: tool call t2 (deploy_release) executed without an approval",
		"  - allowlist: agent coder executed exec",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestMarkdownHasNoMatrixForAnEmptyReport(t *testing.T) {
	if md := BuildReport(ReportMeta{GeneratedAt: t0}, nil).Markdown(); strings.Contains(md, "Evaluators by case") {
		t.Errorf("unexpected matrix:\n%s", md)
	}
}

func TestMarkdownHasNoFailuresSectionWhenAllPass(t *testing.T) {
	md := BuildReport(ReportMeta{GeneratedAt: t0}, []CaseResult{passing("a", SplitPublic)}).Markdown()
	if strings.Contains(md, "## Failures") {
		t.Errorf("unexpected failures section:\n%s", md)
	}
}

func TestMarkdownTruncatesLongReasons(t *testing.T) {
	r := passing("a", SplitPublic)
	r.FailureReason = strings.Repeat("x", 500)
	md := BuildReport(ReportMeta{GeneratedAt: t0}, []CaseResult{r}).Markdown()
	if strings.Contains(md, strings.Repeat("x", 200)) || !strings.Contains(md, "...") {
		t.Errorf("long reason not truncated:\n%s", md)
	}
}

func TestWriteReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports", "nested")
	rep := BuildReport(ReportMeta{GeneratedAt: t0, Split: "public", Provider: "scripted"},
		[]CaseResult{passing("a", SplitPublic), passing("b", SplitPublic)})

	md, js, err := WriteReport(dir, rep)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(md) != "20261015T120000Z.md" || filepath.Base(js) != "20261015T120000Z.json" {
		t.Fatalf("paths = %s, %s", md, js)
	}
	mdBytes, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mdBytes), "public split: 2 cases, deterministic pass 1.000, e2e success 1.000") {
		t.Errorf("markdown missing the summary line:\n%s", mdBytes)
	}
	var back Report
	raw, err := os.ReadFile(js)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("report json does not parse: %v", err)
	}
	if back.Summary.SummaryLine != rep.Summary.SummaryLine || len(back.Cases) != 2 || !back.GeneratedAt.Equal(t0) {
		t.Errorf("json round trip = %+v", back.Summary)
	}

	// A second report from the same second is written next to the first, not over it.
	md2, js2, err := WriteReport(dir, rep)
	if err != nil {
		t.Fatal(err)
	}
	if md2 == md || js2 == js {
		t.Fatalf("second report overwrote the first: %s", md2)
	}
	if filepath.Base(md2) != "20261015T120000Z-2.md" {
		t.Errorf("second report = %s", md2)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Errorf("reports dir has %d files, want 4", len(entries))
	}
}

func TestWriteReportUsesUTC(t *testing.T) {
	loc := time.FixedZone("x", -7*3600)
	rep := BuildReport(ReportMeta{GeneratedAt: time.Date(2026, 10, 15, 5, 0, 0, 0, loc)}, []CaseResult{passing("a", SplitPublic)})
	md, _, err := WriteReport(t.TempDir(), rep)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(md) != "20261015T120000Z.md" {
		t.Fatalf("path = %s", md)
	}
}
