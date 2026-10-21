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
