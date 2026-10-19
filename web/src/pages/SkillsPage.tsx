import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { deleteSkill, type Skill, type SkillSource } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { skillsKey, useSkills } from "@/lib/useSkills";
import Button from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import { ConfirmDialog } from "@/components/Modal";
import { EmptyState, Loading, QueryError, errorText } from "@/components/QueryState";
import SkillDialog from "@/components/skill/SkillDialog";
import SkillFilesDialog from "@/components/skill/SkillFilesDialog";
import { ImportGitDialog, UploadSkillDialog } from "@/components/skill/SkillImportDialogs";
import { SOURCES, SourceBadge, sourceLabel, sourceOf } from "@/components/skill/SkillSource";

/** Shown when an import was stored but the runtime could not be reached. */
const PENDING_NOTICE = "Saved. It will apply when the runtime is reachable.";

export default function SkillsPage() {
  const qc = useQueryClient();
  const q = useSkills();
  const [editing, setEditing] = useState<Skill | "new" | null>(null);
  const [deleting, setDeleting] = useState<Skill | null>(null);
  const [filesOf, setFilesOf] = useState<Skill | null>(null);
  const [importing, setImporting] = useState<"upload" | "git" | null>(null);
  const [source, setSource] = useState<SkillSource | "all">("all");
  const [notice, setNotice] = useState<string | null>(null);

  const remove = useMutation({
    mutationFn: (s: Skill) => deleteSkill(s.id),
    onSuccess: () => {
      setDeleting(null);
      void qc.invalidateQueries({ queryKey: skillsKey });
      void qc.invalidateQueries({ queryKey: ["agents"] });
    },
  });

  const shown = (q.data ?? []).filter((s) => source === "all" || sourceOf(s) === source);

  return (
    <div>
      <PageHeader title="Skills">
        <Button onClick={() => setImporting("upload")}>Upload skill</Button>
        <Button onClick={() => setImporting("git")}>Import from Git</Button>
        <Button variant="primary" onClick={() => setEditing("new")}>
          New skill
        </Button>
      </PageHeader>

      {notice ? (
        <p role="status" className="mb-3 rounded-lg border border-emerald-200 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300">
          {notice}
        </p>
      ) : null}
      {q.data && q.data.length > 0 ? (
        <div className="mb-3 flex items-center gap-2 text-sm">
          <label htmlFor="skill-source" className="text-zinc-600 dark:text-zinc-400">
            Source
          </label>
          <select
            id="skill-source"
            value={source}
            onChange={(e) => setSource(e.target.value as SkillSource | "all")}
            className="rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900"
          >
            <option value="all">All sources</option>
            {SOURCES.map((x) => (
              <option key={x} value={x}>
                {sourceLabel(x)}
              </option>
            ))}
          </select>
        </div>
      ) : null}
      {q.isLoading ? <Loading label="Loading skills…" /> : null}
      {q.error ? <QueryError what="skills" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? <EmptyState>No skills yet. A skill is a set of instructions you can give to any agent.</EmptyState> : null}
      {q.data && q.data.length > 0 && shown.length === 0 ? <EmptyState>No skills come from this source.</EmptyState> : null}
      {shown.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[56rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Description</th>
                <th className="px-4 py-2.5 font-medium">Source</th>
                <th className="px-4 py-2.5 font-medium">Licence</th>
                <th className="px-4 py-2.5 font-medium">Files</th>
                <th className="px-4 py-2.5 font-medium">Updated</th>
                <th className="px-4 py-2.5 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {shown.map((s) => (
                <tr key={s.id} className="hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                  <td className="px-4 py-2.5">
                    <button type="button" onClick={() => setEditing(s)} className="font-mono font-medium hover:underline">
                      {s.name}
                    </button>
                  </td>
                  <td className="max-w-md truncate px-4 py-2.5 text-zinc-600 dark:text-zinc-400" title={s.description}>
                    {s.description || "—"}
                  </td>
                  <td className="px-4 py-2.5">
                    <SourceBadge skill={s} />
                  </td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{s.license || "—"}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{s.files_count ?? 0}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(s.updated_at)}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-right">
                    <div className="flex justify-end gap-1.5">
                      <Button size="sm" onClick={() => setFilesOf(s)} aria-label={`Files of ${s.name}`}>
                        Files
                      </Button>
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
      {filesOf ? <SkillFilesDialog skill={filesOf} onClose={() => setFilesOf(null)} /> : null}
      {importing === "upload" ? (
        <UploadSkillDialog
          onClose={() => setImporting(null)}
          onDone={(sk) => {
            setImporting(null);
            setNotice(sk ? `Skill ${sk.name} uploaded.` : PENDING_NOTICE);
          }}
        />
      ) : null}
      {importing === "git" ? (
        <ImportGitDialog
          onClose={() => setImporting(null)}
          onDone={(sk) => {
            setImporting(null);
            setNotice(sk ? `Skill ${sk.name} imported.` : PENDING_NOTICE);
          }}
        />
      ) : null}
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
