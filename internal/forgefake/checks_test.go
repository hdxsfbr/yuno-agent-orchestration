package forgefake

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"yuno/internal/tools"
)

// Chief platform control, DESIGN wave 3: the fake answers the reads of rules 4, 13 and 14
// and the merge with the head's sha of rule 15 as GitLab does.
func TestChecksDiffsAndExactMerge(t *testing.T) {
	ts, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)
	if ok, err := rp.CIConfigured(ctx, "main"); err != nil || ok {
		t.Fatalf("a project without pipelines: %v %v", ok, err)
	}
	push(t, pc, "feature/one", map[string]string{"one.txt": "one\n"})
	if _, err := rp.Create(ctx, "feature/one", "main", "Add one", "", false); err != nil {
		t.Fatal(err)
	}
	d, err := rp.Details(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := rp.Checks(ctx, 1, d.HeadSHA); err != nil || c.Status != "none" || len(c.Jobs) != 0 {
		t.Fatalf("checks before any pipeline: %+v %v", c, err)
	}

	// A pipeline of main's commit is not the request's.
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": "passed"}, nil, http.StatusCreated)
	call(t, ts, http.MethodPost, "/_forge/repos/acme/app/pipeline", map[string]any{"state": "failed", "ref": "feature/one", "jobs": []map[string]any{
		{"name": "build", "stage": "build", "state": "passed", "log": "built\n"},
		{"name": "unit", "stage": "test", "state": "failed", "log": "one\ntwo\nFAIL\n"},
	}}, nil, http.StatusCreated)
	c, err := rp.Checks(ctx, 1, d.HeadSHA)
	if err != nil || c.Status != "failed" || c.SHA != d.HeadSHA || c.PipelineID == 0 || len(c.Jobs) != 2 ||
		c.Jobs[0].Name != "build" || c.Jobs[0].Status != tools.JobPassed || c.Jobs[1].Stage != "test" || c.Jobs[1].Status != tools.JobFailed {
		t.Fatalf("checks = %+v %v", c, err)
	}
	if lg, err := rp.JobLog(ctx, c.Jobs[1].ID, 5); err != nil || string(lg.Text) != "FAIL\n" || lg.Size != 13 || !lg.Truncated {
		t.Fatalf("log = %+v %v", lg, err)
	}
	if _, err := rp.JobLog(ctx, "999", 5); !errors.Is(err, tools.ErrJobNotFound) {
		t.Fatalf("unknown job: %v", err)
	}
	st, err := rp.Status(ctx, 1)
	if err != nil || st.CIStatus != "failed" || st.PipelineID != c.PipelineID || st.Mergeable == nil || !*st.Mergeable || st.MergeStatus != "mergeable" {
		t.Fatalf("status = %+v %v", st, err)
	}
	diff, err := rp.Diff(ctx, 1, 1<<20)
	if err != nil || len(diff.Files) != 1 || diff.Files[0].Path != "one.txt" || diff.Files[0].Status != "added" ||
		!strings.HasPrefix(diff.Files[0].Patch, "@@") || !strings.Contains(diff.Files[0].Patch, "+one\n") {
		t.Fatalf("diff = %+v %v", diff, err)
	}

	// The merge names the head: another commit is a moved head, the head merges.
	var moved *tools.ErrHeadMoved
	if err := rp.Merge(ctx, 1, strings.Repeat("0", 40)); !errors.As(err, &moved) || !strings.Contains(moved.Message, d.HeadSHA) {
		t.Fatalf("merge with another sha: %v", err)
	}
	if err := rp.Merge(ctx, 1, d.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if ok, err := rp.CIConfigured(ctx, "main"); err != nil || !ok {
		t.Fatalf("main has a pipeline on a recent commit: %v %v", ok, err)
	}
}

// A request that conflicts with its target says so (detailed_merge_status conflict).
func TestConflictingRequestStatus(t *testing.T) {
	_, pc := forge(t)
	ctx := context.Background()
	rp := tools.ProviderFor(&pc, nil)
	push(t, pc, "feature/x", map[string]string{"shared.txt": "x\n"})
	push(t, pc, "feature/y", map[string]string{"shared.txt": "y\n"})
	for _, b := range []string{"feature/x", "feature/y"} {
		if _, err := rp.Create(ctx, b, "main", b, "", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := rp.Merge(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	st, err := rp.Status(ctx, 2)
	if err != nil || st.Mergeable == nil || *st.Mergeable || st.MergeStatus != "conflict" {
		t.Fatalf("status = %+v %v", st, err)
	}
}
