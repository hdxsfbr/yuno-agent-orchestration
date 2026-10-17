package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/project/projecttest"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

// projectEnv is an engine with a configured project whose remote is a local bare
// repository (no network), and the default sandbox image name set.
func projectEnv(t *testing.T, files map[string]string, network bool) (*testEnv, string) {
	t.Helper()
	bare := projecttest.BareRepo(t, files)
	env := newEnv(t)
	env.e.deps.Cfg.OpenClawSandboxImage = "yuno-sandbox:latest"
	env.e.project = &project.Config{RepoURL: bare, Token: "glpat-not-a-real-token-00", DefaultBranch: "main",
		Provider: project.ProviderGitLab, SandboxNetwork: network}
	return env, bare
}

// sandboxPatches lists the sandbox blocks config.patch set or cleared, in order:
// "set <id> <image> <network>" and "clear <id>".
func sandboxPatches(env *testEnv) []string {
	var out []string
	for _, raw := range env.fc.callsFor("config.patch") {
		var p openclaw.ConfigPatchParams
		_ = json.Unmarshal(raw, &p)
		var body struct {
			Agents struct {
				Entries map[string]map[string]json.RawMessage `json:"entries"`
			} `json:"agents"`
		}
		_ = json.Unmarshal([]byte(p.Raw), &body)
		for id, entry := range body.Agents.Entries {
			sb, ok := entry["sandbox"]
			if !ok {
				continue
			}
			if string(sb) == "null" {
				out = append(out, "clear "+id)
				continue
			}
			var block struct {
				Scope  string `json:"scope"`
				Docker struct {
					Image   string `json:"image"`
					Network string `json:"network"`
				} `json:"docker"`
			}
			_ = json.Unmarshal(sb, &block)
			out = append(out, "set "+id+" "+block.Scope+" "+block.Docker.Image+" "+block.Docker.Network)
		}
	}
	return out
}

// T5.16.3: a two-node project workflow clones into the first agent's workspace before
// its turn, records the toolchain and the span, selects the sandbox for each visit,
// carries the checkout to the second agent, whose open_merge_request pushes the edit
// to the bare repository and opens the request on the fake provider.
func TestProjectRunClonesCarriesAndProposes(t *testing.T) {
	env, bare := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, true)
	ctx := context.Background()
	coder, deployer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Deployer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: deployer.ID, Task: "propose"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	repoOf := func(p openclaw.ChatSendParams) string {
		return filepath.Join(env.dir, "workspace-"+sessionAgent(p.SessionKey), "runs", run.ID, "repo")
	}
	var mu sync.Mutex
	seen := map[string]string{} // node → README content when its turn started
	messages := map[string]string{}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		node, _ := visitOf(p.SessionKey)
		b, _ := os.ReadFile(filepath.Join(repoOf(p), "README.md"))
		mu.Lock()
		seen[node] = string(b)
		messages[node] = p.Message
		mu.Unlock()
		if node == "one" {
			writeFile(t, filepath.Join(repoOf(p), "README.md"), "# demo\nedited by one\n")
			writeFile(t, filepath.Join(repoOf(p), "cmd", "main.go"), "package main\n")
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if r.Toolchain == nil || *r.Toolchain != "go" {
		t.Fatalf("toolchain = %v", r.Toolchain)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["one"] != "# demo\n" {
		t.Fatalf("node one started with README %q: the clone did not happen before the turn", seen["one"])
	}
	if seen["two"] != "# demo\nedited by one\n" {
		t.Fatalf("node two started with README %q: the checkout did not carry forward", seen["two"])
	}
	for node, msg := range messages {
		for _, want := range []string{"Project repository: runs/" + run.ID + "/repo/", "branch main, toolchain go", "Sandbox network: on", "(go mod download)", "(go test ./...)"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s task lacks %q:\n%s", node, want, msg)
			}
		}
		if strings.Contains(msg, env.e.project.Token) {
			t.Errorf("%s task carries the token", node)
		}
	}
	// The checkout's git config holds the bare URL only.
	cfg, _ := os.ReadFile(filepath.Join(env.dir, "workspace-"+openclaw.DeriveAgentID(coder.ID), "runs", run.ID, "repo", ".git", "config"))
	if strings.Contains(string(cfg), "extraheader") || strings.Contains(string(cfg), env.e.project.Token) {
		t.Fatalf(".git/config:\n%s", cfg)
	}
	// One clone span with the toolchain, one log line.
	if n := env.count(`SELECT count(*) FROM spans WHERE run_id=$1 AND kind='project' AND name='project_clone' AND attrs->>'toolchain'='go'`, run.ID); n != 1 {
		t.Fatalf("project spans = %d", n)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data::text LIKE '%project repository cloned%toolchain go%'`, run.ID); n != 1 {
		t.Fatalf("clone log lines = %d", n)
	}
	// Each visit selected the go image with network and cleared it afterwards.
	coderID, deployerID := openclaw.DeriveAgentID(coder.ID), openclaw.DeriveAgentID(deployer.ID)
	want := []string{"set " + coderID + " session yuno-sandbox-go:latest bridge", "clear " + coderID,
		"set " + deployerID + " session yuno-sandbox-go:latest bridge", "clear " + deployerID}
	if got := sandboxPatches(env); !slices.Equal(got, want) {
		t.Fatalf("sandbox patches = %v, want %v", got, want)
	}
	if holds := env.e.sandboxHolds(); len(holds) != 0 {
		t.Fatalf("holds left: %v", holds)
	}

	// The Deployer's open_merge_request, as the MCP handler would run it in the
	// deployer's workspace (the fake provider reports branches from the bare repository).
	var mrs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/branches/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/g%2Fp/repository/branches/")
			if !slices.Contains(projecttest.Branches(t, bare), strings.ReplaceAll(name, "%2F", "/")) {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"message":"404 Branch Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"name":"x"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			_ = json.NewEncoder(w).Encode(mrs)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["iid"] = 5
			body["web_url"] = "https://gitlab.test/g/p/-/merge_requests/5"
			mrs = append(mrs, body)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	tool := tools.NewOpenMergeRequest(tools.MergeRequestConfig{Project: *env.e.project, APIBase: srv.URL + "/api/v4/projects/g%2Fp"})
	key, err := tools.IdempotencyKey(run.ID, "visit-two", tool.Name, json.RawMessage(`{"title":"Edit the readme","description":"d"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(ctx, tools.Call{RunID: run.ID, Workspace: filepath.Join(env.dir, "workspace-"+deployerID), IdempotencyKey: key,
		Input: json.RawMessage(`{"run_token":"r","title":"Edit the readme","description":"d"}`)})
	if err != nil || res.IsError {
		t.Fatalf("open_merge_request: %+v %v", res, err)
	}
	var out struct {
		Number int64  `json:"number"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil || out.Number != 5 || out.Branch != tools.GitLabBranch(key) {
		t.Fatalf("output = %s", res.Output)
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "README.md"); got != "# demo\nedited by one\n" {
		t.Fatalf("README on the branch = %q", got)
	}
	if got := projecttest.FileAt(t, bare, out.Branch, "cmd/main.go"); got != "package main\n" {
		t.Fatalf("main.go on the branch = %q", got)
	}
	if len(mrs) != 1 || mrs[0]["target_branch"] != "main" {
		t.Fatalf("mrs = %v", mrs)
	}
}

// A clone that fails (the default branch does not exist) fails the run with "project
// clone:" before any turn starts, and no sandbox block is left behind.
func TestProjectCloneFailureFailsTheRun(t *testing.T) {
	env, _ := projectEnv(t, map[string]string{"README.md": "x"}, false)
	env.e.project.DefaultBranch = "release"
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "project clone: git clone") {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if strings.Contains(deref(r.FailureReason), env.e.project.Token) {
		t.Fatal("the token is in the failure reason")
	}
	if n := len(env.fc.sendParams()); n != 0 {
		t.Fatalf("%d turns started", n)
	}
	if r.Toolchain != nil {
		t.Fatal("toolchain recorded for a failed clone")
	}
	if got := sandboxPatches(env); len(got) != 0 {
		t.Fatalf("sandbox patches = %v", got)
	}
}

// An opted-in run on an engine without a configured project runs as before: no
// checkout, no toolchain, no sandbox block, one log line.
func TestProjectFlagWithoutAProject(t *testing.T) {
	env := newEnv(t)
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID})
	if err != nil {
		t.Fatal(err)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.Contains(p.Message, "Project repository") {
			t.Error("task describes a project without one configured")
		}
		go finishThenFinal(t, env, p, `{"ok":true}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded || r.Toolchain != nil {
		t.Fatalf("run = %s %q toolchain %v", r.Status, deref(r.FailureReason), r.Toolchain)
	}
	if n := env.count(`SELECT count(*) FROM run_events WHERE run_id=$1 AND event='log' AND data::text LIKE '%no repository configured%'`, run.ID); n != 1 {
		t.Fatalf("log lines = %d", n)
	}
	if got := sandboxPatches(env); len(got) != 0 {
		t.Fatalf("sandbox patches = %v", got)
	}
}

// A quick run opts in through the request (RunCreate.project); network off selects
// the image with network "none" and says so in the task.
func TestProjectQuickRun(t *testing.T) {
	env, _ := projectEnv(t, map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""}, false)
	a := env.agent(nil)
	var message string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		message = p.Message
		go finishThenFinal(t, env, p, `{"summary":"ok"}`)
		return startedTurn(p)
	}
	r := env.triggerAndRun(TriggerRequest{AgentID: a.ID, Task: "look", Project: true})
	if r.Status != store.RunSucceeded || r.Toolchain == nil || *r.Toolchain != "node" {
		t.Fatalf("run = %s %q toolchain %v", r.Status, deref(r.FailureReason), r.Toolchain)
	}
	if !strings.Contains(message, "toolchain node") || !strings.Contains(message, "Sandbox network: off") || strings.Contains(message, "pnpm install") {
		t.Fatalf("task:\n%s", message)
	}
	id := openclaw.DeriveAgentID(a.ID)
	if got := sandboxPatches(env); !slices.Equal(got, []string{"set " + id + " session yuno-sandbox-node:latest none", "clear " + id}) {
		t.Fatalf("sandbox patches = %v", got)
	}
	var snap runSnapshot
	if err := json.Unmarshal(r.WorkflowSnapshot, &snap); err != nil || !snap.Project {
		t.Fatalf("snapshot project flag: %v %v", snap.Project, err)
	}
	// Without the flag the same agent runs without a checkout.
	r2 := env.triggerAndRun(TriggerRequest{AgentID: a.ID, Task: "look again"})
	if r2.Status != store.RunSucceeded || r2.Toolchain != nil {
		t.Fatalf("run 2 = %s toolchain %v", r2.Status, r2.Toolchain)
	}
}

// Two project visits of one agent at the same time: the block is cleared once, when
// the last of them ends (DESIGN 5.16 rule 7).
func TestSandboxHoldsClearOnce(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	spec := openclaw.SandboxSpec{Image: "yuno-sandbox-go:latest"}
	if err := env.e.holdSandbox(ctx, "agx", spec); err != nil {
		t.Fatal(err)
	}
	if err := env.e.holdSandbox(ctx, "agx", spec); err != nil {
		t.Fatal(err)
	}
	env.e.releaseSandbox(ctx, "agx")
	if got := sandboxPatches(env); !slices.Equal(got, []string{"set agx session yuno-sandbox-go:latest none", "set agx session yuno-sandbox-go:latest none"}) {
		t.Fatalf("after the first release: %v", got)
	}
	if h := env.e.sandboxHolds(); h["agx"] != 1 {
		t.Fatalf("holds = %v", h)
	}
	env.e.releaseSandbox(ctx, "agx")
	if got := sandboxPatches(env); len(got) != 3 || got[2] != "clear agx" {
		t.Fatalf("after the last release: %v", got)
	}
	if h := env.e.sandboxHolds(); len(h) != 0 {
		t.Fatalf("holds = %v", h)
	}
}

// A sandbox block a dead process left on an agent is cleared by the next full sync;
// one a live visit holds is kept (DESIGN 5.16 rule 7).
func TestSyncAgentsClearsStaleSandboxBlocks(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	_, staleID := env.materialized("Stale")
	_, heldID := env.materialized("Held")
	_, plainID := env.materialized("Plain")
	block := map[string]any{"scope": "session", "docker": map[string]any{"image": "yuno-sandbox-go:latest", "network": "none"}}
	env.fc.mu.Lock()
	env.fc.entries[staleID]["sandbox"] = block
	env.fc.entries[heldID]["sandbox"] = block
	env.fc.mu.Unlock()
	if err := env.e.holdSandbox(ctx, heldID, openclaw.SandboxSpec{Image: "yuno-sandbox-go:latest"}); err != nil {
		t.Fatal(err)
	}
	res, err := env.e.SyncAgents(ctx)
	if err != nil || res.SandboxesCleared != 1 {
		t.Fatalf("sync = %+v %v, want one sandbox cleared", res, err)
	}
	env.fc.mu.Lock()
	_, staleHas := env.fc.entries[staleID]["sandbox"]
	_, heldHas := env.fc.entries[heldID]["sandbox"]
	_, plainHas := env.fc.entries[plainID]["sandbox"]
	env.fc.mu.Unlock()
	if staleHas || !heldHas || plainHas {
		t.Fatalf("after the sync: stale %v held %v plain %v", staleHas, heldHas, plainHas)
	}
	// The held one goes when its visit releases it.
	env.e.releaseSandbox(ctx, heldID)
	env.fc.mu.Lock()
	_, heldHas = env.fc.entries[heldID]["sandbox"]
	env.fc.mu.Unlock()
	if heldHas {
		t.Fatal("the held block survived its release")
	}
}
