package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// DESIGN 5.8 rule 5 with the NULL-safe approval reads: a conversation's exec approval is
// listed in the inbox with its conversation, decided from the web like any exec
// approval (resolved on the gateway), and expired by the sweeper without a run to fail.
func TestConversationApprovalDecidedOnThePlatform(t *testing.T) {
	c := newConvEnv(t, true)
	ctx := context.Background()
	c.fc.push(openclaw.EventSessionsChanged, changed(convKey, "agconv"))
	eventually(t, "subscribed", func() bool { return len(c.fc.callsFor("sessions.messages.subscribe")) == 1 })
	now := time.Now().UnixMilli()
	for _, id := range []string{"exec-web", "exec-old"} {
		c.fc.push("session.approval", map[string]any{"sessionKey": convKey, "phase": "pending", "approval": map[string]any{
			"id": id, "status": "pending", "createdAtMs": now, "expiresAtMs": now + 60_000,
			"presentation": map[string]any{"kind": "exec", "commandText": "sudo ls " + id}}})
	}
	c.sseTypes("approval.requested", 2)
	items, err := store.ListApprovalItems(ctx, c.pool, store.ApprovalPending, "", 10)
	if err != nil || len(items) != 2 || items[0].RunID != "" || items[0].ConversationID == nil || *items[0].ConversationID != c.conversation().ID {
		t.Fatalf("inbox = %+v, %v", items, err)
	}
	var web *store.Approval
	for i := range items {
		if strings.HasSuffix(items[i].Message, "exec-web") {
			web = &items[i].Approval
		}
	}
	decided, err := c.e.DecideApproval(ctx, web.ID, DecisionApproveAlways, "andre", "fine")
	if err != nil || decided.Status != store.ApprovalApproved {
		t.Fatalf("decide = %+v %v", decided, err)
	}
	if res := c.fc.callsFor("exec.approval.resolve"); len(res) != 1 || !strings.Contains(string(res[0]), `"allow-always"`) {
		t.Fatalf("exec.approval.resolve = %s", res)
	}
	got, _ := store.GetAgent(ctx, c.pool, c.agent.ID)
	if len(got.ExecPolicy.Allowlist) != 1 || got.ExecPolicy.Allowlist[0] != "sudo ls exec-web" {
		t.Fatalf("allow-always extends the conversation agent's allowlist: %v", got.ExecPolicy.Allowlist)
	}
	c.clock.Advance(2 * time.Minute)
	c.e.sweepApprovals(ctx)
	old, err := store.GetConversationApprovalByExternalID(ctx, c.pool, "exec-old")
	if err != nil || old.Status != store.ApprovalExpired {
		t.Fatalf("expired = %+v %v", old, err)
	}
}
