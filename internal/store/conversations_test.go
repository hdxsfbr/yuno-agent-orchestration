package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// DESIGN 5.10 rule 6: a conversation's transcript leaves out messages older than its
// agent's conversation_ttl_days (7 by default) before the sweep deletes them.
func TestListConversationMessagesAppliesTTL(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	now := mustParse(t, "2026-10-20T12:00:00Z")
	for _, tc := range []struct {
		name     string
		ttl      int
		wantDays int
		want     string
	}{
		{"default seven days", 0, 7, "6d,1h"},
		{"agent's two days", 2, 2, "1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testAgent(t, "Keeper "+tc.name)
			a.MemoryPolicy.ConversationTTLDays = tc.ttl
			mustDo(t, UpdateAgent(ctx, pool, a))
			conv, err := UpsertConversation(ctx, pool, &Conversation{Channel: ChannelTelegram, ExternalChatID: "42",
				AgentID: a.ID, SessionKey: "agent:" + a.ID + ":telegram:direct:42"})
			mustDo(t, err)
			for _, m := range []struct {
				id  string
				age time.Duration
			}{{"8d", 8 * 24 * time.Hour}, {"6d", 6 * 24 * time.Hour}, {"1h", time.Hour}} {
				_, err := InsertConversationMessage(ctx, pool, &ConversationMessage{ConversationID: conv.ID,
					Direction: DirectionIn, ExternalMessageID: m.id, Text: m.id, CreatedAt: now.Add(-m.age)})
				mustDo(t, err)
			}
			got, err := ListConversationMessages(ctx, pool, conv.ID, 100, now)
			mustDo(t, err)
			var ids []string
			for _, m := range got {
				ids = append(ids, m.ExternalMessageID)
			}
			if strings.Join(ids, ",") != tc.want {
				t.Fatalf("messages = %v, want %s", ids, tc.want)
			}
			if days, err := ConversationTTLDays(ctx, pool, conv.ID); err != nil || days != tc.wantDays {
				t.Fatalf("ttl days = %d, %v; want %d", days, err, tc.wantDays)
			}
		})
	}
}
