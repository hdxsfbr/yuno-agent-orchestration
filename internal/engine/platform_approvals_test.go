package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"yuno/internal/store"
)

// Platform control A6: a platform approval belongs to no run. A decision only records it
// (the agent's repeat of the call reads it), and the sweeper expires one past its time
// without touching any run.
func TestPlatformApprovalDecisionAndExpiry(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	now := env.clock.Now().UTC()
	insert := func(expires time.Time) *store.Approval {
		t.Helper()
		a := &store.Approval{Kind: store.ApprovalKindPlatform, Message: "Chief of Staff asks to run deleteSchedule", Status: store.ApprovalPending,
			RequestedAt: now, ExpiresAt: expires, Binding: json.RawMessage(`{"agent_id":"a1","operation_id":"deleteSchedule",` +
				`"path":"/api/schedules/s1","body_sha256":"e3b0","idempotency_key":"` + store.NewID() + `"}`)}
		if err := store.InsertApproval(ctx, env.pool, a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	approve := insert(now.Add(time.Hour))
	got, err := env.e.DecideApproval(ctx, approve.ID, DecisionApprove, "api", "fine")
	if err != nil || got.Status != store.ApprovalApproved || got.RunID != "" || deref(got.DecidedBy) != "api" || len(got.Binding) == 0 || got.UsedAt != nil {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	reject := insert(now.Add(time.Hour))
	if got, err := env.e.DecideApproval(ctx, reject.ID, DecisionReject, "chat:telegram", "not now"); err != nil || got.Status != store.ApprovalRejected {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	if _, err := env.e.DecideApproval(ctx, approve.ID, DecisionReject, "api", ""); err != ErrNotPending {
		t.Fatalf("decided twice: %v", err)
	}

	stale := insert(now.Add(time.Minute))
	env.clock.Advance(2 * time.Minute)
	env.e.sweepApprovals(ctx)
	got, err = store.GetApproval(ctx, env.pool, stale.ID)
	if err != nil || got.Status != store.ApprovalExpired || deref(got.Reason) != "approval expired" || got.RunID != "" {
		t.Fatalf("swept = %+v, %v", got, err)
	}
	if got, _ := store.GetApproval(ctx, env.pool, approve.ID); got.Status != store.ApprovalApproved {
		t.Fatalf("the sweep touched a decided approval: %+v", got)
	}
}
