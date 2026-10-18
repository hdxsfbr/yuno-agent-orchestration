package openclaw

import (
	"strings"
	"testing"

	"yuno/internal/store"
)

// T5.18.12 (AGENTS.md): an agent that holds a memory tool is told, in one sentence of a
// "## Memory" section, that the tools also take scope "project"; an agent whose memory is
// off, or that holds no memory tool, keeps AGENTS.md as it was.
func TestAgentsMDNamesTheProjectScope(t *testing.T) {
	dir := t.TempDir()
	const id = "0199f0a2-0000-7000-8000-00000000d00d"
	agent := func(on bool, allow ...string) *store.Agent {
		return &store.Agent{ID: id, Name: "Notes", ToolAllowlist: allow, MemoryPolicy: store.MemoryPolicy{Enabled: on}}
	}

	both := plan(dir, agent(true, "read", "memory_save", "memory_recall"), nil).Files["AGENTS.md"]
	want := AgentsMD + "\n## Memory\n\nIn a run on a project, yuno__memory_recall and yuno__memory_save also take scope \"project\", " +
		"the notes shared by every agent that works on that project; without a scope they use your own memory.\n"
	if both != want {
		t.Fatalf("AGENTS.md of an agent with both memory tools:\n%s\nwant\n%s", both, want)
	}
	if got := strings.Count(both, "memory_"); got != 2 {
		t.Errorf("the section names the tools %d times, want once each", got)
	}

	recallOnly := plan(dir, agent(true, "memory_recall"), nil).Files["AGENTS.md"]
	if !strings.Contains(recallOnly, "In a run on a project, yuno__memory_recall also take scope") || strings.Contains(recallOnly, "memory_save") {
		t.Errorf("recall only:\n%s", recallOnly)
	}

	// Memory off: OpenClaw offers no memory tool (toolLists), and AGENTS.md does not
	// mention one. No memory tool allowed: nothing to say either.
	for name, a := range map[string]*store.Agent{
		"memory off":     agent(false, "memory_save", "memory_recall", "read"),
		"no memory tool": agent(true, "read", "exec"),
		"blocked tool":   {ID: id, Name: "Blocked", ToolAllowlist: []string{"memory_save"}, MemoryPolicy: store.MemoryPolicy{Enabled: true}, Guardrails: store.Guardrails{BlockedTools: []string{"memory_save"}}},
	} {
		if got := plan(dir, a, nil).Files["AGENTS.md"]; got != AgentsMD {
			t.Errorf("%s: AGENTS.md = %q, want the operating rules alone", name, got)
		}
	}

	// With a chat token the Chatting section follows the Memory section, and the file
	// (so the desired state) differs from the memory-off one.
	const token = "c_abcdefghijklmnopqrstuvwxyz234567"
	chat := agent(true, "memory_save", "memory_recall")
	chat.ChatToken = token
	md := plan(dir, chat, nil).Files["AGENTS.md"]
	if i, j := strings.Index(md, "\n## Memory\n"), strings.Index(md, "\n## Chatting\n"); i < 0 || j < i {
		t.Errorf("sections out of order (memory %d, chatting %d):\n%s", i, j, md)
	}
	off := agent(false, "memory_save", "memory_recall")
	off.ChatToken = token
	if DesiredHash(dir, chat, nil) == DesiredHash(dir, off, nil) {
		t.Error("turning memory on does not change the agent's desired state")
	}
}
