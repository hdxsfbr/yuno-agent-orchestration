package httpapi

import (
	"testing"
	"time"

	"yuno/internal/config"
)

// DESIGN 5.24 rules 2 and 9: the platform tools reach the engine through the Asker, and
// ask_operator blocks for MCP_TOOL_WAIT. Without an engine there is no Asker: both tools
// answer that they cannot run (a nil interface, not an interface holding a nil engine).
func TestPlatformDepsCarryTheAskerAndTheWait(t *testing.T) {
	cfg := config.Config{MCPToolWait: 7 * time.Second}
	eng := &fakeEngine{}
	pd := (&Server{}).platformDeps(Deps{Cfg: cfg, Engine: eng})
	if pd.Asker != eng {
		t.Errorf("Asker = %v, want the engine", pd.Asker)
	}
	if pd.MCPToolWait != 7*time.Second {
		t.Errorf("MCPToolWait = %v, want 7s", pd.MCPToolWait)
	}
	if pd := (&Server{}).platformDeps(Deps{Cfg: cfg}); pd.Asker != nil {
		t.Errorf("Asker without an engine = %v, want nil", pd.Asker)
	}
}
