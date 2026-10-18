package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"yuno/internal/project"
	"yuno/internal/store"
)

// DESIGN 5.20 edge cases: a later open_merge_request call in a run whose request was
// merged or closed is refused before anything is pushed, with the provider's own
// wording for the request.
func TestUpdateRecordedRefusesAMergedOrClosedRequest(t *testing.T) {
	m := &mergeRequest{}
	for _, tc := range []struct{ provider, state, want string }{
		{"gitlab", store.MergeRequestMerged, "the run's merge request !12 is merged; nothing more can be pushed to it"},
		{"github", store.MergeRequestClosed, "the run's pull request #12 is closed; nothing more can be pushed to it"},
	} {
		rec := &store.MergeRequest{Number: 12, Provider: tc.provider, State: tc.state, SourceBranch: "yuno/abc"}
		_, err := m.updateRecorded(context.Background(), &project.Config{Provider: project.Provider(tc.provider)}, rec, mergeRequestInput{Title: "t"}, t.TempDir())
		var me *ModelError
		if !errors.As(err, &me) || me.Message != tc.want {
			t.Fatalf("%s %s: err = %v, want model error %q", tc.provider, tc.state, err, tc.want)
		}
		if strings.Contains(me.Message, "glpat") {
			t.Fatal("a token leaked into the message")
		}
	}
}
