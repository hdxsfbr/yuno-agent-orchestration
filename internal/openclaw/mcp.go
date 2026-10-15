package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// MCPServerName is the mcp.servers key the baseline registers for the platform tools.
const MCPServerName = "yuno"

// RefreshMCPServer makes the gateway rebuild its client for the platform MCP server.
// OpenClaw marks an MCP server unavailable with a retry-after when it cannot connect,
// which happens on every api restart, and sessions started in that window do not see
// yuno__* tools [probed: "Tool yuno__finish not found" after a restart]. Patching the
// server entry is a hot reload that recreates the runtime; a changing header value is
// the smallest change that counts as one.
func RefreshMCPServer(ctx context.Context, c Client, name string) error {
	var cfg ConfigGetResult
	if err := CallAs(ctx, c, "config.get", map[string]any{}, &cfg); err != nil {
		return fmt.Errorf("config.get: %w", err)
	}
	patch := map[string]any{
		"mcp": map[string]any{"servers": map[string]any{name: map[string]any{
			"headers": map[string]any{"X-Yuno-Boot": strconv.FormatInt(time.Now().Unix(), 10)},
		}}},
		// Re-asserted at every start so a gateway configured before this key existed
		// exposes platform tools directly (see Baseline).
		"tools": map[string]any{"toolSearch": false},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	if _, err := ConfigPatch(ctx, c, string(raw), cfg.Hash, nil); err != nil {
		return fmt.Errorf("config.patch mcp.servers.%s: %w", name, err)
	}
	return nil
}
