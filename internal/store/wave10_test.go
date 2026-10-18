package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Wave 10 (DESIGN 5.24, 5.25, migration 0014): the question columns of approvals and run
// nodes, and the artifact table.

// jsonFields decodes v's JSON into its top-level members.
func jsonFields(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	mustDo(t, err)
	var out map[string]json.RawMessage
	mustDo(t, json.Unmarshal(raw, &out))
	return out
}

// 5.24 rules 2, 3 and 11: a question carries its options and, once answered, its answer
// through the insert and every read of the row; the other kinds read without either, and
// the JSON has them only when set.
func TestQuestionApprovalRoundTrip(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	node := &RunNode{RunID: run.ID, NodeID: "coder", Status: NodeWaitingApproval}
	mustDo(t, CreateRunNode(ctx, pool, node))
	now := time.Now().UTC().Truncate(time.Microsecond)
	newApproval := func(kind, message string, options json.RawMessage) *Approval {
		t.Helper()
		a := &Approval{RunID: run.ID, RunNodeID: node.ID, Kind: kind, NodeID: "coder", Message: message, Options: options,
			RequestedAt: now, ExpiresAt: now.Add(time.Hour)}
		mustDo(t, InsertApproval(ctx, pool, a))
		return a
	}

	withOptions := newApproval(ApprovalKindQuestion, "Which greeting language?", json.RawMessage(`["English","Portuguese"]`))
	freeText := newApproval(ApprovalKindQuestion, "What is the staging host?", nil)
	emptyOptions := newApproval(ApprovalKindQuestion, "Anything else?", json.RawMessage{})
	tool := newApproval(ApprovalKindTool, "deploy_release", nil)
	if ApprovalKindQuestion != "question" {
		t.Fatalf("ApprovalKindQuestion = %q", ApprovalKindQuestion)
	}

	got, err := GetApproval(ctx, pool, withOptions.ID)
	mustDo(t, err)
	var options []string
	mustDo(t, json.Unmarshal(got.Options, &options))
	if got.Kind != ApprovalKindQuestion || !slices.Equal(options, []string{"English", "Portuguese"}) || got.Answer != nil {
		t.Fatalf("question = %+v (options %v)", got, options)
	}
	fields := jsonFields(t, got)
	if string(fields["options"]) != `["English","Portuguese"]` {
		t.Errorf("JSON options = %s", fields["options"])
	}
	if _, ok := fields["answer"]; ok {
		t.Errorf("JSON has an answer before there is one: %s", fields["answer"])
	}

	// Free text, an empty document and a tool approval store NULL and read back without
	// options, whatever the read.
	for name, a := range map[string]*Approval{"free text": freeText, "empty options": emptyOptions, "tool": tool} {
		got, err := GetApproval(ctx, pool, a.ID)
		mustDo(t, err)
		var isNull bool
		mustDo(t, pool.QueryRow(ctx, `SELECT options IS NULL AND answer IS NULL FROM approvals WHERE id=$1`, a.ID).Scan(&isNull))
		if !isNull || len(got.Options) != 0 || got.Answer != nil {
			t.Errorf("%s: options %q, answer %v, columns null %v", name, got.Options, got.Answer, isNull)
		}
		if f := jsonFields(t, got); f["options"] != nil || f["answer"] != nil {
			t.Errorf("%s: JSON has options or an answer: %v", name, f)
		}
	}

	// The answer is recorded with the decision and read back by every query that returns the row.
	mustDo(t, execSQL(ctx, pool, `UPDATE approvals SET status='approved', answer='Portuguese', decided_by='api', decided_at=now() WHERE id='`+withOptions.ID+`'`))
	got, err = GetApproval(ctx, pool, withOptions.ID)
	mustDo(t, err)
	if got.Status != ApprovalApproved || got.Answer == nil || *got.Answer != "Portuguese" || len(got.Options) == 0 {
		t.Fatalf("answered question = %+v", got)
	}
	if f := jsonFields(t, got); string(f["answer"]) != `"Portuguese"` {
		t.Errorf("JSON answer = %s", f["answer"])
	}
	listed, err := ListApprovals(ctx, pool, ApprovalApproved, run.ID, 10)
	if err != nil || len(listed) != 1 || listed[0].Answer == nil || *listed[0].Answer != "Portuguese" || listed[0].ID != withOptions.ID {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
	pending, err := ListPendingApprovals(ctx, pool, run.ID)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %d, %v; want the other three", len(pending), err)
	}
	for _, p := range pending {
		if p.ID == freeText.ID && (len(p.Options) != 0 || p.Answer != nil) {
			t.Errorf("pending free-text question = %+v", p)
		}
	}
	decided, err := DecideApprovalAt(ctx, pool, freeText.ID, ApprovalRejected, "api", "no", now)
	if err != nil || decided.Kind != ApprovalKindQuestion || decided.Answer != nil {
		t.Fatalf("declined question = %+v, %v", decided, err)
	}
	items, err := ListApprovalItems(ctx, pool, ApprovalApproved, run.ID, 10)
	if err != nil || len(items) != 1 || items[0].Answer == nil {
		t.Fatalf("inbox items = %+v, %v", items, err)
	}
}

// 5.24 rule 5: a continuation visit names its question and a parked visit when it parked,
// through the insert and every read of the node; both read nil on any other visit, the
// question column goes null with its approval, and the contract has question_id only.
func TestRunNodeQuestionAndParkedRoundTrip(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	now := time.Now().UTC().Truncate(time.Microsecond)

	parked := &RunNode{RunID: run.ID, NodeID: "coder", Visit: 1, Status: NodeWaitingApproval, StartedAt: &now, ParkedAt: &now}
	mustDo(t, CreateRunNode(ctx, pool, parked))
	q := &Approval{RunID: run.ID, RunNodeID: parked.ID, Kind: ApprovalKindQuestion, NodeID: "coder", Message: "Which one?",
		RequestedAt: now, ExpiresAt: now.Add(time.Hour)}
	mustDo(t, InsertApproval(ctx, pool, q))
	later := now.Add(time.Minute)
	cont := &RunNode{RunID: run.ID, NodeID: "coder", Visit: 2, Status: NodeRunning, StartedAt: &later, QuestionID: &q.ID}
	mustDo(t, CreateRunNode(ctx, pool, cont))

	got, err := GetRunNode(ctx, pool, cont.ID)
	mustDo(t, err)
	if got.QuestionID == nil || *got.QuestionID != q.ID || got.ParkedAt != nil {
		t.Fatalf("continuation visit = %+v", got)
	}
	got, err = GetRunNode(ctx, pool, parked.ID)
	mustDo(t, err)
	if got.QuestionID != nil || got.ParkedAt == nil || !got.ParkedAt.Equal(now) {
		t.Fatalf("parked visit = %+v", got)
	}
	nodes, err := ListRunNodes(ctx, pool, run.ID)
	if err != nil || len(nodes) != 2 || nodes[0].ParkedAt == nil || nodes[1].QuestionID == nil {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	latest, err := LatestNodeVisit(ctx, pool, run.ID, "coder")
	if err != nil || latest.Visit != 2 || latest.QuestionID == nil || *latest.QuestionID != q.ID {
		t.Fatalf("latest visit = %+v, %v", latest, err)
	}
	finished, err := FinishRunNode(ctx, pool, parked.ID, NodeSucceeded, nil, later)
	if err != nil || finished.ParkedAt == nil || finished.Status != NodeSucceeded {
		t.Fatalf("finished parked visit = %+v, %v", finished, err)
	}

	f := jsonFields(t, nodes[1])
	if string(f["question_id"]) != `"`+q.ID+`"` {
		t.Errorf("JSON question_id = %s", f["question_id"])
	}
	if _, ok := f["parked_at"]; ok {
		t.Errorf("parked_at is in the contract: %v", f)
	}
	if f := jsonFields(t, nodes[0]); f["question_id"] != nil || f["parked_at"] != nil {
		t.Errorf("a visit that is not a continuation has question_id or parked_at in its JSON: %v", f)
	}

	// The visit outlives its question: deleting the approval clears the reference.
	mustDo(t, execSQL(ctx, pool, `DELETE FROM approvals WHERE id='`+q.ID+`'`))
	got, err = GetRunNode(ctx, pool, cont.ID)
	if err != nil || got.QuestionID != nil {
		t.Fatalf("after the question was deleted: %+v, %v", got, err)
	}
}

// Migration 0014 on a database that predates it (wave 9 as deployed): the rows already there
// read with the defaults, the question index is partial as 5.24 rule 2 needs it, the
// artifact table is empty with its unique path, and a second Migrate is a no-op.
func TestMigration0014Upgrade(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const schema = "yuno_store_wave10_upgrade_test"
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
	applyBefore(t, ctx, pool, 14)

	// Wave 9 data: a run waiting at a node with a tool approval.
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, definition) VALUES ('wf1', 'Dev pipeline', '{"nodes":[],"edges":[]}')`,
		`INSERT INTO runs (id, workflow_id, workflow_version, workflow_snapshot, trigger, status, limits)
			VALUES ('r1', 'wf1', 1, '{}', 'manual', 'waiting_approval', '{}')`,
		`INSERT INTO run_nodes (id, run_id, node_id, status) VALUES ('rn1', 'r1', 'coder', 'waiting_approval')`,
		`INSERT INTO approvals (id, run_id, run_node_id, kind, node_id, message, status, expires_at)
			VALUES ('a1', 'r1', 'rn1', 'tool', 'coder', 'deploy', 'pending', now() + interval '1 hour')`,
	} {
		_, err = pool.Exec(ctx, stmt)
		mustDo(t, err)
	}

	log := slog.New(slog.DiscardHandler)
	mustDo(t, Migrate(ctx, pool, log))
	mustDo(t, Migrate(ctx, pool, log))

	a, err := GetApproval(ctx, pool, "a1")
	if err != nil || a.Kind != ApprovalKindTool || len(a.Options) != 0 || a.Answer != nil || a.Status != ApprovalPending {
		t.Fatalf("old approval = %+v %v", a, err)
	}
	n, err := GetRunNode(ctx, pool, "rn1")
	if err != nil || n.QuestionID != nil || n.ParkedAt != nil || n.Status != NodeWaitingApproval {
		t.Fatalf("old run node = %+v %v", n, err)
	}
	if got, err := ListRunArtifacts(ctx, pool, "r1"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("old run's artifacts = %#v %v", got, err)
	}

	var def string
	err = pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = 'approvals_question_step_idx'`, schema).Scan(&def)
	mustDo(t, err)
	for _, part := range []string{"(run_id, node_id)", "kind = 'question'"} {
		if !strings.Contains(def, part) {
			t.Errorf("index approvals_question_step_idx = %s, missing %q", def, part)
		}
	}
	// The step's question count (5.24 rule 2) is served by that index.
	plan := explainNoSeqScan(t, ctx, pool, `SELECT count(*) FROM approvals WHERE run_id = $1 AND node_id = $2 AND kind = 'question'`, "r1", "coder")
	if !strings.Contains(plan, "approvals_question_step_idx") {
		t.Errorf("the step's question count does not use the index:\n%s", plan)
	}

	// The new table accepts a row and refuses a second at the same path of the run.
	insert := `INSERT INTO run_artifacts (id, run_id, node_id, visit, path, size, sha256, content_type, created_at, updated_at)
		VALUES ($1, 'r1', 'coder', 1, 'report.md', 10, $2, 'text/markdown', now(), now())`
	_, err = pool.Exec(ctx, insert, "art1", sha1Hex+sha1Hex[:24])
	mustDo(t, err)
	if _, err = pool.Exec(ctx, insert, "art2", sha1Hex+sha1Hex[:24]); err == nil {
		t.Error("two artifacts at one path of a run")
	}
	got, err := ListRunArtifacts(ctx, pool, "r1")
	if err != nil || len(got) != 1 || got[0].Generated {
		t.Fatalf("artifacts = %+v %v; want one, not generated", got, err)
	}
}
