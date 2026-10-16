package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Migration 0008: agents created before it get a chat token from the backfill; new
// agents get one at creation; tokens are unique and resolve only for live agents.
func TestChatTokens(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a := &Agent{Name: "minted", Model: "claude-opus-5-5"}
	mustDo(t, CreateAgent(ctx, pool, a))
	if !strings.HasPrefix(a.ChatToken, "c_") || len(a.ChatToken) != 34 {
		t.Fatalf("minted = %q", a.ChatToken)
	}
	old := &Agent{Name: "predates 0008", Model: "claude-opus-5-5"}
	mustDo(t, CreateAgent(ctx, pool, old))
	_, err := pool.Exec(ctx, `UPDATE agents SET chat_token = NULL WHERE id = $1`, old.ID)
	mustDo(t, err)
	if got, err := GetAgent(ctx, pool, old.ID); err != nil || got.ChatToken != "" {
		t.Fatalf("NULL token reads as %q, %v", got.ChatToken, err)
	}
	n, err := BackfillChatTokens(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("backfill = %d %v", n, err)
	}
	got, err := GetAgent(ctx, pool, old.ID)
	mustDo(t, err)
	if !strings.HasPrefix(got.ChatToken, "c_") || got.ChatToken == a.ChatToken {
		t.Fatalf("backfilled = %q", got.ChatToken)
	}
	if n, _ := BackfillChatTokens(ctx, pool); n != 0 {
		t.Fatalf("second backfill filled %d", n)
	}
	if byToken, err := GetAgentByChatToken(ctx, pool, got.ChatToken); err != nil || byToken.ID != old.ID {
		t.Fatalf("by token = %+v %v", byToken, err)
	}
	if _, err := GetAgentByChatToken(ctx, pool, ""); err != ErrNotFound {
		t.Fatalf("empty token: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE agents SET chat_token = $2 WHERE id = $1`, old.ID, a.ChatToken)
	if err == nil {
		t.Fatal("two agents share a chat token")
	}
	mustDo(t, SetAgentChatToken(ctx, pool, a.ID, "c_zyxwvutsrqponmlkjihgfedcba765432"))
	mustDo(t, DeleteAgent(ctx, pool, a.ID))
	if _, err := GetAgentByChatToken(ctx, pool, "c_zyxwvutsrqponmlkjihgfedcba765432"); err != ErrNotFound {
		t.Fatalf("deleted agent's token: %v", err)
	}
	// Updating an agent keeps its token.
	got.Role = "edited"
	mustDo(t, UpdateAgent(ctx, pool, got))
	if again, _ := GetAgent(ctx, pool, old.ID); again.ChatToken != got.ChatToken {
		t.Fatal("update dropped the chat token")
	}
}

// DESIGN 5.15 rule 3: a chat-time tool call has no run; it belongs to a conversation
// or to nothing, and the conversation lists its calls oldest first.
func TestChatToolCalls(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	a := &Agent{Name: "chief", Model: "claude-opus-5-5"}
	mustDo(t, CreateAgent(ctx, pool, a))
	if _, err := LatestConversationForAgent(ctx, pool, a.ID); err != ErrNotFound {
		t.Fatalf("no conversation: %v", err)
	}
	c1, err := UpsertConversation(ctx, pool, &Conversation{Channel: ChannelTelegram, ExternalChatID: "1", AgentID: a.ID, SessionKey: "agent:x:telegram:direct:1"})
	mustDo(t, err)
	c2, err := UpsertConversation(ctx, pool, &Conversation{Channel: ChannelTelegram, ExternalChatID: "2", AgentID: a.ID, SessionKey: "agent:x:telegram:direct:2"})
	mustDo(t, err)
	if latest, err := LatestConversationForAgent(ctx, pool, a.ID); err != nil || latest.ID != c2.ID {
		t.Fatalf("latest without messages = %+v %v", latest, err)
	}
	_, err = InsertConversationMessage(ctx, pool, &ConversationMessage{ConversationID: c1.ID, Direction: DirectionIn, ExternalMessageID: "m", Text: "hi"})
	mustDo(t, err)
	if latest, err := LatestConversationForAgent(ctx, pool, a.ID); err != nil || latest.ID != c1.ID {
		t.Fatalf("latest = %+v %v", latest, err)
	}

	base := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"list_agents", "delegate_task", "run_status"} {
		at := base.Add(time.Duration(i) * time.Second)
		conv := c1.ID
		mustDo(t, InsertToolCall(ctx, pool, &ToolCall{ToolUseID: "mcp-" + name, ToolName: name, Input: json.RawMessage(`{"chat_token":"[redacted]"}`),
			Status: ToolExecuted, StartedAt: &at, FinishedAt: &at, ConversationID: &conv}))
	}
	orphan := &ToolCall{ToolUseID: "mcp-orphan", ToolName: "list_workflows", Status: ToolExecuted}
	mustDo(t, InsertToolCall(ctx, pool, orphan))
	got, err := GetToolCall(ctx, pool, orphan.ID)
	if err != nil || got.RunID != "" || got.RunNodeID != "" || got.ConversationID != nil {
		t.Fatalf("orphan = %+v %v", got, err)
	}
	calls, err := ListToolCallsByConversation(ctx, pool, c1.ID, 2)
	mustDo(t, err)
	if len(calls) != 2 || calls[0].ToolName != "delegate_task" || calls[1].ToolName != "run_status" || calls[0].RunID != "" {
		t.Fatalf("newest two, oldest first = %+v", calls)
	}
	if calls, _ := ListToolCallsByConversation(ctx, pool, c2.ID, 0); len(calls) != 0 {
		t.Fatalf("other conversation = %+v", calls)
	}
}

// DESIGN 5.15 rules 9 and 10: a report is claimed once, only for a terminal channel run
// with a conversation ref, and an approval notice once per pending approval.
func TestReportClaims(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "w", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	newRun := func(trigger, ref, status string) *Run {
		r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: trigger, Status: status}
		if ref != "" {
			r.TriggerRef = &ref
		}
		mustDo(t, CreateRun(ctx, pool, r))
		return r
	}
	now := time.Now().UTC()
	ref := ConversationRef("conv-1")
	done := newRun(TriggerChannel, ref, RunSucceeded)
	running := newRun(TriggerChannel, ref, RunRunning)
	noRef := newRun(TriggerChannel, "", RunFailed)
	manual := newRun(TriggerManual, ref, RunSucceeded)
	if ids, err := ListUnreportedChannelRuns(ctx, pool, 10); err != nil || len(ids) != 1 || ids[0] != done.ID {
		t.Fatalf("unreported = %v %v", ids, err)
	}
	if r, err := ClaimRunReport(ctx, pool, done.ID, now); err != nil || r.ReportedAt == nil {
		t.Fatalf("claim = %+v %v", r, err)
	}
	for name, id := range map[string]string{"again": done.ID, "running": running.ID, "no ref": noRef.ID, "manual": manual.ID} {
		if _, err := ClaimRunReport(ctx, pool, id, now); err != ErrNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if conv, ok := ConversationOfRun(done); !ok || conv != "conv-1" {
		t.Fatalf("conversation of run = %q %v", conv, ok)
	}
	if _, ok := ConversationOfRun(noRef); ok {
		t.Fatal("a run without a ref has a conversation")
	}

	node := &RunNode{RunID: running.ID, NodeID: "n", Status: NodeWaitingApproval}
	mustDo(t, CreateRunNode(ctx, pool, node))
	var ids []string
	for i := 0; i < 2; i++ {
		a := &Approval{RunID: running.ID, RunNodeID: node.ID, Kind: ApprovalKindTool, NodeID: "n", Message: "m",
			RequestedAt: now.Add(time.Duration(i) * time.Second), ExpiresAt: now.Add(time.Hour)}
		mustDo(t, InsertApproval(ctx, pool, a))
		ids = append(ids, a.ID)
	}
	if runs, err := ListRunsWithUnreportedApprovals(ctx, pool); err != nil || len(runs) != 1 || runs[0] != running.ID {
		t.Fatalf("runs with unreported approvals = %v %v", runs, err)
	}
	claimed, err := ClaimApprovalReports(ctx, pool, running.ID, now)
	if err != nil || len(claimed) != 2 || claimed[0].ID != ids[0] || claimed[1].ReportedAt == nil {
		t.Fatalf("claimed = %+v %v", claimed, err)
	}
	if again, _ := ClaimApprovalReports(ctx, pool, running.ID, now); len(again) != 0 {
		t.Fatalf("claimed twice: %+v", again)
	}
	if a, _ := GetApproval(ctx, pool, ids[0]); a.ReportedAt == nil {
		t.Fatal("reported_at not read back")
	}
}

// DESIGN 5.15 rule 13: a workflow records the template it was made from.
func TestWorkflowFromTemplate(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	if ok, err := WorkflowFromTemplateExists(ctx, pool, "dev_pipeline"); err != nil || ok {
		t.Fatalf("empty: %v %v", ok, err)
	}
	key := "dev_pipeline"
	wf := &Workflow{Name: "Dev pipeline", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`), FromTemplate: &key}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	if got, _ := GetWorkflow(ctx, pool, wf.ID); got.FromTemplate == nil || *got.FromTemplate != key {
		t.Fatalf("from_template = %v", got.FromTemplate)
	}
	if ok, _ := WorkflowFromTemplateExists(ctx, pool, key); !ok {
		t.Fatal("instance not found")
	}
	mustDo(t, DeleteWorkflow(ctx, pool, wf.ID))
	if ok, _ := WorkflowFromTemplateExists(ctx, pool, key); ok {
		t.Fatal("a deleted instance counts")
	}
}
