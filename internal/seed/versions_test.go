package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"

	"yuno/internal/store"
)

// DESIGN 5.35 rule 1: every workflow the seed and the template loader write (the template
// rows, the copies made from them, an upgrade of a copy and the reset) has a version for
// each version it reached, the newest holding its definition, written by the platform.
func TestSeedWritesWorkflowVersions(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	_, _, err := Seed(ctx, pool)
	must(t, err)
	ts := newTemplateServer(t, pool)
	_, _, err = Templates(ctx, pool, ts.sh, log)
	must(t, err)
	ts.change(t, "wave_pipeline")
	_, upgraded, err := Templates(ctx, pool, ts.sh, log)
	if err != nil || !slices.Equal(upgraded, []string{"Wave pipeline"}) {
		t.Fatalf("upgrade: %v %v", upgraded, err)
	}
	_, _, err = ResetShipped(ctx, pool, ts.sh)
	must(t, err)

	all, err := store.ListWorkflows(ctx, pool)
	must(t, err)
	if len(all) != 6 {
		t.Fatalf("%d workflows, want the three templates and their copies", len(all))
	}
	for _, wf := range all {
		list, err := store.ListWorkflowVersions(ctx, pool, wf.ID)
		must(t, err)
		got := make([]int, len(list))
		for i, v := range list {
			got[i] = v.Version
		}
		var want []int
		for v := wf.Version; v >= 1; v-- {
			want = append(want, v)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: versions %v, want %v", wf.Name, got, want)
			continue
		}
		latest, err := store.GetWorkflowVersion(ctx, pool, wf.ID, wf.Version)
		must(t, err)
		var a, b bytes.Buffer
		must(t, json.Compact(&a, latest.Definition))
		must(t, json.Compact(&b, wf.Definition))
		var by map[string]string
		must(t, json.Unmarshal(latest.CreatedBy, &by))
		if a.String() != b.String() || latest.Name != wf.Name || len(by) != 1 || by["kind"] != "platform" {
			t.Errorf("%s: latest version %+v does not match the workflow", wf.Name, latest.WorkflowVersionInfo)
		}
	}
	if wave := copyOf(t, pool, "Wave pipeline"); wave.Version != 3 {
		t.Fatalf("wave pipeline copy at version %d: instantiated, upgraded and reset", wave.Version)
	}
}
