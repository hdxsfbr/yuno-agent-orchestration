import type { AgentMessage } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import JsonView from "../JsonView";

/** The messages agents sent each other during the run, oldest first. */
export default function MessagesSection({ messages }: { messages: AgentMessage[] }) {
  const sorted = messages
    .map((m, i) => ({ m, i }))
    .sort((a, b) => Date.parse(a.m.created_at) - Date.parse(b.m.created_at) || a.i - b.i)
    .map((x) => x.m);
  return (
    <ul className="space-y-2">
      {sorted.map((m) => (
        <li key={m.id} className="rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
          <div className="flex flex-wrap items-center gap-x-3 text-sm">
            <span className="font-mono font-medium">{m.from_node}</span>
            <span aria-label="to" className="text-zinc-400">
              →
            </span>
            <span className="font-mono font-medium">{m.to_node}</span>
            <span className="ml-auto text-xs text-zinc-500">{formatDateTime(m.created_at)}</span>
          </div>
          <div className="mt-2">
            <JsonView label="Message" value={m.payload} defaultOpen />
          </div>
        </li>
      ))}
    </ul>
  );
}
