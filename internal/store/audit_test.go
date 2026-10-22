package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// DESIGN 5.31 rule 3: a key is claimed once; while it lives a second claim finds the row; a
// key 24 hours old counts as unused.
func TestClaimIdempotencyKey(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	k := IdempotencyKey{Actor: "operator", Method: "POST", Path: "/api/runs", Key: "claim-" + NewID(), BodySHA256: "aa"}
	claimed, held, err := ClaimIdempotencyKey(ctx, pool, k, t0)
	if err != nil || !claimed || held != nil {
		t.Fatalf("first claim = %v %v %v", claimed, held, err)
	}
	k.BodySHA256 = "bb"
	claimed, held, err = ClaimIdempotencyKey(ctx, pool, k, t0.Add(time.Hour))
	if err != nil || claimed || held == nil || held.BodySHA256 != "aa" || held.State != IdempotencyRunning {
		t.Fatalf("second claim = %v %+v %v", claimed, held, err)
	}
	k.BodySHA256, k.Status, k.Body, k.ContentType = "aa", 201, []byte(`{"id":"r1"}`), "application/json"
	mustDo(t, CompleteIdempotencyKey(ctx, pool, k))
	got, err := GetIdempotencyKey(ctx, pool, k.Actor, k.Method, k.Path, k.Key)
	if err != nil || got.State != IdempotencyDone || got.Status != 201 || string(got.Body) != `{"id":"r1"}` ||
		got.ContentType != "application/json" || !got.ExpiresAt.Equal(t0.Add(IdempotencyTTL)) {
		t.Fatalf("completed = %+v, %v", got, err)
	}
	// Another actor's key of the same name is its own.
	other := k
	other.Actor = "agent:a1"
	if claimed, _, err := ClaimIdempotencyKey(ctx, pool, other, t0.Add(time.Hour)); err != nil || !claimed {
		t.Fatalf("another actor's claim = %v %v", claimed, err)
	}
	k.BodySHA256 = "cc"
	claimed, _, err = ClaimIdempotencyKey(ctx, pool, k, t0.Add(IdempotencyTTL))
	if err != nil || !claimed {
		t.Fatalf("claim of an expired key = %v %v", claimed, err)
	}
	mustDo(t, ReleaseIdempotencyKey(ctx, pool, k.Actor, k.Method, k.Path, k.Key))
	if _, err := GetIdempotencyKey(ctx, pool, k.Actor, k.Method, k.Path, k.Key); err != ErrNotFound {
		t.Fatalf("released key: %v", err)
	}
}

// DESIGN 5.31 rule 3: the sweep deletes keys 24 hours after first use, and claims still
// running an hour after they were made.
func TestSweepIdempotencyKeys(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	done := IdempotencyKey{Actor: "operator", Method: "PUT", Path: "/api/agents/a1", Key: "done-" + NewID(), BodySHA256: "aa", Status: 200}
	running := IdempotencyKey{Actor: "operator", Method: "PUT", Path: "/api/agents/a1", Key: "running-" + NewID(), BodySHA256: "aa"}
	for _, k := range []IdempotencyKey{done, running} {
		if claimed, _, err := ClaimIdempotencyKey(ctx, pool, k, t0); err != nil || !claimed {
			t.Fatalf("claim: %v %v", claimed, err)
		}
	}
	mustDo(t, CompleteIdempotencyKey(ctx, pool, done))
	exists := func(k IdempotencyKey) bool {
		_, err := GetIdempotencyKey(ctx, pool, k.Actor, k.Method, k.Path, k.Key)
		return err == nil
	}
	for _, step := range []struct {
		at                 time.Duration
		wantDone, wantRuns bool
	}{
		{59 * time.Minute, true, true},
		{IdempotencyAbandonedAfter, true, false},
		{IdempotencyTTL - time.Second, true, false},
		{IdempotencyTTL, false, false},
	} {
		if _, err := SweepIdempotencyKeys(ctx, pool, t0.Add(step.at)); err != nil {
			t.Fatal(err)
		}
		if exists(done) != step.wantDone || exists(running) != step.wantRuns {
			t.Fatalf("after a sweep at +%s: done %v, running %v", step.at, exists(done), exists(running))
		}
	}
}

// DESIGN 5.31 rules 1 and 5: a row is inserted started, completed, and listed newest first.
func TestAuditEventInsertCompleteList(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	target := "/api/agents/" + NewID()
	var ids []string
	for i := range 3 {
		e := &AuditEvent{ActorKind: "operator", Via: "api", Method: "PUT", Route: "/api/agents/{id}", Path: target, Target: target,
			Request: json.RawMessage(`{"name":"x"}`)}
		mustDo(t, InsertAuditEvent(ctx, pool, e))
		got, err := GetAuditEvent(ctx, pool, e.ID)
		if err != nil || got.Outcome != AuditStarted || got.Status != nil || string(got.Request) != `{"name": "x"}` || got.Before != nil {
			t.Fatalf("started row = %+v, %v", got, err)
		}
		if i < 2 {
			status, d := 200, int64(5)
			e.Status, e.Outcome, e.After, e.DurationMs, e.Evidence = &status, AuditOK, json.RawMessage(`{"name":"y"}`), &d, json.RawMessage(`{"k":1}`)
			mustDo(t, CompleteAuditEvent(ctx, pool, e))
		}
		ids = append(ids, e.ID)
	}
	all, err := ListAuditEvents(ctx, pool, AuditFilter{Target: target, Limit: 10})
	if err != nil || len(all) != 3 || all[0].ID != ids[2] || all[2].ID != ids[0] {
		t.Fatalf("list = %v, %v", all, err)
	}
	if all[1].Outcome != AuditOK || *all[1].Status != 200 || string(all[1].Evidence) != `{"k": 1}` || *all[1].DurationMs != 5 {
		t.Fatalf("completed row = %+v", all[1])
	}
	ok, err := ListAuditEvents(ctx, pool, AuditFilter{Target: target, Outcome: AuditOK, Limit: 10, AfterAt: &all[1].At, AfterID: all[1].ID})
	if err != nil || len(ok) != 1 || ok[0].ID != ids[0] {
		t.Fatalf("after the second row, ok only = %v, %v", ok, err)
	}
	if err := CompleteAuditEvent(ctx, pool, &AuditEvent{ID: "no-such-row", Outcome: AuditOK}); err != ErrNotFound {
		t.Fatalf("completing a missing row: %v", err)
	}
}
