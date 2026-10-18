package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The intake's queries (DESIGN 5.23 rules 2, 3 and 5), over the helpers of wave9_test.go.

func issueRef(ref string) func(*Run) {
	return func(r *Run) { r.Trigger, r.TriggerRef = TriggerIssue, &ref }
}

func TestIssueTriggerRef(t *testing.T) {
	ref := IssueTriggerRef("0199-abc", 12)
	if ref != "0199-abc#12" {
		t.Fatalf("ref = %q", ref)
	}
	if pid, n, ok := ParseIssueTriggerRef(ref); !ok || pid != "0199-abc" || n != 12 {
		t.Fatalf("parse = %q %d %v", pid, n, ok)
	}
	for _, bad := range []string{"", "#12", "abc", "abc#", "abc#x", "abc#0", "abc#-3", "abc#1.5", "conversation:abc"} {
		if _, _, ok := ParseIssueTriggerRef(bad); ok {
			t.Errorf("ParseIssueTriggerRef(%q) accepted", bad)
		}
	}
}

// w9Finish stamps the finish time of a run, which the closer's list orders by.
func w9Finish(t *testing.T, pool *pgxpool.Pool, id string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE runs SET finished_at = $2 WHERE id = $1`, id, at)
	mustDo(t, err)
}

// T5.23.6 (the closer's list): terminal issue runs whose outcome is not posted, newest
// finished_at first (review M1), with the project id and the number parsed; reported ones,
// live ones, other triggers and a reference nobody can parse are left out; the limit and
// the project filter apply.
func TestListUnreportedIssueRuns(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	base := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)

	// The four runs of pa finish in creation order, the run of pb last, and one more run of
	// pa without a finish time (written by hand) after them all: the newest finished comes
	// first and a run with no finish time comes last.
	var want []*Run
	for i, status := range terminalStatuses {
		r := w9MustRun(t, pool, wf, issueRef(IssueTriggerRef("pa", int64(10+i))))
		w9SetStatus(t, pool, r.ID, status)
		w9Finish(t, pool, r.ID, base.Add(time.Duration(i)*time.Hour))
		want = append(want, r)
	}
	other := w9MustRun(t, pool, wf, issueRef(IssueTriggerRef("pb", 5)))
	w9SetStatus(t, pool, other.ID, RunFailed)
	w9Finish(t, pool, other.ID, base.Add(10*time.Hour))
	unstamped := w9MustRun(t, pool, wf, issueRef(IssueTriggerRef("pa", 30)))
	w9SetStatus(t, pool, unstamped.ID, RunFailed)

	// Left out.
	live := w9MustRun(t, pool, wf, issueRef("pa#20"))
	w9SetStatus(t, pool, live.ID, RunRunning)
	reported := w9MustRun(t, pool, wf, issueRef("pa#21"))
	w9SetStatus(t, pool, reported.ID, RunSucceeded)
	mustDo(t, SetRunReportedAt(ctx, pool, reported.ID, time.Now()))
	manual := w9MustRun(t, pool, wf, func(r *Run) { ref := "pa#22"; r.Trigger, r.TriggerRef = TriggerManual, &ref })
	w9SetStatus(t, pool, manual.ID, RunFailed)
	garbled := w9MustRun(t, pool, wf, issueRef("not a reference"))
	w9SetStatus(t, pool, garbled.ID, RunFailed)

	got, err := ListUnreportedIssueRuns(ctx, pool, 50)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]IssueRun{}
	for _, r := range got {
		ids[r.ID] = r
	}
	for _, r := range []*Run{live, reported, manual, garbled} {
		if _, ok := ids[r.ID]; ok {
			t.Errorf("run %s (%s) is listed", r.ID, *r.TriggerRef)
		}
	}
	if len(got) != 6 {
		t.Fatalf("%d runs listed, want the 4 and the unstamped one of pa and the 1 of pb: %+v", len(got), got)
	}
	// Newest finished first: pb's, then pa's from the last to the first, then the run
	// without a finish time.
	order := []*Run{other, want[3], want[2], want[1], want[0], unstamped}
	for i, w := range order {
		if got[i].ID != w.ID {
			t.Errorf("run %d is %s (%s), want %s (%s)", i, got[i].ID, *got[i].TriggerRef, w.ID, *w.TriggerRef)
		}
	}
	for i, w := range want {
		g := ids[w.ID]
		if g.ProjectID != "pa" || g.Number != int64(10+i) || g.Status != terminalStatuses[i] {
			t.Errorf("run %d = %s %s#%d %s, want %s pa#%d", i, g.ID, g.ProjectID, g.Number, g.Status, w.ID, 10+i)
		}
	}
	if g := got[0]; g.ProjectID != "pb" || g.Number != 5 {
		t.Errorf("first run = %+v", g)
	}

	// The limit keeps the newest: the oldest runs wait, they do not crowd the newer out.
	limited, err := ListUnreportedIssueRuns(ctx, pool, 2)
	if err != nil || len(limited) != 2 || limited[0].ID != other.ID || limited[1].ID != want[3].ID {
		t.Fatalf("limit 2 = %+v %v", limited, err)
	}
	mine, err := ListUnreportedIssueRunsOfProject(ctx, pool, "pb", 50)
	if err != nil || len(mine) != 1 || mine[0].ID != other.ID {
		t.Fatalf("project pb = %+v %v", mine, err)
	}
	pa, err := ListUnreportedIssueRunsOfProject(ctx, pool, "pa", 50)
	if err != nil || len(pa) != 5 || pa[0].ID != want[3].ID || pa[4].ID != unstamped.ID {
		t.Fatalf("project pa = %+v %v", pa, err)
	}
	// "p" is the prefix of the projects pa and pb, not a project of its own.
	if none, err := ListUnreportedIssueRunsOfProject(ctx, pool, "p", 50); err != nil || len(none) != 0 {
		t.Fatalf("project p = %+v %v", none, err)
	}
}

// DESIGN 5.23 rule 3 (review H1): an issue is taken by the live run of its reference, or
// by the newest run that ended with its outcome not posted yet; a run whose outcome is
// posted does not count, so re-labelling an issue starts a new one (rule 6).
func TestTakenIssueRun(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	ref, otherRef := "proj-1#7", "proj-1#8"
	base := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)

	if _, err := TakenIssueRun(ctx, pool, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no run: %v, want ErrNotFound", err)
	}
	// Two ended runs of the reference, the older one posted: the unposted one counts.
	older := w9MustRun(t, pool, wf, issueRef(ref))
	w9SetStatus(t, pool, older.ID, RunFailed)
	w9Finish(t, pool, older.ID, base)
	mustDo(t, SetRunReportedAt(ctx, pool, older.ID, base.Add(time.Minute)))
	if _, err := TakenIssueRun(ctx, pool, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a run whose outcome is posted: %v, want ErrNotFound", err)
	}
	for _, status := range terminalStatuses {
		ended := w9MustRun(t, pool, wf, issueRef(ref))
		w9SetStatus(t, pool, ended.ID, status)
		w9Finish(t, pool, ended.ID, base.Add(time.Hour))
		got, err := TakenIssueRun(ctx, pool, ref)
		if err != nil || got.ID != ended.ID {
			t.Fatalf("%s run, outcome not posted: %+v %v, want %s", status, got, err, ended.ID)
		}
		if _, err := TakenIssueRun(ctx, pool, otherRef); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run, other issue: %v, want ErrNotFound", status, err)
		}
		mustDo(t, SetRunReportedAt(ctx, pool, ended.ID, base.Add(2*time.Hour)))
		if _, err := TakenIssueRun(ctx, pool, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run, outcome posted: %v, want ErrNotFound", status, err)
		}
	}
	// A manual run of the same reference does not take the issue.
	manual := w9MustRun(t, pool, wf, func(r *Run) { r.Trigger, r.TriggerRef = TriggerManual, &ref })
	w9SetStatus(t, pool, manual.ID, RunFailed)
	if _, err := TakenIssueRun(ctx, pool, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a manual run: %v, want ErrNotFound", err)
	}
	// The live run answers before an ended one that is not posted.
	ended := w9MustRun(t, pool, wf, issueRef(ref))
	w9SetStatus(t, pool, ended.ID, RunSucceeded)
	w9Finish(t, pool, ended.ID, base.Add(3*time.Hour))
	for _, status := range liveStatuses {
		live := w9MustRun(t, pool, wf, issueRef(ref))
		w9SetStatus(t, pool, live.ID, status)
		got, err := TakenIssueRun(ctx, pool, ref)
		if err != nil || got.ID != live.ID {
			t.Fatalf("%s run next to an unposted one: %+v %v, want %s", status, got, err, live.ID)
		}
		w9SetStatus(t, pool, live.ID, RunCancelled)
		mustDo(t, SetRunReportedAt(ctx, pool, live.ID, base.Add(4*time.Hour)))
	}
}

// DESIGN 5.23 rule 2 (a), review M2: the number of the project's live runs and the sum of
// their max_cost_usd limits in micro-USD; a run whose limits name none adds nothing, an
// ended run and another project's run are not counted.
func TestLiveRunCostCeiling(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	mkProject := func(name string) *Project {
		t.Helper()
		p := &Project{Name: name, RepoURL: "https://gitlab.test/group/" + name + ".git", Provider: "gitlab", DefaultBranch: "main"}
		mustDo(t, CreateProject(ctx, pool, p, []byte("sealed")))
		return p
	}
	p, other := mkProject("w9-ceiling"), mkProject("w9-ceiling-other")

	if n, sum, err := LiveRunCostCeiling(ctx, pool, p.ID); err != nil || n != 0 || sum != 0 {
		t.Fatalf("no runs: %d %d %v", n, sum, err)
	}
	mk := func(projectID string, cost float64, status string) {
		t.Helper()
		r := w9MustRun(t, pool, wf, func(r *Run) {
			r.ProjectID = &projectID
			r.Limits = Limits{MaxCostUSD: cost}
		})
		w9SetStatus(t, pool, r.ID, status)
	}
	mk(p.ID, 0.25, RunQueued)
	mk(p.ID, 1.5, RunRunning)
	mk(p.ID, 0.333333, RunWaitingApproval)
	mk(p.ID, 0, RunRunning) // no max_cost_usd in its limits
	mk(p.ID, 9, RunSucceeded)
	mk(p.ID, 9, RunFailed)
	mk(other.ID, 7, RunRunning)
	n, sum, err := LiveRunCostCeiling(ctx, pool, p.ID)
	if err != nil || n != 4 || sum != 2_083_333 {
		t.Fatalf("project p: %d runs, %d micro-USD, %v; want 4 and 2083333", n, sum, err)
	}
	if n, sum, err = LiveRunCostCeiling(ctx, pool, other.ID); err != nil || n != 1 || sum != 7_000_000 {
		t.Fatalf("other project: %d runs, %d micro-USD, %v", n, sum, err)
	}
}

// T5.23.6 and T5.23.7: SetRunReportedAt posts the outcome, once; the channel report's
// queries never see an issue run; a retry (5.21) clears it and the run is listed again.
func TestSetRunReportedAt(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	run := w9MustRun(t, pool, wf, issueRef("pa#1"))
	// A live run is not reported: a retry re-queued it between the list and the write.
	mustDo(t, SetRunReportedAt(ctx, pool, run.ID, at))
	if got, _ := GetRun(ctx, pool, run.ID); got.ReportedAt != nil {
		t.Fatalf("a queued run was marked reported at %v", got.ReportedAt)
	}

	w9SetStatus(t, pool, run.ID, RunFailed)
	// The channel report cannot claim it, and its sweep does not list it.
	if _, err := ClaimRunReport(ctx, pool, run.ID, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ClaimRunReport on an issue run = %v, want ErrNotFound", err)
	}
	if ids, err := ListUnreportedChannelRuns(ctx, pool, 100); err != nil {
		t.Fatal(err)
	} else {
		for _, id := range ids {
			if id == run.ID {
				t.Fatal("the channel report's sweep lists an issue run")
			}
		}
	}
	if got, _ := GetRun(ctx, pool, run.ID); got.ReportedAt != nil {
		t.Fatal("ClaimRunReport changed an issue run")
	}

	mustDo(t, SetRunReportedAt(ctx, pool, run.ID, at))
	got, _ := GetRun(ctx, pool, run.ID)
	if got.ReportedAt == nil || !got.ReportedAt.Equal(at) {
		t.Fatalf("reported_at = %v, want %v", got.ReportedAt, at)
	}
	if list, _ := ListUnreportedIssueRuns(ctx, pool, 50); len(list) != 0 {
		t.Fatalf("a reported run is listed: %+v", list)
	}
	// Reported once: a second write does not move it.
	mustDo(t, SetRunReportedAt(ctx, pool, run.ID, at.Add(time.Hour)))
	if got, _ = GetRun(ctx, pool, run.ID); !got.ReportedAt.Equal(at) {
		t.Fatalf("reported_at moved to %v", got.ReportedAt)
	}

	// The operator's retry clears it; the run ends again and is listed again.
	if _, err := RetryRun(ctx, pool, run.ID, nil, at); err != nil {
		t.Fatal(err)
	}
	if got, _ = GetRun(ctx, pool, run.ID); got.ReportedAt != nil || got.Status != RunQueued {
		t.Fatalf("after the retry = %+v", got)
	}
	w9SetStatus(t, pool, run.ID, RunSucceeded)
	list, err := ListUnreportedIssueRuns(ctx, pool, 50)
	if err != nil || len(list) != 1 || list[0].ID != run.ID {
		t.Fatalf("after the retry ended = %+v %v", list, err)
	}
}

// T5.23.1 (storage): the projects the loop passes over are the live ones with intake
// enabled and a workflow, oldest first, with the workflow's name joined; the poll's
// bookkeeping is written without touching the settings or updated_at.
func TestListIntakeProjectsAndPollBookkeeping(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)

	mk := func(name string, set func(*Project)) *Project {
		t.Helper()
		p := &Project{Name: name, RepoURL: "https://gitlab.test/group/" + name + ".git", Provider: "gitlab", DefaultBranch: "main"}
		if set != nil {
			set(p)
		}
		mustDo(t, CreateProject(ctx, pool, p, []byte("sealed")))
		return p
	}
	enabled := func(p *Project) { p.IntakeEnabled, p.IntakeWorkflowID = true, &wf.ID }
	first := mk("intake-first", enabled)
	mk("intake-off", func(p *Project) { p.IntakeWorkflowID = &wf.ID })
	mk("intake-no-workflow", func(p *Project) { p.IntakeEnabled = true })
	gone := mk("intake-deleted", enabled)
	mustDo(t, DeleteProject(ctx, pool, gone.ID))
	second := mk("intake-second", func(p *Project) { enabled(p); p.IntakeLabel = "bot" })

	list, err := ListIntakeProjects(ctx, pool)
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("list = %+v %v, want the first and the second project", list, err)
	}
	if list[0].IntakeLabel != "yuno" || list[1].IntakeLabel != "bot" {
		t.Errorf("labels = %q %q", list[0].IntakeLabel, list[1].IntakeLabel)
	}
	in := list[0].Intake
	if in == nil || !in.Enabled || in.Label != "yuno" || in.WorkflowID == nil || *in.WorkflowID != wf.ID ||
		in.WorkflowName == nil || *in.WorkflowName != wf.Name || in.PolledAt != nil || in.LastError != nil {
		t.Errorf("intake view = %+v", in)
	}

	// A soft-deleted workflow keeps the project in the list: the loop says what is wrong.
	mustDo(t, DeleteWorkflow(ctx, pool, wf.ID))
	if list, err = ListIntakeProjects(ctx, pool); err != nil || len(list) != 2 {
		t.Fatalf("with the workflow deleted = %d projects %v", len(list), err)
	}

	before, _ := GetProject(ctx, pool, first.ID)
	at := time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC)
	msg := "gitlab: HTTP 403: 403 Forbidden"
	mustDo(t, SetProjectIntakePoll(ctx, pool, first.ID, at, &msg))
	after, err := GetProject(ctx, pool, first.ID)
	if err != nil || after.Intake.PolledAt == nil || !after.Intake.PolledAt.Equal(at) || after.Intake.LastError == nil || *after.Intake.LastError != msg {
		t.Fatalf("after a failed poll = %+v %v", after.Intake, err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || !after.IntakeEnabled || after.IntakeLabel != "yuno" || after.IntakeWorkflowID == nil {
		t.Errorf("the bookkeeping changed the settings or updated_at: %+v", after)
	}
	// A clean pass clears the error and moves the time.
	later := at.Add(2 * time.Minute)
	mustDo(t, SetProjectIntakePoll(ctx, pool, first.ID, later, nil))
	after, _ = GetProject(ctx, pool, first.ID)
	if !after.Intake.PolledAt.Equal(later) || after.Intake.LastError != nil {
		t.Fatalf("after a clean poll = %+v", after.Intake)
	}
	// UpdateProject keeps the bookkeeping while it rewrites the settings.
	after.Description = "changed"
	mustDo(t, UpdateProject(ctx, pool, after))
	if again, _ := GetProject(ctx, pool, first.ID); again.Intake.PolledAt == nil || !again.Intake.PolledAt.Equal(later) {
		t.Fatalf("UpdateProject lost the poll time: %+v", again.Intake)
	}

	// A project deleted during the pass still takes its bookkeeping; no row is not found.
	mustDo(t, SetProjectIntakePoll(ctx, pool, gone.ID, at, nil))
	if err := SetProjectIntakePoll(ctx, pool, "no-such-project", at, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project = %v, want ErrNotFound", err)
	}
}
