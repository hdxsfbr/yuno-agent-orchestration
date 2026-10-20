package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yuno/internal/obs"
	"yuno/internal/store"
)

// DESIGN 5.15 rules 1 and 3: a chat token resolves to its agent, the agent's workspace
// and its most recent conversation (the one with the latest message).
func TestResolveChatToken(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	oc := "agchat"
	a := env.agent(func(a *store.Agent) {
		a.OpenClawAgentID = &oc
		a.ToolAllowlist = []string{"delegate_task", "list_agents"}
		a.InteractionRules = map[string]string{"delegate_task": "require_approval"}
	})
	if len(a.ChatToken) != 34 {
		t.Fatalf("chat token = %q", a.ChatToken)
	}
	cc, ok := env.e.ResolveChatToken(a.ChatToken)
	if !ok || cc.AgentID != a.ID || cc.OpenClawAgentID != oc || cc.Workspace != filepath.Join(env.dir, "workspace-"+oc) ||
		cc.ConversationID != "" || len(cc.ToolAllowlist) != 2 || cc.InteractionRules["delegate_task"] != "require_approval" {
		t.Fatalf("no conversation: %+v %v", cc, ok)
	}

	older, err := store.UpsertConversation(ctx, env.pool, &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: "1",
		AgentID: a.ID, SessionKey: "agent:agchat:telegram:direct:1"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.UpsertConversation(ctx, env.pool, &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: "2",
		AgentID: a.ID, SessionKey: "agent:agchat:telegram:direct:2"})
	if err != nil {
		t.Fatal(err)
	}
	say := func(convID, id string, at time.Time) {
		if _, err := store.InsertConversationMessage(ctx, env.pool, &store.ConversationMessage{ConversationID: convID,
			Direction: store.DirectionIn, ExternalMessageID: id, Text: "hi", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	say(newer.ID, "m1", now.Add(-time.Hour))
	say(older.ID, "m2", now)
	if cc, ok := env.e.ResolveChatToken(a.ChatToken); !ok || cc.ConversationID != older.ID {
		t.Fatalf("latest conversation = %+v", cc)
	}

	for name, token := range map[string]string{"unknown": "c_" + "abcdefghijklmnopqrstuvwxyz234567", "empty": "", "run token": "r_abcdefghijklmnopqrstuvwx"} {
		if _, ok := env.e.ResolveChatToken(token); ok {
			t.Errorf("%s token resolved", name)
		}
	}
	if err := store.DeleteAgent(ctx, env.pool, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.e.ResolveChatToken(a.ChatToken); ok {
		t.Fatal("a deleted agent's chat token resolved")
	}
}

// DESIGN 5.15 rule 15: materialising a chat agent writes its token into AGENTS.md and
// nowhere else: not into the logs, nor the runtime view the API returns.
func TestMaterializeChatAgentKeepsTokenOutOfLogs(t *testing.T) {
	env := newEnv(t)
	var buf bytes.Buffer
	env.e.log = slog.New(obs.RedactingHandler(slog.NewJSONHandler(&buf, nil)))
	a := env.agent(func(a *store.Agent) {
		a.ToolAllowlist = []string{"delegate_task", "list_agents", "list_workflows", "run_status"}
	})
	view, err := env.e.MaterializeAgent(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(env.dir, "workspace-"+view.OpenClawAgentID, "AGENTS.md"))
	if err != nil || !strings.Contains(string(md), a.ChatToken) || !strings.Contains(string(md), "## Chatting") {
		t.Fatalf("AGENTS.md = %s %v", md, err)
	}
	// Whatever is logged about the agent, its AGENTS.md included, is redacted.
	env.e.log.Info("agent files", "agents_md", string(md), "chat_token", a.ChatToken)
	viewJSON, _ := json.Marshal(view)
	if !strings.Contains(buf.String(), "agent materialised") || strings.Contains(buf.String(), a.ChatToken) ||
		strings.Contains(string(viewJSON), a.ChatToken) {
		t.Fatalf("chat token leaked:\nlogs: %s\nview: %s", buf.String(), viewJSON)
	}
}

// A chat token the model copied with a slip of one or two characters still names its
// agent; three edits, or a slip that two agents' tokens are equally close to, do not.
func TestResolveChatTokenCopyingSlip(t *testing.T) {
	env := newEnv(t)
	a := env.agent(nil)
	tok := a.ChatToken
	flip := func(s string, i int) string {
		c := byte('a')
		if s[i] == 'a' {
			c = 'b'
		}
		return s[:i] + string(c) + s[i+1:]
	}
	for name, cand := range map[string]string{
		"one changed": flip(tok, 10),
		"one dropped": tok[:20] + tok[21:],
		"two changed": flip(flip(tok, 5), 25),
		"one added":   tok[:7] + "x" + tok[7:],
	} {
		cc, ok := env.e.ResolveChatToken(cand)
		if !ok || cc.AgentID != a.ID {
			t.Fatalf("%s: resolved %v %q", name, ok, cc.AgentID)
		}
	}
	if _, ok := env.e.ResolveChatToken(flip(flip(flip(tok, 3), 15), 30)); ok {
		t.Fatal("three edits resolved")
	}
	if _, ok := env.e.ResolveChatToken(tok[:10]); ok {
		t.Fatal("a short prefix resolved")
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		max  int
		want int
	}{
		{"abcdef", "abcdef", 2, 0},
		{"abcdef", "abxdef", 2, 1},
		{"abcdef", "abdef", 2, 1},
		{"abcdef", "xbcdey", 2, 2},
		{"abcdef", "xycdez", 2, 3},
		{"abcdef", "abcdefghi", 2, 3},
	} {
		if got := editDistance(c.a, c.b, c.max); got != c.want {
			t.Fatalf("editDistance(%q, %q, %d) = %d, want %d", c.a, c.b, c.max, got, c.want)
		}
	}
}
