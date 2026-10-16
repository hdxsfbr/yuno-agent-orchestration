import { Link } from "react-router";
import type { Conversation } from "@/lib/api";
import { formatCount, formatDateTime } from "@/lib/format";
import { byRecentActivity } from "@/lib/useChannels";
import { cardClass } from "../Card";
import { channelLabel, chatName } from "./Transcript";

/** Conversations as a list of links, newest activity first; the open one is marked. */
export default function ConversationList({ conversations, selectedId }: { conversations: Conversation[]; selectedId?: string }) {
  return (
    <ul className={`${cardClass} divide-y divide-zinc-200 overflow-hidden dark:divide-zinc-800`}>
      {byRecentActivity(conversations).map((c) => {
        const selected = c.id === selectedId;
        return (
          <li key={c.id}>
            <Link
              to={`/channels/${c.id}`}
              aria-current={selected ? "true" : undefined}
              className={`block px-4 py-3 text-sm ${selected ? "bg-zinc-100 dark:bg-zinc-800" : "hover:bg-zinc-50 dark:hover:bg-zinc-800/40"}`}
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="truncate font-medium">{c.agent_name}</span>
                <span className="shrink-0 text-xs tabular-nums text-zinc-500">
                  {formatCount(c.message_count)} {c.message_count === 1 ? "message" : "messages"}
                </span>
              </div>
              <div className="mt-0.5 truncate text-xs text-zinc-600 dark:text-zinc-400">
                {channelLabel(c.channel)} · {chatName(c)}
              </div>
              <div className="mt-0.5 text-xs text-zinc-500">Last message {formatDateTime(c.last_message_at)}</div>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
