package seed

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"yuno/internal/store"
)

// UpgradeAgents keeps the shipped agents current (D-080, DESIGN 5.15 rule 12). An agent
// the seed wrote and nobody edited gets the shipped role, prompt and allowlist, and an
// empty MaterializedHash so the api pushes it to the gateway; an agent already equal only
// gets its hash recorded. An agent counts as untouched when its seeded_hash matches the
// hash of its current role, prompt and allowlist, or has no hash (made before the column;
// nothing records an operator edit of those fields, so it is taken as untouched). An
// edited agent is kept and logged. It returns the names upgraded.
func UpgradeAgents(ctx context.Context, q store.Querier, log *slog.Logger) ([]string, error) {
	var done []string
	for _, tpl := range Agents {
		a, err := findAgent(ctx, q, tpl.Name)
		if err != nil {
			return done, err
		}
		if a == nil {
			continue
		}
		want := store.AgentSeedHash(tpl.Role, tpl.SystemPrompt, tpl.ToolAllowlist)
		current := store.AgentSeedHash(a.Role, a.SystemPrompt, a.ToolAllowlist)
		if current == want {
			if a.SeededHash == nil || *a.SeededHash != want { // already current: track it from now on
				if err := store.SetAgentSeededHash(ctx, q, a.ID, want); err != nil {
					return done, err
				}
			}
			continue
		}
		if a.SeededHash != nil && *a.SeededHash != current {
			log.Info("seed: kept: edited since seeded; use seed-reset to restore", "agent", a.Name, "id", a.ID)
			continue
		}
		a.Role, a.SystemPrompt, a.ToolAllowlist = tpl.Role, tpl.SystemPrompt, slices.Clone(tpl.ToolAllowlist)
		a.MaterializedHash = "" // the runtime copy is out of date until the api pushes it
		if err := store.UpdateAgent(ctx, q, a); err != nil {
			return done, fmt.Errorf("seed: upgrade agent %s: %w", tpl.Name, err)
		}
		if err := store.SetAgentSeededHash(ctx, q, a.ID, want); err != nil {
			return done, err
		}
		done = append(done, tpl.Name)
	}
	return done, nil
}
