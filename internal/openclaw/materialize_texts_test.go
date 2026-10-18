package openclaw_test

import (
	"testing"

	"yuno/internal/engine"
	"yuno/internal/openclaw"
)

// The materialiser cannot import the engine (the engine imports it), so AGENTS.md keeps its
// own copy of the sentences the task protocol block carries (DESIGN 5.24 rule 12, 5.25
// rule 1). They are the same text up to the run id, which AGENTS.md writes as <run_id>.
func TestAgentsMDTextsMatchTheProtocolBlock(t *testing.T) {
	const placeholder = "<run_id>"
	if got := engine.QuestionsParagraph(placeholder); got != openclaw.QuestionsRule {
		t.Errorf("the Questions paragraph differs:\n protocol block %q\n AGENTS.md      %q", got, openclaw.QuestionsRule)
	}
	if got := engine.DeliverablesLine(placeholder); got != openclaw.DeliverablesRule+"\n" {
		t.Errorf("the Deliverables line differs:\n protocol block %q\n AGENTS.md      %q", got, openclaw.DeliverablesRule+"\n")
	}
}
