import { useEffect, useRef } from "react";
import { Link } from "react-router";
import type { Conversation, ConversationMessage } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { useConversationMessages } from "@/lib/useChannels";
import { cardClass } from "../Card";
import { EmptyState, Loading, QueryError } from "../QueryState";

export const channelLabel = (channel: string) => channel.charAt(0).toUpperCase() + channel.slice(1);

export const chatName = (c: Pick<Conversation, "display_name" | "external_chat_id">) => c.display_name || c.external_chat_id;

function Bubble({ message, who }: { message: ConversationMessage; who: string }) {
  const incoming = message.direction === "in";
  return (
    <li data-direction={message.direction} className={`flex ${incoming ? "justify-start" : "justify-end"}`}>
      <div
        className={`max-w-[80%] rounded-2xl px-3.5 py-2 text-sm ${
          incoming ? "rounded-bl-sm bg-zinc-100 dark:bg-zinc-800" : "rounded-br-sm bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900"
        }`}
      >
        <span className="sr-only">{who}: </span>
        <p className="whitespace-pre-wrap break-words">{message.text}</p>
        <p className={`mt-1 flex items-center gap-2 text-[11px] ${incoming ? "text-zinc-500" : "text-zinc-300 dark:text-zinc-600"}`}>
          <time dateTime={message.created_at}>{formatDateTime(message.created_at)}</time>
          {message.run_id ? (
            <Link to={`/runs/${message.run_id}`} className="font-mono underline" title={message.run_id}>
              Run {shortId(message.run_id)}
            </Link>
          ) : null}
        </p>
      </div>
    </li>
  );
}

/** The messages of one conversation, oldest first; new ones arrive through the event stream. */
export default function Transcript({ conversation }: { conversation: Conversation }) {
  const q = useConversationMessages(conversation.id);
  const end = useRef<HTMLDivElement>(null);
  const count = q.data?.length ?? 0;
  useEffect(() => {
    end.current?.scrollIntoView?.({ block: "end" });
  }, [count, conversation.id]);

  const messages = (q.data ?? []).slice().sort((a, b) => (Date.parse(a.created_at) || 0) - (Date.parse(b.created_at) || 0));

  return (
    <section aria-label={`Conversation with ${conversation.agent_name}`} className={`${cardClass} flex min-h-64 flex-col`}>
      <header className="border-b border-zinc-200 px-4 py-3 dark:border-zinc-800">
        <h3 className="text-sm font-semibold">{chatName(conversation)}</h3>
        <p className="text-xs text-zinc-500">
          With {conversation.agent_name} · {channelLabel(conversation.channel)}
        </p>
      </header>
      <div className="max-h-[32rem] flex-1 overflow-y-auto p-4">
        {q.isLoading ? <Loading label="Loading messages…" /> : null}
        {q.error ? <QueryError what="the messages" error={q.error} onRetry={() => void q.refetch()} /> : null}
        {q.data && messages.length === 0 ? <EmptyState>No messages in this conversation yet.</EmptyState> : null}
        {messages.length > 0 ? (
          <ul className="space-y-2">
            {messages.map((m) => (
              <Bubble key={m.id} message={m} who={m.direction === "in" ? chatName(conversation) : conversation.agent_name} />
            ))}
          </ul>
        ) : null}
        <div ref={end} />
      </div>
    </section>
  );
}
