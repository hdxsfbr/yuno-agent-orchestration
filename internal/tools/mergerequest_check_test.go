package tools

import (
	"context"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// Chief platform control, DESIGN wave 3 rule 11: a push of open_merge_request onto a
// recorded request moves the record to the pushed commit with its CI pending, so a pass on
// the old head never stands for the new one; a call that pushes nothing leaves it.
func TestOpenMergeRequestPushInvalidatesTheCheck(t *testing.T) {
	e := newRecordEnv(t, project.ProviderGitLab)
	rec := e.openFirst()
	old := e.tip(rec.SourceBranch)
	_, err := testPool.Exec(context.Background(), `UPDATE merge_requests SET ci_status = 'passed', head_sha = $2, ci_sha = $2,
		ci_pipeline_id = 9, ci_url = 'https://gitlab.test/p/9' WHERE id = $1`, rec.ID, old)
	must(t, err)

	u := e.newUpdater(rec, nil)
	u.edit(t, "README.md", "# demo\nv1\nreviewed\n")
	out, got, err := e.call(u, 'a', "Address the review")
	if err != nil || out.Result.IsError || !got.Updated {
		t.Fatalf("call = %+v %v", out, err)
	}
	tip := e.tip(rec.SourceBranch)
	after := e.record()
	if tip == old || after.HeadSHA == nil || *after.HeadSHA != tip || after.CISHA == nil || *after.CISHA != tip ||
		after.CIStatus != store.CIStatusPending || after.CIPipelineID != nil || after.CIURL != nil || after.UpdatedFromDefaultAt != nil {
		t.Fatalf("record after the push = %+v (tip %s)", after, tip)
	}

	// Nothing to push: the record stays as the push left it.
	_, err = testPool.Exec(context.Background(), `UPDATE merge_requests SET ci_status = 'passed' WHERE id = $1`, rec.ID)
	must(t, err)
	if out, got, err := e.call(u, 'b', "Again"); err != nil || out.Result.IsError || got.Updated {
		t.Fatalf("second call = %+v %v", out, err)
	}
	if again := e.record(); again.CIStatus != store.CIStatusPassed || *again.HeadSHA != tip {
		t.Fatalf("a call that pushed nothing changed the record: %+v", again)
	}
}
