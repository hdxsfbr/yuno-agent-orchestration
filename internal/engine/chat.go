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
	if errors.Is(err, store.ErrNotFound) {
		a, err = e.nearChatToken(ctx, token)
	}
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

// chatTokenSlack is how many single-character edits a chat token may be off by and still
// name its agent. The model copies its token from AGENTS.md into every chat call and now
// and then slips a character; a caller without the token cannot come that close.
const chatTokenSlack = 2

// nearChatToken finds the agent whose chat token is within chatTokenSlack edits of token,
// when exactly one is; otherwise store.ErrNotFound. The distance is logged, never a token.
func (e *Engine) nearChatToken(ctx context.Context, token string) (*store.Agent, error) {
	if len(token) < 16 {
		return nil, store.ErrNotFound
	}
	agents, err := store.ListAgents(ctx, e.deps.Pool)
	if err != nil {
		return nil, err
	}
	var found *store.Agent
	dist := 0
	for i := range agents {
		a := &agents[i]
		if a.ChatToken == "" {
			continue
		}
		if d := editDistance(token, a.ChatToken, chatTokenSlack); d <= chatTokenSlack {
			if found != nil {
				return nil, store.ErrNotFound // ambiguous: two agents are that close
			}
			found, dist = a, d
		}
	}
	if found == nil {
		return nil, store.ErrNotFound
	}
	e.log.Warn("chat token accepted with a copying slip", "agent_id", found.ID, "edits", dist)
	return found, nil
}

// editDistance is the Levenshtein distance of a and b, or max+1 once it exceeds max.
func editDistance(a, b string, max int) int {
	if d := len(a) - len(b); d > max || -d > max {
		return max + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			low = min(low, cur[j])
		}
		if low > max {
			return max + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
