package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yuno/internal/engine"
	"yuno/internal/openclaw"
	"yuno/internal/store"
)

type fileView struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Exists    bool            `json:"exists"`
	Content   string          `json:"content"`
	Truncated bool            `json:"truncated"`
	Addition  json.RawMessage `json:"addition"`
	Override  json.RawMessage `json:"override"`
	UpdatedAt string          `json:"updated_at"`
}

// stubGateway answers the calls Materialize makes with an agent that already exists.
type stubGateway struct{ fakeGateway }

func (g *stubGateway) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "agents.list":
		return json.RawMessage(`{"agents":[{"id":"main"},{"id":"` + g.id() + `"}]}`), nil
	case "config.get":
		return json.RawMessage(`{"hash":"h","config":{}}`), nil
	case "exec.approvals.get":
		return json.RawMessage(`{"exists":true,"hash":"e","file":{"version":1,"agents":{}}}`), nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func (g *stubGateway) id() string { return g.info.Version }

// writingEngine is the fake engine whose MaterializeAgent also does what the real one
// does to the workspace: it composes the agent's edits into the files.
type writingEngine struct {
	*fakeEngine
	stateDir string
}

func (w *writingEngine) MaterializeAgent(ctx context.Context, id string) (*engine.RuntimeView, error) {
	view, err := w.fakeEngine.MaterializeAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	a, err := store.GetAgent(ctx, w.fakeEngine.pool, id)
	if err != nil {
		return nil, err
	}
	ex, err := openclaw.LoadExtras(ctx, w.fakeEngine.pool, id, nil)
	if err != nil {
		return nil, err
	}
	a.MaterializedHash = ""
	gw := &stubGateway{}
	gw.info.Version = *a.OpenClawAgentID
	if _, err := openclaw.MaterializeWith(ctx, gw, w.stateDir, a, nil, ex); err != nil {
		return nil, err
	}
	return view, nil
}

type filesFixture struct {
	ts    *httptest.Server
	agent *store.Agent
	ws    string
	fe    *fakeEngine
}

func newFilesFixture(t *testing.T, materialized bool) *filesFixture {
	t.Helper()
	pool := needDB(t)
	fe := &fakeEngine{pool: pool}
	cfg := testConfig()
	cfg.OpenClawStateDir = t.TempDir()
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: adminGateway()}, &writingEngine{fakeEngine: fe, stateDir: cfg.OpenClawStateDir})
	a := &store.Agent{Name: "Files Agent", Model: "claude-opus-5-5", ToolAllowlist: []string{"exec"}}
	if err := store.CreateAgent(context.Background(), pool, a); err != nil {
		t.Fatal(err)
	}
	f := &filesFixture{ts: ts, agent: a, fe: fe}
	if materialized {
		oc := "t-" + a.ID
		if err := store.SetAgentMaterialized(context.Background(), pool, a.ID, oc, "h0"); err != nil {
			t.Fatal(err)
		}
		f.ws = openclaw.WorkspaceDir(cfg.OpenClawStateDir, oc)
		if err := os.MkdirAll(f.ws, 0o755); err != nil {
			t.Fatal(err)
		}
		f.agent, _ = store.GetAgent(context.Background(), pool, a.ID)
	}
	return f
}

func (f *filesFixture) list(t *testing.T) map[string]fileView {
	t.Helper()
	code, body := do(t, f.ts, http.MethodGet, "/api/agents/"+f.agent.ID+"/files", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var vs []fileView
	if err := json.Unmarshal(body, &vs); err != nil {
		t.Fatal(err)
	}
	out := map[string]fileView{}
	for _, v := range vs {
		out[v.Name] = v
	}
	if len(out) != len(vs) {
		t.Fatalf("duplicate names in %s", body)
	}
	return out
}

func (f *filesFixture) put(t *testing.T, name, body string) (int, []byte) {
	t.Helper()
	return do(t, f.ts, http.MethodPut, "/api/agents/"+f.agent.ID+"/files/"+name, body)
}

// T5.27.1: the list has every name with its kind, the disk content redacted (chat token
// included), the truncation flag, and the null edit fields.
func TestAgentFilesList(t *testing.T) {
	f := newFilesFixture(t, true)
	token := f.agent.ChatToken
	if token == "" {
		t.Fatal("agent has no chat token")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(f.ws, "AGENTS.md"), []byte("rules\npass "+token+" and sk-ant-api03-abcdefghijklmnop\n"), 0o644))
	must(os.WriteFile(filepath.Join(f.ws, "IDENTITY.md"), []byte("I am Files Agent"), 0o644))
	must(os.WriteFile(filepath.Join(f.ws, "TOOLS.md"), []byte(strings.Repeat("a", 300<<10)), 0o644))
	// A symlink out of the workspace is never followed.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	must(os.WriteFile(outside, []byte("outside"), 0o644))
	must(os.Symlink(outside, filepath.Join(f.ws, "USER.md")))

	got := f.list(t)
	want := map[string]string{"AGENTS.md": "generated", "SOUL.md": "generated", "MEMORY.md": "generated",
		"IDENTITY.md": "runtime", "USER.md": "runtime", "BOOTSTRAP.md": "runtime", "HEARTBEAT.md": "runtime", "TOOLS.md": "runtime", "DREAMS.md": "runtime"}
	if len(got) != len(want) {
		t.Fatalf("names = %v", got)
	}
	for n, k := range want {
		if got[n].Kind != k {
			t.Fatalf("%s kind = %q", n, got[n].Kind)
		}
		if k == "generated" && string(got[n].Addition) != "null" || k == "runtime" && string(got[n].Override) != "null" {
			t.Fatalf("%s edit fields: %s %s", n, got[n].Addition, got[n].Override)
		}
	}
	ag := got["AGENTS.md"]
	if !ag.Exists || strings.Contains(ag.Content, token) || strings.Contains(ag.Content, "sk-ant-api03") || !strings.Contains(ag.Content, "[redacted]") {
		t.Fatalf("AGENTS.md not redacted: %q", ag.Content)
	}
	if v := got["IDENTITY.md"]; !v.Exists || v.Content != "I am Files Agent" {
		t.Fatalf("IDENTITY.md = %+v", v)
	}
	if v := got["SOUL.md"]; v.Exists || v.Content != "" {
		t.Fatalf("SOUL.md = %+v", v)
	}
	if v := got["TOOLS.md"]; !v.Truncated || len(v.Content) != 256<<10 {
		t.Fatalf("TOOLS.md truncated=%v len=%d", v.Truncated, len(v.Content))
	}
	if v := got["USER.md"]; v.Exists || strings.Contains(v.Content, "outside") {
		t.Fatalf("symlink followed: %+v", v)
	}
}

// T5.27.2 and T5.27.3 over HTTP: an addition and an override are stored, re-materialised
// and listed; clearing them behaves as the rules say.
func TestAgentFilesPut(t *testing.T) {
	f := newFilesFixture(t, true)
	code, body := f.put(t, "AGENTS.md", `{"addition":"Greet in Spanish."}`)
	if code != http.StatusOK {
		t.Fatalf("put addition: %d %s", code, body)
	}
	var v fileView
	_ = json.Unmarshal(body, &v)
	if v.Name != "AGENTS.md" || !v.Exists || string(v.Addition) != `"Greet in Spanish."` || v.UpdatedAt == "" ||
		!strings.HasPrefix(v.Content, openclaw.AgentsMD) || !strings.HasSuffix(v.Content, "\n\n## Operator additions\n\nGreet in Spanish.") {
		t.Fatalf("addition view = %s", body)
	}
	if len(f.fe.materialized) != 1 {
		t.Fatalf("materialised %d times", len(f.fe.materialized))
	}
	if got := f.list(t)["AGENTS.md"]; string(got.Addition) != `"Greet in Spanish."` {
		t.Fatalf("list addition = %s", got.Addition)
	}

	// Empty addition removes the section.
	if code, body = f.put(t, "AGENTS.md", `{"addition":""}`); code != http.StatusOK {
		t.Fatalf("clear addition: %d %s", code, body)
	}
	if got := f.list(t)["AGENTS.md"]; got.Content != openclaw.AgentsMD || string(got.Addition) != "null" {
		t.Fatalf("after clear = %+v", got)
	}

	// Runtime override: written; null stops the platform writing and keeps the text.
	if code, body = f.put(t, "IDENTITY.md", `{"override":"I am the operator's"}`); code != http.StatusOK {
		t.Fatalf("put override: %d %s", code, body)
	}
	if got := f.list(t)["IDENTITY.md"]; !got.Exists || got.Content != "I am the operator's" || string(got.Override) != `"I am the operator's"` {
		t.Fatalf("override view = %+v", got)
	}
	if code, body = f.put(t, "IDENTITY.md", `{"override":null}`); code != http.StatusOK {
		t.Fatalf("remove override: %d %s", code, body)
	}
	if err := os.WriteFile(filepath.Join(f.ws, "IDENTITY.md"), []byte("openclaw's own"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body = f.put(t, "SOUL.md", `{"addition":"x"}`); code != http.StatusOK {
		t.Fatalf("put soul: %d %s", code, body)
	}
	if got := f.list(t)["IDENTITY.md"]; got.Content != "openclaw's own" || string(got.Override) != "null" {
		t.Fatalf("after override removal = %+v", got)
	}
}

// T5.27.4: unknown names, non-UTF-8 bodies, the wrong field for the kind and oversize
// edits are refused and store nothing; an unknown agent is 404.
func TestAgentFilesPutRefusals(t *testing.T) {
	f := newFilesFixture(t, true)
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"NOPE.md", `{"addition":"x"}`, http.StatusBadRequest},
		{"../AGENTS.md", `{"addition":"x"}`, http.StatusNotFound},
		{"AGENTS.md", "{\"addition\":\"bad \xff\xfe\"}", http.StatusBadRequest},
		{"AGENTS.md", `{"override":"x"}`, http.StatusBadRequest},
		{"IDENTITY.md", `{"addition":"x"}`, http.StatusBadRequest},
		{"AGENTS.md", `{"addition":null}`, http.StatusBadRequest},
		{"AGENTS.md", `{"addition":"x","override":null}`, http.StatusBadRequest},
		{"AGENTS.md", `[]`, http.StatusBadRequest},
		{"AGENTS.md", `{"addition":"` + strings.Repeat("a", 64<<10+1) + `"}`, http.StatusRequestEntityTooLarge},
		{"IDENTITY.md", `{"override":"` + strings.Repeat("a", 64<<10+1) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		code, body := f.put(t, c.name, c.body)
		if code != c.want {
			t.Fatalf("PUT %s %.40q: %d %s, want %d", c.name, c.body, code, body, c.want)
		}
	}
	// The limit itself is accepted.
	if code, body := f.put(t, "SOUL.md", `{"addition":"`+strings.Repeat("a", 64<<10)+`"}`); code != http.StatusOK {
		t.Fatalf("64 KiB: %d %.200s", code, body)
	}
	if code, _ := do(t, f.ts, http.MethodPut, "/api/agents/00000000-0000-7000-8000-000000000000/files/AGENTS.md", `{"addition":"x"}`); code != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", code)
	}
	edits, _ := store.GetAgentFileEdits(context.Background(), testPool, f.agent.ID)
	if len(edits) != 1 || edits["SOUL.md"].Addition == nil {
		t.Fatalf("refused edits were stored: %+v", edits)
	}
}

func TestAgentFilesNotMaterialized(t *testing.T) {
	f := newFilesFixture(t, false)
	code, body := do(t, f.ts, http.MethodGet, "/api/agents/"+f.agent.ID+"/files", "")
	if code != http.StatusConflict || decodeError(t, body).Error.Code != "not_materialized" {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, body = f.put(t, "AGENTS.md", `{"addition":"x"}`); code != http.StatusConflict {
		t.Fatalf("put: %d %s", code, body)
	}
	if edits, _ := store.GetAgentFileEdits(context.Background(), testPool, f.agent.ID); len(edits) != 0 {
		t.Fatalf("stored without a workspace: %+v", edits)
	}
}

// A failed materialise answers 502 with the edit stored.
func TestAgentFilesPutMaterializeFailed(t *testing.T) {
	pool := needDB(t)
	fe := &fakeEngine{pool: pool, materializeErr: errors.New("config.patch: boom")}
	cfg := testConfig()
	cfg.OpenClawStateDir = t.TempDir()
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg, Pool: pool, Gateway: adminGateway()}, fe)
	a := &store.Agent{Name: "Failing", Model: "claude-opus-5-5"}
	if err := store.CreateAgent(context.Background(), pool, a); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAgentMaterialized(context.Background(), pool, a.ID, "t-"+a.ID, "h0"); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, ts, http.MethodPut, "/api/agents/"+a.ID+"/files/AGENTS.md", `{"addition":"kept"}`)
	if code != http.StatusBadGateway || decodeError(t, body).Error.Code != "materialize_failed" {
		t.Fatalf("put: %d %s", code, body)
	}
	edits, _ := store.GetAgentFileEdits(context.Background(), pool, a.ID)
	if e := edits["AGENTS.md"]; e.Addition == nil || *e.Addition != "kept" {
		t.Fatalf("edit not stored: %+v", edits)
	}
}

// Finding 2 and 11 (backend): a secret that straddles the read limit is never half shown,
// and redacted says when redaction changed the content.
func TestAgentFilesRedactedFlagAndStraddlingSecret(t *testing.T) {
	f := newFilesFixture(t, true)
	const limit = agentFileReadMax + 1 + 1024
	early := strings.Repeat("sk-ant-"+strings.Repeat("x", 60)+"\n", 100) // shrinks by far more than 1 KiB
	body := early + strings.Repeat("a", limit-12-len(early)) + "sk-ant-abcde" + "fghijklmnopqrstuvwxyz" + strings.Repeat("b", 100)
	if err := os.WriteFile(filepath.Join(f.ws, "TOOLS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ws, "AGENTS.md"), []byte("key sk-ant-api03-abcdefghijklmnop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ws, "IDENTITY.md"), []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, raw := do(t, f.ts, http.MethodGet, "/api/agents/"+f.agent.ID+"/files", "")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	var vs []struct {
		Name      string `json:"name"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
		Redacted  *bool  `json:"redacted"`
	}
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatal(err)
	}
	by := map[string]int{}
	for i, v := range vs {
		by[v.Name] = i
		if v.Redacted == nil {
			t.Fatalf("%s has no redacted field", v.Name)
		}
	}
	tools := vs[by["TOOLS.md"]]
	if strings.Contains(tools.Content, "sk-ant") || !tools.Truncated || !*tools.Redacted {
		t.Fatalf("TOOLS.md: sk-ant shown=%v truncated=%v redacted=%v", strings.Contains(tools.Content, "sk-ant"), tools.Truncated, *tools.Redacted)
	}
	if v := vs[by["AGENTS.md"]]; !*v.Redacted || strings.Contains(v.Content, "sk-ant") {
		t.Fatalf("AGENTS.md = %+v", v)
	}
	if v := vs[by["IDENTITY.md"]]; *v.Redacted || v.Content != "plain text" {
		t.Fatalf("IDENTITY.md = %+v", v)
	}
	if v := vs[by["SOUL.md"]]; *v.Redacted {
		t.Fatalf("a missing file is not redacted: %+v", v)
	}
}
