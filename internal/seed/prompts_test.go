package seed

import (
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

// shippedAgentDef is a factory agent's shipped definition, straight from Agents.
func shippedAgentDef(t *testing.T, name string) store.Agent {
	t.Helper()
	for _, a := range Agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no shipped agent %q", name)
	return store.Agent{}
}

// P9: the Integrator runs no commands and changes no code, so its merge request says how
// the change was checked from what the earlier steps reported, or says plainly that the
// project's CI is the only check. It never writes NOT VERIFIED or claims a check it was
// not told about.
func TestIntegratorPromptSaysHowTheChangeWasChecked(t *testing.T) {
	a := shippedAgentDef(t, "Integrator")
	if slices.Contains(a.ToolAllowlist, "exec") {
		t.Error("the Integrator holds exec")
	}
	for _, want := range []string{
		`"How it was checked:"`,
		"the Builder's test result and the Code Reviewer's verdict",
		`"How it was checked: Not run in this task; the project's CI checks this merge request."`,
		`Never write "NOT VERIFIED"`,
		"never claim a check you were not told about",
		"A later call that names draft replaces the description",
	} {
		if !strings.Contains(a.SystemPrompt, want) {
			t.Errorf("the Integrator prompt lacks %q", want)
		}
	}
	if strings.Contains(a.SystemPrompt, "how it was tested") {
		t.Error("the Integrator prompt still asks for how the change was tested")
	}
}

// P9: the Builder and the Scout install from the lockfile, and report a new release of an
// unpinned lint or test tool that breaks the project instead of changing unrelated code.
func TestBuilderAndScoutPromptsOnUnpinnedTools(t *testing.T) {
	for _, name := range []string{"Builder", "Scout"} {
		p := shippedAgentDef(t, name).SystemPrompt
		for _, want := range []string{
			"install the project's dependencies from",
			"lockfile",
			"lint or test tools are unpinned",
			"a new release breaks them",
			"pinning them in the project",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("the %s prompt lacks %q", name, want)
			}
		}
	}
	if p := shippedAgentDef(t, "Builder").SystemPrompt; !strings.Contains(p, "do not change unrelated code to satisfy the new release") {
		t.Error("the Builder prompt does not forbid changing unrelated code for a new release")
	}
}

// D-090: the Code Reviewer only reads. It has no exec, reads the change from the changes
// file the platform writes, judges the tests without running them, lists every finding at
// once in its first review, and later only checks those and the operator's items, raising
// nothing new; a failing suite the Builder reports sends the work back.
func TestCodeReviewerOnlyReadsAndChecksItsFirstFindings(t *testing.T) {
	a := shippedAgentDef(t, "Code Reviewer")
	if slices.Contains(a.ToolAllowlist, "exec") || slices.ContainsFunc(a.ToolAllowlist, func(tool string) bool { return tool == "write" || tool == "edit" }) {
		t.Errorf("the Code Reviewer holds a tool that runs or changes something: %v", a.ToolAllowlist)
	}
	if want := []string{"read", "ls", "read_merge_request", "ask_operator", "escalate", "finish"}; !slices.Equal(a.ToolAllowlist, want) {
		t.Errorf("the Code Reviewer's tools = %v, want %v", a.ToolAllowlist, want)
	}
	for _, want := range []string{
		"You only read: you run nothing, install nothing and never change a file",
		"runs/<run id>/changes.diff",
		"new and deleted files included",
		"does a meaningful test exist for each acceptance criterion",
		"You judge the tests by reading them and never run them",
		"Your first review is your one chance to ask for changes: list every finding at once, numbered R1, R2",
		"specific and actionable",
		"A later review only verifies",
		"bugs from the manual test, a decision after the documentation audit",
		"done, explained (the Builder declined it with a reason that holds) or still open",
		"copy your findings forward unchanged, and raise no new finding",
		"Approve when every item is done or explained and the Builder reports the whole suite passing",
		"the Builder reports the suite failing, which is verification, not a new finding",
		"never rewrite",
	} {
		if !strings.Contains(a.SystemPrompt, want) {
			t.Errorf("the Code Reviewer prompt lacks %q", want)
		}
	}
	for _, gone := range []string{"git diff", "git status", "Run the project's tests yourself"} {
		if strings.Contains(a.SystemPrompt, gone) {
			t.Errorf("the Code Reviewer prompt still says %q", gone)
		}
	}
}

// D-090, D-108: the Builder runs only the affected tests while it works and the whole suite
// once, last, every round, reports the command and the result, and answers every numbered
// finding fixed or declined with the reason.
func TestBuilderReportsTheSuiteAndAnswersEachFinding(t *testing.T) {
	a := shippedAgentDef(t, "Builder")
	if !slices.Contains(a.ToolAllowlist, "exec") {
		t.Error("the Builder cannot run the suite")
	}
	for _, want := range []string{
		"Run the project's whole suite once, as the last thing before you hand over, every round",
		"run only the tests that cover what you change",
		"the Code Reviewer only reads and runs nothing",
		"the exact command in test_command",
		"the pass count or the failures",
		"answer every finding by its id in responses",
		`"fixed" with a note on what you changed`,
		`"declined" with the reason`,
	} {
		if !strings.Contains(a.SystemPrompt, want) {
			t.Errorf("the Builder prompt lacks %q", want)
		}
	}
}
