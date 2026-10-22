package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// The review loop converges (D-090, DESIGN 5.29 rule 5): the Code Reviewer lists every
// finding in its first review, numbered, and a later round only checks those findings, the
// Builder's answer to each and what the operator sent back.

const (
	findingR1 = `greet.go: a blank name returns "Hello, !"; return "Hello, stranger!" and test it.`
	findingR2 = `greet.go: export greet as Greet so the command line can call it.`
	answerR1  = `greet trims the name and greets a stranger for a blank one; TestGreetBlank proves it.`
	answerR2  = `The ticket and ARCHITECTURE.md keep greet unexported: the command line is in the same package.`

	builderRound1 = `{"summary":"Built greet; go test ./... passed 3 tests.","files":["greet.go","greet_test.go"],"tests_passed":true,"test_command":"go test ./..."}`
	reviewRound1  = `{"verdict":"changes_requested","comments":"Two findings, R1 and R2.","findings":[` +
		`{"id":"R1","text":"greet.go: a blank name returns \"Hello, !\"; return \"Hello, stranger!\" and test it."},` +
		`{"id":"R2","text":"` + findingR2 + `"}]}`
	builderRound2 = `{"summary":"Fixed R1, declined R2; go test ./... passed 4 tests.","files":["greet.go","greet_test.go"],"tests_passed":true,` +
		`"test_command":"go test ./...","responses":[{"id":"R1","action":"fixed","note":"` + answerR1 + `"},` +
		`{"id":"R2","action":"declined","note":"` + answerR2 + `"}]}`
	reviewRound2 = `{"verdict":"approved","comments":"R1 is done and R2 is explained.","findings":[` +
		`{"id":"R1","text":"greet.go: a blank name returns \"Hello, !\"; return \"Hello, stranger!\" and test it."},` +
		`{"id":"R2","text":"` + findingR2 + `"}],` +
		`"checks":[{"id":"R1","status":"done","note":"TestGreetBlank"},{"id":"R2","status":"explained","note":"the plan keeps it unexported"}]}`
)

// block is the lines of message after the line head, up to the first empty line or the line
// that starts with stop.
func block(message, head, stop string) []string {
	_, rest, ok := strings.Cut(message, head+"\n")
	if !ok {
		return nil
	}
	var out []string
	for _, l := range strings.Split(rest, "\n") {
		if l == "" || strings.HasPrefix(l, stop) {
			break
		}
		out = append(out, l)
	}
	return out
}

// A scripted ticket: the first review sends back R1 and R2; the Builder fixes R1 and
// declines R2 with a reason; the second review's task holds exactly R1 and R2 and the two
// answers, and it approves with R1 done and R2 explained. The findings stay in state as the
// first review wrote them.
func TestWavePipelineReviewLoopChecksTheFirstFindings(t *testing.T) {
	env := newEnv(t)
	wf := templatePipeline(t, env, "wave_pipeline.json")
	sc := env.script(map[string][]string{
		"builder":          {builderRound1, builderRound2},
		"code_review":      {reviewRound1, reviewRound2},
		"integrator_draft": {draftMR1},
	})
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(waveTicket)})
	if r.Status != store.RunWaitingApproval || deref(r.CurrentNode) != "manual_test" {
		t.Fatalf("run = %s at %s %q", r.Status, deref(r.CurrentNode), deref(r.FailureReason))
	}
	want := []string{"builder#1:succeeded", "code_review#1:succeeded", "builder#2:succeeded", "code_review#2:succeeded", "integrator_draft#1:succeeded",
		"manual_test#1:waiting_approval"}
	if got := visitsOf(runNodes(t, env, r.ID)); !slices.Equal(got, want) {
		t.Fatalf("visits = %v", got)
	}

	first := sc.message("code_review", 1)
	for _, want := range []string{"This is your first review of this ticket and your only chance to ask for changes",
		"numbered R1, R2", "runs/" + r.ID + "/changes.diff", "Run nothing and change nothing", "command: go test ./..."} {
		if !strings.Contains(first, want) {
			t.Errorf("the first review's task lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "This is a later round") {
		t.Errorf("the first review's task is a later round's:\n%s", first)
	}

	b2 := sc.message("builder", 2)
	if got := block(b2, "The code reviewer asked for changes. Its findings:", "Its comments:"); !slices.Equal(got, []string{"- R1: " + findingR1, "- R2: " + findingR2}) {
		t.Errorf("the builder's findings = %q:\n%s", got, b2)
	}
	for _, want := range []string{"Answer every finding by its id in responses", `"fixed"`, `"declined"`, "test_command"} {
		if !strings.Contains(b2, want) {
			t.Errorf("the builder's second task lacks %q:\n%s", want, b2)
		}
	}

	second := sc.message("code_review", 2)
	if got := block(second, "This is a later round: verify, do not review again. The findings of your first review:", "The Builder's answers:"); !slices.Equal(got,
		[]string{"- R1: " + findingR1, "- R2: " + findingR2}) {
		t.Errorf("the second review's findings = %q:\n%s", got, second)
	}
	if got := block(second, "The Builder's answers:", "Check these items"); !slices.Equal(got,
		[]string{"- R1: fixed (" + answerR1 + ")", "- R2: declined (" + answerR2 + ")"}) {
		t.Errorf("the second review's answers = %q:\n%s", got, second)
	}
	for _, want := range []string{"Check these items and nothing else", "Raise nothing new", "findings copied unchanged",
		`verdict "approved" when every item is done or explained`, "runs/" + r.ID + "/changes.diff"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second review's task lacks %q:\n%s", want, second)
		}
	}
	for _, unwanted := range []string{"first review of this ticket", "- R3", "item manual_test", "item doc_audit"} {
		if strings.Contains(second, unwanted) {
			t.Errorf("the second review's task has %q:\n%s", unwanted, second)
		}
	}

	review, _ := stateOf(t, r)["code_review"].(map[string]any)
	findings, _ := review["findings"].([]any)
	checks, _ := review["checks"].([]any)
	if review["verdict"] != "approved" || len(findings) != 2 || len(checks) != 2 {
		t.Fatalf("state.code_review = %v", review)
	}
	if f, _ := findings[0].(map[string]any); f["id"] != "R1" || f["text"] != findingR1 {
		t.Fatalf("state.code_review.findings = %v", findings)
	}
	if builder, _ := stateOf(t, r)["builder"].(map[string]any); builder["test_command"] != "go test ./..." {
		t.Fatalf("state.builder = %v", builder)
	}
}

// Both rounds' tasks render from state alone: the first round's full review, a later
// round's findings, last check, answers and the operator's items, and the Builder's task
// with the reviewer's check of its last answers. Every task renders with an empty state.
func TestWavePipelineReviewTasksRender(t *testing.T) {
	def := wavePipelineDef(t)
	review, _ := def.Node("code_review")
	builder, _ := def.Node("builder")
	render := func(task, state string) string {
		t.Helper()
		out, err := workflow.Render(task, json.RawMessage(waveTicket), json.RawMessage(state))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "<no value>") {
			t.Fatalf("a missing value printed:\n%s", out)
		}
		return out
	}

	first := render(review.Task, `{"builder":`+builderRound1+`}`)
	for _, want := range []string{"T1.1 add the endpoint", "The builder reports: Built greet; go test ./... passed 3 tests.",
		"Files: greet.go greet_test.go", "Whole suite passed (tests_passed): true, command: go test ./...",
		"your only chance to ask for changes", "List every finding at once in findings, numbered R1, R2",
		"a meaningful test exists for every acceptance test", "a whole suite the Builder reports failing is a finding too"} {
		if !strings.Contains(first, want) {
			t.Errorf("first round lacks %q:\n%s", want, first)
		}
	}

	// A third round, after the operator rejected the manual test and the audit sent a
	// decision back: the last check and both operator items are listed with the answers.
	later := render(review.Task, `{"builder":{"summary":"s","files":["greet.go"],"tests_passed":false,"responses":[`+
		`{"id":"R2","action":"fixed","note":"exported"},{"id":"manual_test","action":"fixed","note":"the export works on Safari"}]},`+
		`"code_review":{"verdict":"changes_requested","comments":"R2 is open.","findings":[{"id":"R1","text":"one"},{"id":"R2","text":"two"}],`+
		`"checks":[{"id":"R1","status":"done"},{"id":"R2","status":"open","note":"the reason does not hold"}]},`+
		`"manual_test":{"decision":"rejected","reason":"the export does nothing on Safari"},`+
		`"doc_audit":{"verdict":"rework","trace_path":"t.md","feature":"x","notes":"reject a user without the admin role"}}`)
	for _, want := range []string{"Whole suite passed (tests_passed): false\n",
		"The findings of your first review:\n- R1: one\n- R2: two\nYour last check:\n- R1: done\n- R2: open (the reason does not hold)\n",
		"The Builder's answers:\n- R2: fixed (exported)\n- manual_test: fixed (the export works on Safari)\n",
		"The operator's manual test sent bugs back (item manual_test): the export does nothing on Safari\n",
		"The operator decided after the documentation audit (item doc_audit): reject a user without the admin role\n",
		"An item your last check found done or explained stays so unless the change undid it"} {
		if !strings.Contains(later, want) {
			t.Errorf("later round lacks %q:\n%s", want, later)
		}
	}
	if strings.Contains(later, "first review of this ticket") {
		t.Errorf("a later round carries the first round's instructions:\n%s", later)
	}

	// A later round after a first review without numbered findings (an approval, or a result
	// from before them) shows its comments, and no answers.
	bare := render(review.Task, `{"builder":{"summary":"s","files":[],"tests_passed":true},"code_review":{"verdict":"approved","comments":"fine"}}`)
	if !strings.Contains(bare, "The findings of your first review:\n(none; your last comments: fine)\n") ||
		!strings.Contains(bare, "The Builder's answers:\n(none)\n") || strings.Contains(bare, "item manual_test") {
		t.Errorf("later round without findings:\n%s", bare)
	}

	third := render(builder.Task, `{"code_review":{"verdict":"changes_requested","comments":"R2 is open.","findings":[{"id":"R1","text":"one"},{"id":"R2","text":"two"}],`+
		`"checks":[{"id":"R1","status":"done"},{"id":"R2","status":"open","note":"the reason does not hold"}]}}`)
	for _, want := range []string{"Its findings:\n- R1: one\n- R2: two\n", "Its check of your last answers:\n- R1: done\n- R2: open (the reason does not hold)\n",
		"Its comments: R2 is open.\n", "A finding it checked as done or explained needs no new work"} {
		if !strings.Contains(third, want) {
			t.Errorf("the builder's third task lacks %q:\n%s", want, third)
		}
	}
	plain := render(builder.Task, `{"code_review":{"verdict":"changes_requested","comments":"cover the empty case"}}`)
	if !strings.Contains(plain, "Address every point, with tests, before finishing:\ncover the empty case") || strings.Contains(plain, "by its id") {
		t.Errorf("the builder's task for comments without findings:\n%s", plain)
	}
	if msgs := def.ValidateTemplates(json.RawMessage(waveTicket)); len(msgs) > 0 {
		t.Fatalf("templates: %v", msgs)
	}
}

// The new fields are optional and checked where they are given: a result of before them
// passes, an answer other than fixed or declined, an answer without its note, a check other
// than done, explained or open and a finding that is not numbered R<n> are refused, by the
// steps' output schemas and by the state schema alike.
func TestWavePipelineReviewSchemas(t *testing.T) {
	def := wavePipelineDef(t)
	var state struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(def.StateSchema, &state); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]struct {
		result string
		ok     bool
	}{
		"builder": {
			{builderOK, true},
			{builderRound1, true},
			{builderRound2, true},
			{`{"summary":"s","files":[],"tests_passed":true,"responses":[{"id":"manual_test","action":"fixed","note":"n"},{"id":"doc_audit","action":"fixed","note":"n"}]}`, true},
			{`{"summary":"s","files":[],"tests_passed":true,"responses":[{"id":"R1","action":"maybe","note":"n"}]}`, false},
			{`{"summary":"s","files":[],"tests_passed":true,"responses":[{"id":"R1","action":"declined"}]}`, false},
			{`{"summary":"s","files":[],"tests_passed":true,"responses":[{"id":"R1","action":"declined","note":""}]}`, false},
			{`{"summary":"s","files":[],"tests_passed":true,"responses":[{"id":"first","action":"fixed","note":"n"}]}`, false},
			{`{"summary":"s","files":[],"tests_passed":true,"test_command":7}`, false},
		},
		"code_review": {
			{reviewNo, true},
			{reviewYes, true},
			{reviewRound1, true},
			{reviewRound2, true},
			{`{"verdict":"changes_requested","comments":"c","checks":[{"id":"manual_test","status":"open"}]}`, true},
			{`{"verdict":"approved","comments":"c","checks":[{"id":"R1","status":"partly"}]}`, false},
			{`{"verdict":"changes_requested","comments":"c","findings":[{"id":"F1","text":"t"}]}`, false},
			{`{"verdict":"changes_requested","comments":"c","findings":[{"id":"R1"}]}`, false},
			{`{"verdict":"changes_requested","findings":[{"id":"R1","text":"t"}]}`, false},
		},
	}
	for id, tcs := range cases {
		node, _ := def.Node(id)
		for name, raw := range map[string]json.RawMessage{"output schema": node.OutputSchema, "state schema": state.Properties[id]} {
			s, err := compileSchema(raw)
			if err != nil {
				t.Fatalf("%s %s: %v", id, name, err)
			}
			for _, tc := range tcs {
				if got := validateOutput(s, json.RawMessage(tc.result)); (got == "") != tc.ok {
					t.Errorf("%s %s: %s: %q, want valid %v", id, name, tc.result, got, tc.ok)
				}
			}
		}
	}
}
