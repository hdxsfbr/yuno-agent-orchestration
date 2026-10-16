import type { ToolCall } from "@/lib/api";
import { formatCount, formatDateTime } from "@/lib/format";
import { useConversationToolCalls } from "@/lib/useChannels";
import { Badge, StatusBadge } from "../Badge";
import { cardClass } from "../Card";
import JsonView from "../JsonView";
import { QueryError } from "../QueryState";

const startedAt = (c: ToolCall) => c.started_at ?? c.finished_at;
const at = (c: ToolCall) => Date.parse(startedAt(c) ?? "") || 0;

/** The tools an agent called while talking in a conversation, oldest first. Nothing is shown when it called none. */
export default function ToolCalls({ conversationId }: { conversationId: string }) {
  const q = useConversationToolCalls(conversationId);
  const calls = (q.data ?? []).slice().sort((a, b) => at(a) - at(b));

  if (q.error) return <QueryError what="the tool calls" error={q.error} onRetry={() => void q.refetch()} />;
  if (calls.length === 0) return null;

  return (
    <section aria-labelledby={`tool-calls-${conversationId}`} className={cardClass}>
      <h3 id={`tool-calls-${conversationId}`} className="border-b border-zinc-200 px-4 py-3 text-sm font-semibold dark:border-zinc-800">
        Tool calls<span className="ml-2 font-normal text-zinc-500">{formatCount(calls.length)}</span>
      </h3>
      <ul className="max-h-[32rem] divide-y divide-zinc-200 overflow-y-auto dark:divide-zinc-800">
        {calls.map((c) => (
          <li key={c.id} className="space-y-2 px-4 py-3">
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="font-mono font-medium">{c.tool_name}</span>
              <StatusBadge kind="tool" status={c.status} />
              {c.is_error && c.status === "executed" ? <Badge tone="red">Returned an error</Badge> : null}
              <time className="text-xs text-zinc-500" dateTime={startedAt(c)}>
                {formatDateTime(startedAt(c))}
              </time>
            </div>
            <JsonView label="Input" value={c.input} />
            <JsonView label="Output" value={c.output} />
          </li>
        ))}
      </ul>
    </section>
  );
}
