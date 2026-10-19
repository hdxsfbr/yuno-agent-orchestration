import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, listAgentFiles, putAgentFile, type AgentFile } from "@/lib/api";
import { formatBytes, formatDateTime } from "@/lib/format";
import { Badge } from "../Badge";
import Button from "../Button";
import { ConfirmDialog } from "../Modal";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";
import { control } from "../fields";

export const agentFilesKey = (agentId: string) => ["agent-files", agentId] as const;

/** The most an addition or an override may hold. */
export const FILE_LIMIT = 64 * 1024;

export const byteLength = (s: string) => new TextEncoder().encode(s).length;

/** What a save told the operator: saved, or saved and waiting for the runtime. */
type Outcome = { kind: "saved" | "pending"; text: string };

/** The message for a refused save, by status. */
function saveFailure(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 409) return `This file cannot be saved yet: ${err.message}`;
    if (err.status === 400) return `The file was not saved: ${err.message}`;
  }
  return errorText(err);
}

/** Every workspace file of an agent, with a way to add to the generated ones and to override the runtime ones. */
export default function AgentFiles({ agentId }: { agentId: string }) {
  const q = useQuery({ queryKey: agentFilesKey(agentId), queryFn: () => listAgentFiles(agentId) });
  const [selected, setSelected] = useState<string | null>(null);
  const [dirty, setDirty] = useState(false);
  const [switchTo, setSwitchTo] = useState<string | null>(null);

  // A page reload or tab close would lose the text being typed.
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const files = q.data ?? [];
  const file = files.find((f) => f.name === selected);

  const open = (name: string) => {
    if (name === selected) return;
    if (dirty) setSwitchTo(name);
    else setSelected(name);
  };

  return (
    <section aria-labelledby={`files-${agentId}`} className="rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
      <h2 id={`files-${agentId}`} className="text-sm font-semibold">
        Workspace files
      </h2>
      <p className="mt-1 text-sm text-zinc-500">The files the agent reads from its workspace. Add your own text to the ones the platform writes, or take over the ones the runtime keeps.</p>
      {q.isLoading ? <Loading label="Loading files…" /> : null}
      {q.error ? <QueryError what="the files" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {files.length > 0 ? (
        <ul className="mt-3 divide-y divide-zinc-200 rounded-md border border-zinc-200 dark:divide-zinc-800 dark:border-zinc-800">
          {files.map((f) => (
            <li key={f.name} className="flex flex-wrap items-center justify-between gap-2 p-2.5 text-sm">
              <div className="flex flex-wrap items-center gap-2">
                <span className="font-mono font-medium">{f.name}</span>
                <Badge tone={f.kind === "generated" ? "blue" : "zinc"}>{f.kind === "generated" ? "Written by the platform" : "Runtime file"}</Badge>
                {!f.exists && f.override == null ? <Badge tone="amber">Not created yet</Badge> : null}
                {f.kind === "generated" && f.addition ? <Badge tone="green">Your additions</Badge> : null}
                {f.kind === "runtime" && f.override != null ? <Badge tone="green">Overridden</Badge> : null}
                {f.truncated ? <Badge tone="amber">Truncated</Badge> : null}
              </div>
              <Button size="sm" onClick={() => open(f.name)} aria-label={`${f.name === selected ? "Editing" : f.exists || f.override != null || f.kind === "generated" ? "Open" : "Create"} ${f.name}`}>
                {f.name === selected ? "Editing" : f.exists || f.override != null || f.kind === "generated" ? "Open" : "Create"}
              </Button>
            </li>
          ))}
        </ul>
      ) : null}

      {file ? <FileEditor key={file.name} agentId={agentId} file={file} onDirty={setDirty} /> : null}

      {switchTo ? (
        <ConfirmDialog
          title="Discard your changes?"
          message={`You have unsaved changes in ${selected}. Opening ${switchTo} discards them.`}
          confirmLabel="Discard changes"
          onConfirm={() => {
            setDirty(false);
            setSelected(switchTo);
            setSwitchTo(null);
          }}
          onCancel={() => setSwitchTo(null)}
        />
      ) : null}
    </section>
  );
}

function SizeCounter({ text }: { text: string }) {
  const n = byteLength(text);
  return (
    <span className={`text-xs ${n > FILE_LIMIT ? "text-red-600 dark:text-red-400" : "text-zinc-500"}`} aria-live="polite">
      {formatBytes(n)} of {formatBytes(FILE_LIMIT)}
    </span>
  );
}

const editorClass = (extra = "") => control(false, `font-mono text-xs leading-relaxed ${extra}`);

function FileEditor({ agentId, file, onDirty }: { agentId: string; file: AgentFile; onDirty: (d: boolean) => void }) {
  const qc = useQueryClient();
  const generated = file.kind === "generated";
  const locked = !generated && file.redacted === true;
  const stored = generated ? (file.addition ?? "") : locked ? "" : (file.override ?? file.content);
  const [text, setText] = useState(stored);
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [confirmStop, setConfirmStop] = useState(false);

  const dirty = text !== stored;
  useEffect(() => {
    onDirty(dirty);
    return () => onDirty(false);
  }, [dirty, onDirty]);

  const refresh = () => qc.invalidateQueries({ queryKey: agentFilesKey(agentId) });

  const save = useMutation({
    mutationFn: (edit: { addition?: string; override?: string | null }) => putAgentFile(agentId, file.name, edit),
    onSuccess: (_, edit) => {
      setFailure(null);
      setOutcome({ kind: "saved", text: edit.override === null ? "Stopped overriding. The runtime manages this file again." : "Saved." });
      setConfirmStop(false);
      if (edit.override === null) setText(locked ? "" : file.content);
      void refresh();
    },
    onError: (err, edit) => {
      setConfirmStop(false);
      if (err instanceof ApiError && err.status === 502) {
        // The edit is stored; only the push to the runtime failed.
        setFailure(null);
        setOutcome({ kind: "pending", text: "Saved. It will apply when the runtime is reachable." });
        if (edit.override === null) setText(locked ? "" : file.content);
        void refresh();
      } else {
        setOutcome(null);
        setFailure(saveFailure(err));
      }
    },
  });

  const tooBig = byteLength(text) > FILE_LIMIT;
  const submit = () => {
    setOutcome(null);
    setFailure(null);
    save.mutate(generated ? { addition: text } : { override: text });
  };
  const creating = !generated && !file.exists && file.override == null;

  return (
    <div className="mt-4 space-y-3 border-t border-zinc-200 pt-4 dark:border-zinc-800">
      <h3 className="font-mono text-sm font-semibold">{file.name}</h3>
      {generated ? (
        <>
          <p className="text-sm text-zinc-600 dark:text-zinc-400">The platform writes this file and keeps its own part. What you add below is placed after it.</p>
          <div>
            <label htmlFor={`current-${file.name}`} className="mb-1 block text-sm font-medium">
              Current content
            </label>
            <textarea id={`current-${file.name}`} readOnly rows={10} value={file.content} className={editorClass("bg-zinc-50 dark:bg-zinc-950")} />
            {file.truncated ? <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">The file is longer than shown. Only the first 256 KiB are displayed.</p> : null}
            {!file.exists ? <p className="mt-1 text-xs text-zinc-500">This file has not been written yet.</p> : null}
          </div>
          <div>
            <label htmlFor={`edit-${file.name}`} className="mb-1 block text-sm font-medium">
              Your additions
            </label>
            <textarea id={`edit-${file.name}`} rows={8} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} placeholder="Text to add after the platform's part" className={editorClass()} />
          </div>
        </>
      ) : locked ? (
        <p className="text-sm text-amber-700 dark:text-amber-400">This file contains secrets that are hidden here, so it can't be edited from this page.</p>
      ) : (
        <div>
          <p className="mb-2 text-sm text-zinc-600 dark:text-zinc-400">
            {creating
              ? "This file does not exist yet. Write its content and save to create it."
              : file.override != null
                ? "You are overriding this file. The platform writes your text over what the runtime wrote."
                : "The runtime manages this file. Save your own text to take it over."}
          </p>
          <label htmlFor={`edit-${file.name}`} className="mb-1 block text-sm font-medium">
            Content
          </label>
          <textarea id={`edit-${file.name}`} rows={14} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} className={editorClass()} />
          {file.truncated && file.override == null ? (
            <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">The file is longer than shown. Only the first 256 KiB are displayed, and saving replaces the whole file with this text.</p>
          ) : null}
        </div>
      )}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-3">
          {locked ? null : <SizeCounter text={text} />}
          {file.updated_at ? <span className="text-xs text-zinc-500">Last edited {formatDateTime(file.updated_at)}</span> : null}
          {dirty ? <span className="text-xs text-amber-700 dark:text-amber-400">Unsaved changes</span> : null}
        </div>
        <div className="flex gap-2">
          {!generated && file.override != null ? (
            <Button variant="ghost" onClick={() => setConfirmStop(true)} disabled={save.isPending}>
              Stop overriding
            </Button>
          ) : null}
          <Button variant="primary" onClick={submit} disabled={locked || save.isPending || !dirty || tooBig}>
            {save.isPending ? "Saving…" : creating ? "Create file" : "Save"}
          </Button>
        </div>
      </div>
      {tooBig ? <InlineError>This is over the 64 KiB limit. Shorten it to save.</InlineError> : null}
      {failure ? <InlineError>{failure}</InlineError> : null}
      {outcome ? (
        <p role="status" className={`text-sm ${outcome.kind === "saved" ? "text-emerald-700 dark:text-emerald-400" : "text-amber-700 dark:text-amber-400"}`}>
          {outcome.text}
        </p>
      ) : null}

      {confirmStop ? (
        <ConfirmDialog
          title={`Stop overriding ${file.name}?`}
          message="Your text is deleted and the runtime manages the file again. The platform does not restore the text the runtime had written before."
          confirmLabel="Stop overriding"
          busy={save.isPending}
          onConfirm={() => {
            setOutcome(null);
            setFailure(null);
            save.mutate({ override: null });
          }}
          onCancel={() => setConfirmStop(false)}
        />
      ) : null}
    </div>
  );
}
