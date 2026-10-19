package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// Wave 11 (DESIGN 5.26, migration 0015): the branch rows of a fork visit and the branch a
// visit was made in.

// 5.26 rules 3, 4 and 12: a branch row round-trips, a visit names its branch, the trace
// lists the run's branches by start, and deleting a branch row clears the visit's
// reference while the visit stays.
func TestRunBranchRoundTrip(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	now := time.Now().UTC().Truncate(time.Microsecond)
	fork := &RunNode{RunID: run.ID, NodeID: "panel", Status: NodeRunning, StartedAt: &now}
	mustDo(t, CreateRunNode(ctx, pool, fork))
	if got, err := ListRunBranches(ctx, pool, run.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("branches of a run without any = %#v %v", got, err)
	}
	b := &RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "security", StartedAt: now}
	mustDo(t, InsertRunBranch(ctx, pool, b))
	later := &RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "performance", StartedAt: now.Add(time.Millisecond)}
	mustDo(t, InsertRunBranch(ctx, pool, later))
	if b.ID == "" || b.Status != BranchRunning || b.CurrentNode != "security" {
		t.Fatalf("insert defaults: %+v", b)
	}
	// Two branches of one fork visit never share a head.
	if err := InsertRunBranch(ctx, pool, &RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "security", StartedAt: now}); err == nil {
		t.Fatal("a duplicate head was accepted")
	}

	visit := &RunNode{RunID: run.ID, NodeID: "security", Status: NodeRunning, StartedAt: &now, BranchID: &b.ID}
	mustDo(t, CreateRunNode(ctx, pool, visit))
	got, err := GetRunNode(ctx, pool, visit.ID)
	mustDo(t, err)
	if got.BranchID == nil || *got.BranchID != b.ID {
		t.Fatalf("visit's branch = %v", got.BranchID)
	}
	if f := jsonFields(t, got); string(f["branch_id"]) != `"`+b.ID+`"` {
		t.Fatalf("branch_id in JSON = %s", f["branch_id"])
	}
	if f := jsonFields(t, fork); f["branch_id"] != nil {
		t.Fatalf("a visit outside a fork carries branch_id %s", f["branch_id"])
	}

	list, err := ListRunBranches(ctx, pool, run.ID)
	mustDo(t, err)
	if len(list) != 2 || list[0].Head != "security" || list[1].Head != "performance" || list[0].FinishedAt != nil {
		t.Fatalf("ListRunBranches = %+v", list)
	}
	if f := jsonFields(t, list[0]); f["failure_reason"] != nil || f["finished_at"] != nil || string(f["status"]) != `"running"` {
		t.Fatalf("branch JSON = %v", f)
	}
	byFork, err := ListForkBranches(ctx, pool, fork.ID)
	if err != nil || len(byFork) != 2 {
		t.Fatalf("ListForkBranches = %+v %v", byFork, err)
	}
	tr, err := GetTrace(ctx, pool, run.ID)
	mustDo(t, err)
	if len(tr.Branches) != 2 || tr.Branches[1].Head != "performance" {
		t.Fatalf("trace branches = %+v", tr.Branches)
	}
	if f := jsonFields(t, tr); string(f["branches"])[0] != '[' {
		t.Fatalf("trace JSON branches = %s", f["branches"])
	}

	// The visit outlives its branch row.
	mustDo(t, execSQL(ctx, pool, `DELETE FROM run_branches WHERE id='`+b.ID+`'`))
	got, err = GetRunNode(ctx, pool, visit.ID)
	if err != nil || got.BranchID != nil {
		t.Fatalf("after the branch was deleted: %+v, %v", got, err)
	}
	// A run without branches serialises an empty list, not null.
	other := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	tr, err = GetTrace(ctx, pool, other.ID)
	mustDo(t, err)
	if f := jsonFields(t, tr); string(f["branches"]) != "[]" {
		t.Fatalf("trace JSON branches of a run without forks = %s", f["branches"])
	}
}

// Migration 0015 on a database that predates it (wave 10 as deployed): the rows already
// there read with the defaults, the branch table is empty, and a second Migrate is a no-op.
func TestMigration0015Upgrade(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const schema = "yuno_store_wave11_upgrade_test"
	base, err := Open(ctx, url)
	mustDo(t, err)
	_, err = base.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema)
	base.Close()
	mustDo(t, err)
	t.Cleanup(func() {
		if base, err := Open(context.Background(), url); err == nil {
			_, _ = base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			base.Close()
		}
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := Open(ctx, url+sep+"search_path="+schema)
	mustDo(t, err)
	defer pool.Close()
	applyBefore(t, ctx, pool, 15)
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, definition) VALUES ('wf1', 'Dev pipeline', '{"nodes":[],"edges":[]}')`,
		`INSERT INTO runs (id, workflow_id, workflow_version, workflow_snapshot, trigger, status, limits)
			VALUES ('r1', 'wf1', 1, '{}', 'manual', 'running', '{}')`,
		`INSERT INTO run_nodes (id, run_id, node_id, status) VALUES ('rn1', 'r1', 'coder', 'running')`,
	} {
		_, err = pool.Exec(ctx, stmt)
		mustDo(t, err)
	}
	log := slog.New(slog.DiscardHandler)
	mustDo(t, Migrate(ctx, pool, log))
	mustDo(t, Migrate(ctx, pool, log))
	n, err := GetRunNode(ctx, pool, "rn1")
	if err != nil || n.BranchID != nil || n.Status != NodeRunning {
		t.Fatalf("old run node = %+v %v", n, err)
	}
	if got, err := ListRunBranches(ctx, pool, "r1"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("old run's branches = %#v %v", got, err)
	}
	var def string
	err = pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = 'run_nodes_branch_idx'`, schema).Scan(&def)
	mustDo(t, err)
	if !strings.Contains(def, "branch_id IS NOT NULL") {
		t.Errorf("index run_nodes_branch_idx = %s", def)
	}
}

// 5.26 rules 8 and 9 (review H1, H2): the lost-worker bookkeeping of a claim leaves the
// named fork visit and every still-parked visit alone; the parked predicates see a parked
// visit only while it is still waiting, so a consumed park is never re-queued again.
func TestParkedBranchVisitsAreKeptAndConsumedParksAreNotListed(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	now := time.Now().UTC().Truncate(time.Microsecond)
	fork := &RunNode{RunID: run.ID, NodeID: "panel", Status: NodeRunning, StartedAt: &now}
	mustDo(t, CreateRunNode(ctx, pool, fork))
	a := &RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "a", Status: BranchWaitingApproval, CurrentNode: "a", StartedAt: now}
	b := &RunBranch{RunID: run.ID, ForkVisitID: fork.ID, ForkID: "panel", Head: "b", Status: BranchWaitingApproval, CurrentNode: "b", StartedAt: now.Add(time.Microsecond)}
	mustDo(t, InsertRunBranch(ctx, pool, a))
	mustDo(t, InsertRunBranch(ctx, pool, b))
	parkedA := &RunNode{RunID: run.ID, NodeID: "a", Status: NodeWaitingApproval, StartedAt: &now, BranchID: &a.ID, ParkedAt: &now}
	parkedB := &RunNode{RunID: run.ID, NodeID: "b", Status: NodeWaitingApproval, StartedAt: &now, BranchID: &b.ID, ParkedAt: &now}
	lost := &RunNode{RunID: run.ID, NodeID: "c", Status: NodeRunning, StartedAt: &now}
	for _, n := range []*RunNode{parkedA, parkedB, lost} {
		mustDo(t, CreateRunNode(ctx, pool, n))
	}
	qa := &Approval{RunID: run.ID, RunNodeID: parkedA.ID, Kind: ApprovalKindQuestion, NodeID: "a", Message: "a?", RequestedAt: now, ExpiresAt: now.Add(time.Hour)}
	qb := &Approval{RunID: run.ID, RunNodeID: parkedB.ID, Kind: ApprovalKindQuestion, NodeID: "b", Message: "b?", RequestedAt: now.Add(time.Second), ExpiresAt: now.Add(time.Hour)}
	mustDo(t, InsertApproval(ctx, pool, qa))
	mustDo(t, InsertApproval(ctx, pool, qb))
	// The run was released on the fork visit, then re-queued by a's answer: the claim finds
	// resume_node_visit_id still the fork visit.
	_, err := pool.Exec(ctx, `UPDATE runs SET status=$2, resume_node_visit_id=$3 WHERE id=$1`, run.ID, RunQueued, fork.ID)
	mustDo(t, err)

	n, err := FailRunningNodesExcept(ctx, pool, run.ID, fork.ID, "worker lost", now)
	if err != nil || n != 1 {
		t.Fatalf("FailRunningNodesExcept closed %d visits (%v), want the one lost visit only", n, err)
	}
	for _, id := range []string{fork.ID, parkedA.ID, parkedB.ID} {
		got, err := GetRunNode(ctx, pool, id)
		mustDo(t, err)
		if got.Status == NodeFailed {
			t.Fatalf("visit %s was marked lost: %+v", got.NodeID, got)
		}
	}
	if got, _ := GetRunNode(ctx, pool, lost.ID); got.Status != NodeFailed {
		t.Fatalf("the lost visit = %+v", got)
	}

	// b's question is answered while the run waits on the fork visit: the re-queue goes
	// through the fork visit, closes the parked visit and sets the branch running.
	_, err = pool.Exec(ctx, `UPDATE runs SET status=$2 WHERE id=$1`, run.ID, RunWaitingApproval)
	mustDo(t, err)
	_, err = AnswerApprovalAt(ctx, pool, qb.ID, "yes", "api", now)
	mustDo(t, err)
	if parked, _ := IsParkedOn(ctx, pool, run.ID, parkedB.ID); !parked {
		t.Fatal("IsParkedOn does not see a branch park through the fork visit")
	}
	listed, err := ListParkedQuestions(ctx, pool)
	if err != nil || len(listed) != 1 || listed[0].ID != qb.ID {
		t.Fatalf("ListParkedQuestions = %+v %v; want b's decided question only", listed, err)
	}
	requeued, err := RequeueParkedRun(ctx, pool, run.ID, parkedB.ID, now.Add(time.Minute))
	if err != nil || requeued.Status != RunQueued {
		t.Fatalf("RequeueParkedRun = %+v %v", requeued, err)
	}
	if got, _ := GetRunNode(ctx, pool, parkedB.ID); got.Status != NodeSucceeded || got.FinishedAt == nil {
		t.Fatalf("parked visit after the re-queue = %+v", got)
	}
	if rows, _ := ListForkBranches(ctx, pool, fork.ID); rows[1].Status != BranchRunning || rows[0].Status != BranchWaitingApproval {
		t.Fatalf("branches after the re-queue = %+v", rows)
	}
	// The consumed park no longer matches: released again, nothing re-queues it on b's old
	// answer, and a's pending question is not listed.
	_, err = pool.Exec(ctx, `UPDATE runs SET status=$2 WHERE id=$1`, run.ID, RunWaitingApproval)
	mustDo(t, err)
	if parked, _ := IsParkedOn(ctx, pool, run.ID, parkedB.ID); parked {
		t.Fatal("a consumed park still matches IsParkedOn")
	}
	if _, err := RequeueParkedRun(ctx, pool, run.ID, parkedB.ID, now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RequeueParkedRun on a consumed park = %v, want ErrNotFound", err)
	}
	if listed, _ := ListParkedQuestions(ctx, pool); len(listed) != 0 {
		t.Fatalf("ListParkedQuestions after the consumed park = %+v", listed)
	}
	if got, err := ParkedVisitOfBranch(ctx, pool, a.ID); err != nil || got.ID != parkedA.ID {
		t.Fatalf("ParkedVisitOfBranch(a) = %+v %v", got, err)
	}
	if _, err := ParkedVisitOfBranch(ctx, pool, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ParkedVisitOfBranch(b) after the re-queue = %v", err)
	}
	closed, err := CloseOpenBranches(ctx, pool, run.ID, BranchFailed, nil, now)
	if err != nil || closed != 2 {
		t.Fatalf("CloseOpenBranches = %d %v", closed, err)
	}
}
