package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
)

// SandboxSpec is the sandbox a project visit needs for its duration (DESIGN 5.16 rule
// 7): the toolchain's image and whether the container gets network.
type SandboxSpec struct {
	Image   string
	Network bool
}

// entry is the agents.entries.<id>.sandbox block the spec becomes. Scope "session" is
// deliberate: the gateway keeps a hot agent-scoped container running when its
// configuration changes [docs], so a per-session scope is what makes the next node
// visit start a container from the current image and network [probed]. The block says
// workspaceAccess "none" like the defaults, so a project visit also works in its own
// private workspace and never sees the agent's (DESIGN 5.30 rule 1).
func (s SandboxSpec) entry() map[string]any {
	network := "none"
	if s.Network {
		network = "bridge"
	}
	return map[string]any{"scope": "session", "workspaceAccess": "none", "docker": map[string]any{"image": s.Image, "network": network}}
}

// SetAgentSandbox sets agents.entries.<id>.sandbox to the spec [probed: the agent's
// next session runs in that image with that network; agents.defaults.sandbox is
// untouched; the paths reload hot].
func SetAgentSandbox(ctx context.Context, c Client, agentID string, spec SandboxSpec) error {
	return patchAgentKey(ctx, c, agentID, "sandbox", spec.entry(), nil)
}

// ClearAgentSandbox deletes agents.entries.<id>.sandbox (null, with the path in
// replacePaths [probed]) so the agent is back on the defaults.
func ClearAgentSandbox(ctx context.Context, c Client, agentID string) error {
	return patchAgentKey(ctx, c, agentID, "sandbox", nil, []string{"agents.entries." + agentID + ".sandbox"})
}

// patchAgentKey sets one key of an agent's config entry (nil deletes it), retrying a
// stale baseHash like patchEntry. It shares writeMu with the materialiser so two
// read-modify-write cycles in this process never race each other's hash.
func patchAgentKey(ctx context.Context, c Client, agentID, key string, value any, replace []string) error {
	if c == nil {
		return fmt.Errorf("patch %s.%s: %w", agentID, key, ErrDisconnected)
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	raw, err := json.Marshal(map[string]any{"agents": map[string]any{"ownership": "explicit",
		"entries": map[string]any{agentID: map[string]any{key: value}}}})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cfg, err := ConfigGet(ctx, c)
		if err != nil {
			return fmt.Errorf("config.get: %w", err)
		}
		if _, err = ConfigPatch(ctx, c, string(raw), cfg.Hash, replace); err == nil {
			return nil
		}
		lastErr = err
		if !isStaleHash(err) {
			break
		}
	}
	return fmt.Errorf("config.patch %s.%s: %w", agentID, key, lastErr)
}
