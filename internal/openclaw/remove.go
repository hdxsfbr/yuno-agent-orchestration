package openclaw

import (
	"context"
	"fmt"
	"os"
	"regexp"
)

// MainAgentID is the gateway's own default agent, which the baseline keeps configured
// (explicit ownership needs one) and the platform never removes.
const MainAgentID = "main"

// removableID is the shape of an agent id the platform may delete and whose workspace
// path it may build: the slugs agents.create derives, so an id the gateway reports can
// never name a path outside the state directory.
var removableID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Removable reports whether id may be deleted from the gateway: a well-formed agent id
// that is not the gateway's default agent.
func Removable(id string) bool {
	return id != MainAgentID && removableID.MatchString(id)
}

// DeleteAgent is the inverse of Materialize: agents.delete, which drops the agent and
// its config entry [probed], then the workspace directory under the state directory. A
// refused RPC leaves the workspace in place, so a later attempt finds both again.
func DeleteAgent(ctx context.Context, c Client, stateDir, id string) error {
	if !Removable(id) {
		return fmt.Errorf("agent %q is not removable", id)
	}
	if c == nil {
		return fmt.Errorf("delete %s: %w", id, ErrDisconnected)
	}
	// agents.delete rewrites the config; a materialisation in flight must not patch it
	// with a stale baseHash in between.
	writeMu.Lock()
	err := AgentsDelete(ctx, c, id)
	writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("delete %s: agents.delete: %w", id, err)
	}
	return RemoveWorkspace(stateDir, id)
}

// RemoveWorkspace deletes workspace-<id> under the state directory, run directories
// included. Nothing is removed for an id Removable refuses or without a state directory.
func RemoveWorkspace(stateDir, id string) error {
	if stateDir == "" || !Removable(id) {
		return nil
	}
	if err := os.RemoveAll(WorkspaceDir(stateDir, id)); err != nil {
		return fmt.Errorf("remove workspace of %s: %w", id, err)
	}
	return nil
}
