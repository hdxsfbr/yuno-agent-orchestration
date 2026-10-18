package sched

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// T5.22.9, the tick half (DESIGN 5.22 rule 7, D-049): the schedule's merge request goes
// in the trigger request; a request that turned merged or closed disables the schedule
// with its state as the reason; any other refusal, one that another run is updating the
// request included, records its text and advances.

// request stores an open merge request record of project p, opened by a run that has
// ended, with the provider's number and name for it.
func (f *fixture) request(p *store.Project, number int64, provider string) *store.MergeRequest {
	f.t.Helper()
	ctx := context.Background()
	opener := &store.Run{WorkflowID: f.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: f.wf.Definition, Trigger: store.TriggerManual,
		ProjectID: &p.ID, Status: store.RunSucceeded}
	if err := store.CreateRun(ctx, f.pool, opener); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runs SET status='succeeded' WHERE id=$1`, opener.ID); err != nil {
		f.t.Fatal(err)
	}
	mr := &store.MergeRequest{ProjectID: p.ID, RunID: opener.ID, Provider: provider, Number: number,
		URL: "https://gitlab.test/g/p/-/merge_requests/" + strconv.FormatInt(number, 10), SourceBranch: "yuno/abc123",
		TargetBranch: "main", Title: "Add a thing"}
	if ok, err := store.InsertMergeRequest(ctx, f.pool, mr); err != nil || !ok {
		f.t.Fatalf("insert record: %v %v", ok, err)
	}
	f.t.Cleanup(func() { _, _ = f.pool.Exec(ctx, `DELETE FROM merge_requests WHERE id=$1`, mr.ID) })
	return mr
}

// scheduleOn stores a cron schedule due at due, on the project and the request.
func (f *fixture) scheduleOn(p *store.Project, mr *store.MergeRequest, due time.Time) *store.Schedule {
	f.t.Helper()
	expr := "0 9 * * *"
	s := &store.Schedule{WorkflowID: f.wf.ID, ProjectID: &p.ID, MergeRequestID: &mr.ID, Kind: store.ScheduleCron, CronExpr: &expr, TZ: "UTC",
		Enabled: true, NextRunAt: &due, Input: f.input}
	if err := store.CreateSchedule(context.Background(), f.pool, s); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = store.DeleteSchedule(context.Background(), f.pool, s.ID) })
	return s
}

// The id travels in the trigger request next to the project's; a schedule without a
// request sends none.
func TestTickPassesMergeRequest(t *testing.T) {
	f := newFixture(t, at(t, "2001-11-02T09:00:05Z"))
	p := f.newProject()
	mr := f.request(p, 4, "gitlab")
	on := f.scheduleOn(p, mr, at(t, "2001-11-02T09:00:00Z"))
	f.project = &p.ID
	off := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-11-02T08:00:00Z"))
	if fired := f.tick(); len(fired) != 2 {
		t.Fatalf("fired %d, want 2", len(fired))
	}
	got := map[string]engine.TriggerRequest{}
	for _, req := range f.trig.reqs {
		got[store.ScheduleIDOfRef(req.TriggerRef)] = req
	}
	if got[on.ID].MergeRequestID != mr.ID || got[on.ID].ProjectID != p.ID || got[off.ID].MergeRequestID != "" {
		t.Fatalf("trigger requests = %+v, want the request %s on %s and none on %s", got, mr.ID, on.ID, off.ID)
	}
}

// A request that is merged or closed disables the schedule, since no later occurrence can
// succeed: no run, no next occurrence, the state as last_error with the provider's name
// for the request. The typed refusal decides, not the text: the same words in a plain
// validation error are a refusal like any other.
func TestTickDisablesAScheduleWhoseRequestEnded(t *testing.T) {
	for _, tc := range []struct {
		provider, label, state string
	}{
		{"gitlab", "merge request !4", store.MergeRequestMerged},
		{"github", "pull request #4", store.MergeRequestClosed},
	} {
		f := newFixture(t, at(t, "2001-11-03T09:00:05Z"))
		p := f.newProject()
		mr := f.request(p, 4, tc.provider)
		s := f.scheduleOn(p, mr, at(t, "2001-11-03T09:00:00Z"))
		msg := "merge_request_id: " + tc.label + " is " + tc.state
		f.trig.err = &engine.MergeRequestStateError{ValidationError: &engine.ValidationError{Messages: []string{msg}}, Label: tc.label, State: tc.state}
		if fired := f.tick(); len(fired) != 0 {
			t.Fatalf("%s: fired %d", tc.state, len(fired))
		}
		got := f.get(s.ID)
		if got.Enabled || got.NextRunAt != nil || got.LastRunID != nil || got.LastError == nil || *got.LastError != tc.label+" is "+tc.state {
			t.Fatalf("%s: enabled %v next %v last run %v error %v, want disabled with %q", tc.state, got.Enabled, got.NextRunAt, got.LastRunID,
				got.LastError, tc.label+" is "+tc.state)
		}
		if len(f.events) != 0 {
			t.Fatalf("%s: events for a refused run: %+v", tc.state, f.events)
		}
		// The next tick finds it disabled: nothing fires.
		f.clock.Advance(24 * time.Hour)
		if f.trig.err = nil; len(f.tick()) != 0 || f.trig.count() != 1 {
			t.Fatalf("%s: a disabled schedule fired again (%d trigger requests)", tc.state, f.trig.count())
		}
	}

	// The text alone is not the marker.
	f := newFixture(t, at(t, "2001-11-04T09:00:05Z"))
	p := f.newProject()
	mr := f.request(p, 4, "gitlab")
	s := f.scheduleOn(p, mr, at(t, "2001-11-04T09:00:00Z"))
	f.trig.err = &engine.ValidationError{Messages: []string{"merge_request_id: merge request !4 is merged"}}
	f.tick()
	got := f.get(s.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-11-05T09:00:00Z")) ||
		got.LastError == nil || *got.LastError != "the workflow cannot run: merge_request_id: merge request !4 is merged" {
		t.Fatalf("untyped refusal: enabled %v next %v error %v", got.Enabled, got.NextRunAt, got.LastError)
	}
}

// A request another run is updating is a refusal that passes: the text becomes last_error
// and the schedule advances, as for a rate limit, and a later occurrence fires.
func TestTickAdvancesPastABusyRequest(t *testing.T) {
	f := newFixture(t, at(t, "2001-11-06T09:00:05Z"))
	p := f.newProject()
	mr := f.request(p, 4, "gitlab")
	due := at(t, "2001-11-06T09:00:00Z")
	s := f.scheduleOn(p, mr, due)
	f.trig.err = &engine.ValidationError{Messages: []string{"merge_request_id: merge request !4 is being updated by run run-1"}}
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired %d", len(fired))
	}
	got := f.get(s.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-11-07T09:00:00Z")) || got.LastRunID != nil ||
		got.LastError == nil || *got.LastError != "the workflow cannot run: merge_request_id: merge request !4 is being updated by run run-1" {
		t.Fatalf("busy: enabled %v next %v last run %v error %v", got.Enabled, got.NextRunAt, got.LastRunID, got.LastError)
	}
	f.trig.err = nil
	f.clock.Advance(24 * time.Hour)
	if fired := f.tick(); len(fired) != 1 {
		t.Fatalf("recovery fired %d", len(fired))
	}
	if got := f.get(s.ID); got.LastError != nil || !got.Enabled {
		t.Fatalf("after the recovery: error %v enabled %v", got.LastError, got.Enabled)
	}
}

// Through the real engine: the occurrence queues a run that names the request; the next
// occurrence, while that run is live, is refused by the live-run index with the run's id
// and the schedule moves on; once the run has ended and the request is merged, the
// schedule is disabled with the state.
func TestTickWithTheEngineOnARequest(t *testing.T) {
	f := newFixture(t, at(t, "2001-12-10T09:00:10Z"))
	f.s.Engine = engine.New(engine.Deps{Pool: f.pool, Cfg: engineConfig(), Clock: f.clock})
	ctx := context.Background()
	f.wf = f.devPipeline()
	p := f.newProject()
	mr := f.request(p, 4, "gitlab")
	f.input = json.RawMessage(`{"task":"address the review comments"}`)
	s := f.scheduleOn(p, mr, at(t, "2001-12-10T09:00:00Z"))

	fired := f.tick()
	if len(fired) != 1 {
		t.Fatalf("fired %d, want 1 (last error %v)", len(fired), f.get(s.ID).LastError)
	}
	run, err := store.GetRun(ctx, f.pool, fired[0].Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.MergeRequestID == nil || *run.MergeRequestID != mr.ID || run.ProjectID == nil || *run.ProjectID != p.ID {
		t.Fatalf("scheduled run names request %v on project %v, want %s on %s", run.MergeRequestID, run.ProjectID, mr.ID, p.ID)
	}

	// The first run is still queued: the next occurrence is refused, naming it.
	f.clock.Advance(24 * time.Hour)
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired %d while the request is being updated", len(fired))
	}
	got := f.get(s.ID)
	want := "the workflow cannot run: merge_request_id: merge request !4 is being updated by run " + run.ID
	if got.LastError == nil || *got.LastError != want || !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-12-12T09:00:00Z")) {
		t.Fatalf("busy: error %v enabled %v next %v, want %q", got.LastError, got.Enabled, got.NextRunAt, want)
	}

	// The run ends and the request is merged on the provider: no later occurrence can succeed.
	if _, err := f.pool.Exec(ctx, `UPDATE runs SET status='succeeded' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE merge_requests SET state='merged' WHERE id=$1`, mr.ID); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(48 * time.Hour)
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired %d on a merged request", len(fired))
	}
	got = f.get(s.ID)
	if got.Enabled || got.NextRunAt != nil || got.LastError == nil || *got.LastError != "merge request !4 is merged" {
		t.Fatalf("merged: enabled %v next %v error %v, want disabled with \"merge request !4 is merged\"", got.Enabled, got.NextRunAt, got.LastError)
	}
}
