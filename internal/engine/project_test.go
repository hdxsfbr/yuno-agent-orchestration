package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

// testProjectToken is the token the test projects store (sealed, as the API does).
const testProjectToken = "glpat-not-a-real-token-00"

// project creates a project row whose remote is repoURL (a local bare repository in
// these tests) with the token sealed under the engine's key (DESIGN 5.17 rule 2).
func (env *testEnv) project(repoURL string, network bool) *store.Project {
	env.t.Helper()
	// The tail of a UUIDv7 is random; its head is a timestamp two fixtures can share.
	p := &store.Project{ID: store.NewID(), Name: "demo-" + strings.ReplaceAll(store.NewID(), "-", "")[20:], RepoURL: repoURL, Provider: "gitlab",
		DefaultBranch: "main", SandboxNetwork: network}
	blob, err := env.e.projects.Cipher.Seal(p.ID, testProjectToken)
	if err != nil {
		env.t.Fatal(err)
	}
	if err := store.CreateProject(context.Background(), env.pool, p, blob); err != nil {
		env.t.Fatal(err)
	}
	return p
}

// projectEnv is an engine with a project whose remote is a local bare repository (no
// network), and the default sandbox image name set.
func projectEnv(t *testing.T, files map[string]string, network bool) (*testEnv, string, *store.Project) {
	t.Helper()
	bare := projecttest.BareRepo(t, files)
	env := newEnv(t)
	env.e.deps.Cfg.OpenClawSandboxImage = "yuno-sandbox:latest"
	p := env.project(bare, network)
	return env, bare, p
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

// T5.16.3, T5.17.4, T5.30.2: a two-node project workflow run queued with a project clones
// from its snapshot (the token read from the row) into the first visit's private
// workspace before its turn, records the toolchain and the span, selects the sandbox for
// each visit, carries the checkout to the second visit through the run repository (no
// folder is shared: the agents' workspaces hold nothing of the run), whose
// open_merge_request resolves the same project, pushes the edit to the bare repository
// and opens the request on the fake.
func TestProjectRunClonesCarriesAndProposes(t *testing.T) {
	env, bare, p := projectEnv(t, map[string]string{"README.md": "# demo\n", "go.mod": "module demo\n"}, true)
	ctx := context.Background()
	coder, deployer := env.agent(nil), env.agent(func(a *store.Agent) { a.Name = "Deployer" })
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}, workflow.Node{ID: "two", AgentID: deployer.ID, Task: "propose"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if run.ProjectID == nil || *run.ProjectID != p.ID || run.Project == nil || run.Project.Name != p.Name || run.Project.RepoURL != bare {
		t.Fatalf("queued run project = %v %+v", run.ProjectID, run.Project)
	}
	repoOf := func(p openclaw.ChatSendParams) string {
		return filepath.Join(turnWS(env, p), "runs", run.ID, "repo")
	}
	snapshot := filepath.Join(t.TempDir(), "run")
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
		if node == "two" {
			// The run's folders go when it succeeds; keep the deployer's as it stood.
			if out, err := exec.Command("cp", "-a", filepath.Dir(repoOf(p)), snapshot).CombinedOutput(); err != nil {
				t.Errorf("snapshot: %v %s", err, out)
			}
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
		if strings.Contains(msg, testProjectToken) {
			t.Errorf("%s task carries the token", node)
		}
	}
	// The second visit's checkout came from the run repository: the edit is a change
	// against the cloned branch, and its git config holds the bare URL only.
	cfg, _ := os.ReadFile(filepath.Join(snapshot, "repo", ".git", "config"))
	if strings.Contains(string(cfg), "extraheader") || strings.Contains(string(cfg), testProjectToken) || !strings.Contains(string(cfg), bare) {
		t.Fatalf(".git/config:\n%s", cfg)
	}
	checkout := filepath.Join(snapshot, "repo")
	if changed, added := projecttest.Git(t, checkout, "diff", "--name-status"), projecttest.Git(t, checkout, "ls-files", "--others", "--exclude-standard"); changed != "M\tREADME.md" || added != "cmd/main.go" {
		t.Fatalf("the deployer's checkout: changed %q, added %q", changed, added)
	}
	// Each visit recorded its private workspace and the commit its checkout ended at: the
	// clone's commit for neither, the coder's commit for both (the deployer changed nothing).
	nodes := runNodes(t, env, run.ID)
	if len(nodes) != 2 || nodes[0].WorkspaceDir == nil || nodes[1].WorkspaceDir == nil || *nodes[0].WorkspaceDir == *nodes[1].WorkspaceDir ||
		nodes[0].RepoSHA == nil || nodes[1].RepoSHA == nil || *nodes[0].RepoSHA != *nodes[1].RepoSHA || *nodes[0].RepoSHA == deref(r.CheckoutSHA) {
		t.Fatalf("visits = %+v", nodes)
	}
	for _, n := range nodes {
		if !strings.HasPrefix(*n.WorkspaceDir, filepath.Join(env.dir, "sandboxes", "workspace-")) {
			t.Fatalf("workspace of %s = %s", n.NodeID, *n.WorkspaceDir)
		}
		if _, err := os.Lstat(*n.WorkspaceDir); !os.IsNotExist(err) {
			t.Fatalf("the succeeded run's workspace %s is still there: %v", *n.WorkspaceDir, err)
		}
	}
	if _, err := os.Lstat(project.RunRepoPath(env.dir, run.ID)); !os.IsNotExist(err) {
		t.Fatalf("the succeeded run's repository is still there: %v", err)
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
	for _, id := range []string{coderID, deployerID} {
		if _, err := os.Lstat(filepath.Join(env.dir, "workspace-"+id, "runs")); !os.IsNotExist(err) {
			t.Fatalf("the agent workspace %s has a runs folder: %v", id, err)
		}
	}
	want := []string{"set " + coderID + " session yuno-sandbox-go:latest bridge", "clear " + coderID,
		"set " + deployerID + " session yuno-sandbox-go:latest bridge", "clear " + deployerID}
	if got := sandboxPatches(env); !slices.Equal(got, want) {
		t.Fatalf("sandbox patches = %v, want %v", got, want)
	}
	if holds := env.e.sandboxHolds(); len(holds) != 0 {
		t.Fatalf("holds left: %v", holds)
	}

	// The Deployer's open_merge_request, as the MCP handler would run it in the deployer
	// visit's workspace, restored as it stood (the fake provider reports branches from the
	// bare repository).
	deployerWS := t.TempDir()
	deployerRun := filepath.Join(deployerWS, "runs", run.ID)
	if err := os.MkdirAll(filepath.Dir(deployerRun), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", snapshot, deployerRun).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	var mrs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repository/branches/"):
			// The tool looks the branch up again after the push, so the fake must find it: the
			// name is what follows the route in the escaped path, its slashes encoded.
			_, name, _ := strings.Cut(r.URL.EscapedPath(), "/repository/branches/")
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
	tool := tools.NewOpenMergeRequest(tools.MergeRequestConfig{Projects: env.e.projects, APIBase: srv.URL + "/api/v4/projects/g%2Fp"})
	key, err := tools.IdempotencyKey(run.ID, "visit-two", tool.Name, json.RawMessage(`{"title":"Edit the readme","description":"d"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(ctx, tools.Call{RunID: run.ID, Workspace: deployerWS, IdempotencyKey: key,
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
	env, _, p := projectEnv(t, map[string]string{"README.md": "x"}, false)
	p.DefaultBranch = "release"
	if err := store.UpdateProject(context.Background(), env.pool, p); err != nil {
		t.Fatal(err)
	}
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	wf := env.createWorkflow(def)
	run, err := env.e.Trigger(context.Background(), TriggerRequest{WorkflowID: wf.ID, ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunFailed || !strings.HasPrefix(deref(r.FailureReason), "project clone: git fetch: fatal: couldn't find remote ref refs/heads/release") {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	if strings.Contains(deref(r.FailureReason), testProjectToken) {
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

// A run whose snapshot opts in but names no project (one queued before DESIGN 5.17)
// runs as 5.16 rule 2 said: no checkout, no toolchain, no sandbox block, one log line.
func TestProjectFlagWithoutAProject(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	coder := env.agent(nil)
	def := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	def.Project = true
	def.Limits = store.DefaultLimits
	wf := env.createWorkflow(def)
	snap, err := json.Marshal(runSnapshot{Definition: def})
	if err != nil {
		t.Fatal(err)
	}
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: snap, Trigger: store.TriggerManual, Limits: store.DefaultLimits}
	if err := store.CreateRun(ctx, env.pool, run); err != nil {
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

// A quick run opts in by naming a project (DESIGN 5.17 rule 4); network off selects
// the image with network "none" and says so in the task.
func TestProjectQuickRun(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""}, false)
	a := env.agent(nil)
	var message string
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		message = p.Message
		go finishThenFinal(t, env, p, `{"summary":"ok"}`)
		return startedTurn(p)
	}
	r := env.triggerAndRun(TriggerRequest{AgentID: a.ID, Task: "look", ProjectID: p.ID})
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
	if r.ProjectID == nil || *r.ProjectID != p.ID || r.Project == nil || r.Project.Name != p.Name {
		t.Fatalf("run project = %v %+v", r.ProjectID, r.Project)
	}
	// The hidden quick-run workflow carries no flag (DESIGN 5.17 edge case).
	hidden, err := store.GetWorkflowByTemplateKey(context.Background(), env.pool, QuickRunTemplateKey(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	var hiddenDef workflow.Definition
	if err := json.Unmarshal(hidden.Definition, &hiddenDef); err != nil || hiddenDef.Project {
		t.Fatalf("hidden workflow project flag = %v %v", hiddenDef.Project, err)
	}
	// Without a project the same agent runs without a checkout.
	r2 := env.triggerAndRun(TriggerRequest{AgentID: a.ID, Task: "look again"})
	if r2.Status != store.RunSucceeded || r2.Toolchain != nil || r2.ProjectID != nil {
		t.Fatalf("run 2 = %s toolchain %v project %v", r2.Status, r2.Toolchain, r2.ProjectID)
	}
}

// T5.17.4: the trigger refuses a project workflow without a project and a plain
// workflow with one, and names an unknown or deleted id; a valid request snapshots the
// settings.
func TestProjectRequestValidation(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"README.md": "x"}, false)
	ctx := context.Background()
	coder := env.agent(nil)
	needs := line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"})
	needs.Project = true
	wfNeeds := env.createWorkflow(needs)
	wfPlain := env.createWorkflow(line(workflow.Node{ID: "one", AgentID: coder.ID, Task: "write"}))
	gone := env.project(p.RepoURL, false)
	if err := store.DeleteProject(ctx, env.pool, gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		req  TriggerRequest
		want string
	}{
		{"project workflow without a project", TriggerRequest{WorkflowID: wfNeeds.ID}, "project_id: required; the workflow works on a project repository"},
		{"plain workflow with a project", TriggerRequest{WorkflowID: wfPlain.ID, ProjectID: p.ID}, "project_id: the workflow does not use a project"},
		{"unknown id", TriggerRequest{WorkflowID: wfNeeds.ID, ProjectID: "nope"}, "project_id: no project with id nope"},
		{"deleted id", TriggerRequest{WorkflowID: wfNeeds.ID, ProjectID: gone.ID}, "project_id: no project with id " + gone.ID},
		{"quick run with an unknown id", TriggerRequest{AgentID: coder.ID, Task: "t", ProjectID: "nope"}, "project_id: no project with id nope"},
	} {
		_, err := env.e.Trigger(ctx, tc.req)
		var ve *ValidationError
		if !errors.As(err, &ve) || !slices.Contains(ve.Messages, tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	run, err := env.e.Trigger(ctx, TriggerRequest{WorkflowID: wfNeeds.ID, ProjectID: " " + p.ID + " "})
	if err != nil || run.ProjectID == nil || *run.ProjectID != p.ID || run.Project == nil || run.Project.RepoURL != p.RepoURL || run.Project.DefaultBranch != "main" {
		t.Fatalf("valid request: %v %+v", err, run)
	}
}

// T5.17.4: a project deleted after the run was queued still clones and reads the
// row's token (DESIGN 5.17 rule 3); a renamed one keeps the snapshot's name.
func TestProjectDeletedAfterQueueingStillClones(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"go.mod": "module demo\n"}, false)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "look", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, env.pool, p.ID); err != nil {
		t.Fatal(err)
	}
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		go finishThenFinal(t, env, p, `{"summary":"ok"}`)
		return startedTurn(p)
	}
	r := env.resume(env.e, run.ID)
	if r.Status != store.RunSucceeded || r.Toolchain == nil || *r.Toolchain != "go" || r.Project == nil || r.Project.Name != p.Name {
		t.Fatalf("run = %s %q toolchain %v project %+v", r.Status, deref(r.FailureReason), r.Toolchain, r.Project)
	}
}

// T5.17.4: a token that does not open (the key changed) fails the run before any turn
// with DESIGN 5.17 rule 2's reason, which carries no secret.
func TestProjectUnreadableTokenFailsTheRun(t *testing.T) {
	env, _, p := projectEnv(t, map[string]string{"go.mod": "module demo\n"}, false)
	ctx := context.Background()
	a := env.agent(nil)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "look", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE projects SET token_enc=$2 WHERE id=$1`, p.ID, []byte("not-a-blob-sealed-by-us-0000000000")); err != nil {
		t.Fatal(err)
	}
	r := env.resume(env.e, run.ID)
	want := "project: " + project.ErrCannotRead.Error()
	if r.Status != store.RunFailed || deref(r.FailureReason) != want {
		t.Fatalf("run = %s %q, want %q", r.Status, deref(r.FailureReason), want)
	}
	if n := len(env.fc.sendParams()); n != 0 {
		t.Fatalf("%d turns started", n)
	}
	if got := sandboxPatches(env); len(got) != 0 {
		t.Fatalf("sandbox patches = %v", got)
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
