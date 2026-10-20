package openclaw

import (
	"os"
	"path/filepath"
	"testing"
)

// T5.30.1: the path rule of a session's private workspace (DESIGN 5.30 rule 2) against the
// two folders the probe on the live gateway (OpenClaw 2026.10.3) observed. An OpenClaw
// upgrade that changes the rule fails here; the probe in docs/test-results.md is repeated.
func TestSandboxWorkspacePathRule(t *testing.T) {
	const agentWS = "/home/node/.openclaw/workspace-ag7a991cd2d9"
	for key, want := range map[string]string{
		"agent:ag7a991cd2d9:yuno-probe-preplace-1": "workspace-640eac68f7f7ae4e9195a7761731a2ec",
		"agent:ag7a991cd2d9:yuno-probe-preplace-2": "workspace-3f9d4c915577bdee18379089b8c36e51",
	} {
		if got := SandboxWorkspaceName(key, agentWS); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
	// The platform hashes the workspace its configuration entry names, under the state
	// directory the gateway and the api share.
	got := SessionWorkspaceDir("/home/node/.openclaw", "ag7a991cd2d9", "agent:ag7a991cd2d9:yuno-probe-preplace-1")
	if want := "/home/node/.openclaw/sandboxes/workspace-640eac68f7f7ae4e9195a7761731a2ec"; got != want {
		t.Errorf("SessionWorkspaceDir = %s, want %s", got, want)
	}
	if !ValidSandboxName("workspace-640eac68f7f7ae4e9195a7761731a2ec") {
		t.Error("a computed name is not a valid sandbox name")
	}
	for _, bad := range []string{"workspace-640EAC68f7f7ae4e9195a7761731a2ec", "workspace-640eac68", "workspace-ag7a991cd2d9", "agent-main-x", ".."} {
		if ValidSandboxName(bad) {
			t.Errorf("%q is a valid sandbox name", bad)
		}
	}
}

// RemoveSandboxWorkspace removes only <state>/sandboxes/<valid name>, unlinks a link in
// its place without following it, and accepts a folder already gone.
func TestRemoveSandboxWorkspace(t *testing.T) {
	state := t.TempDir()
	name := SandboxWorkspaceName("agent:a:yuno-x-n-1", WorkspaceDir(state, "a"))
	dir := filepath.Join(state, SandboxesDir, name)
	put(t, filepath.Join(dir, "runs", runA, "notes.md"))
	if err := RemoveSandboxWorkspace(state, dir); err != nil || exists(dir) {
		t.Fatalf("remove: %v, still there %v", err, exists(dir))
	}
	if err := RemoveSandboxWorkspace(state, dir); err != nil {
		t.Fatalf("a folder already gone: %v", err)
	}
	for _, bad := range []string{filepath.Join(state, "workspace-a"), filepath.Join(state, SandboxesDir, "chief"),
		filepath.Join(state, SandboxesDir, name, ".."), "/etc"} {
		if err := RemoveSandboxWorkspace(state, bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	outside := t.TempDir()
	put(t, filepath.Join(outside, "secret.txt"))
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSandboxWorkspace(state, dir); err != nil || exists(dir) {
		t.Fatalf("link: %v, still there %v", err, exists(dir))
	}
	if !exists(filepath.Join(outside, "secret.txt")) {
		t.Fatal("the removal followed a link")
	}
}

// ListSandboxWorkspaces lists only private workspace folders, and nothing when sandboxes/
// does not exist; EnsureSandboxesDir creates it.
func TestListSandboxWorkspaces(t *testing.T) {
	state := t.TempDir()
	if got, err := ListSandboxWorkspaces(state); err != nil || len(got) != 0 {
		t.Fatalf("no sandboxes/: %v %v", got, err)
	}
	if err := EnsureSandboxesDir(state); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(state, SandboxesDir)); err != nil || !info.IsDir() {
		t.Fatalf("sandboxes/: %v", err)
	}
	name := SandboxWorkspaceName("agent:a:yuno-x-n-1", WorkspaceDir(state, "a"))
	put(t, filepath.Join(state, SandboxesDir, name, "AGENTS.md"))
	put(t, filepath.Join(state, SandboxesDir, "other", "x"))
	got, err := ListSandboxWorkspaces(state)
	if err != nil || len(got) != 1 || got[0] != filepath.Join(state, SandboxesDir, name) {
		t.Fatalf("listed %v %v", got, err)
	}
}
