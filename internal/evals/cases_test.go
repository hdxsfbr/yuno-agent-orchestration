package evals

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// caseSrc builds case YAML with sensible defaults; tests override single fields.
type caseSrc struct {
	ID, Version, Split, Task, Script, Expect, Extra string
}

func (s caseSrc) String() string {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&s.ID, "case-a")
	def(&s.Version, "v1")
	def(&s.Split, "public")
	def(&s.Task, "Do the thing.")
	def(&s.Script, `script:
  - tool:
      name: exec
      args: { command: "echo hi" }
  - text: done`)
	def(&s.Expect, "expect:\n  status: succeeded")
	return "id: " + s.ID + "\nversion: " + s.Version + "\nsplit: " + s.Split +
		"\ndescription: test case\nagent:\n  name: A\n  system_prompt: be brief\n  tool_allowlist: [exec]\n  limits: { max_steps: 5 }\n" +
		"task: " + s.Task + "\n" + s.Script + "\n" + s.Expect + "\nrubric: []\n" + s.Extra
}

func writeCase(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCasesFiltersBySplitAndSorts(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/b.yaml", caseSrc{ID: "b-case"}.String())
	writeCase(t, root, "v1/a.yml", caseSrc{ID: "a-case"}.String())
	writeCase(t, root, "v1/p.yaml", caseSrc{ID: "p-case", Split: "private"}.String())
	writeCase(t, root, "v1/notes.txt", "not a case")
	writeCase(t, root, "v1/.gitkeep", "")

	ids := func(split string) string {
		t.Helper()
		cases, err := LoadCases(root, split)
		if err != nil {
			t.Fatalf("LoadCases(%q): %v", split, err)
		}
		var out []string
		for _, c := range cases {
			out = append(out, c.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("public"); got != "a-case,b-case" {
		t.Errorf("public = %s", got)
	}
	if got := ids("private"); got != "p-case" {
		t.Errorf("private = %s", got)
	}
	if got := ids("all"); got != "a-case,b-case,p-case" {
		t.Errorf("all = %s", got)
	}
	if got := ids(" PUBLIC "); got != "a-case,b-case" {
		t.Errorf("split filter should be case and space insensitive, got %s", got)
	}
}

func TestLoadCasesIsRecursiveWithinAVersionDirectory(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/security/deep/x.yaml", caseSrc{ID: "deep"}.String())
	writeCase(t, root, "v2/y.yaml", caseSrc{ID: "next", Version: "v2"}.String())
	cases, err := LoadCases(root, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[0].ID != "deep" || cases[0].Version != "v1" || cases[1].Version != "v2" {
		t.Fatalf("cases = %+v", cases)
	}
	if !strings.HasSuffix(filepath.ToSlash(cases[0].Path), "v1/security/deep/x.yaml") {
		t.Errorf("path = %s", cases[0].Path)
	}
}

func TestLoadCasesCanBePointedAtAVersionDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "v1")
	writeCase(t, root, "x.yaml", caseSrc{}.String())
	if _, err := LoadCases(root, "all"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCasesRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name string
		rel  string
		src  string
		want string
	}{
		{"version differs from directory", "v1/x.yaml", caseSrc{Version: "v2"}.String(), `version "v2" does not match its directory "v1"`},
		{"missing version", "v1/x.yaml", strings.Replace(caseSrc{}.String(), "version: v1\n", "", 1), "version is required"},
		{"file outside a version directory", "x.yaml", caseSrc{}.String(), "does not match its directory"},
		{"bad split", "v1/x.yaml", caseSrc{Split: "secret"}.String(), `split "secret" must be`},
		{"split all is only a filter", "v1/x.yaml", caseSrc{Split: "all"}.String(), `split "all" must be`},
		{"missing script", "v1/x.yaml", caseSrc{Script: "script: []"}.String(), "script is required"},
		{"script key absent", "v1/x.yaml", caseSrc{Script: "# no script"}.String(), "script is required"},
		{"step with neither text nor tool", "v1/x.yaml", caseSrc{Script: "script:\n  - usage: { prompt: 1, completion: 1 }"}.String(), "neither text nor tool"},
		{"step with both text and tool", "v1/x.yaml", caseSrc{Script: "script:\n  - text: hi\n    tool: { name: ls, args: {} }"}.String(), "both text and tool"},
		{"tool without a name", "v1/x.yaml", caseSrc{Script: "script:\n  - tool: { args: {} }"}.String(), "tool name is required"},
		{"negative usage", "v1/x.yaml", caseSrc{Script: "script:\n  - text: hi\n    usage: { prompt: -1, completion: 0 }"}.String(), "usage must not be negative"},
		{"bad id", "v1/x.yaml", caseSrc{ID: "Bad Id"}.String(), "id \"Bad Id\" must match"},
		{"missing task", "v1/x.yaml", caseSrc{Task: "\"  \""}.String(), "task is required"},
		{"missing expect status", "v1/x.yaml", caseSrc{Expect: "expect: {}"}.String(), "expect.status"},
		{"non terminal expect status", "v1/x.yaml", caseSrc{Expect: "expect:\n  status: running"}.String(), `expect.status "running"`},
		{"negative min tool calls", "v1/x.yaml", caseSrc{Expect: "expect:\n  status: succeeded\n  min_tool_calls: -1"}.String(), "must not be negative"},
		{"negative cancel_after_steps", "v1/x.yaml", caseSrc{Extra: "actions:\n  cancel_after_steps: -1\n"}.String(), "cancel_after_steps"},
		{"unknown field", "v1/x.yaml", caseSrc{Extra: "taks: typo\n"}.String(), "field taks not found"},
		{"not yaml", "v1/x.yaml", "id: [unclosed", "decode"},
		{"empty file", "v1/x.yaml", "", "decode"},
		{"args that cannot be JSON", "v1/x.yaml", caseSrc{Script: "script:\n  - tool:\n      name: exec\n      args:\n        nested:\n          1: one"}.String(), "not JSON-encodable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeCase(t, root, tt.rel, tt.src)
			_, err := LoadCases(root, "all")
			if err == nil {
				t.Fatalf("expected an error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "x.yaml") {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestLoadCasesReportsEveryBadFile(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/a.yaml", caseSrc{ID: "a", Version: "v9"}.String())
	writeCase(t, root, "v1/b.yaml", caseSrc{ID: "b", Split: "nope"}.String())
	writeCase(t, root, "v1/c.yaml", caseSrc{ID: "c"}.String())
	_, err := LoadCases(root, "all")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"a.yaml", `version "v9"`, "b.yaml", `split "nope"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "c.yaml") {
		t.Errorf("error %q blames the valid file", err)
	}
}

func TestLoadCasesRejectsDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/a.yaml", caseSrc{ID: "same"}.String())
	writeCase(t, root, "v1/b.yaml", caseSrc{ID: "same"}.String())
	if _, err := LoadCases(root, "all"); err == nil || !strings.Contains(err.Error(), "duplicate case id") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadCasesErrors(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/a.yaml", caseSrc{}.String())
	if _, err := LoadCases(root, "bogus"); err == nil || !strings.Contains(err.Error(), "unknown eval split") {
		t.Errorf("bad filter: %v", err)
	}
	if _, err := LoadCases(root, "private"); err == nil || !strings.Contains(err.Error(), "no eval cases") {
		t.Errorf("empty split: %v", err)
	}
	if _, err := LoadCases(filepath.Join(root, "missing"), "all"); err == nil {
		t.Error("missing directory should be an error")
	}
	if _, err := LoadCases(t.TempDir(), "all"); err == nil || !strings.Contains(err.Error(), "no eval cases") {
		t.Errorf("empty directory: %v", err)
	}
}

func TestModelSteps(t *testing.T) {
	root := t.TempDir()
	writeCase(t, root, "v1/x.yaml", caseSrc{Script: `script:
  - tool:
      name: yuno__finish
      args:
        run_token: "$RUN_TOKEN"
        result: { summary: ok, items: [1, 2] }
    usage: { prompt: 10, completion: 3 }
  - text: bye
  - tool: { name: ls }`}.String())
	cases, err := LoadCases(root, "all")
	if err != nil {
		t.Fatal(err)
	}
	steps, err := cases[0].ModelSteps()
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d", len(steps))
	}
	raw, _ := json.Marshal(steps[0])
	want := `{"tool":{"name":"yuno__finish","args":{"result":{"items":[1,2],"summary":"ok"},"run_token":"$RUN_TOKEN"}},"usage":{"prompt":10,"completion":3}}`
	if string(raw) != want {
		t.Errorf("step 1 = %s\nwant    %s", raw, want)
	}
	if steps[1].Text != "bye" || steps[1].Tool != nil || steps[1].Usage != nil {
		t.Errorf("step 2 = %+v", steps[1])
	}
	if steps[2].Tool == nil || string(steps[2].Tool.Args) != "{}" {
		t.Errorf("a tool step without args must post an empty object, got %+v", steps[2].Tool)
	}
}

func TestLimitsStore(t *testing.T) {
	l := Limits{MaxSteps: 1, MaxDurationSeconds: 2, MaxNodeIterations: 3, MaxLoopVisits: 4, MaxCostUSD: 5.5, MaxToolCalls: 6, ApprovalTimeoutSeconds: 7}.Store()
	if l.MaxSteps != 1 || l.MaxDurationSeconds != 2 || l.MaxNodeIterations != 3 || l.MaxLoopVisits != 4 ||
		l.MaxCostUSD != 5.5 || l.MaxToolCalls != 6 || l.ApprovalTimeoutSeconds != 7 {
		t.Fatalf("limits = %+v", l)
	}
}

// shippedCasesDir is the repository's committed case directory.
const shippedCasesDir = "../../evals/cases"

// shippedSplits is the committed case list: every case and its split.
var shippedSplits = map[string]string{
	"001-quick-run-happy":               SplitPublic,
	"002-step-limit":                    SplitPublic,
	"003-cancel":                        SplitPublic,
	"004-dev-pipeline-happy":            SplitPublic,
	"005-feedback-loop-once":            SplitPublic,
	"006-feedback-loop-limit":           SplitPublic,
	"007-approval-rejected":             SplitPublic,
	"008-approval-expiry":               SplitPublic,
	"009-cost-limit":                    SplitPublic,
	"010-idempotent-replay":             SplitPublic,
	"011-blocked-tool":                  SplitPublic,
	"012-prompt-injection-exec":         SplitPrivate,
	"013-output-schema-repair":          SplitPublic,
	"014-tool-approval-gate":            SplitPrivate,
	"015-research-pipeline-happy":       SplitPublic,
	"016-time-limit":                    SplitPublic,
	"017-tool-call-limit":               SplitPublic,
	"018-schedule-triggered-run":        SplitPublic,
	"019-rate-limit":                    SplitPublic,
	"020-memory-across-nodes":           SplitPrivate,
	"021-http-fetch-untrusted-content":  SplitPrivate,
	"022-cancel-while-waiting-approval": SplitPrivate,
	"023-gitlab-tool-absent":            SplitPrivate,
	"024-guardrail-blocked-input":       SplitPrivate,
	"025-plan-pipeline-happy":           SplitPublic,
	"026-review-loop-limit-escalation":  SplitPublic,
	"027-prd-gap-escalation":            SplitPublic,
}

func TestShippedCasesAreValid(t *testing.T) {
	cases, err := LoadCases(shippedCasesDir, "all")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Case{}
	for _, c := range cases {
		byID[c.ID] = c
		want, known := shippedSplits[c.ID]
		if !known {
			t.Errorf("%s is not in the shipped case list of this test", c.ID)
		} else if c.Split != want {
			t.Errorf("%s: split = %s, want %s", c.ID, c.Split, want)
		}
		if n := len(c.Rubric); n != 0 && (n < MinRubricCriteria || n > MaxRubricCriteria) {
			t.Errorf("%s: rubric has %d criteria, want %d to %d or none", c.ID, n, MinRubricCriteria, MaxRubricCriteria)
		}
		if _, err := c.ModelSteps(); err != nil {
			t.Errorf("%s: %v", c.ID, err)
		}
	}
	if len(cases) != len(shippedSplits) {
		t.Fatalf("shipped %d cases, want %d", len(cases), len(shippedSplits))
	}

	happy := byID["001-quick-run-happy"]
	if happy.Expect.Status != "succeeded" || happy.Expect.MinToolCalls != 2 {
		t.Errorf("001 expect = %+v", happy.Expect)
	}
	if !strings.Contains(happy.Task, "$RUN_ID") {
		t.Errorf("001 task should name the run directory with $RUN_ID: %q", happy.Task)
	}
	var names []string
	for _, s := range happy.Script {
		if s.Tool != nil {
			names = append(names, s.Tool.Name)
		}
	}
	if strings.Join(names, ",") != "exec,write,yuno__finish" {
		t.Errorf("001 tools = %v", names)
	}
	fin := happy.Script[2].Tool.Args
	if fin["run_token"] != "$RUN_TOKEN" {
		t.Errorf("001 finish args = %v", fin)
	}
	if p, _ := happy.Script[1].Tool.Args["path"].(string); p != "runs/$RUN_ID/notes.md" {
		t.Errorf("001 write path = %q", p)
	}

	step := byID["002-step-limit"]
	if step.Agent.Limits.MaxSteps != 2 || step.Expect.Status != "failed" || step.Expect.FailureReasonContains != "step limit 2 exceeded" {
		t.Errorf("002 = limits %+v expect %+v", step.Agent.Limits, step.Expect)
	}
	if len(step.Script) != 3 {
		t.Errorf("002 script has %d steps, want 3", len(step.Script))
	}
	for i, s := range step.Script {
		if s.Tool == nil || s.Tool.Name == "yuno__finish" {
			t.Errorf("002 step %d must be a non-finishing tool call: %+v", i+1, s)
		}
	}

	cancel := byID["003-cancel"]
	if cancel.Actions.CancelAfterSteps != 1 || cancel.Expect.Status != "cancelled" || len(cancel.Script) != 4 {
		t.Errorf("003 = actions %+v expect %+v steps %d", cancel.Actions, cancel.Expect, len(cancel.Script))
	}
}
