package seed

import (
	"context"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

// T5.18.12: the Coder's and the Reviewer's shipped prompts say to recall project notes
// before deciding how to build or test and to save what they learn as project notes, and
// the reset (make seed-reset) carries the text to an install that has the old prompts;
// the other shipped agents' prompts are not about project notes.
func TestShippedPromptsNameProjectNotes(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, _, err := Seed(ctx, pool)
	must(t, err)

	phrases := []string{"memory_recall", "memory_save", `scope "project"`, "before deciding how to build or test",
		"how the project is built, tested or laid out"}
	for _, name := range []string{"Coder", "Reviewer"} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		for _, phrase := range phrases {
			if !strings.Contains(a.SystemPrompt, phrase) {
				t.Errorf("%s prompt lacks %q:\n%s", name, phrase, a.SystemPrompt)
			}
		}
	}
	for _, name := range []string{"Deployer", "Researcher", "Critic", "Writer"} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		if strings.Contains(a.SystemPrompt, `scope "project"`) {
			t.Errorf("%s prompt names project notes", name)
		}
	}

	// An install from before this wave has the old prompts; the reset restores the new ones.
	for _, name := range []string{"Coder", "Reviewer"} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		a.SystemPrompt = "You are a careful " + strings.ToLower(name) + "."
		must(t, store.UpdateAgent(ctx, pool, a))
	}
	agents, _, err := ResetShipped(ctx, pool, nil)
	must(t, err)
	if !slices.Equal(agents, []string{"Coder", "Reviewer"}) {
		t.Fatalf("reset %v", agents)
	}
	for i, name := range []string{"Coder", "Reviewer"} {
		a, err := findAgent(ctx, pool, name)
		must(t, err)
		if a.SystemPrompt != Agents[i].SystemPrompt || !strings.Contains(a.SystemPrompt, `scope "project"`) {
			t.Errorf("%s after the reset: %q", name, a.SystemPrompt)
		}
	}
	agents, _, err = ResetShipped(ctx, pool, nil)
	must(t, err)
	if len(agents) != 0 {
		t.Fatalf("a second reset changed %v", agents)
	}
}
