import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { deleteSkill, type Skill } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { skillsKey, useSkills } from "@/lib/useSkills";
import Button from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import { EmptyState, Loading, QueryError, errorText } from "@/components/QueryState";
import SkillDialog from "@/components/skill/SkillDialog";

export default function SkillsPage() {
  const qc = useQueryClient();
  const q = useSkills();
  const [editing, setEditing] = useState<Skill | "new" | null>(null);
  const [deleting, setDeleting] = useState<Skill | null>(null);

  const remove = useMutation({
    mutationFn: (s: Skill) => deleteSkill(s.id),
    onSuccess: () => {
      setDeleting(null);
      void qc.invalidateQueries({ queryKey: skillsKey });
      void qc.invalidateQueries({ queryKey: ["agents"] });
    },
  });

  return (
    <div>
      <PageHeader title="Skills">
        <Button variant="primary" onClick={() => setEditing("new")}>
          New skill
        </Button>
      </PageHeader>

      {q.isLoading ? <Loading label="Loading skills…" /> : null}
      {q.error ? <QueryError what="skills" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No skills yet. A skill is a set of instructions you can give to any agent.</EmptyState> : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[40rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Description</th>
                <th className="px-4 py-2.5 font-medium">Updated</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((s) => (
                <tr key={s.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                  <td className="px-4 py-2.5">
                    <button type="button" onClick={() => setEditing(s)} className="font-mono font-medium hover:underline">
                      {s.name}
                    </button>
                  </td>
                  <td className="max-w-md truncate px-4 py-2.5 text-zinc-600 dark:text-zinc-400" title={s.description}>
                    {s.description || "—"}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(s.updated_at)}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-right">
                    <div className="flex justify-end gap-1.5">
                      <Button size="sm" onClick={() => setEditing(s)} aria-label={`Edit ${s.name}`}>
                        Edit
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => setDeleting(s)} aria-label={`Delete ${s.name}`}>
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

      {editing ? <SkillDialog key={editing === "new" ? "new" : editing.id} skill={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} /> : null}
      {deleting ? (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          message="The skill is deleted and agents can no longer use it."
          confirmLabel="Delete skill"
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
