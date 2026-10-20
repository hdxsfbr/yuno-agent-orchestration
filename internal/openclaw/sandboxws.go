package openclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Private step workspaces (DESIGN 5.30, D-083). With workspaceAccess "none" OpenClaw gives
// every session a workspace of its own under <state>/sandboxes/, mounted at /workspace in
// the session's container instead of the agent's workspace. The platform computes where
// it is, prepares it before the visit's first turn, reads it when the visit ends, and
// deletes it when the run no longer needs it.

// SandboxesDir is the folder under the state directory that holds the sessions' private
// workspaces.
const SandboxesDir = "sandboxes"

// SandboxWorkspaceName is the folder OpenClaw gives the private workspace of a session
// (DESIGN 5.30 rule 2) [probed, OpenClaw 2026.10.3 sandbox/shared.ts]: the scope key is
// the session key, ":workspace:" and the first 32 hex digits of sha256(agentWorkspace);
// the folder is "workspace-" and the first 32 hex digits of sha256(scope key).
// agentWorkspace is the agent's workspace as the gateway names it (the workspace field of
// its configuration entry). The rule is OpenClaw's internal behaviour, not an API; a test
// pins it against the paths the probe observed.
func SandboxWorkspaceName(sessionKey, agentWorkspace string) string {
	ws := sha256.Sum256([]byte(agentWorkspace))
	scope := sha256.Sum256([]byte(sessionKey + ":workspace:" + hex.EncodeToString(ws[:])[:32]))
	return "workspace-" + hex.EncodeToString(scope[:])[:32]
}

// SessionWorkspaceDir is the private workspace of a session of the agent openclawID. The
// agent's configuration entry names WorkspaceDir(stateDir, openclawID) as its workspace
// (the materialiser writes it from the same state directory), so that is the path the
// gateway hashes; the api mounts the state directory at the gateway's path, so the
// result is also where the api reads and writes the folder.
func SessionWorkspaceDir(stateDir, openclawID, sessionKey string) string {
	return filepath.Join(stateDir, SandboxesDir, SandboxWorkspaceName(sessionKey, WorkspaceDir(stateDir, openclawID)))
}

// ValidSandboxName reports whether name has the shape of a private workspace folder
// ("workspace-" and 32 lower-case hex digits), the only names the platform ever removes
// from sandboxes/.
func ValidSandboxName(name string) bool {
	hexPart, ok := strings.CutPrefix(name, "workspace-")
	if !ok || len(hexPart) != 32 {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		if c := hexPart[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// EnsureSandboxesDir creates <state>/sandboxes when it is missing and, when the caller
// is root, gives it to uid and gid 1000, the gateway's and the api's user, so the api can
// create a visit's private workspace in it before OpenClaw does (DESIGN 5.30 rule 2;
// openclaw-init runs it).
func EnsureSandboxesDir(stateDir string) error {
	dir := filepath.Join(stateDir, SandboxesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return os.Lchown(dir, 1000, 1000)
	}
	return nil
}

// ListSandboxWorkspaces lists the private workspace folders under <state>/sandboxes as
// full paths; entries of any other name are left out. A missing folder lists nothing.
func ListSandboxWorkspaces(stateDir string) ([]string, error) {
	dir := filepath.Join(stateDir, SandboxesDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if ValidSandboxName(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// RemoveSandboxWorkspace deletes a private workspace recorded on a visit (DESIGN 5.30
// rule 8). dir must be <state>/sandboxes/<valid name>; anything else is refused. The
// folder is removed through an os.Root of sandboxes/, so a link in its place is unlinked
// and never followed. A folder already gone is not an error.
func RemoveSandboxWorkspace(stateDir, dir string) error {
	parent := filepath.Join(stateDir, SandboxesDir)
	name := filepath.Base(dir)
	if filepath.Clean(dir) != filepath.Join(parent, name) || !ValidSandboxName(name) {
		return fmt.Errorf("remove workspace: %q is not a private workspace under %s", dir, parent)
	}
	root, err := os.OpenRoot(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	rm := root.RemoveAll
	if !info.IsDir() {
		rm = root.Remove // a link or file in the folder's place: removed, never followed
	}
	if err := rm(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// maxMemoryBytes caps the MEMORY.md copied into a private workspace; the platform renders
// the file itself, so a larger one is not the platform's.
const maxMemoryBytes = 1 << 20

// ReadMemoryMD returns the agent's MEMORY.md as its workspace holds it, read through an
// os.Root; a missing file, or one that is not a regular file, reads as not found.
func ReadMemoryMD(stateDir, openclawID string) (string, error) {
	root, err := os.OpenRoot(WorkspaceDir(stateDir, openclawID))
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat("MEMORY.md")
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("MEMORY.md: %w", fs.ErrNotExist)
	}
	if info.Size() > maxMemoryBytes {
		return "", fmt.Errorf("MEMORY.md is over %d bytes", maxMemoryBytes)
	}
	b, err := root.ReadFile("MEMORY.md")
	return string(b), err
}
