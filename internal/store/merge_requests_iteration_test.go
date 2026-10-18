package store

import (
	"context"
	"errors"
	"testing"
)

// Wave 9, DESIGN 5.22: the lookups the queue-time checks, the retry refusals and the
// Chief of Staff read, and the representation of a run that names a request.

// insertMR stores a record, failing the test when it is refused.
func insertMR(t *testing.T, pool Querier, mr *MergeRequest) *MergeRequest {
	t.Helper()
	if ok, err := InsertMergeRequest(context.Background(), pool, mr); err != nil || !ok {
		t.Fatalf("insert record %d: %v %v", mr.Number, ok, err)
	}
	return mr
}

// MergeRequestByNumber resolves a number inside one project: the unique pair
// (project_id, number) names one record, another project's same number is another
// record, and a number without a record answers nil, not an error.
func TestMergeRequestByNumber(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p1, run1 := mrProject(t, pool, "by-number-one")
	p2, run2 := mrProject(t, pool, "by-number-two")
	r1 := insertMR(t, pool, mrOf(p1, run1, 12))
	r2 := insertMR(t, pool, mrOf(p2, run2, 12))

	for _, tc := range []struct {
		project string
		number  int64
		want    *MergeRequest
	}{{p1.ID, 12, r1}, {p2.ID, 12, r2}, {p1.ID, 13, nil}, {"no-such-project", 12, nil}} {
		got, err := MergeRequestByNumber(ctx, pool, tc.project, tc.number)
		if err != nil {
			t.Fatalf("MergeRequestByNumber(%s, %d): %v", tc.project, tc.number, err)
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("MergeRequestByNumber(%s, %d) = %+v, want nil", tc.project, tc.number, got)
		case tc.want != nil && (got == nil || got.ID != tc.want.ID || got.ProjectID != tc.project || got.Number != 12):
			t.Errorf("MergeRequestByNumber(%s, %d) = %+v, want record %s", tc.project, tc.number, got, tc.want.ID)
		}
	}
}

// ProjectNameAny reads the name of a soft-deleted project as well, and says ErrNotFound
// for an id with no row.
func TestProjectNameAny(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, _ := mrProject(t, pool, "name-any")
	if name, err := ProjectNameAny(ctx, pool, p.ID); err != nil || name != "name-any" {
		t.Fatalf("live project: %q %v", name, err)
	}
	mustDo(t, DeleteProject(ctx, pool, p.ID))
	if name, err := ProjectNameAny(ctx, pool, p.ID); err != nil || name != "name-any" {
		t.Fatalf("deleted project: %q %v", name, err)
	}
	if _, err := ProjectNameAny(ctx, pool, "no-such-project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}

// DESIGN 5.22 rule 1: LiveRunForMergeRequest finds the queued, running or waiting run that
// names the record, and nothing for an ended run, another record's run or the record's
// own opening run (which names nothing).
func TestLiveRunForMergeRequest(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "live-for-mr")
	rec := insertMR(t, pool, mrOf(p, opener, 1))
	other := insertMR(t, pool, mrOf(p, mrRun(t, pool, p, nil), 2))
	wf := w9Workflow(t, pool)

	if _, err := LiveRunForMergeRequest(ctx, pool, rec.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no run names the record: %v, want ErrNotFound", err)
	}
	named := w9MustRun(t, pool, wf, func(r *Run) { r.MergeRequestID = &rec.ID; r.ProjectID = &p.ID })
	for _, status := range liveStatuses {
		w9SetStatus(t, pool, named.ID, status)
		got, err := LiveRunForMergeRequest(ctx, pool, rec.ID)
		if err != nil || got.ID != named.ID || got.MergeRequestID == nil || *got.MergeRequestID != rec.ID {
			t.Fatalf("%s run: %+v %v", status, got, err)
		}
		if _, err := LiveRunForMergeRequest(ctx, pool, other.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run of another record: %v, want ErrNotFound", status, err)
		}
	}
	for _, status := range terminalStatuses {
		w9SetStatus(t, pool, named.ID, status)
		if _, err := LiveRunForMergeRequest(ctx, pool, rec.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run: %v, want ErrNotFound", status, err)
		}
	}
}

// DESIGN 5.23 rule 3 and 5.21 rule 3: LiveRunForTrigger is the lookup of a trigger
// reference that skips the runs that ended.
func TestLiveRunForTrigger(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := w9Workflow(t, pool)
	ref, otherRef := "proj-1#7", "proj-1#8"
	issue := w9MustRun(t, pool, wf, func(r *Run) { r.Trigger, r.TriggerRef = TriggerIssue, &ref })

	for _, status := range liveStatuses {
		w9SetStatus(t, pool, issue.ID, status)
		if got, err := LiveRunForTrigger(ctx, pool, TriggerIssue, ref); err != nil || got.ID != issue.ID {
			t.Fatalf("%s run: %+v %v", status, got, err)
		}
		// Another issue, and the same reference under another trigger, are other runs.
		if _, err := LiveRunForTrigger(ctx, pool, TriggerIssue, otherRef); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run, other issue: %v, want ErrNotFound", status, err)
		}
		if _, err := LiveRunForTrigger(ctx, pool, TriggerChannel, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run, other trigger: %v, want ErrNotFound", status, err)
		}
	}
	for _, status := range terminalStatuses {
		w9SetStatus(t, pool, issue.ID, status)
		if _, err := LiveRunForTrigger(ctx, pool, TriggerIssue, ref); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s run: %v, want ErrNotFound", status, err)
		}
	}
}

// DESIGN 5.22 rule 1, 5.20 rule 6 amended: Run.merge_request is the record the run names
// when it names one, else the record the run opened, else nothing; a deleted record
// leaves the run with neither.
func TestFillRunMergeRequestsFromTheNamedRecord(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p, opener := mrProject(t, pool, "fill-named")
	opened := insertMR(t, pool, mrOf(p, opener, 1))
	// A second run opened its own request and also names the first (a fixture only: the
	// engine gives an updating run no record of its own, but the rule is "named first").
	second := mrRun(t, pool, p, nil)
	own := insertMR(t, pool, mrOf(p, second, 2))
	wf := w9Workflow(t, pool)
	updater := w9MustRun(t, pool, wf, func(r *Run) { r.MergeRequestID = &opened.ID; r.ProjectID = &p.ID })
	plain := w9MustRun(t, pool, wf, nil)

	runs := make([]Run, 0, 4)
	for _, id := range []string{opener.ID, second.ID, updater.ID, plain.ID} {
		r, err := GetRun(ctx, pool, id)
		mustDo(t, err)
		runs = append(runs, *r)
	}
	mustDo(t, FillRunMergeRequests(ctx, pool, runs))
	want := []*MergeRequest{opened, own, opened, nil}
	for i, w := range want {
		got := runs[i].MergeRequest
		if (w == nil) != (got == nil) || (w != nil && got.ID != w.ID) {
			t.Errorf("run %d: merge_request = %+v, want %+v", i, got, w)
		}
	}

	// A run that names the record and opened another one is filled from the named one
	// (the updater ended first: one live run per request).
	w9SetStatus(t, pool, updater.ID, RunSucceeded)
	mustDo(t, execSQL(ctx, pool, `UPDATE runs SET merge_request_id='`+opened.ID+`' WHERE id='`+second.ID+`'`))
	r, err := GetRun(ctx, pool, second.ID)
	mustDo(t, err)
	one := []Run{*r}
	mustDo(t, FillRunMergeRequests(ctx, pool, one))
	if one[0].MergeRequest == nil || one[0].MergeRequest.ID != opened.ID {
		t.Fatalf("named and opened: merge_request = %+v, want the named record %s", one[0].MergeRequest, opened.ID)
	}

	// Deleting the named record sets the column null: the run has no request any more.
	mustDo(t, execSQL(ctx, pool, `DELETE FROM merge_requests WHERE id='`+opened.ID+`'`))
	r, err = GetRun(ctx, pool, updater.ID)
	mustDo(t, err)
	one = []Run{*r}
	mustDo(t, FillRunMergeRequests(ctx, pool, one))
	if one[0].MergeRequestID != nil || one[0].MergeRequest != nil {
		t.Fatalf("after the record's deletion: %v %+v", one[0].MergeRequestID, one[0].MergeRequest)
	}
}
