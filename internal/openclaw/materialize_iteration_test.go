package openclaw

import (
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// T5.22.12 (AGENTS.md, DESIGN 5.16 rule 9 amended by 5.22 rule 4): the project rule every
// agent reads says the platform commits the working tree when a merge request is opened
// and also when it is pushed onto the merge request the run updates.
func TestProjectRuleCoversUpdatingARequest(t *testing.T) {
	if !strings.HasSuffix(ProjectRule, "the platform commits your working tree when a merge request is opened or pushed onto the merge request the run updates.") {
		t.Fatalf("ProjectRule = %q", ProjectRule)
	}
	for _, kept := range []string{"its checkout at runs/<run_id>/repo/ is the platform's", "never clone, commit or push", "never change its .git directory"} {
		if !strings.Contains(ProjectRule, kept) {
			t.Errorf("ProjectRule lost %q", kept)
		}
	}
	dir := t.TempDir()
	a := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000d11d", Name: "Coder", ToolAllowlist: []string{"read", "exec", "finish"}}
	if md := plan(dir, a, nil).Files["AGENTS.md"]; md != AgentsMD || !strings.Contains(md, ProjectRule) {
		t.Fatalf("AGENTS.md does not carry the project rule:\n%s", md)
	}
}

// T5.22.12 (DESIGN 5.15 rule 15, 5.22 rule 6): read_merge_request is chat-capable, so an
// agent that holds it is told in the Chatting section that it works in a conversation.
func TestAgentsMDListsReadMergeRequestAsAChatTool(t *testing.T) {
	dir := t.TempDir()
	const token = "c_abcdefghijklmnopqrstuvwxyz234567"
	chief := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000d12d", Name: "Chief", ChatToken: token,
		ToolAllowlist: []string{"delegate_task", "read_merge_request", "run_status"}}
	md := plan(dir, chief, nil).Files["AGENTS.md"]
	if !strings.Contains(md, "yuno__delegate_task, yuno__read_merge_request, yuno__run_status.") {
		t.Errorf("AGENTS.md lacks the chat tools:\n%s", md)
	}
	// Alone it is enough for the section.
	only := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000d13d", Name: "Reader", ChatToken: token, ToolAllowlist: []string{"read_merge_request"}}
	if md := plan(dir, only, nil).Files["AGENTS.md"]; !strings.Contains(md, "\n## Chatting\n") || !strings.Contains(md, "these platform tools: yuno__read_merge_request.") {
		t.Errorf("the tool alone:\n%s", md)
	}
}

// Every chat-capable platform tool is a platform tool of the materialiser: an allowlist
// entry that is not in PlatformTools reaches the runtime without the yuno__ prefix, as a
// name no MCP tool has, and the agent cannot call it (DESIGN 5.7 rule 2).
func TestEveryChatToolIsMaterialisedWithThePrefix(t *testing.T) {
	for _, name := range tools.ChatToolNames {
		if got := ToolName(name); got != PlatformToolPrefix+name {
			t.Errorf("ToolName(%q) = %q, want %q", name, got, PlatformToolPrefix+name)
		}
	}
	a := &store.Agent{ID: "0199f0a2-0000-7000-8000-00000000d14d", Name: "Coder", MemoryPolicy: store.MemoryPolicy{Enabled: true},
		ToolAllowlist: []string{"read", "memory_recall", "read_merge_request", "finish"}}
	allow, _ := toolLists(a)
	for _, want := range []string{"read", "yuno__finish", "yuno__memory_recall", "yuno__read_merge_request"} {
		if !slices.Contains(allow, want) {
			t.Errorf("tools.allow %v lacks %s", allow, want)
		}
	}
}
