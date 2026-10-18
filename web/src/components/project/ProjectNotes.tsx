import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { deleteProjectFact, listProjectFacts, type ProjectFact } from "@/lib/api";
import { formatDateTime, shortId } from "@/lib/format";
import { projectFactsKey } from "@/lib/useProjects";
import Button from "../Button";
import { Card } from "../Card";
import { ConfirmDialog } from "../Modal";
import { Loading, QueryError, errorText } from "../QueryState";

/** The notes agents saved while they worked on the project, newest first, each with a way to delete it. */
export default function ProjectNotes({ projectId }: { projectId: string }) {
  const qc = useQueryClient();
  const key = projectFactsKey(projectId);
  const q = useQuery({ queryKey: key, queryFn: () => listProjectFacts(projectId) });
  const [deleting, setDeleting] = useState<ProjectFact | null>(null);

  const remove = useMutation({
    mutationFn: (fact: ProjectFact) => deleteProjectFact(projectId, fact.id),
    onSuccess: (_, fact) => {
      setDeleting(null);
      qc.setQueryData<ProjectFact[]>(key, (old) => old?.filter((f) => f.id !== fact.id));
      void qc.invalidateQueries({ queryKey: key });
    },
  });

  const notes = q.data ?? [];
  return (
    <section aria-labelledby="project-notes">
      <h2 id="project-notes" className="mb-2 text-base font-semibold">
        Notes
        {q.data ? <span className="ml-2 text-sm font-normal text-zinc-500">{notes.length}</span> : null}
      </h2>
      <Card>
        {q.isLoading ? <Loading label="Loading notes…" /> : null}
        {q.error ? <QueryError what="the notes" error={q.error} onRetry={() => void q.refetch()} /> : null}
        {q.data && notes.length === 0 ? <p className="text-sm text-zinc-500">No notes yet. Agents save what they learn about the project while they work on it.</p> : null}
        {notes.length > 0 ? (
          <ul className="divide-y divide-zinc-200 dark:divide-zinc-800">
            {notes.map((f) => (
              <li key={f.id} className="flex items-start justify-between gap-3 py-3 first:pt-0 last:pb-0">
                <div className="min-w-0 text-sm">
                  <p className="break-words font-mono text-xs font-medium">{f.key}</p>
                  <p className="mt-0.5 max-h-32 overflow-auto whitespace-pre-wrap break-words">{f.value}</p>
                  <p className="mt-1 text-xs text-zinc-500">
                    {`Saved by ${f.agent_name || "an agent that was deleted"}`}
                    {f.source_run_id ? (
                      <>
                        {" · "}
                        <Link to={`/runs/${encodeURIComponent(f.source_run_id)}`} className="underline" title="The run it was saved in">
                          Run {shortId(f.source_run_id)}
                        </Link>
                      </>
                    ) : null}
                    {` · ${formatDateTime(f.updated_at)}`}
                    {f.expires_at ? ` · Expires ${formatDateTime(f.expires_at)}` : ""}
                  </p>
                </div>
                <Button size="sm" variant="ghost" onClick={() => setDeleting(f)} aria-label={`Delete note ${f.key}`}>
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        ) : null}
      </Card>

      {deleting ? (
        <ConfirmDialog
          title={`Delete note ${deleting.key}?`}
          message="Agents working on this project will no longer see it."
          confirmLabel="Delete note"
          busy={remove.isPending}
          error={remove.error ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate(deleting)}
          onCancel={() => {
            remove.reset();
            setDeleting(null);
          }}
        />
      ) : null}
    </section>
  );
}
