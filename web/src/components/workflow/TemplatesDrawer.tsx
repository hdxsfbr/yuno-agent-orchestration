import { useEffect, useId, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { ApiError, instantiateTemplate, listWorkflowTemplates, type WorkflowTemplate } from "@/lib/api";
import { groupErrors, type Problem } from "@/lib/workflowErrors";
import Button from "../Button";
import { TextField } from "../fields";
import { EmptyState, InlineError, Loading, QueryError, errorText } from "../QueryState";

/** Names of the agents a template's agent nodes use, in order of appearance. */
function agentNames(t: WorkflowTemplate): string[] {
  const seen: string[] = [];
  for (const n of t.definition.nodes) if (n.kind === "agent" && n.agent_name && !seen.includes(n.agent_name)) seen.push(n.agent_name);
  return seen;
}

function TemplateCard({ template, onCreated }: { template: WorkflowTemplate; onCreated: (id: string) => void }) {
  const [name, setName] = useState(template.name);
  const [problems, setProblems] = useState<Problem[]>([]);
  const [error, setError] = useState<string | null>(null);
  const qc = useQueryClient();

  const create = useMutation({
    mutationFn: () => instantiateTemplate(template.key, name.trim() || undefined),
    onSuccess: (wf) => {
      void qc.invalidateQueries({ queryKey: ["workflows"] });
      onCreated(wf.id);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setProblems(groupErrors(err.details).list);
        setError(err.message);
      } else {
        setProblems([]);
        setError(errorText(err));
      }
    },
  });

  const agents = agentNames(template);
  return (
    <li className="space-y-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
      <div>
        <h3 className="text-sm font-semibold">{template.name}</h3>
        <p className="mt-1 text-sm text-zinc-600 dark:text-zinc-400">{template.description}</p>
        <p className="mt-1 text-xs text-zinc-500">
          {template.definition.nodes.length} nodes{agents.length > 0 ? ` · Agents: ${agents.join(", ")}` : ""}
        </p>
      </div>
      <TextField label="Workflow name" value={name} onChange={setName} maxLength={80} />
      {problems.length > 0 ? (
        <div role="alert" className="space-y-1 text-sm text-red-600 dark:text-red-400">
          {problems.map((p, i) => {
            const node = p.target?.type === "node" ? template.definition.nodes[p.target.index] : undefined;
            return <p key={i}>{node ? `Node ${node.id}: ${p.message}` : p.message}</p>;
          })}
          <p className="text-zinc-600 dark:text-zinc-400">
            Create the missing agents on the{" "}
            <Link to="/agents" className="underline">
              Agents page
            </Link>{" "}
            and try again.
          </p>
        </div>
      ) : error ? (
        <InlineError>{error}</InlineError>
      ) : null}
      <div className="flex justify-end">
        <Button variant="primary" onClick={() => create.mutate()} disabled={create.isPending} aria-label={`Use ${template.name}`}>
          {create.isPending ? "Creating…" : "Create workflow"}
        </Button>
      </div>
    </li>
  );
}

/** A panel sliding in from the right with the workflow templates; picking one makes an editable copy. */
export default function TemplatesDrawer({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const titleId = useId();
  const q = useQuery({ queryKey: ["workflow-templates"], queryFn: listWorkflowTemplates });

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-50 flex justify-end bg-zinc-950/50" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <aside
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="flex h-full w-full max-w-md flex-col border-l border-zinc-200 bg-white shadow-xl dark:border-zinc-800 dark:bg-zinc-900"
      >
        <header className="flex items-center justify-between border-b border-zinc-200 px-5 py-3 dark:border-zinc-800">
          <h2 id={titleId} className="text-base font-semibold">
            Start from a template
          </h2>
          <Button size="sm" variant="ghost" onClick={onClose}>
            Close
          </Button>
        </header>
        <div className="flex-1 space-y-3 overflow-y-auto p-5">
          {q.isLoading ? <Loading label="Loading templates…" /> : null}
          {q.error ? <QueryError what="templates" error={q.error} onRetry={() => void q.refetch()} /> : null}
          {q.data && q.data.length === 0 ? <EmptyState>No templates are available.</EmptyState> : null}
          {q.data && q.data.length > 0 ? (
            <ul className="space-y-3">
              {q.data.map((t) => (
                <TemplateCard key={t.key} template={t} onCreated={(id) => navigate(`/workflows/${id}`)} />
              ))}
            </ul>
          ) : null}
        </div>
      </aside>
    </div>
  );
}
