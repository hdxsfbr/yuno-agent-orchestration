package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"yuno/internal/actor"
)

// sameDoc reports whether two JSON documents are equal whatever their key order.
func sameDoc(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	mustDo(t, json.Unmarshal(a, &x))
	mustDo(t, json.Unmarshal(b, &y))
	return reflect.DeepEqual(x, y)
}

// versionNumbers lists the workflow's version numbers as ListWorkflowVersions returns them.
func versionNumbers(t *testing.T, ctx context.Context, q Querier, id string) []int {
	t.Helper()
	list, err := ListWorkflowVersions(ctx, q, id)
	mustDo(t, err)
	out := make([]int, len(list))
	for i, v := range list {
		out[i] = v.Version
	}
	return out
}

// DESIGN 5.35 rule 1: migration 0031 records each workflow's current version and every older
// version a run holds, from its snapshot without the run's overrides, as a backfill. When
// a version's runs hold different snapshots, the one most runs hold wins; the current
// version always comes from the workflow row.
func TestWorkflowVersionsBackfill(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const schema = "yuno_store_w4_versions_test"
	base, err := Open(ctx, url)
	mustDo(t, err)
	_, err = base.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE; CREATE SCHEMA `+schema)
	base.Close()
	mustDo(t, err)
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	pool, err := Open(ctx, url+sep+"search_path="+schema)
	mustDo(t, err)
	defer pool.Close()
	applyBefore(t, ctx, pool, 31)

	const (
		current = `{"nodes":[],"edges":[],"v":3}`
		first   = `{"nodes":[],"edges":[],"v":1}`
		other   = `{"nodes":[],"edges":[],"v":1,"limits":{"max_steps":3}}`
	)
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, description, definition, version, updated_at) VALUES
			('wf-a', 'Pipeline', 'd', '` + current + `', 3, '2026-10-20T10:00:00Z'),
			('wf-quiet', 'Quiet', '', '{"nodes":[]}', 1, '2026-10-01T00:00:00Z'),
			('wf-gone', 'Gone', '', '{"nodes":[],"v":2}', 2, '2026-10-02T00:00:00Z')`,
		`UPDATE workflows SET deleted_at = now() WHERE id = 'wf-gone'`,
		// Version 1 of wf-a: two runs hold the same definition (one with run overrides),
		// an earlier one ran it with its own limits.
		`INSERT INTO runs (id, workflow_id, workflow_version, workflow_snapshot, trigger, status, limits, created_at) VALUES
			('r1', 'wf-a', 1, '` + first + `', 'manual', 'succeeded', '{}', '2026-10-10T10:00:00Z'),
			('r2', 'wf-a', 1, '{"nodes":[],"edges":[],"v":1,"run_overrides":{"model":"scripted/scripted-1"}}', 'eval', 'failed', '{}', '2026-10-11T10:00:00Z'),
			('r0', 'wf-a', 1, '` + other + `', 'manual', 'succeeded', '{}', '2026-10-09T10:00:00Z'),
			('r3', 'wf-a', 3, '{"nodes":[],"edges":[],"v":"as run"}', 'manual', 'running', '{}', '2026-10-21T10:00:00Z'),
			('r4', 'wf-gone', 1, '{"nodes":[],"v":1}', 'manual', 'succeeded', '{}', '2026-10-01T12:00:00Z')`,
	} {
		_, err = pool.Exec(ctx, stmt)
		mustDo(t, err)
	}

	mustDo(t, Migrate(ctx, pool, slog.New(slog.DiscardHandler)))

	for id, want := range map[string][]int{"wf-a": {3, 1}, "wf-quiet": {1}, "wf-gone": {2, 1}} {
		if got := versionNumbers(t, ctx, pool, id); !slices.Equal(got, want) {
			t.Errorf("%s: versions %v, want %v", id, got, want)
		}
	}
	v3, err := GetWorkflowVersion(ctx, pool, "wf-a", 3)
	mustDo(t, err)
	if !sameDoc(t, v3.Definition, json.RawMessage(current)) || v3.Name != "Pipeline" || v3.Description != "d" ||
		!v3.CreatedAt.Equal(mustParse(t, "2026-10-20T10:00:00Z")) || !sameDoc(t, v3.CreatedBy, json.RawMessage(`{"kind":"backfill"}`)) {
		t.Fatalf("current version = %+v %s", v3, v3.Definition)
	}
	v1, err := GetWorkflowVersion(ctx, pool, "wf-a", 1)
	mustDo(t, err)
	if !sameDoc(t, v1.Definition, json.RawMessage(first)) || v1.Name != "Pipeline" ||
		!v1.CreatedAt.Equal(mustParse(t, "2026-10-10T10:00:00Z")) || !sameDoc(t, v1.CreatedBy, json.RawMessage(`{"kind":"backfill"}`)) {
		t.Fatalf("version 1 = %+v %s", v1, v1.Definition)
	}
	if _, err := GetWorkflowVersion(ctx, pool, "wf-a", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version 2 had no run and no row: %v", err)
	}

	// After the migration every write adds its version.
	wf, err := GetWorkflow(ctx, pool, "wf-a")
	mustDo(t, err)
	wf.Definition = json.RawMessage(`{"nodes":[],"edges":[],"v":4}`)
	mustDo(t, UpdateWorkflow(ctx, pool, wf))
	if got := versionNumbers(t, ctx, pool, "wf-a"); wf.Version != 4 || !slices.Equal(got, []int{4, 3, 1}) {
		t.Fatalf("after an update: version %d, versions %v", wf.Version, got)
	}
}

// DESIGN 5.35 rule 1: create and every update write the version in the same statement, with
// the request's actor as created_by, or the platform without one. A conditional update
// writes nothing when the workflow moved on.
func TestWorkflowVersionAuthor(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	wf := &Workflow{Name: "authored", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	v1, err := GetWorkflowVersion(ctx, pool, wf.ID, 1)
	mustDo(t, err)
	if !sameDoc(t, v1.CreatedBy, json.RawMessage(`{"kind":"platform"}`)) || !sameDoc(t, v1.Definition, wf.Definition) || v1.Name != "authored" {
		t.Fatalf("version 1 = %+v", v1)
	}

	chief := actor.Actor{Kind: actor.Agent, ID: "ag-1", Name: "Chief of Staff", Via: actor.ViaChief, OnBehalfOf: "telegram:42"}
	wf.Name, wf.Definition = "authored 2", json.RawMessage(`{"nodes":[],"edges":[],"x":2}`)
	mustDo(t, UpdateWorkflow(actor.With(ctx, chief), pool, wf))
	v2, err := GetWorkflowVersion(ctx, pool, wf.ID, 2)
	mustDo(t, err)
	var by actor.Actor
	mustDo(t, json.Unmarshal(v2.CreatedBy, &by))
	if by != chief || v2.Name != "authored 2" || !sameDoc(t, v2.Definition, wf.Definition) {
		t.Fatalf("version 2 = %+v by %+v", v2, by)
	}

	operator := actor.With(ctx, actor.Actor{Kind: actor.Operator, Via: actor.ViaUI})
	wf.Definition = json.RawMessage(`{"nodes":[],"edges":[],"x":3}`)
	if err := UpdateWorkflowIfVersion(operator, pool, wf, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	mustDo(t, UpdateWorkflowIfVersion(operator, pool, wf, 2))
	if got := versionNumbers(t, ctx, pool, wf.ID); wf.Version != 3 || !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("versions %v (workflow at %d)", got, wf.Version)
	}
	mustDo(t, DeleteWorkflow(ctx, pool, wf.ID))
	if err := UpdateWorkflowIfVersion(operator, pool, wf, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a deleted workflow: %v", err)
	}
	if err := UpdateWorkflow(ctx, pool, wf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plain update of a deleted workflow: %v", err)
	}
	if got := versionNumbers(t, ctx, pool, wf.ID); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("a refused update wrote a version: %v", got)
	}
}
