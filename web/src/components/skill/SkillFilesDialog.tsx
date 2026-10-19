import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, deleteSkillFile, downloadSkillExport, getSkillFile, listSkillFiles, putSkillFile, skillExportUrl, type Skill, type SkillFile } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { skillsKey } from "@/lib/useSkills";
import Button from "../Button";
import { ConfirmDialog, Modal } from "../Modal";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";
import { control } from "../fields";
import { SourceLink } from "./SkillSource";

export const skillFilesKey = (skillId: string) => ["skill-files", skillId] as const;

const EXECUTABLE = 0o755;

/** The tree of a bundle: a heading per folder, then its files, in path order. */
function treeRows(files: SkillFile[]): { key: string; depth: number; folder?: string; file?: SkillFile }[] {
  const rows: { key: string; depth: number; folder?: string; file?: SkillFile }[] = [];
  const seen = new Set<string>();
  for (const f of [...files].sort((a, b) => a.path.localeCompare(b.path))) {
    const parts = f.path.split("/");
    for (let i = 1; i < parts.length; i++) {
      const dir = parts.slice(0, i).join("/");
      if (!seen.has(dir)) {
        seen.add(dir);
        rows.push({ key: `dir:${dir}`, depth: i - 1, folder: parts[i - 1] });
      }
    }
    rows.push({ key: `file:${f.path}`, depth: parts.length - 1, file: f });
  }
  return rows;
}

/** A skill's files: a tree, a text editor for the text ones, and the whole bundle as a zip. */
export default function SkillFilesDialog({ skill, onClose }: { skill: Skill; onClose: () => void }) {
  const q = useQuery({ queryKey: skillFilesKey(skill.id), queryFn: () => listSkillFiles(skill.id) });
  const [path, setPath] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [switchTo, setSwitchTo] = useState<string | null | undefined>(undefined);
  const [exportError, setExportError] = useState<string | null>(null);

  const files = q.data ?? [];
  const open = (next: string) => {
    if (next === path) return;
    if (dirty) setSwitchTo(next);
    else setPath(next);
  };
  const close = () => {
    if (dirty) setSwitchTo(null);
    else onClose();
  };

  const exportZip = async (e: { preventDefault: () => void }) => {
    e.preventDefault();
    setExportError(null);
    try {
      const blob = await downloadSkillExport(skill.id);
      const href = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = href;
      a.download = `${skill.name}.zip`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(href);
    } catch (err) {
      setExportError(`Could not export the skill: ${errorText(err)}`);
    }
  };

  return (
    <Modal title={`Files of ${skill.name}`} onClose={close} wide>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3 text-sm">
        <div className="text-zinc-600 dark:text-zinc-400">
          <SourceLink skill={skill} />
        </div>
        <a href={skillExportUrl(skill.id)} onClick={(e) => void exportZip(e)} className="rounded-md border border-zinc-300 px-3 py-1 text-sm font-medium hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800">
          Export zip
        </a>
      </div>
      {exportError ? <InlineError>{exportError}</InlineError> : null}
      {q.isLoading ? <Loading label="Loading files…" /> : null}
      {q.error ? <QueryError what="the files" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && files.length === 0 ? <p className="text-sm text-zinc-500">This skill has no files besides its instructions.</p> : null}
      {files.length > 0 ? (
        <div className="grid gap-4 md:grid-cols-[16rem_1fr]">
          <ul aria-label="Files" className="max-h-96 overflow-auto rounded-md border border-zinc-200 p-1 text-sm dark:border-zinc-800">
            {treeRows(files).map((r) =>
              r.file ? (
                <li key={r.key} style={{ paddingLeft: `${r.depth * 0.75}rem` }}>
                  <button
                    type="button"
                    onClick={() => open(r.file!.path)}
                    aria-current={r.file.path === path ? "true" : undefined}
                    className={`flex w-full items-center justify-between gap-2 rounded px-2 py-1 text-left hover:bg-zinc-100 dark:hover:bg-zinc-800 ${r.file.path === path ? "bg-zinc-100 dark:bg-zinc-800" : ""}`}
                  >
                    <span className="min-w-0 truncate font-mono text-xs">
                      {r.file.path.split("/").pop()}
                      {r.file.mode === EXECUTABLE ? (
                        <span className="ml-1.5 rounded bg-amber-100 px-1 text-[10px] font-sans text-amber-800 dark:bg-amber-950 dark:text-amber-300" title="Executable">
                          exec
                        </span>
                      ) : null}
                    </span>
                    <span className="shrink-0 text-xs text-zinc-500">{formatBytes(r.file.size)}</span>
                  </button>
                </li>
              ) : (
                <li key={r.key} style={{ paddingLeft: `${r.depth * 0.75}rem` }} className="px-2 py-1 font-mono text-xs text-zinc-500">
                  {r.folder}/
                </li>
              ),
            )}
          </ul>
          <div>
            {path ? (
              <FileView key={path} skillId={skill.id} path={path} listed={files.find((f) => f.path === path)} onDirty={setDirty} onRemoved={() => setPath(null)} />
            ) : (
              <p className="text-sm text-zinc-500">Choose a file to read or edit it.</p>
            )}
          </div>
        </div>
      ) : null}
      <div className="mt-4 flex justify-end">
        <Button onClick={close}>Close</Button>
      </div>
      {switchTo !== undefined ? (
        <ConfirmDialog
          title="Discard your changes?"
          message={`You have unsaved changes in ${path}. ${switchTo === null ? "Closing discards them." : `Opening ${switchTo} discards them.`}`}
          confirmLabel="Discard changes"
          onConfirm={() => {
            setDirty(false);
            if (switchTo === null) onClose();
            else setPath(switchTo);
            setSwitchTo(undefined);
          }}
          onCancel={() => setSwitchTo(undefined)}
        />
      ) : null}
    </Modal>
  );
}

const MAX_TEXT = 256 * 1024;

function FileView({ skillId, path, listed, onDirty, onRemoved }: { skillId: string; path: string; listed?: SkillFile; onDirty: (d: boolean) => void; onRemoved: () => void }) {
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["skill-file", skillId, path],
    queryFn: () => getSkillFile(skillId, path),
    retry: false,
    // The editor owns the text once it is open.
    staleTime: Infinity,
    gcTime: 0,
  });
  const [text, setText] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const loaded = q.data?.content;
  useEffect(() => {
    if (loaded !== undefined) {
      setText(loaded);
      setSaved(loaded);
    }
  }, [loaded]);

  const dirty = text !== null && text !== saved;
  useEffect(() => {
    onDirty(dirty);
    return () => onDirty(false);
  }, [dirty, onDirty]);

  const changed = () => {
    void qc.invalidateQueries({ queryKey: skillFilesKey(skillId) });
    void qc.invalidateQueries({ queryKey: skillsKey });
    void qc.invalidateQueries({ queryKey: ["agents"] });
  };

  const save = useMutation({
    mutationFn: (content: string) => putSkillFile(skillId, path, content, listed?.mode),
    onSuccess: (_, content) => {
      setSaved(content);
      setFailure(null);
      setNotice("Saved.");
      changed();
    },
    onError: (err, content) => {
      if (err instanceof ApiError && err.status === 502) {
        setSaved(content);
        setFailure(null);
        setNotice("Saved. Agents that use this skill will get it when the runtime is reachable.");
        changed();
      } else {
        setNotice(null);
        setFailure(errorText(err));
      }
    },
  });

  const remove = useMutation({
    mutationFn: () => deleteSkillFile(skillId, path),
    onSuccess: () => {
      onDirty(false);
      changed();
      onRemoved();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 502) {
        onDirty(false);
        changed();
        onRemoved();
      }
    },
  });

  if (q.isLoading) return <Loading label="Loading file…" />;
  if (q.error instanceof ApiError && q.error.status === 415) {
    return (
      <div className="space-y-1 text-sm">
        <p className="font-mono text-xs font-semibold">{path}</p>
        <p>
          binary file{listed?.size != null ? <span className="text-zinc-500"> · {formatBytes(listed.size)}</span> : null}
        </p>
        <p className="text-xs text-zinc-500">It is kept in the skill and given to the agent as it is. Export the zip to read it.</p>
      </div>
    );
  }
  if (q.error) return <QueryError what="the file" error={q.error} onRetry={() => void q.refetch()} />;
  if (text === null) return <Loading label="Loading file…" />;

  const size = new TextEncoder().encode(text).length;
  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between gap-2">
        <p className="min-w-0 truncate font-mono text-xs font-semibold">{path}</p>
        <span className={`text-xs ${size > MAX_TEXT ? "text-red-600 dark:text-red-400" : "text-zinc-500"}`}>{formatBytes(size)}</span>
      </div>
      <label htmlFor="skill-file-text" className="sr-only">
        Content of {path}
      </label>
      <textarea
        id="skill-file-text"
        rows={16}
        value={text}
        spellCheck={false}
        onChange={(e) => {
          setText(e.target.value);
          setNotice(null);
        }}
        className={control(false, "font-mono text-xs leading-relaxed")}
      />
      {dirty ? <p className="text-xs text-amber-700 dark:text-amber-400">Unsaved changes</p> : null}
      {failure ? <InlineError>{failure}</InlineError> : null}
      {notice ? (
        <p role="status" className="text-sm text-emerald-700 dark:text-emerald-400">
          {notice}
        </p>
      ) : null}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" onClick={() => setConfirmDelete(true)} disabled={save.isPending}>
          Delete file
        </Button>
        <Button variant="primary" onClick={() => save.mutate(text)} disabled={save.isPending || !dirty}>
          {save.isPending ? "Saving…" : "Save file"}
        </Button>
      </div>
      {confirmDelete ? (
        <ConfirmDialog
          title={`Delete ${path}?`}
          message="The file is removed from the skill. Agents that use the skill lose it too."
          confirmLabel="Delete file"
          busy={remove.isPending}
          error={remove.error && !(remove.error instanceof ApiError && remove.error.status === 502) ? errorText(remove.error) : null}
          onConfirm={() => remove.mutate()}
          onCancel={() => {
            remove.reset();
            setConfirmDelete(false);
          }}
        />
      ) : null}
    </div>
  );
}
