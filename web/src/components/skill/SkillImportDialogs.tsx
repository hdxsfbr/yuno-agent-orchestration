import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, importSkillGit, importSkillZip, type Skill } from "@/lib/api";
import { skillsKey } from "@/lib/useSkills";
import Button from "../Button";
import { Modal } from "../Modal";
import { InlineError, errorText } from "../QueryState";
import { TextField } from "../fields";

/** A refused import in words: the message, then each field reason the server gave (once). */
export function importErrorText(err: unknown): string {
  const base = errorText(err);
  if (!(err instanceof ApiError) || err.status !== 422 || !Array.isArray(err.details)) return base;
  const parts = [base];
  for (const d of err.details) {
    const m = d && typeof d === "object" ? (d as { message?: unknown }).message : undefined;
    if (typeof m === "string" && m !== "" && !parts.some((p) => p.includes(m))) parts.push(m);
  }
  return parts.join(": ");
}

/** The runtime push failed after the skill was stored: the import counts as saved. */
const savedPending = (err: unknown) => err instanceof ApiError && err.status === 502 && err.code === "materialize_failed";

/** Refreshes what a new or replaced skill changes: the list, and the agents that use it. */
function useAfterImport(onDone: (skill: Skill | null) => void) {
  const qc = useQueryClient();
  return (skill: Skill | null) => {
    void qc.invalidateQueries({ queryKey: skillsKey });
    void qc.invalidateQueries({ queryKey: ["agents"] });
    void qc.invalidateQueries({ queryKey: ["skill-files"] });
    onDone(skill);
  };
}

function ReplaceBox({ checked, onChange, offer }: { checked: boolean; onChange: (v: boolean) => void; offer: boolean }) {
  return (
    <label className="flex items-start gap-2 text-sm">
      <input type="checkbox" className="mt-0.5" checked={checked} onChange={(e) => onChange(e.target.checked)} />
      <span>
        Replace the existing skill of the same name
        {offer ? <span className="block text-xs text-amber-700 dark:text-amber-400">A skill with this name already exists. Tick this box to replace its instructions and files.</span> : null}
      </span>
    </label>
  );
}

/** Upload a skill as a zip: SKILL.md at its root, or inside one top folder. */
export function UploadSkillDialog({ onClose, onDone }: { onClose: () => void; onDone: (skill: Skill | null) => void }) {
  const after = useAfterImport(onDone);
  const [file, setFile] = useState<File | null>(null);
  const [replace, setReplace] = useState(false);
  const [exists, setExists] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const upload = useMutation({
    mutationFn: () => importSkillZip(file as File, replace),
    onSuccess: after,
    onError: (err) => {
      if (savedPending(err)) {
        after(null);
        return;
      }
      setExists(err instanceof ApiError && err.status === 409);
      setError(importErrorText(err));
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (upload.isPending) return;
    if (!file) {
      setError("Choose a .zip file to upload.");
      return;
    }
    setError(null);
    setExists(false);
    upload.mutate();
  };

  return (
    <Modal title="Upload skill" onClose={upload.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} noValidate className="space-y-4">
        <div>
          <label htmlFor="skill-zip" className="mb-1 block text-sm font-medium">
            Skill archive
          </label>
          <input
            id="skill-zip"
            type="file"
            accept=".zip,application/zip"
            onChange={(e) => {
              setFile(e.target.files?.[0] ?? null);
              setError(null);
            }}
            className="block w-full text-sm"
          />
          <p className="mt-1 text-xs text-zinc-500">A .zip with SKILL.md at its top level, or inside a single folder. To upload a folder, zip it first.</p>
        </div>
        <ReplaceBox checked={replace} onChange={setReplace} offer={exists} />
        {error ? <InlineError>{error}</InlineError> : null}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={upload.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={upload.isPending}>
            {upload.isPending ? "Uploading…" : "Upload skill"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

/** Import a skill from a Git repository. The server's reasons for refusing are shown as they are. */
export function ImportGitDialog({ onClose, onDone }: { onClose: () => void; onDone: (skill: Skill | null) => void }) {
  const after = useAfterImport(onDone);
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [path, setPath] = useState("");
  const [replace, setReplace] = useState(false);
  const [exists, setExists] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = useMutation({
    mutationFn: () => importSkillGit({ url: url.trim(), ...(ref.trim() ? { ref: ref.trim() } : {}), ...(path.trim() ? { path: path.trim() } : {}) }, replace),
    onSuccess: after,
    onError: (err) => {
      if (savedPending(err)) {
        after(null);
        return;
      }
      setExists(err instanceof ApiError && err.status === 409);
      setError(importErrorText(err));
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (run.isPending) return;
    if (url.trim() === "") {
      setError("Enter the repository's https address.");
      return;
    }
    setError(null);
    setExists(false);
    run.mutate();
  };

  return (
    <Modal title="Import from Git" onClose={run.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} noValidate className="space-y-4">
        <TextField label="Repository address" required mono value={url} onChange={setUrl} placeholder="https://github.com/owner/skills" hint="An https address without a user name or password." />
        <TextField label="Branch, tag or commit" mono value={ref} onChange={setRef} placeholder="main" hint="Leave empty for the default branch." />
        <TextField label="Folder in the repository" mono value={path} onChange={setPath} placeholder="skills/code-review" hint="The folder that holds SKILL.md. Leave empty when it is at the top." />
        <ReplaceBox checked={replace} onChange={setReplace} offer={exists} />
        {error ? <InlineError>{error}</InlineError> : null}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={run.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={run.isPending}>
            {run.isPending ? "Importing…" : "Import skill"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
