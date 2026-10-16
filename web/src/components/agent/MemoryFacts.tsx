import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { deleteAgentMemory, listAgentMemory, type MemoryFact } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import Button from "../Button";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";

export const memoryKey = (agentId: string) => ["agent-memory", agentId] as const;

/** The facts an agent has saved, each with a way to delete it. `enabled` is the agent's saved memory setting. */
export default function MemoryFacts({ agentId, enabled }: { agentId: string; enabled: boolean }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: memoryKey(agentId), queryFn: () => listAgentMemory(agentId) });
  const remove = useMutation({
    mutationFn: (fact: MemoryFact) => deleteAgentMemory(agentId, fact.id),
    onSuccess: (_, fact) => {
      qc.setQueryData<MemoryFact[]>(memoryKey(agentId), (old) => old?.filter((f) => f.id !== fact.id));
      void qc.invalidateQueries({ queryKey: memoryKey(agentId) });
    },
  });

  const facts = q.data ?? [];
  return (
    <section aria-labelledby={`facts-${agentId}`} className="mt-5 border-t border-zinc-200 pt-4 dark:border-zinc-800">
      <h3 id={`facts-${agentId}`} className="text-sm font-semibold">
        Saved facts
      </h3>
      {q.isLoading ? <Loading label="Loading facts…" /> : null}
      {q.error ? <QueryError what="the saved facts" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {remove.error ? <InlineError>Could not delete the fact: {errorText(remove.error)}</InlineError> : null}
      {q.data && !enabled ? (
        <p className="mt-1 text-sm text-zinc-500">Memory is turned off for this agent. It does not save or recall facts.</p>
      ) : null}
      {q.data && enabled && facts.length === 0 ? <p className="mt-1 text-sm text-zinc-500">No facts saved yet.</p> : null}
      {facts.length > 0 ? (
        <ul className="mt-2 divide-y divide-zinc-200 rounded-md border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
          {facts.map((f) => (
            <li key={f.id} className="flex items-start justify-between gap-3 p-3 text-sm">
              <div className="min-w-0">
                <p className="font-mono text-xs font-medium">{f.key}</p>
                <p className="mt-0.5 max-h-32 overflow-auto whitespace-pre-wrap break-words">{f.value}</p>
                <p className="mt-1 text-xs text-zinc-500">
                  Saved {formatDateTime(f.created_at)} · {f.expires_at ? `Expires ${formatDateTime(f.expires_at)}` : "Does not expire"}
                </p>
              </div>
              <Button size="sm" variant="ghost" onClick={() => remove.mutate(f)} disabled={remove.isPending && remove.variables?.id === f.id} aria-label={`Delete ${f.key}`}>
                Delete
              </Button>
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}
