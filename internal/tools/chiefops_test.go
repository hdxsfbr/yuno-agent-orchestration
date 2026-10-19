package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

// D-081: the three operator tools are chat tools, computational or read-only, and answer
// a run token or a conversation that is not the Chief's bound one with a refusal.
func TestChiefOpsAreChatToolsAndRefuseRuns(t *testing.T) {
	pool := needDB(t)
	for _, n := range []string{AddProjectName, DecideApprovalName, ListApprovalsName} {
		if !slices.Contains(ChatToolNames, n) {
			t.Errorf("%s is not in ChatToolNames", n)
		}
	}
	if !slices.IsSorted(ChatToolNames) {
		t.Fatalf("ChatToolNames unsorted: %v", ChatToolNames)
	}
	chief := chiefAgent(t, pool, "Chief-"+uniq(), "list_approvals")
	for _, tl := range []struct {
		tool *Tool
		in   string
	}{
		{NewListApprovals(pool, []byte("k")), `{"chat_token":"x"}`},
		{NewDecideApproval(pool, nil, []byte("k")), `{"chat_token":"x","approval_id":"a","code":"ABCD","decision":"approve"}`},
		{NewAddProject(pool, nil), `{"chat_token":"x","repo_url":"https://h.test/g/p.git"}`},
	} {
		if tl.tool.Risk == RiskSideEffecting || !tl.tool.Chat {
			t.Errorf("%s: risk %s chat %v", tl.tool.Name, tl.tool.Risk, tl.tool.Chat)
		}
		for name, call := range map[string]Call{
			"run":         {RunID: "r1", AgentID: chief.ID, Input: json.RawMessage(tl.in)},
			"no chat":     {AgentID: chief.ID, Input: json.RawMessage(tl.in)},
			"unbound":     {Chat: true, AgentID: chief.ID, Input: json.RawMessage(tl.in)},
			"no such one": {Chat: true, AgentID: chief.ID, ConversationID: "no-such", Input: json.RawMessage(tl.in)},
		} {
			if res := invoke(t, tl.tool, call); !res.IsError || !strings.Contains(res.Text, "only available") {
				t.Errorf("%s with a %s call = %+v", tl.tool.Name, name, res)
			}
		}
	}
}

// add_project's schema has no token member and refuses any other one.
func TestAddProjectSchemaRefusesTokens(t *testing.T) {
	tool := NewAddProject(nil, nil)
	NewRegistry().Register(tool)
	for _, f := range []string{"token", "git_token", "password"} {
		in := `{"chat_token":"x","repo_url":"https://h.test/g/p.git","` + f + `":"s"}`
		if err := tool.ValidateInput(json.RawMessage(in)); err == nil {
			t.Errorf("%s accepted", f)
		}
	}
	if strings.Contains(strings.ToLower(string(addProjectSchema)), `"token"`) {
		t.Error("the schema names a token member")
	}
}

// D-081: the code is stable, short, from the unambiguous alphabet and keyed; it is matched
// as a whole token, ignoring case.
func TestApprovalCodeAndMatching(t *testing.T) {
	a, b := ApprovalCode([]byte("k1"), "id-1"), ApprovalCode([]byte("k1"), "id-2")
	if a != ApprovalCode([]byte("k1"), "id-1") || len(a) != 4 || len(b) != 4 {
		t.Fatalf("codes %q %q", a, b)
	}
	for i := 0; i < 200; i++ {
		for _, c := range ApprovalCode([]byte("k"), store.NewID()) {
			if !strings.ContainsRune(approvalCodeAlphabet, c) {
				t.Fatalf("%q outside the alphabet", c)
			}
		}
	}
	for text, want := range map[string]bool{
		"approve K7QX": true, "k7qx": true, "yes (K7QX).": true, "K7QX": true,
		"K7QXA": false, "AK7QX": false, "K7Q X": false, "": false, "12K7QX": false,
	} {
		if got := containsCode(text, "K7QX"); got != want {
			t.Errorf("containsCode(%q) = %v", text, got)
		}
	}
}
