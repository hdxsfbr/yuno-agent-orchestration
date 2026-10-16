import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { deleteWorkflow, instantiateTemplate, listWorkflows, type Workflow } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { isShippedTemplate } from "@/lib/workflow";
import { Badge } from "@/components/Badge";
import Button, { buttonClass } from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import { EmptyState, InlineError, Loading, QueryError, errorText } from "@/components/QueryState";
import RunWorkflowDialog from "@/components/workflow/RunWorkflowDialog";
import TemplatesDrawer from "@/components/workflow/TemplatesDrawer";

export default function WorkflowsPage() {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({ queryKey: ["workflows"], queryFn: listWorkflows });
  const [templates, setTemplates] = useState(false);
  const [running, setRunning] = useState<Workflow | null>(null);
  const [deleting, setDeleting] = useState<Workflow | null>(null);

  const remove = useMutation({
    mutationFn: (w: Workflow) => deleteWorkflow(w.id),
    onSuccess: (_, w) => {
      setDeleting(null);
      qc.removeQueries({ queryKey: ["workflow", w.id] });
      void qc.invalidateQueries({ queryKey: ["workflows"] });
    },
  });

  // A shipped template names its agents instead of pointing at them, so it cannot run or be
  // edited as it is; "Create copy" makes the editable workflow.
  const copy = useMutation({
    mutationFn: (w: Workflow) => instantiateTemplate(w.template_key ?? ""),
    onSuccess: (wf) => {
      void qc.invalidateQueries({ queryKey: ["workflows"] });
      navigate(`/workflows/${wf.id}`);
    },
  });

  return (
    <div>
      <PageHeader title="Workflows">
        <Button onClick={() => setTemplates(true)}>From template</Button>
        <Link to="/workflows/new" className={buttonClass("primary")}>
          New workflow
        </Link>
      </PageHeader>

      {copy.error ? (
        <div className="mb-3">
          <InlineError>Could not create the copy: {errorText(copy.error)}</InlineError>
        </div>
      ) : null}
      {q.isLoading ? <Loading label="Loading workflows…" /> : null}
      {q.error ? <QueryError what="workflows" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No workflows yet. Start from a template or build one from scratch.</EmptyState> : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[48rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 text-right font-medium">Version</th>
                <th className="px-4 py-2.5 text-right font-medium">Nodes</th>
                <th className="px-4 py-2.5 font-medium">Updated</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((w) => {
                const isTemplate = isShippedTemplate(w);
                const madeFrom = w.template_key || w.from_template;
                return (
                  <tr key={w.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                    <td className="px-4 py-2.5">
                      <div className="flex items-center gap-2">
                        <Link to={`/workflows/${w.id}`} className="font-medium hover:underline">
                          {w.name}
                        </Link>
                        {isTemplate ? <Badge tone="blue">Template</Badge> : madeFrom ? <Badge tone="zinc" title={`Made from the ${madeFrom} template`}>From template</Badge> : null}
                      </div>
                      {w.description ? <p className="mt-0.5 max-w-xl truncate text-xs text-zinc-500" title={w.description}>{w.description}</p> : null}
                    </td>
                    <td className="px-4 py-2.5 text-right tabular-nums">{w.version}</td>
                    <td className="px-4 py-2.5 text-right tabular-nums">{w.definition.nodes.length}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(w.updated_at)}</td>
                    <td className="whitespace-nowrap px-4 py-2.5 text-right">
                      <div className="flex justify-end gap-1.5">
                        {isTemplate ? (
                          <Button size="sm" onClick={() => copy.mutate(w)} disabled={copy.isPending} aria-label={`Create a copy of ${w.name}`}>
                            Create copy
                          </Button>
                        ) : (
                          <>
                            <Button size="sm" onClick={() => setRunning(w)} aria-label={`Run ${w.name}`}>
                              Run
                            </Button>
                            <Link to={`/workflows/${w.id}`} className={buttonClass("secondary", "sm")} aria-label={`Edit ${w.name}`}>
                              Edit
                            </Link>
                            <Button size="sm" variant="ghost" onClick={() => setDeleting(w)} aria-label={`Delete ${w.name}`}>
                              Delete
                            </Button>
                          </>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      {templates ? <TemplatesDrawer onClose={() => setTemplates(false)} /> : null}
      {running ? <RunWorkflowDialog workflow={running} onClose={() => setRunning(null)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          message="The workflow is removed. Runs that already used it keep their history."
          confirmLabel="Delete workflow"
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
