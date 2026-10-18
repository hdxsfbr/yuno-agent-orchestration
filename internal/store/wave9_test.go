package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Wave 9 (DESIGN 5.22, 5.23, migration 0013): the columns, the two live-run indexes, the
// run filter, the schedule's merge request and the project's intake settings.

const (
	sha1Hex = "0123456789abcdef0123456789abcdef01234567"
	sha2Hex = "89abcdef0123456789abcdef0123456789abcdef"
)

// w9Workflow stores a workflow runs can name.
func w9Workflow(t *testing.T, pool *pgxpool.Pool) *Workflow {
	t.Helper()
	wf := &Workflow{Name: "wave9 " + NewID(), Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(context.Background(), pool, wf))
	return wf
}

// w9Run inserts a run of wf; set adjusts it before the insert. The error is returned.
func w9Run(pool *pgxpool.Pool, wf *Workflow, set func(*Run)) (*Run, error) {
	r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual}
	if set != nil {
		set(r)
	}
	return r, CreateRun(context.Background(), pool, r)
}

// w9MustRun is w9Run for an insert that must succeed.
func w9MustRun(t *testing.T, pool *pgxpool.Pool, wf *Workflow, set func(*Run)) *Run {
	t.Helper()
	r, err := w9Run(pool, wf, set)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r
}

func w9SetStatus(t *testing.T, pool *pgxpool.Pool, id, status string) {
	t.Helper()
	mustDo(t, execSQL(context.Background(), pool, `UPDATE runs SET status='`+status+`' WHERE id='`+id+`'`))
}

var (
	liveStatuses     = []string{RunQueued, RunRunning, RunWaitingApproval}
	terminalStatuses = []string{RunSucceeded, RunFailed, RunCancelled, RunTimedOut}
)

// DESIGN 5.22 rules 1 and 2: a run names a merge request record and carries the commit
// its checkout was at; both round trip through the insert and every read of the run, and
// the JSON has them under their contract names.
func TestRunMergeRequestAndCheckoutSHARoundTrip(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "w9-roundtrip")
	rec := mrOf(p, opener, 4)
	if ok, err := InsertMergeRequest(ctx, pool, rec); err != nil || !ok {
		t.Fatalf("insert record: %v %v", ok, err)
	}
	wf := w9Workflow(t, pool)
	sha := sha1Hex
	named := w9MustRun(t, pool, wf, func(r *Run) {
		r.MergeRequestID, r.CheckoutSHA = &rec.ID, &sha
		r.ProjectID = &p.ID
	})
	got, err := GetRun(ctx, pool, named.ID)
	if err != nil || got.MergeRequestID == nil || *got.MergeRequestID != rec.ID || got.CheckoutSHA == nil || *got.CheckoutSHA != sha1Hex {
		t.Fatalf("get = %+v %v", got, err)
	}
	if b, _ := json.Marshal(got); !bytes.Contains(b, []byte(`"merge_request_id":"`+rec.ID+`"`)) ||
		!bytes.Contains(b, []byte(`"checkout_sha":"`+sha1Hex+`"`)) {
		t.Fatalf("json = %s", b)
	}

	// A run that names nothing has neither, and the JSON omits both.
	plain := w9MustRun(t, pool, wf, nil)
	got, err = GetRun(ctx, pool, plain.ID)
	if err != nil || got.MergeRequestID != nil || got.CheckoutSHA != nil {
		t.Fatalf("plain run = %+v %v", got, err)
	}
	if b, _ := json.Marshal(got); bytes.Contains(b, []byte("merge_request_id")) || bytes.Contains(b, []byte("checkout_sha")) {
		t.Fatalf("json of a plain run = %s", b)
	}

	// The record's own run does not "name" it: the opener was live when the run above
	// was inserted (the queue-time read of rule 1 covers that case, not the index). A
	// second run naming the record is refused while the first is live.
	if got, _ := GetRun(ctx, pool, opener.ID); got.Status != RunQueued {
		t.Fatalf("the opener is %s, not live", got.Status)
	}
	if _, err := w9Run(pool, wf, func(r *Run) { r.MergeRequestID = &rec.ID; r.ProjectID = &p.ID }); !errors.Is(err, ErrMergeRequestBusy) {
		t.Fatalf("second naming run: %v, want ErrMergeRequestBusy", err)
	}

	// The record's deletion leaves the run: merge_request_id goes to null (set null).
	mustDo(t, execSQL(ctx, pool, `DELETE FROM merge_requests WHERE id='`+rec.ID+`'`))
	if got, err = GetRun(ctx, pool, named.ID); err != nil || got.MergeRequestID != nil || got.CheckoutSHA == nil {
		t.Fatalf("after the record's deletion = %+v %v", got, err)
	}
}

// DESIGN 5.22 rule 1: runs_merge_request_live_idx. Of the runs that name one record, one
// at a time is queued, running or waiting for approval; the second insert is
// ErrMergeRequestBusy, a terminal first run frees the request, and another record is
// independent.
func TestCreateRunMergeRequestBusy(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "w9-busy")
	rec, other := mrOf(p, opener, 1), mrOf(p, mrRun(t, pool, p, nil), 2)
	for _, r := range []*MergeRequest{rec, other} {
		if ok, err := InsertMergeRequest(ctx, pool, r); err != nil || !ok {
			t.Fatalf("insert record %d: %v %v", r.Number, ok, err)
		}
	}
	wf := w9Workflow(t, pool)
	naming := func(id string) func(*Run) {
		return func(r *Run) { r.MergeRequestID = &id; r.ProjectID = &p.ID }
	}

	first := w9MustRun(t, pool, wf, naming(rec.ID))
	for _, status := range liveStatuses {
		w9SetStatus(t, pool, first.ID, status)
		if _, err := w9Run(pool, wf, naming(rec.ID)); !errors.Is(err, ErrMergeRequestBusy) {
			t.Fatalf("first run %s: second insert = %v, want ErrMergeRequestBusy", status, err)
		}
	}
	if _, err := w9Run(pool, wf, naming(rec.ID)); err == nil || err.Error() != "another live run is updating this merge request" {
		t.Fatalf("the error text = %v", err)
	}
	// Another record, and a run that names none, are not refused.
	w9MustRun(t, pool, wf, naming(other.ID))
	w9MustRun(t, pool, wf, nil)

	// A run that ended leaves the index; the next one takes the request, and is then
	// the live one.
	for _, status := range terminalStatuses {
		w9SetStatus(t, pool, first.ID, status)
		next := w9MustRun(t, pool, wf, naming(rec.ID))
		if _, err := w9Run(pool, wf, naming(rec.ID)); !errors.Is(err, ErrMergeRequestBusy) {
			t.Fatalf("after a %s first run the new run is live; a third insert = %v", status, err)
		}
		w9SetStatus(t, pool, next.ID, RunSucceeded)
	}
}

// DESIGN 5.23 rule 3: runs_issue_live_idx. One live run per issue (trigger_ref
// "<project id>#<n>", trigger issue): the second is ErrIssueBusy, a terminal first run
// frees the issue, another issue and the other triggers are independent.
func TestCreateRunIssueBusy(t *testing.T) {
	pool := needDB(t)
	wf := w9Workflow(t, pool)
	issue := func(ref string) func(*Run) {
		return func(r *Run) { r.Trigger, r.TriggerRef = TriggerIssue, &ref }
	}
	if TriggerIssue != "issue" {
		t.Fatalf("TriggerIssue = %q", TriggerIssue)
	}

	first := w9MustRun(t, pool, wf, issue("proj1#12"))
	for _, status := range liveStatuses {
		w9SetStatus(t, pool, first.ID, status)
		if _, err := w9Run(pool, wf, issue("proj1#12")); !errors.Is(err, ErrIssueBusy) {
			t.Fatalf("first run %s: second insert = %v, want ErrIssueBusy", status, err)
		}
	}
	if _, err := w9Run(pool, wf, issue("proj1#12")); err == nil || err.Error() != "this issue already has a live run" {
		t.Fatalf("the error text = %v", err)
	}
	// Another issue, another project's issue with the same number, and a run of another
	// trigger that carries the same reference are not refused.
	w9MustRun(t, pool, wf, issue("proj1#13"))
	w9MustRun(t, pool, wf, issue("proj2#12"))
	w9MustRun(t, pool, wf, func(r *Run) { ref := "proj1#12"; r.Trigger, r.TriggerRef = TriggerEval, &ref })
	w9MustRun(t, pool, wf, func(r *Run) { ref := "proj1#12"; r.Trigger, r.TriggerRef = TriggerManual, &ref })

	// Retried by relabelling: once the run ended, a new run for the issue is accepted.
	for _, status := range terminalStatuses {
		w9SetStatus(t, pool, first.ID, status)
		next := w9MustRun(t, pool, wf, issue("proj1#12"))
		w9SetStatus(t, pool, next.ID, RunFailed)
	}
}

// DESIGN 5.22 rule 1, 5.23 rule 3 at the retry (5.21 rule 3): a retry re-enters the live
// indexes, so it is refused with the same two errors while another live run holds the
// request or the issue, and the run stays as it was; once that run ends the retry goes
// through.
func TestRetryRunReturnsTheBusyErrors(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "w9-retry")
	rec := mrOf(p, opener, 3)
	if ok, err := InsertMergeRequest(ctx, pool, rec); err != nil || !ok {
		t.Fatalf("insert record: %v %v", ok, err)
	}
	wf := w9Workflow(t, pool)

	cases := []struct {
		name string
		set  func(r *Run)
		want error
	}{
		{"merge request", func(r *Run) { r.MergeRequestID = &rec.ID; r.ProjectID = &p.ID }, ErrMergeRequestBusy},
		{"issue", func(r *Run) { ref := "proj1#7"; r.Trigger, r.TriggerRef = TriggerIssue, &ref }, ErrIssueBusy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			failed := w9MustRun(t, pool, wf, c.set)
			w9SetStatus(t, pool, failed.ID, RunFailed)
			// Another run took the request or the issue after this one ended.
			holder := w9MustRun(t, pool, wf, c.set)

			got, err := RetryRun(ctx, pool, failed.ID, nil, time.Now())
			if !errors.Is(err, c.want) || got != nil {
				t.Fatalf("retry = %+v %v, want %v", got, err, c.want)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatal("the busy refusal reads as a missing run")
			}
			after, err := GetRun(ctx, pool, failed.ID)
			if err != nil || after.Status != RunFailed || after.Retries != 0 {
				t.Fatalf("the refused retry changed the run: %+v %v", after, err)
			}

			// The holder ends: the retry re-enters the index.
			w9SetStatus(t, pool, holder.ID, RunSucceeded)
			got, err = RetryRun(ctx, pool, failed.ID, nil, time.Now())
			if err != nil || got.Status != RunQueued || got.Retries != 1 {
				t.Fatalf("retry after the holder ended = %+v %v", got, err)
			}
			if c.want == ErrMergeRequestBusy && (got.MergeRequestID == nil || *got.MergeRequestID != rec.ID) {
				t.Fatalf("the retried run lost its request: %+v", got.MergeRequestID)
			}
			// And now the retried run holds it.
			if _, err := w9Run(pool, wf, c.set); !errors.Is(err, c.want) {
				t.Fatalf("insert while the retried run is live: %v, want %v", err, c.want)
			}
		})
	}
}

// DESIGN 5.22 rule 4: the commit id is written after the clone and again after each push;
// the second write replaces the first.
func TestSetRunCheckoutSHA(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	run := w9MustRun(t, pool, w9Workflow(t, pool), nil)
	if got, _ := GetRun(ctx, pool, run.ID); got.CheckoutSHA != nil {
		t.Fatalf("a new run has checkout_sha %v", *got.CheckoutSHA)
	}
	mustDo(t, SetRunCheckoutSHA(ctx, pool, run.ID, sha1Hex))
	got, err := GetRun(ctx, pool, run.ID)
	if err != nil || got.CheckoutSHA == nil || *got.CheckoutSHA != sha1Hex {
		t.Fatalf("after the clone = %+v %v", got, err)
	}
	mustDo(t, SetRunCheckoutSHA(ctx, pool, run.ID, sha2Hex))
	if got, _ = GetRun(ctx, pool, run.ID); got.CheckoutSHA == nil || *got.CheckoutSHA != sha2Hex {
		t.Fatalf("after the push = %v", got.CheckoutSHA)
	}
	// The checkout's commit survives what the engine does to the row afterwards.
	mustDo(t, SetRunToolchain(ctx, pool, run.ID, "go"))
	if got, _ = GetRun(ctx, pool, run.ID); got.CheckoutSHA == nil || *got.CheckoutSHA != sha2Hex || got.Toolchain == nil {
		t.Fatalf("after the toolchain = %+v", got)
	}
	// An unknown run is not an error (like SetRunToolchain): no row to write.
	mustDo(t, SetRunCheckoutSHA(ctx, pool, "no-such-run", sha1Hex))
}

// DESIGN 5.22 rule 1 (the runs list): each member of RunFilter narrows the list on its
// own, an empty member does not, and the limit rule is the old one.
func TestListRunsFilter(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p1, opener := mrProject(t, pool, "w9-list-one")
	p2, _ := mrProject(t, pool, "w9-list-two")
	rec := mrOf(p1, opener, 9)
	if ok, err := InsertMergeRequest(ctx, pool, rec); err != nil || !ok {
		t.Fatalf("insert record: %v %v", ok, err)
	}
	wf := w9Workflow(t, pool)
	ref := "p#1"
	mk := func(set func(*Run)) *Run { return w9MustRun(t, pool, wf, set) }
	// Newest last; mrProject made two runs (one per project) before these.
	manualP1 := mk(func(r *Run) { r.ProjectID = &p1.ID })
	issueP1 := mk(func(r *Run) { r.ProjectID = &p1.ID; r.Trigger, r.TriggerRef = TriggerIssue, &ref })
	namingP1 := mk(func(r *Run) { r.ProjectID = &p1.ID; r.MergeRequestID = &rec.ID })
	scheduleP2 := mk(func(r *Run) { r.ProjectID = &p2.ID; r.Trigger = TriggerSchedule })
	noProject := mk(nil)
	w9SetStatus(t, pool, manualP1.ID, RunFailed)
	w9SetStatus(t, pool, scheduleP2.ID, RunSucceeded)

	ids := func(f RunFilter) []string {
		t.Helper()
		runs, err := ListRuns(ctx, pool, f)
		if err != nil || runs == nil {
			t.Fatalf("list %+v = %v, %v", f, runs, err)
		}
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ID
		}
		return out
	}
	same := func(label string, got []string, want ...*Run) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d runs %v, want %d", label, len(got), got, len(want))
		}
		for i, r := range want {
			if got[i] != r.ID {
				t.Fatalf("%s: run %d = %s, want %s (newest first)", label, i, got[i], r.ID)
			}
		}
	}

	all := ids(RunFilter{})
	if len(all) != 7 || all[0] != noProject.ID {
		t.Fatalf("unfiltered = %v", all)
	}
	same("status", ids(RunFilter{Status: RunFailed}), manualP1)
	same("project", ids(RunFilter{ProjectID: p1.ID}), namingP1, issueP1, manualP1, opener)
	same("trigger issue", ids(RunFilter{Trigger: TriggerIssue}), issueP1)
	same("trigger schedule", ids(RunFilter{Trigger: TriggerSchedule}), scheduleP2)
	same("merge request", ids(RunFilter{MergeRequestID: rec.ID}), namingP1)
	same("merge request and status", ids(RunFilter{MergeRequestID: rec.ID, Status: RunFailed}))
	same("project and trigger", ids(RunFilter{ProjectID: p1.ID, Trigger: TriggerIssue}), issueP1)
	same("all four", ids(RunFilter{Status: RunQueued, ProjectID: p1.ID, Trigger: TriggerManual, MergeRequestID: rec.ID}), namingP1)
	same("none match", ids(RunFilter{Trigger: TriggerChannel}))
	same("an id nobody names", ids(RunFilter{MergeRequestID: "no-such-record"}))

	// The limit rule as before: 1..500 as given, otherwise 100.
	if got := ids(RunFilter{Limit: 2}); len(got) != 2 || got[0] != noProject.ID {
		t.Fatalf("limit 2 = %v", got)
	}
	for _, limit := range []int{0, -1, 501} {
		if got := ids(RunFilter{Limit: limit}); len(got) != 7 {
			t.Fatalf("limit %d = %d runs", limit, len(got))
		}
	}
}

// DESIGN 5.22 rule 7: a schedule names the merge request its runs update. The id round
// trips through create, get, list, the tick's locked rows and update; the record's
// number is joined; an update can clear it; the record's deletion leaves the schedule
// with neither member.
func TestScheduleMergeRequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "w9-schedule")
	rec := mrOf(p, opener, 12)
	if ok, err := InsertMergeRequest(ctx, pool, rec); err != nil || !ok {
		t.Fatalf("insert record: %v %v", ok, err)
	}
	wf := &Workflow{Name: "project workflow", Definition: json.RawMessage(`{"nodes":[],"edges":[],"project":true}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	expr := "0 9 * * *"
	due := mustParse(t, "2001-01-01T09:00:00Z")
	s := &Schedule{WorkflowID: wf.ID, ProjectID: &p.ID, MergeRequestID: &rec.ID, Kind: ScheduleCron, CronExpr: &expr, TZ: "UTC",
		Enabled: true, NextRunAt: &due}
	mustDo(t, CreateSchedule(ctx, pool, s))
	plain := &Schedule{WorkflowID: wf.ID, ProjectID: &p.ID, Kind: ScheduleCron, CronExpr: &expr, TZ: "UTC"}
	mustDo(t, CreateSchedule(ctx, pool, plain))

	check := func(label string, got *Schedule, id *string, number *int64) {
		t.Helper()
		if (got.MergeRequestID == nil) != (id == nil) || (id != nil && *got.MergeRequestID != *id) ||
			(got.MergeRequestNumber == nil) != (number == nil) || (number != nil && *got.MergeRequestNumber != *number) {
			t.Fatalf("%s: merge_request_id=%v number=%v, want %v and %v", label, deref(got.MergeRequestID), got.MergeRequestNumber, deref(id), number)
		}
	}
	twelve := int64(12)
	got, err := GetSchedule(ctx, pool, s.ID)
	mustDo(t, err)
	check("get", got, &rec.ID, &twelve)
	if b, _ := json.Marshal(got); !bytes.Contains(b, []byte(`"merge_request_id":"`+rec.ID+`"`)) || !bytes.Contains(b, []byte(`"merge_request_number":12`)) {
		t.Fatalf("json = %s", b)
	}
	list, err := ListSchedules(ctx, pool)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}
	for _, l := range list {
		if l.ID == s.ID {
			check("list", &l, &rec.ID, &twelve)
		} else {
			check("list, plain", &l, nil, nil)
		}
	}
	for _, l := range list {
		if b, _ := json.Marshal(l); l.ID == plain.ID && (bytes.Contains(b, []byte("merge_request_id")) || bytes.Contains(b, []byte("merge_request_number"))) {
			t.Fatalf("json of a plain schedule = %s", b)
		}
	}

	// The tick's rows scan with the join.
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	locked, err := LockDueSchedules(ctx, tx, due, 10)
	mustDo(t, err)
	mustDo(t, tx.Rollback(ctx))
	if len(locked) != 1 || locked[0].ID != s.ID || locked[0].WorkflowGone {
		t.Fatalf("locked = %+v", locked)
	}
	check("locked", &locked[0].Schedule, &rec.ID, &twelve)

	// An update that leaves it keeps it; one that clears it drops both members.
	got.LastError = ptr("a note")
	mustDo(t, UpdateSchedule(ctx, pool, got))
	got, _ = GetSchedule(ctx, pool, s.ID)
	check("after an unrelated update", got, &rec.ID, &twelve)
	got.MergeRequestID = nil
	mustDo(t, UpdateSchedule(ctx, pool, got))
	got, _ = GetSchedule(ctx, pool, s.ID)
	check("after clearing", got, nil, nil)
	got.MergeRequestID = &rec.ID
	mustDo(t, UpdateSchedule(ctx, pool, got))
	got, _ = GetSchedule(ctx, pool, s.ID)
	check("after setting it again", got, &rec.ID, &twelve)

	// The record goes (its run is deleted, or the project): set null, the schedule stays.
	mustDo(t, execSQL(ctx, pool, `DELETE FROM merge_requests WHERE id='`+rec.ID+`'`))
	got, err = GetSchedule(ctx, pool, s.ID)
	if err != nil {
		t.Fatalf("the schedule went with the record: %v", err)
	}
	check("after the record's deletion", got, nil, nil)
	if got.ProjectID == nil || *got.ProjectID != p.ID || !got.Enabled {
		t.Fatalf("the schedule changed with the record: %+v", got)
	}
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// DESIGN 5.23 rule 1: a project's intake settings round trip through create, every read
// and update; the columns stay out of the JSON and the intake view carries them with the
// workflow's name joined; a project that never set them reads as the defaults; the poll's
// bookkeeping is not written by create or update.
func TestProjectIntakeSettings(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	other := w9Workflow(t, pool)

	// Defaults: off, the label yuno, no workflow, never polled.
	plain := &Project{Name: "Plain", RepoURL: "https://gitlab.test/g/plain.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, plain, nil))
	got, err := GetProject(ctx, pool, plain.ID)
	mustDo(t, err)
	if in := got.Intake; in == nil || in.Enabled || in.Label != "yuno" || in.WorkflowID != nil || in.WorkflowName != nil ||
		in.PolledAt != nil || in.LastError != nil {
		t.Fatalf("defaults = %+v", got.Intake)
	}
	if b, _ := json.Marshal(got); !bytes.Contains(b, []byte(`"intake":{"enabled":false,"label":"yuno"}`)) ||
		bytes.Contains(b, []byte("intake_")) {
		t.Fatalf("json of a plain project = %s", b)
	}

	// Set at creation.
	p := &Project{Name: "Intake", RepoURL: "https://gitlab.test/g/intake.git", Provider: "gitlab", DefaultBranch: "main",
		IntakeEnabled: true, IntakeLabel: "ship-it", IntakeWorkflowID: &wf.ID}
	mustDo(t, CreateProject(ctx, pool, p, []byte("sealed")))
	check := func(label string, got *Project, enabled bool, name string, workflow *Workflow) {
		t.Helper()
		in := got.Intake
		if in == nil || in.Enabled != enabled || in.Label != name {
			t.Fatalf("%s: intake = %+v, want enabled=%v label=%q", label, in, enabled, name)
		}
		switch {
		case workflow == nil && (in.WorkflowID != nil || in.WorkflowName != nil):
			t.Fatalf("%s: workflow = %v %v, want none", label, deref(in.WorkflowID), in.WorkflowName)
		case workflow != nil && (in.WorkflowID == nil || *in.WorkflowID != workflow.ID || in.WorkflowName == nil || *in.WorkflowName != workflow.Name):
			t.Fatalf("%s: workflow = %v %v, want %s %q", label, deref(in.WorkflowID), in.WorkflowName, workflow.ID, workflow.Name)
		}
		if got.IntakeEnabled != enabled || got.IntakeLabel != name {
			t.Fatalf("%s: columns = %v %q", label, got.IntakeEnabled, got.IntakeLabel)
		}
	}
	byID, err := GetProject(ctx, pool, p.ID)
	mustDo(t, err)
	check("get", byID, true, "ship-it", wf)
	byName, err := GetProjectByName(ctx, pool, "intake")
	mustDo(t, err)
	check("by name", byName, true, "ship-it", wf)
	byURL, err := GetProjectByRepoURL(ctx, pool, p.RepoURL)
	mustDo(t, err)
	check("by url", byURL, true, "ship-it", wf)
	found, err := FindProjectByRepoURL(ctx, pool, p.RepoURL)
	mustDo(t, err)
	check("find by url", found, true, "ship-it", wf)
	list, err := ListProjects(ctx, pool)
	if err != nil || len(list) != 2 || list[0].Name != "Intake" || list[1].Name != "Plain" {
		t.Fatalf("list = %+v %v", list, err)
	}
	check("list, intake", &list[0], true, "ship-it", wf)
	check("list, plain", &list[1], false, "yuno", nil)
	if !byID.TokenSet || list[1].TokenSet {
		t.Fatalf("token_set = %v and %v", byID.TokenSet, list[1].TokenSet)
	}
	b, _ := json.Marshal(byID)
	if !bytes.Contains(b, []byte(`"intake":{"enabled":true,"label":"ship-it","workflow_id":"`+wf.ID+`","workflow_name":"`+wf.Name+`"}`)) ||
		bytes.Contains(b, []byte("intake_")) {
		t.Fatalf("json = %s", b)
	}
	if b, _ := json.Marshal(p.Settings()); bytes.Contains(b, []byte(`"intake`)) {
		t.Fatalf("a run's project snapshot carries the intake settings: %s", b)
	}

	// The poll's bookkeeping is the loop's: update does not write it, and it shows in the view.
	polled := mustParse(t, "2026-10-18T09:30:00Z")
	mustDo(t, execSQL(ctx, pool, `UPDATE projects SET intake_polled_at='2026-10-18T09:30:00Z', intake_last_error='gitlab: HTTP 500'
		WHERE id='`+p.ID+`'`))
	got, _ = GetProject(ctx, pool, p.ID)
	if in := got.Intake; in.PolledAt == nil || !in.PolledAt.Equal(polled) || in.LastError == nil || *in.LastError != "gitlab: HTTP 500" {
		t.Fatalf("poll bookkeeping = %+v", in)
	}
	if b, _ := json.Marshal(got); !bytes.Contains(b, []byte(`"polled_at":"2026-10-18T09:30:00Z"`)) || !bytes.Contains(b, []byte(`"last_error":"gitlab: HTTP 500"`)) {
		t.Fatalf("json = %s", b)
	}

	// Update carries the three settings; the label and the workflow are replaced, and
	// an empty label keeps the stored one (no valid label is empty).
	got.IntakeLabel, got.IntakeWorkflowID, got.Description = "triage", &other.ID, "changed"
	mustDo(t, UpdateProject(ctx, pool, got))
	got, _ = GetProject(ctx, pool, p.ID)
	check("after update", got, true, "triage", other)
	if got.Description != "changed" || got.Intake.PolledAt == nil || got.Intake.LastError == nil {
		t.Fatalf("update lost something: %+v", got)
	}
	got.IntakeLabel, got.IntakeEnabled = "", false
	mustDo(t, UpdateProject(ctx, pool, got))
	got, _ = GetProject(ctx, pool, p.ID)
	check("after update with no label", got, false, "triage", other)
	got.IntakeWorkflowID = nil
	mustDo(t, UpdateProject(ctx, pool, got))
	got, _ = GetProject(ctx, pool, p.ID)
	check("after clearing the workflow", got, false, "triage", nil)

	// A soft-deleted workflow shows no name on the view (review M2): the reference stays,
	// since the API only soft-deletes, but the project's card must not name a workflow that
	// is gone. The same holds for every read of a project and for the intake loop's list.
	got.IntakeWorkflowID, got.IntakeEnabled = &wf.ID, true
	mustDo(t, UpdateProject(ctx, pool, got))
	mustDo(t, DeleteWorkflow(ctx, pool, wf.ID))
	reads := map[string]func() (*Project, error){
		"get":         func() (*Project, error) { return GetProject(ctx, pool, p.ID) },
		"by name":     func() (*Project, error) { return GetProjectByName(ctx, pool, "intake") },
		"by url":      func() (*Project, error) { return GetProjectByRepoURL(ctx, pool, p.RepoURL) },
		"find by url": func() (*Project, error) { return FindProjectByRepoURL(ctx, pool, p.RepoURL) },
	}
	for name, read := range reads {
		got, err = read()
		mustDo(t, err)
		if in := got.Intake; in == nil || !in.Enabled || in.WorkflowID == nil || *in.WorkflowID != wf.ID || in.WorkflowName != nil {
			t.Fatalf("workflow soft-deleted, %s: intake = %+v, want the id kept and no name", name, in)
		}
	}
	list, err = ListProjects(ctx, pool)
	mustDo(t, err)
	for _, lp := range list {
		if lp.Name == "Intake" && lp.Intake.WorkflowName != nil {
			t.Fatalf("workflow soft-deleted, list: name %q", *lp.Intake.WorkflowName)
		}
	}
	listed, err := ListIntakeProjects(ctx, pool)
	mustDo(t, err)
	if len(listed) != 1 || listed[0].IntakeWorkflowID == nil || *listed[0].IntakeWorkflowID != wf.ID || listed[0].IntakeWorkflowName != nil {
		t.Fatalf("workflow soft-deleted, intake list: %+v", listed)
	}
	b, _ = json.Marshal(got)
	if bytes.Contains(b, []byte(`"workflow_name"`)) || !bytes.Contains(b, []byte(`"workflow_id":"`+wf.ID+`"`)) {
		t.Fatalf("workflow soft-deleted, json = %s", b)
	}
	// A workflow row that is gone empties the reference (set null).
	mustDo(t, execSQL(ctx, pool, `DELETE FROM workflows WHERE id='`+wf.ID+`'`))
	got, err = GetProject(ctx, pool, p.ID)
	if err != nil {
		t.Fatalf("the project went with the workflow: %v", err)
	}
	check("workflow row deleted", got, true, "triage", nil)
}

// DESIGN 5.22 and 5.23 refusals name the request the way the provider does.
func TestMergeRequestLabel(t *testing.T) {
	for _, c := range []struct {
		provider string
		number   int64
		want     string
	}{
		{"gitlab", 12, "merge request !12"},
		{"github", 12, "pull request #12"},
		{"", 3, "merge request !3"},
	} {
		if got := (&MergeRequest{Provider: c.provider, Number: c.number}).Label(); got != c.want {
			t.Errorf("Label(%q, %d) = %q, want %q", c.provider, c.number, got, c.want)
		}
	}
}

// Migration 0013 on a database that predates it (wave 8 as deployed): the rows already
// there read with the defaults, the two indexes are in place, and a second Migrate is a
// no-op.
func TestMigration0013Upgrade(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const schema = "yuno_store_wave9_upgrade_test"
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
	applyBefore(t, ctx, pool, 13)

	// Wave 8 data: a project, a run on it, its merge request, a schedule on the project.
	_, err = pool.Exec(ctx, `INSERT INTO workflows (id, name, definition) VALUES ('wf1', 'Dev pipeline', '{"nodes":[],"edges":[]}')`)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO projects (id, name, repo_url, provider, default_branch) VALUES
		('p1', 'Old project', 'https://gitlab.test/g/old.git', 'gitlab', 'main')`)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO runs (id, workflow_id, workflow_version, workflow_snapshot, trigger, status, limits, project_id)
		VALUES ('r1', 'wf1', 1, '{}', 'manual', 'running', '{}', 'p1')`)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO merge_requests (id, project_id, run_id, provider, number, url, source_branch, target_branch)
		VALUES ('mr1', 'p1', 'r1', 'gitlab', 4, 'https://gitlab.test/g/old/-/merge_requests/4', 'yuno/abc', 'main')`)
	mustDo(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO schedules (id, workflow_id, project_id, kind, cron_expr, tz) VALUES
		('s1', 'wf1', 'p1', 'cron', '0 9 * * *', 'UTC')`)
	mustDo(t, err)

	log := slog.New(slog.DiscardHandler)
	mustDo(t, Migrate(ctx, pool, log))
	mustDo(t, Migrate(ctx, pool, log))

	run, err := GetRun(ctx, pool, "r1")
	if err != nil || run.MergeRequestID != nil || run.CheckoutSHA != nil || run.Status != RunRunning {
		t.Fatalf("old run = %+v %v", run, err)
	}
	p, err := GetProject(ctx, pool, "p1")
	if err != nil || p.Intake == nil || p.Intake.Enabled || p.Intake.Label != "yuno" || p.Intake.WorkflowID != nil || p.Intake.PolledAt != nil {
		t.Fatalf("old project = %+v %v", p, err)
	}
	s, err := GetSchedule(ctx, pool, "s1")
	if err != nil || s.MergeRequestID != nil || s.MergeRequestNumber != nil {
		t.Fatalf("old schedule = %+v %v", s, err)
	}

	// The partial indexes exist and are unique and partial as 5.22 rule 1 and 5.23 rule 3 say.
	for name, parts := range map[string][]string{
		"runs_merge_request_live_idx": {"UNIQUE", "(merge_request_id)", "merge_request_id IS NOT NULL", "queued", "running", "waiting_approval"},
		"runs_issue_live_idx":         {"UNIQUE", "(trigger_ref)", "issue", "queued", "running", "waiting_approval"},
		"runs_issue_unreported_idx":   {"(finished_at DESC NULLS LAST, id)", "issue", "reported_at IS NULL"},
	} {
		var def string
		err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = $2`, schema, name).Scan(&def)
		if err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		for _, part := range parts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s = %s, missing %q", name, def, part)
			}
		}
	}
	// The issue queries write the trigger as a literal so the planner can prove the partial
	// indexes' predicates (review L9): with sequential scans off each plan names its index.
	for name, c := range map[string]struct {
		sql   string
		args  []any
		index string
	}{
		"closer's list":              {unreportedIssueRunsSQL, []any{RunSucceeded, RunFailed, RunCancelled, RunTimedOut, "", 50}, "runs_issue_unreported_idx"},
		"closer's list of a project": {unreportedIssueRunsSQL, []any{RunSucceeded, RunFailed, RunCancelled, RunTimedOut, "p1#", 50}, "runs_issue_unreported_idx"},
		"live run of an issue":       {liveIssueRunSQL, []any{"p1#7"}, "runs_issue_live_idx"},
		"unposted run of an issue":   {takenIssueRunSQL, []any{"p1#7", RunSucceeded, RunFailed, RunCancelled, RunTimedOut}, "runs_issue_unreported_idx"},
	} {
		if plan := explainNoSeqScan(t, ctx, pool, c.sql, c.args...); !strings.Contains(plan, c.index) {
			t.Errorf("%s: the plan does not use %s:\n%s", name, c.index, plan)
		}
	}

	// The index works on the migrated data: the old run 'r1' opened the request and is
	// live, a run naming it is accepted, a second is refused, and once the first ended
	// a third is accepted.
	wf := &Workflow{ID: "wf1", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mrID := "mr1"
	live, err := w9Run(pool, wf, func(r *Run) { r.MergeRequestID = &mrID })
	if err != nil {
		t.Fatalf("first naming run: %v", err)
	}
	if _, err := w9Run(pool, wf, func(r *Run) { r.MergeRequestID = &mrID }); !errors.Is(err, ErrMergeRequestBusy) {
		t.Fatalf("second naming run: %v", err)
	}
	mustDo(t, execSQL(ctx, pool, `UPDATE runs SET status='succeeded' WHERE id='`+live.ID+`'`))
	if _, err := w9Run(pool, wf, func(r *Run) { r.MergeRequestID = &mrID }); err != nil {
		t.Fatalf("third naming run after the first ended: %v", err)
	}
}

// explainNoSeqScan is the plan of a query with sequential scans off, so a partial index the
// planner can use is the plan it picks even on a table of a few rows.
func explainNoSeqScan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	mustDo(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	mustDo(t, err)
	rows, err := tx.Query(ctx, `EXPLAIN `+sql, args...)
	mustDo(t, err)
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		mustDo(t, rows.Scan(&line))
		plan.WriteString(line + "\n")
	}
	mustDo(t, rows.Err())
	return plan.String()
}
