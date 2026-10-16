import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { deleteAgent, listAgents, type Agent } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { useRefreshOnEvents } from "@/lib/useRefreshOnEvents";
import { Badge } from "@/components/Badge";
import Button, { buttonClass } from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { ConfirmDialog } from "@/components/Modal";
import QuickRunDialog from "@/components/QuickRunDialog";
import { EmptyState, Loading, QueryError, errorText } from "@/components/QueryState";

const CHANNEL_LABEL: Record<string, string> = { telegram: "Telegram" };

/** "Telegram", or a dash for an agent no channel reaches. */
const channelText = (a: Agent) => (a.channels?.length ? a.channels.map((c) => CHANNEL_LABEL[c] ?? c).join(", ") : "—");

/** What the agent is doing right now: runs in flight, or Idle. */
function AgentState({ agent }: { agent: Agent }) {
  const running = agent.activity?.running ?? 0;
  const waiting = agent.activity?.waiting_approval ?? 0;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {running > 0 ? (
        <Badge tone="blue" pulse>
          Running ({running})
        </Badge>
      ) : null}
      {waiting > 0 ? <Badge tone="amber">Waiting ({waiting})</Badge> : null}
      {running === 0 && waiting === 0 ? <Badge>Idle</Badge> : null}
      {agent.openclaw_agent_id ? null : <Badge tone="amber">Not materialised</Badge>}
    </div>
  );
}

/** Run events that move an agent between Idle, Running and Waiting. */
const ACTIVITY_EVENTS = ["run.status", "node.status"];

export default function AgentsPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["agents"], queryFn: listAgents });
  const connection = useRefreshOnEvents(["agents"], ACTIVITY_EVENTS);
  const [running, setRunning] = useState<Agent | null>(null);
  const [deleting, setDeleting] = useState<Agent | null>(null);
  const remove = useMutation({
    mutationFn: (a: Agent) => deleteAgent(a.id),
    onSuccess: (_, a) => {
      setDeleting(null);
      qc.removeQueries({ queryKey: ["agent", a.id] });
      void qc.invalidateQueries({ queryKey: ["agents"] });
    },
  });

  return (
    <div>
      <PageHeader title="Agents">
        <ConnectionIndicator state={connection} />
        <Link to="/agents/new" className={buttonClass("primary")}>
          New agent
        </Link>
      </PageHeader>

      {q.isLoading ? <Loading label="Loading agents…" /> : null}
      {q.error ? <QueryError what="agents" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No agents yet. Create the first one to start running tasks.</EmptyState> : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[60rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Role</th>
                <th className="px-4 py-2.5 font-medium">Model</th>
                <th className="px-4 py-2.5 font-medium">Channel</th>
                <th className="px-4 py-2.5 text-right font-medium">Tools</th>
                <th className="px-4 py-2.5 font-medium">State</th>
                <th className="px-4 py-2.5 font-medium">Created</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((a) => (
                <tr key={a.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                  <td className="px-4 py-2.5 font-medium">
                    <Link to={`/agents/${a.id}`} className="hover:underline">
                      {a.name}
                    </Link>
                  </td>
                  <td className="max-w-xs truncate px-4 py-2.5 text-zinc-600 dark:text-zinc-400" title={a.role}>
                    {a.role || "—"}
                  </td>
                  <td className="px-4 py-2.5 font-mono text-xs">{a.model}</td>
                  <td className="px-4 py-2.5">{channelText(a)}</td>
                  <td className="px-4 py-2.5 text-right tabular-nums">{a.tool_allowlist?.length ?? 0}</td>
                  <td className="px-4 py-2.5">
                    <AgentState agent={a} />
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(a.created_at)}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-right">
                    <div className="flex justify-end gap-1.5">
                      <Button size="sm" onClick={() => setRunning(a)}>
                        Run
                      </Button>
                      <Link to={`/agents/${a.id}`} className={buttonClass("secondary", "sm")}>
                        Edit
                      </Link>
                      <Button size="sm" variant="ghost" onClick={() => setDeleting(a)}>
                        Delete
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {running ? <QuickRunDialog agent={running} onClose={() => setRunning(null)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          message="The agent is removed. Runs that already used it keep their history."
          confirmLabel="Delete agent"
          busy={remove.isPending}
          error={remove.error ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate(deleting)}
          onCancel={() => {
            remove.reset();
            setDeleting(null);
          }}
        />
      ) : null}
    </div>
  );
}
