import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getAgentRuntime, materializeAgent } from "@/lib/api";
import Button from "../Button";
import { Badge } from "../Badge";
import { Card } from "../Card";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";

/** What OpenClaw currently holds for the agent, with a way to push the configuration again. */
export default function RuntimeCard({ agentId }: { agentId: string }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["agent-runtime", agentId], queryFn: () => getAgentRuntime(agentId) });
  const push = useMutation({
    mutationFn: () => materializeAgent(agentId),
    onSuccess: (runtime) => {
      qc.setQueryData(["agent-runtime", agentId], runtime);
      void qc.invalidateQueries({ queryKey: ["agents"] });
      void qc.invalidateQueries({ queryKey: ["agent", agentId] });
    },
  });

  return (
    <Card>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold">OpenClaw runtime</h2>
        {q.data && (!q.data.materialized || !q.data.in_sync) ? (
          <Button size="sm" onClick={() => push.mutate()} disabled={push.isPending}>
            {push.isPending ? "Materialising…" : "Materialise now"}
          </Button>
        ) : null}
      </div>
      {q.isLoading ? <Loading label="Loading runtime…" /> : null}
      {q.error ? <QueryError what="the runtime" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {push.error ? <InlineError>Materialisation failed: {errorText(push.error)}</InlineError> : null}
      {q.data ? (
        <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
          <div>
            <dt className="text-xs uppercase tracking-wide text-zinc-500">OpenClaw agent id</dt>
            <dd className="mt-0.5 font-mono">{q.data.openclaw_agent_id || "—"}</dd>
          </div>
          <div>
            <dt className="text-xs uppercase tracking-wide text-zinc-500">State</dt>
            <dd className="mt-0.5 flex flex-wrap gap-1.5">
              <Badge tone={q.data.materialized ? "green" : "amber"}>{q.data.materialized ? "Materialised" : "Not materialised"}</Badge>
              <Badge tone={q.data.in_sync ? "green" : "amber"}>{q.data.in_sync ? "In sync" : "Out of sync"}</Badge>
            </dd>
          </div>
          <div className="sm:col-span-2">
            <dt className="text-xs uppercase tracking-wide text-zinc-500">Workspace</dt>
            <dd className="mt-0.5 break-all font-mono">{q.data.workspace || "—"}</dd>
          </div>
          <div className="sm:col-span-2">
            <dt className="text-xs uppercase tracking-wide text-zinc-500">Files</dt>
            <dd className="mt-0.5">
              {q.data.files && q.data.files.length > 0 ? (
                <ul className="max-h-40 overflow-auto rounded-md border border-zinc-200 bg-zinc-50 p-2 font-mono text-xs dark:border-zinc-800 dark:bg-zinc-950">
                  {q.data.files.map((f) => (
                    <li key={f}>{f}</li>
                  ))}
                </ul>
              ) : (
                <span className="text-zinc-500">No files</span>
              )}
            </dd>
          </div>
        </dl>
      ) : null}
    </Card>
  );
}
