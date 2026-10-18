package store

import (
	"context"
	"errors"
	"testing"
	"time"
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

// T5.23.6 (the closer's list): terminal issue runs whose outcome is not posted, oldest
// first, with the project id and the number parsed; reported ones, live ones, other
// triggers and a reference nobody can parse are left out; the limit and the project
// filter apply.
func TestListUnreportedIssueRuns(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)

	var want []*Run
	for i, status := range terminalStatuses {
		r := w9MustRun(t, pool, wf, issueRef(IssueTriggerRef("pa", int64(10+i))))
		w9SetStatus(t, pool, r.ID, status)
		want = append(want, r)
	}
	other := w9MustRun(t, pool, wf, issueRef(IssueTriggerRef("pb", 5)))
	w9SetStatus(t, pool, other.ID, RunFailed)

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
	if len(got) != 5 {
		t.Fatalf("%d runs listed, want the 4 of pa and the 1 of pb: %+v", len(got), got)
	}
	// Oldest first.
	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.Before(got[i-1].CreatedAt) {
			t.Errorf("run %d is older than the one before it", i)
		}
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.ID || g.ProjectID != "pa" || g.Number != int64(10+i) || g.Status != terminalStatuses[i] {
			t.Errorf("run %d = %s %s#%d %s, want %s pa#%d", i, g.ID, g.ProjectID, g.Number, g.Status, w.ID, 10+i)
		}
	}
	if g := got[4]; g.ID != other.ID || g.ProjectID != "pb" || g.Number != 5 {
		t.Errorf("last run = %+v", g)
	}

	limited, err := ListUnreportedIssueRuns(ctx, pool, 2)
	if err != nil || len(limited) != 2 || limited[0].ID != want[0].ID || limited[1].ID != want[1].ID {
		t.Fatalf("limit 2 = %+v %v", limited, err)
	}
	mine, err := ListUnreportedIssueRunsOfProject(ctx, pool, "pb", 50)
	if err != nil || len(mine) != 1 || mine[0].ID != other.ID {
		t.Fatalf("project pb = %+v %v", mine, err)
	}
	// "p" is the prefix of the projects pa and pb, not a project of its own.
	if none, err := ListUnreportedIssueRunsOfProject(ctx, pool, "p", 50); err != nil || len(none) != 0 {
		t.Fatalf("project p = %+v %v", none, err)
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
