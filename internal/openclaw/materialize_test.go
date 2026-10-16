package openclaw

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"yuno/internal/store"
)

func testAgent() *store.Agent {
	return &store.Agent{
		ID:               "0199f0a2-7c1e-7d3a-9b2c-4e5f6a7b8c9d",
		Name:             "Quick Coder",
		Role:             "Writes and tests Python.",
		SystemPrompt:     "You are careful.",
		Model:            "claude-opus-5-5",
		Thinking:         "low",
		ToolAllowlist:    []string{"exec", "write", "finish", "http_fetch"},
		InteractionRules: map[string]string{"deploy_release": "blocked", "write": "auto"},
		Guardrails:       store.Guardrails{BlockedTools: []string{"browser", "http_fetch"}},
		ExecPolicy:       store.ExecPolicy{Allowlist: []string{"/usr/bin/python3", "python3 -m pytest"}},
	}
}

type matEnv struct {
	f     *fakeGateway
	state *fakeOpenClawState
	gw    *Gateway
	dir   string
}

func newMatEnv(t *testing.T) *matEnv {
	t.Helper()
	f := newFakeGateway(t, testToken)
	st := newFakeOpenClawState()
	st.install(f)
	gw, _ := dialFake(t, f, DialConfig{})
	return &matEnv{f: f, state: st, gw: gw, dir: t.TempDir()}
}

func (m *matEnv) entry(t *testing.T, id string) map[string]any {
	t.Helper()
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	e, _ := m.state.config["agents"].(map[string]any)["entries"].(map[string]any)[id].(map[string]any)
	b, _ := json.Marshal(e)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func writeCalls(f *fakeGateway) []string {
	var out []string
	for _, c := range f.allCalls() {
		switch c.Method {
		case "agents.create", "config.patch", "exec.approvals.set":
			out = append(out, c.Method)
		}
	}
	return out
}

func TestDeriveAgentID(t *testing.T) {
	a := DeriveAgentID("0199f0a2-7c1e-7d3a-9b2c-4e5f6a7b8c9d")
	b := DeriveAgentID("0199f0a2-7c1e-7d3a-9b2c-000000000001")
	if a != "ag5f6a7b8c9d" || len(a) != 12 {
		t.Fatalf("id = %q", a)
	}
	if a == b {
		t.Fatal("agents created in the same millisecond must not share an id")
	}
}

func TestToolListsDerivation(t *testing.T) {
	allow, deny := toolLists(testAgent())
	if want := []string{"exec", "write", "yuno__finish"}; !slices.Equal(allow, want) {
		t.Fatalf("allow = %v, want %v", allow, want)
	}
	// Memory is off for this agent, so its tools are denied too (DESIGN 5.10 edge case).
	if want := []string{"browser", "yuno__deploy_release", "yuno__http_fetch", "yuno__memory_recall", "yuno__memory_save"}; !slices.Equal(deny, want) {
		t.Fatalf("deny = %v, want %v", deny, want)
	}
	_, deny = toolLists(&store.Agent{MemoryPolicy: store.MemoryPolicy{Enabled: true}})
	if deny == nil || len(deny) != 0 {
		t.Fatalf("empty deny must be [] not nil: %v", deny)
	}
}

// T5.11.3: a guardrail-blocked tool is absent from the tool list OpenClaw receives and
// denied there, even when the allowlist names it; memory tools follow the policy.
func TestBlockedToolNotOffered(t *testing.T) {
	a := &store.Agent{ToolAllowlist: []string{"finish", "http_fetch", "memory_save", "exec"},
		MemoryPolicy: store.MemoryPolicy{Enabled: true},
		Guardrails:   store.Guardrails{BlockedTools: []string{"yuno__http_fetch", "exec", "finish"}}}
	allow, deny := toolLists(a)
	if want := []string{"yuno__finish", "yuno__memory_save"}; !slices.Equal(allow, want) {
		t.Fatalf("allow = %v, want %v (finish is never blocked)", allow, want)
	}
	if want := []string{"exec", "yuno__http_fetch"}; !slices.Equal(deny, want) {
		t.Fatalf("deny = %v, want %v", deny, want)
	}
}

// DESIGN 5.11 implementation note: with an exec input pattern the exec policy pushed to
// OpenClaw asks about every host command (no allowlist, on-miss, fallback deny).
func TestExecPolicyAppliesBlockedPatterns(t *testing.T) {
	a := testAgent()
	a.Guardrails.BlockedInputPatterns = []store.BlockedPattern{{Name: "no pytest", Tool: "exec", Regex: "pytest"}}
	dir := t.TempDir()
	d := plan(dir, a, nil)
	if len(d.Exec.Allowlist) != 0 || d.Exec.Security != "allowlist" || d.Exec.Ask != "on-miss" || d.Exec.AskFallback != "deny" {
		t.Fatalf("exec policy = %+v", d.Exec)
	}
	if DesiredHash(dir, a, nil) == DesiredHash(dir, testAgent(), nil) {
		t.Fatal("a new exec pattern must change the desired state")
	}
}

func TestMaterializeCreateThenUpdate(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	skills := []store.Skill{{Name: "pytest-basics", Description: "Run tests: quickly", Body: "Use pytest -q.\n"}}

	hash, err := Materialize(ctx, m.gw, m.dir, a, skills)
	if err != nil {
		t.Fatal(err)
	}
	id := "ag5f6a7b8c9d"
	if a.OpenClawAgentID == nil || *a.OpenClawAgentID != id {
		t.Fatalf("openclaw id = %v", a.OpenClawAgentID)
	}
	create := m.f.callsFor("agents.create")
	if len(create) != 1 || !strings.Contains(string(create[0]), `"name":"`+id+`"`) ||
		!strings.Contains(string(create[0]), `"workspace":"`+filepath.Join(m.dir, "workspace-"+id)+`"`) {
		t.Fatalf("agents.create = %s", create)
	}
	e := m.entry(t, id)
	if e["name"] != "Quick Coder" || e["thinkingDefault"] != "low" || e["reasoningDefault"] != "off" {
		t.Fatalf("entry = %v", e)
	}
	if e["model"].(map[string]any)["primary"] != "yuno-anthropic/claude-opus-5-5" {
		t.Fatalf("model = %v", e["model"])
	}
	tools := e["tools"].(map[string]any)
	if tools["profile"] != "coding" {
		t.Fatalf("tools = %v", tools)
	}
	if got := toStrings(tools["deny"]); !slices.Equal(got, []string{"browser", "yuno__deploy_release", "yuno__http_fetch", "yuno__memory_recall", "yuno__memory_save"}) {
		t.Fatalf("deny = %v", got)
	}
	if got := toStrings(e["skills"]); !slices.Equal(got, []string{"pytest-basics"}) {
		t.Fatalf("skills = %v", got)
	}
	if e["identity"].(map[string]any)["name"] != "Quick Coder" {
		t.Fatalf("identity = %v", e["identity"])
	}
	patches := m.f.callsFor("config.patch")
	var first ConfigPatchParams
	_ = json.Unmarshal(patches[0], &first)
	if first.BaseHash == "" {
		t.Fatal("config.patch without baseHash")
	}

	m.state.mu.Lock()
	pol := m.state.exec["agents"].(map[string]any)[id]
	m.state.mu.Unlock()
	pb, _ := json.Marshal(pol)
	if string(pb) != `{"allowlist":[{"pattern":"/usr/bin/python3"},{"pattern":"python3 -m pytest"}],"ask":"on-miss","askFallback":"deny","security":"allowlist"}` {
		t.Fatalf("exec policy = %s", pb)
	}

	ws := filepath.Join(m.dir, "workspace-"+id)
	for name, want := range map[string]string{
		"AGENTS.md":                     AgentsMD,
		"MEMORY.md":                     "# Memory\n",
		"SOUL.md":                       "# Quick Coder\n\n## Role\n\nWrites and tests Python.\n\n## Instructions\n\nYou are careful.\n",
		"skills/pytest-basics/SKILL.md": "---\nname: pytest-basics\ndescription: \"Run tests: quickly\"\n---\n\nUse pytest -q.\n",
	} {
		b, err := os.ReadFile(filepath.Join(ws, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v", name, b, err)
		}
		if fi, _ := os.Stat(filepath.Join(ws, name)); fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode = %v", name, fi.Mode())
		}
	}
	if fi, _ := os.Stat(filepath.Join(ws, "skills")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("skills dir mode = %v", fi.Mode())
	}

	// Update: drop the skill, change the allowlist and the model.
	a.MaterializedHash = hash
	a.ToolAllowlist = []string{"exec", "finish"}
	a.Model = "claude-sonnet-5-5"
	hash2, err := Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil {
		t.Fatalf("update: %v (refused paths %v)", err, m.state.rejected)
	}
	if hash2 == hash {
		t.Fatal("hash did not change")
	}
	if n := len(m.f.callsFor("agents.create")); n != 1 {
		t.Fatalf("agents.create called %d times", n)
	}
	patches = m.f.callsFor("config.patch")
	var upd ConfigPatchParams
	_ = json.Unmarshal(patches[len(patches)-1], &upd)
	wantPaths := []string{"agents.entries." + id + ".tools.allow", "agents.entries." + id + ".tools.deny", "agents.entries." + id + ".skills"}
	if !slices.Equal(upd.ReplacePaths, wantPaths) {
		t.Fatalf("replacePaths = %v, want %v", upd.ReplacePaths, wantPaths)
	}
	e = m.entry(t, id)
	if _, ok := e["skills"]; ok {
		t.Fatalf("skills not removed: %v", e["skills"])
	}
	if got := toStrings(e["tools"].(map[string]any)["allow"]); !slices.Equal(got, []string{"exec", "yuno__finish"}) {
		t.Fatalf("allow = %v", got)
	}
	if e["model"].(map[string]any)["primary"] != "yuno-anthropic/claude-sonnet-5-5" {
		t.Fatalf("model = %v", e["model"])
	}
	if _, err := os.Stat(filepath.Join(ws, "skills", "pytest-basics")); !os.IsNotExist(err) {
		t.Fatalf("stale skill kept: %v", err)
	}
}

func TestMaterializeUnchangedHashIsNoOp(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	hash, err := Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.MaterializedHash = hash
	before := len(m.f.allCalls())
	if err := os.RemoveAll(filepath.Join(m.dir, "workspace-"+*a.OpenClawAgentID)); err != nil {
		t.Fatal(err)
	}
	got, err := Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil || got != hash {
		t.Fatalf("hash = %s, %v", got, err)
	}
	if n := len(m.f.allCalls()) - before; n != 0 {
		t.Fatalf("%d gateway calls on an unchanged agent", n)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "workspace-"+*a.OpenClawAgentID)); !os.IsNotExist(err) {
		t.Fatal("files written on an unchanged agent")
	}
	if DesiredHash(m.dir, a, nil) != hash {
		t.Fatal("DesiredHash differs from Materialize's hash")
	}
}

func TestMaterializeRetriesStaleBaseHash(t *testing.T) {
	m := newMatEnv(t)
	bumped := false
	m.f.on("config.get", func(json.RawMessage) (any, *RPCError) {
		m.state.mu.Lock()
		defer m.state.mu.Unlock()
		cfg, _ := json.Marshal(m.state.config)
		h := m.state.cfgHash()
		if !bumped { // someone else patches between our get and our patch
			bumped = true
			m.state.cfgRev++
		}
		return map[string]any{"hash": h, "config": json.RawMessage(cfg)}, nil
	})
	if _, err := Materialize(context.Background(), m.gw, m.dir, testAgent(), nil); err != nil {
		t.Fatal(err)
	}
	if n := len(m.f.callsFor("config.patch")); n != 2 {
		t.Fatalf("config.patch calls = %d, want 2", n)
	}
}

func TestRuntimeView(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	v, err := Runtime(ctx, m.gw, m.dir, a)
	if err != nil || v.Materialized || v.OpenClawAgentID != "" || len(v.Files) != 0 {
		t.Fatalf("before = %+v, %v", v, err)
	}
	hash, err := Materialize(ctx, m.gw, m.dir, a, []store.Skill{{Name: "s1", Body: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	a.MaterializedHash = hash
	writesBefore := len(writeCalls(m.f))
	v, err = Runtime(ctx, m.gw, m.dir, a)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Materialized || !v.InSync || v.OpenClawAgentID != *a.OpenClawAgentID || len(v.ConfigEntry) == 0 || len(v.ExecPolicy) == 0 {
		t.Fatalf("view = %+v", v)
	}
	if want := []string{"AGENTS.md", "MEMORY.md", "SOUL.md", "skills/s1/SKILL.md"}; !slices.Equal(v.Files, want) {
		t.Fatalf("files = %v", v.Files)
	}
	if len(writeCalls(m.f)) != writesBefore {
		t.Fatal("Runtime wrote to the gateway")
	}
	a.Model = "claude-haiku-5-5"
	if v, _ = Runtime(ctx, m.gw, m.dir, a); v.InSync {
		t.Fatal("in_sync after a model change")
	}
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// max_tokens is materialised as params.maxTokens, changes the desired hash and is
// removed again when the agent no longer sets it.
func TestMaterializeMaxTokens(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	a := testAgent()
	a.MaxTokens = 4096
	hash, err := Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := *a.OpenClawAgentID
	params, _ := m.entry(t, id)["params"].(map[string]any)
	if params["maxTokens"] != float64(4096) {
		t.Fatalf("params = %v", m.entry(t, id)["params"])
	}
	a.MaterializedHash = hash
	a.MaxTokens = 2048
	if DesiredHash(m.dir, a, nil) == hash {
		t.Fatal("max_tokens is not part of the desired hash")
	}
	hash, err = Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := m.entry(t, id)["params"].(map[string]any); p["maxTokens"] != float64(2048) {
		t.Fatalf("params after update = %v", m.entry(t, id)["params"])
	}
	a.MaterializedHash = hash
	a.MaxTokens = 0
	if _, err := Materialize(ctx, m.gw, m.dir, a, nil); err != nil {
		t.Fatal(err)
	}
	if p, ok := m.entry(t, id)["params"]; ok {
		t.Fatalf("params kept without max_tokens: %v", p)
	}
}

// A symlink the agent plants in its workspace must not redirect the platform's writes
// or stale-skill deletions to another directory (finding 2).
func TestMaterializeDoesNotFollowWorkspaceSymlinks(t *testing.T) {
	m := newMatEnv(t)
	ctx := context.Background()
	outside := t.TempDir()
	victimSkill := filepath.Join(outside, "pytest-basics", "SKILL.md")
	victimFile := filepath.Join(outside, "target.txt")
	if err := os.MkdirAll(filepath.Dir(victimSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	const skillBody = "---\nname: pytest-basics\ndescription: \"x\"\n---\n\nnot yours\n"
	if err := os.WriteFile(victimSkill, []byte(skillBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victimFile, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(outside, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p)
				out[p] = string(b)
			}
			return nil
		})
		return out
	}
	before := snapshot()

	a := testAgent()
	ws := WorkspaceDir(m.dir, DeriveAgentID(a.ID))
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimFile, filepath.Join(ws, "SOUL.md")); err != nil {
		t.Fatal(err)
	}

	// No skills: the stale-skill cleanup must not delete through skills -> outside, and
	// SOUL.md is replaced as a file instead of written through the link.
	hash, err := Materialize(ctx, m.gw, m.dir, a, nil)
	if err != nil {
		t.Fatalf("materialize without skills: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(ws, "SOUL.md")); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("SOUL.md is still a symlink: %v", err)
	}
	// With a skill the write under skills/ is refused rather than landing outside.
	a.MaterializedHash = hash
	if _, err := Materialize(ctx, m.gw, m.dir, a, []store.Skill{{Name: "other", Body: "b"}}); err == nil {
		t.Fatal("a skill was written through a symlink that leaves the workspace")
	}
	if after := snapshot(); !maps.Equal(after, before) {
		t.Fatalf("files outside the workspace changed:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(outside, "other")); !os.IsNotExist(err) {
		t.Fatalf("directory created outside the workspace: %v", err)
	}

	// runs/<run_id> is created through the same root.
	if err := os.Symlink(outside, filepath.Join(ws, "runs")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRunDir(m.dir, DeriveAgentID(a.ID), "run-1"); err == nil {
		t.Fatal("run directory created through a symlink that leaves the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "run-1")); !os.IsNotExist(err) {
		t.Fatalf("run directory created outside the workspace: %v", err)
	}
}
