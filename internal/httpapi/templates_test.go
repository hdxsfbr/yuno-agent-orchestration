package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
	"yuno/internal/workflow"
)

// writeTemplate writes a template file naming agents by name.
func writeTemplate(t *testing.T, dir, key, name string, agentNames ...string) {
	t.Helper()
	nodes := []map[string]any{{"id": "start", "kind": "start"}}
	edges := []map[string]any{}
	prev := "start"
	for i, an := range agentNames {
		id := "n" + string(rune('a'+i))
		nodes = append(nodes, map[string]any{"id": id, "kind": "agent", "agent_name": an,
			"task": "Work on {{.input.task}} after {{.state." + prev + ".summary}}"})
		edges = append(edges, map[string]any{"from": prev, "to": id})
		prev = id
	}
	nodes = append(nodes, map[string]any{"id": "end", "kind": "end"})
	edges = append(edges, map[string]any{"from": prev, "to": "end"})
	b, _ := json.MarshalIndent(map[string]any{"key": key, "name": name, "description": "test template",
		"definition": map[string]any{"nodes": nodes, "edges": edges, "ui": map[string]any{"zoom": 1}}}, "", "  ")
	mustDo(t, os.WriteFile(filepath.Join(dir, key+".json"), b, 0o644))
}

func uniqueSuffix() string { return strings.ReplaceAll(store.NewID(), "-", "")[20:] }

// Templates load into workflow rows: created, then unchanged, then updated in place
// when the file changes; an invalid file is skipped.
func TestLoadTemplates(t *testing.T) {
	pool := needDB(t)
	dir := t.TempDir()
	sfx := uniqueSuffix()
	key := "t_load_" + sfx
	writeTemplate(t, dir, key, "Load test", "Coder")
	mustDo(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"key":"t_broken_`+sfx+`","name":"x","definition":{"nodes":[]}}`), 0o644))
	mustDo(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a template"), 0o644))
	s, err := New(Deps{Cfg: testConfig(), Pool: pool, Log: slog.New(slog.DiscardHandler), TemplatesDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustDo(t, s.LoadTemplates(ctx))
	first, err := store.GetWorkflowByTemplateKey(ctx, pool, key)
	if err != nil || first.Name != "Load test" || first.Version != 1 {
		t.Fatalf("created: %+v %v", first, err)
	}
	if _, err := store.GetWorkflowByTemplateKey(ctx, pool, "t_broken_"+sfx); err == nil {
		t.Fatal("an invalid template was loaded")
	}
	mustDo(t, s.LoadTemplates(ctx))
	if again, _ := store.GetWorkflowByTemplateKey(ctx, pool, key); again.Version != 1 {
		t.Fatalf("an unchanged file updated the row (version %d)", again.Version)
	}
	writeTemplate(t, dir, key, "Load test", "Coder", "Reviewer")
	mustDo(t, s.LoadTemplates(ctx))
	updated, _ := store.GetWorkflowByTemplateKey(ctx, pool, key)
	if updated.ID != first.ID || updated.Version != 2 || !strings.Contains(string(updated.Definition), "Reviewer") {
		t.Fatalf("updated in place: %+v", updated)
	}
}

// The shipped templates load and pass every save-time check (agent names aside).
func TestShippedTemplatesAreValid(t *testing.T) {
	pool := needDB(t)
	s, err := New(Deps{Cfg: testConfig(), Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../templates/*.json")
	if len(files) < 2 {
		t.Fatalf("found %d templates", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		mustDo(t, err)
		var tpl workflowTemplate
		mustDo(t, json.Unmarshal(raw, &tpl))
		if !templateKeyPattern.MatchString(tpl.Key) {
			t.Errorf("%s: key %q", f, tpl.Key)
		}
		fe, err := s.checkDefinition(context.Background(), tpl.Definition, definitionRules{})
		if err != nil || len(fe) > 0 {
			t.Errorf("%s: %v %+v", f, err, fe)
		}
		def, _ := workflow.Parse(tpl.Definition)
		for i, n := range def.Nodes {
			if n.Kind == workflow.KindAgent && (n.AgentName == "" || n.AgentID != "") {
				t.Errorf("%s nodes[%d]: templates name agents by agent_name", f, i)
			}
		}
	}
}

// T5.6.10: instantiate resolves agent names case-insensitively and reports a missing
// one at its node.
func TestInstantiateTemplate(t *testing.T) {
	pool := needDB(t)
	dir := t.TempDir()
	sfx := uniqueSuffix()
	builder := testAgent(t, "builder-"+sfx)
	checker := testAgent(t, "Checker-"+sfx)
	testAgent(t, "BUILDER-"+sfx) // a younger agent with the same name loses to the oldest
	okKey, missingKey := "t_ok_"+sfx, "t_missing_"+sfx
	writeTemplate(t, dir, okKey, "Build and check", "Builder-"+sfx, "checker-"+sfx)
	writeTemplate(t, dir, missingKey, "Missing agent", "Builder-"+sfx, "Nobody-"+sfx, "Ghost-"+sfx)
	s, ts := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool, TemplatesDir: dir}, &fakeEngine{pool: pool})
	mustDo(t, s.LoadTemplates(context.Background()))

	code, body := do(t, ts, http.MethodGet, "/api/workflow-templates", "")
	var tpls []workflowTemplate
	if code != http.StatusOK || json.Unmarshal(body, &tpls) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	if !slices.ContainsFunc(tpls, func(w workflowTemplate) bool { return w.Key == okKey && w.Name == "Build and check" }) ||
		slices.ContainsFunc(tpls, func(w workflowTemplate) bool { return strings.HasPrefix(w.Key, "quickrun:") }) {
		t.Fatalf("templates = %s", body)
	}

	code, body = do(t, ts, http.MethodPost, "/api/workflow-templates/"+okKey+"/instantiate", `{"name":"My pipeline"}`)
	var wf store.Workflow
	if code != http.StatusCreated || json.Unmarshal(body, &wf) != nil || wf.Name != "My pipeline" || wf.TemplateKey != nil || wf.Version != 1 {
		t.Fatalf("instantiate: %d %s", code, body)
	}
	def, _ := workflow.Parse(wf.Definition)
	if def.Nodes[1].AgentID != builder || def.Nodes[2].AgentID != checker || def.Nodes[1].AgentName != "" ||
		strings.Contains(string(wf.Definition), "agent_name") || !strings.Contains(string(wf.Definition), `"zoom"`) {
		t.Fatalf("definition = %s", wf.Definition)
	}
	if code, _ = do(t, ts, http.MethodPost, "/api/workflow-templates/"+okKey+"/instantiate", ""); code != http.StatusCreated {
		t.Fatalf("instantiate without a body: %d", code)
	}

	code, body = do(t, ts, http.MethodPost, "/api/workflow-templates/"+missingKey+"/instantiate", `{}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("missing agent: %d %s", code, body)
	}
	var details []fieldError
	_ = json.Unmarshal(decodeError(t, body).Error.Details, &details)
	want := []fieldError{
		{Path: "nodes[2].agent_name", Message: "no agent named 'Nobody-" + sfx + "'"},
		{Path: "nodes[3].agent_name", Message: "no agent named 'Ghost-" + sfx + "'"},
	}
	if !slices.Equal(details, want) {
		t.Fatalf("details = %+v, want %+v", details, want)
	}
	for _, k := range []string{"no_such_template", "quickrun:" + builder} {
		if code, _ := do(t, ts, http.MethodPost, "/api/workflow-templates/"+k+"/instantiate", ""); code != http.StatusNotFound {
			t.Fatalf("%s: %d", k, code)
		}
	}
}
