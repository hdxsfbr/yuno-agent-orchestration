import { useCallback } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getChannels, listConversationMessages, listConversations, type Conversation, type ConversationMessage, type SSEEvent } from "./api";
import { useEventStream, type Connection } from "./useEventStream";

export const channelsKey = ["channels"] as const;
export const conversationsKey = ["conversations"] as const;
export const messagesKey = (id: string) => ["conversation-messages", id] as const;

export const useChannels = () => useQuery({ queryKey: channelsKey, queryFn: getChannels });
export const useConversations = () => useQuery({ queryKey: conversationsKey, queryFn: listConversations });
export const useConversationMessages = (id: string | undefined) =>
  useQuery({ queryKey: messagesKey(id ?? ""), queryFn: () => listConversationMessages(id ?? ""), enabled: !!id });

/** Newest conversation first. */
export function byRecentActivity(list: Conversation[]): Conversation[] {
  const at = (c: Conversation) => Date.parse(c.last_message_at) || 0;
  return list.slice().sort((a, b) => at(b) - at(a));
}

/**
 * Keeps the conversation list and any open transcript current from the global event
 * stream: a message is appended to the transcript that holds it, and the conversation's
 * row moves to the top with its new count.
 */
export function useConversationEvents(): Connection {
  const qc = useQueryClient();
  const onEvent = useCallback(
    (e: SSEEvent) => {
      if (e.event !== "conversation.message") return;
      const d = e.data as { conversation?: Partial<Conversation>; message?: ConversationMessage } | null;
      const conv = d?.conversation;
      const msg = d?.message;
      if (!conv?.id) {
        void qc.invalidateQueries({ queryKey: conversationsKey });
        return;
      }

      if (msg) {
        const key = messagesKey(conv.id);
        // A fetch that started before the message may not include it: ask again instead.
        if (qc.isFetching({ queryKey: key }) > 0) {
          void qc.invalidateQueries({ queryKey: key });
        } else {
          qc.setQueryData<ConversationMessage[]>(key, (old) => (old && !old.some((m) => m.id === msg.id) ? [...old, msg] : old));
        }
      }

      const known = qc.getQueryData<Conversation[]>(conversationsKey)?.some((c) => c.id === conv.id);
      if (known) {
        qc.setQueryData<Conversation[]>(conversationsKey, (old) => old?.map((c) => (c.id === conv.id ? { ...c, ...conv } : c)));
      }
      const complete = typeof conv.message_count === "number" && !!conv.last_message_at;
      if (!known || !complete) void qc.invalidateQueries({ queryKey: conversationsKey });
    },
    [qc],
  );
  return useEventStream("/api/events", onEvent);
}
