package store

import (
	"context"
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
