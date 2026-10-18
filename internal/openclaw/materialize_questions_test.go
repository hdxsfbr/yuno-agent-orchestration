package openclaw

import (
	"strings"
	"testing"

	"yuno/internal/store"
)

const (
	amendedQuestionSentence = "Do not ask questions in your reply: nobody reads it while you work; decide, act, and finish."
	deliverablesSentence    = "Deliverables: put files the operator should receive (reports, data, documents) under runs/<run_id>/out/; " +
		"the platform collects them when your step ends and shows them on the run page."
	questionsHeading = "\n## Questions\n\n"
)

// questionsParagraph is DESIGN 5.24 rule 12's paragraph as AGENTS.md writes it, typed out
// here so a change to the constant is a change to this test.
const questionsParagraph = `Questions: when one fact you cannot find out yourself decides the work, ask the operator with ` +
	`yuno__ask_operator: a short question, with options when a few answers are likely; at most 3 per step. ` +
	`The answer usually comes back in the tool result. If the result says "pending", save your progress in files under ` +
	`runs/<run_id>/ and end your turn without calling yuno__finish: the platform runs this step again with the answer ` +
	`added to your task. Decide yourself what you can.` + "\n"

// T5.24.14 (DESIGN 5.24 rule 12): every agent's AGENTS.md says not to ask questions in the
// reply, in the amended words, and no longer says the old ones.
func TestAgentsMDQuestionSentenceAmended(t *testing.T) {
	if !strings.Contains(AgentsMD, amendedQuestionSentence) || strings.Contains(AgentsMD, "Do not ask questions; decide, act, and finish.") {
		t.Fatalf("AgentsMD = %s", AgentsMD)
	}
	for name, a := range map[string]*store.Agent{
		"a plain agent":      {ID: "0199f0a2-0000-7000-8000-00000000e001", Name: "Writer", ToolAllowlist: []string{"read", "write", "finish"}},
		"an asking agent":    {ID: "0199f0a2-0000-7000-8000-00000000e002", Name: "Coder", ToolAllowlist: []string{"read", "ask_operator", "finish"}},
		"the Chief of Staff": {ID: "0199f0a2-0000-7000-8000-00000000e003", Name: "Chief", ChatToken: "c_abcdefghijklmnopqrstuvwxyz234567", ToolAllowlist: []string{"delegate_task", "answer_question"}},
	} {
		if md := plan(t.TempDir(), a, nil).Files["AGENTS.md"]; !strings.HasPrefix(md, AgentsMD) || !strings.Contains(md, amendedQuestionSentence) {
			t.Errorf("%s: AGENTS.md lacks the amended sentence:\n%s", name, md)
		}
	}
}

// T5.25.9 (DESIGN 5.25 rule 1): AGENTS.md carries the Deliverables sentence, with
// <run_id> as the file writes it, for every agent.
func TestAgentsMDDeliverablesSentence(t *testing.T) {
	if DeliverablesRule != deliverablesSentence || !strings.Contains(AgentsMD, deliverablesSentence) {
		t.Fatalf("DeliverablesRule = %q", DeliverablesRule)
	}
	a := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e004", Name: "Researcher", ToolAllowlist: []string{"read", "write", "finish"}}
	if md := plan(t.TempDir(), a, nil).Files["AGENTS.md"]; md != AgentsMD || !strings.Contains(md, deliverablesSentence) {
		t.Fatalf("AGENTS.md = %s", md)
	}
	// The sentence sits among the operating rules, before the project and chat rules.
	if i, j := strings.Index(AgentsMD, deliverablesSentence), strings.Index(AgentsMD, ProjectRule); i < 0 || j < i {
		t.Errorf("the Deliverables sentence is at %d, the project rule at %d", i, j)
	}
}

// T5.24.14: the "## Questions" section is written for an agent that holds ask_operator and
// for no other, between the memory and chatting sections.
func TestAgentsMDQuestionsSection(t *testing.T) {
	if QuestionsRule != questionsParagraph {
		t.Fatalf("QuestionsRule = %q", QuestionsRule)
	}
	dir := t.TempDir()
	const token = "c_abcdefghijklmnopqrstuvwxyz234567"
	holder := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e005", Name: "Coder", ChatToken: token, MemoryPolicy: store.MemoryPolicy{Enabled: true},
		ToolAllowlist: []string{"read", "memory_recall", "ask_operator", "run_status", "finish"}}
	md := plan(dir, holder, nil).Files["AGENTS.md"]
	if !strings.Contains(md, questionsHeading+questionsParagraph) {
		t.Fatalf("a holder of ask_operator has no Questions section:\n%s", md)
	}
	mem, qs, chat := strings.Index(md, "\n## Memory\n"), strings.Index(md, questionsHeading), strings.Index(md, "\n## Chatting\n")
	if !(0 < mem && mem < qs && qs < chat) {
		t.Errorf("sections out of order: Memory %d, Questions %d, Chatting %d\n%s", mem, qs, chat, md)
	}
	if strings.Count(md, questionsHeading) != 1 {
		t.Errorf("Questions section written %d times", strings.Count(md, questionsHeading))
	}

	// Alone, with nothing else to say, it follows the operating rules directly.
	only := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e006", Name: "Researcher", ToolAllowlist: []string{"read", "yuno__ask_operator"}}
	if got := plan(dir, only, nil).Files["AGENTS.md"]; got != AgentsMD+questionsHeading+questionsParagraph {
		t.Errorf("AGENTS.md = %q", got)
	}

	// No section without the tool, for the agents that hold the others, and when the tool
	// is denied (blocked by an interaction rule or a guardrail) it is not offered either.
	blocked := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e007", Name: "Blocked", ToolAllowlist: []string{"read", "ask_operator"},
		InteractionRules: map[string]string{"ask_operator": "blocked"}}
	guarded := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e008", Name: "Guarded", ToolAllowlist: []string{"read", "ask_operator"},
		Guardrails: store.Guardrails{BlockedTools: []string{"ask_operator"}}}
	for name, a := range map[string]*store.Agent{
		"no allowlist":       {ID: "0199f0a2-0000-7000-8000-00000000e009", Name: "Bare"},
		"other tools":        {ID: "0199f0a2-0000-7000-8000-00000000e00a", Name: "Writer", ToolAllowlist: []string{"read", "write", "finish"}},
		"the Chief's tools":  {ID: "0199f0a2-0000-7000-8000-00000000e00b", Name: "Chief", ChatToken: token, ToolAllowlist: []string{"delegate_task", "answer_question", "run_status"}},
		"blocked by a rule":  blocked,
		"blocked by a guard": guarded,
	} {
		md := plan(dir, a, nil).Files["AGENTS.md"]
		if strings.Contains(md, questionsHeading) || strings.Contains(md, "yuno__ask_operator") {
			t.Errorf("%s: AGENTS.md has a Questions section:\n%s", name, md)
		}
	}
	for name, a := range map[string]*store.Agent{
		"the allowlist's prefixed spelling": {ToolAllowlist: []string{"yuno__finish", "yuno__ask_operator"}},
		"the short spelling":                {ToolAllowlist: []string{"ask_operator"}},
	} {
		if got := questionsSection(a); got != questionsHeading+questionsParagraph {
			t.Errorf("questionsSection with %s = %q", name, got)
		}
	}
	if got := questionsSection(&store.Agent{}); got != "" {
		t.Errorf("questionsSection of an agent with no tools = %q", got)
	}
}

// The Chief's Chatting section lists answer_question with the other chat tools.
func TestAgentsMDChattingListsAnswerQuestion(t *testing.T) {
	const token = "c_abcdefghijklmnopqrstuvwxyz234567"
	chief := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000e00c", Name: "Chief", ChatToken: token,
		ToolAllowlist: []string{"answer_question", "delegate_task", "run_status"}}
	md := plan(t.TempDir(), chief, nil).Files["AGENTS.md"]
	if !strings.Contains(md, "yuno__answer_question, yuno__delegate_task, yuno__run_status.") {
		t.Errorf("AGENTS.md lacks the chat tools:\n%s", md)
	}
}

// Review M2: AllowsTool answers from the effective tools.allow (toolLists), the list the
// materialiser writes: the allowlist and finish, less the guardrails' blocked tools and the
// interaction rules set to blocked. The task's protocol block and AGENTS.md both ask it.
func TestAllowsToolReadsTheEffectiveAllowList(t *testing.T) {
	for name, tc := range map[string]struct {
		agent *store.Agent
		tool  string
		want  bool
	}{
		"in the allowlist":             {&store.Agent{ToolAllowlist: []string{"read", "ask_operator"}}, "ask_operator", true},
		"the prefixed spelling listed": {&store.Agent{ToolAllowlist: []string{"yuno__ask_operator"}}, "ask_operator", true},
		"asked with the prefix":        {&store.Agent{ToolAllowlist: []string{"ask_operator"}}, "yuno__ask_operator", true},
		"padded entries":               {&store.Agent{ToolAllowlist: []string{" ask_operator "}}, " ask_operator", true},
		"not in the allowlist":         {&store.Agent{ToolAllowlist: []string{"read", "write"}}, "ask_operator", false},
		"no allowlist":                 {&store.Agent{}, "ask_operator", false},
		"blocked by a rule":            {&store.Agent{ToolAllowlist: []string{"ask_operator"}, InteractionRules: map[string]string{"ask_operator": "blocked"}}, "ask_operator", false},
		"blocked by a rule, prefixed":  {&store.Agent{ToolAllowlist: []string{"ask_operator"}, InteractionRules: map[string]string{"yuno__ask_operator": "blocked"}}, "ask_operator", false},
		"another rule":                 {&store.Agent{ToolAllowlist: []string{"ask_operator"}, InteractionRules: map[string]string{"ask_operator": "auto"}}, "ask_operator", true},
		"blocked by a guardrail":       {&store.Agent{ToolAllowlist: []string{"ask_operator"}, Guardrails: store.Guardrails{BlockedTools: []string{"ask_operator"}}}, "ask_operator", false},
		"finish is always there":       {&store.Agent{}, "finish", true},
	} {
		if got := AllowsTool(tc.agent, tc.tool); got != tc.want {
			t.Errorf("%s: AllowsTool(%q) = %v, want %v", name, tc.tool, got, tc.want)
		}
	}
}
