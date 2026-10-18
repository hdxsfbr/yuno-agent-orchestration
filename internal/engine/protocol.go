package engine

// The task protocol block's wave 10 texts (DESIGN 5.6 rule 5; 5.16 rule 9 amended). The
// exact wording is the contract: the materialiser's AGENTS.md carries the same sentences.

// DeliverablesLine is DESIGN 5.25 rule 1's line of the task protocol block.
func DeliverablesLine(runID string) string {
	return "Deliverables: put files the operator should receive (reports, data, documents) under runs/" +
		runID + "/out/; the platform collects them when your step ends and shows them on the run page.\n"
}

// QuestionsParagraph is DESIGN 5.24 rule 12's paragraph, for a visit whose agent holds
// ask_operator.
func QuestionsParagraph(runID string) string {
	return "Questions: when one fact you cannot find out yourself decides the work, ask the operator with " +
		"yuno__ask_operator: a short question, with options when a few answers are likely; at most 3 per step. " +
		"The answer usually comes back in the tool result. If the result says \"pending\", save your progress " +
		"in files under runs/" + runID + "/ and end your turn without calling yuno__finish: the platform runs " +
		"this step again with the answer added to your task. Decide yourself what you can.\n"
}
