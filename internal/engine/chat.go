package engine

import (
	"context"
	"errors"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// chatResolveTimeout bounds the lookups behind one chat token.
const chatResolveTimeout = 5 * time.Second

// ResolveChatToken maps a chat token to the agent that owns it and that agent's most
// recent conversation (DESIGN 5.15 rules 1 and 3). Unknown tokens, a deleted agent's
// and lookups that fail all answer ok=false; a failure is logged.
func (e *Engine) ResolveChatToken(token string) (ChatContext, bool) {
	if e.deps.Pool == nil || token == "" {
		return ChatContext{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatResolveTimeout)
	defer cancel()
	a, err := store.GetAgentByChatToken(ctx, e.deps.Pool, token)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.log.Warn("resolve chat token failed", "error", err)
		}
		return ChatContext{}, false
	}
	cc := ChatContext{AgentID: a.ID, AgentName: a.Name, ToolAllowlist: a.ToolAllowlist, InteractionRules: a.InteractionRules}
	if a.OpenClawAgentID != nil && *a.OpenClawAgentID != "" {
		cc.OpenClawAgentID = *a.OpenClawAgentID
		cc.Workspace = openclaw.WorkspaceDir(e.deps.StateDir, cc.OpenClawAgentID)
	}
	conv, err := store.LatestConversationForAgent(ctx, e.deps.Pool, a.ID)
	switch {
	case err == nil:
		cc.ConversationID = conv.ID
	case !errors.Is(err, store.ErrNotFound):
		e.log.Warn("read the agent's latest conversation failed", "agent_id", a.ID, "error", err)
		return ChatContext{}, false
	}
	return cc, true
}
