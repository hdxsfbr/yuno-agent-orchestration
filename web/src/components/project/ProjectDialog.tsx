import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError, createProject, updateProject, type Project, type ProjectInput, type ProjectProvider } from "@/lib/api";
import { errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { projectsKey } from "@/lib/useProjects";
import Button from "../Button";
import { Modal } from "../Modal";
import { InlineError, errorText } from "../QueryState";
import { CheckboxField, Field, SelectField, TextAreaField, TextField, control } from "../fields";

const FIELDS = ["name", "repo_url", "provider", "default_branch", "token", "description"];
const NAME_MAX = 64;
const DESCRIPTION_MAX = 1024;

const PROVIDER_OPTIONS = [
  { value: "", label: "Detect from the URL" },
  { value: "gitlab", label: "GitLab" },
  { value: "github", label: "GitHub" },
];

const isProvider = (v: string): v is ProjectProvider | "" => v === "" || v === "gitlab" || v === "github";

/**
 * Create or edit a project. The token is write-only: the form never receives the stored
 * one, and an edit that leaves the field empty keeps it.
 */
export default function ProjectDialog({ project, onClose }: { project?: Project; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(project?.name ?? "");
  const [repoUrl, setRepoUrl] = useState(project?.repo_url ?? "");
  const [provider, setProvider] = useState<ProjectProvider | "">(project?.provider ?? "");
  const [branch, setBranch] = useState(project?.default_branch ?? "");
  const [token, setToken] = useState("");
  const [network, setNetwork] = useState(project?.sandbox_network ?? false);
  const [description, setDescription] = useState(project?.description ?? "");
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  const tokenSet = !!project?.token_set;

  const save = useMutation({
    mutationFn: (input: ProjectInput) => (project ? updateProject(project.id, input) : createProject(input)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: projectsKey });
      onClose();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (save.isPending) return;
    setServerErrors({});
    setFormError(null);
    const found: FieldErrors = {};
    const n = name.trim();
    if (n === "") found.name = ["Name is required"];
    if (repoUrl.trim() === "") found.repo_url = ["Repository URL is required"];
    if (!project && token.trim() === "") found.token = ["A token is required"];
    if (description.length > DESCRIPTION_MAX) found.description = [`Description must be at most ${DESCRIPTION_MAX} characters`];
    setClientErrors(found);
    if (Object.keys(found).length > 0) return;

    const input: ProjectInput = {
      name: n,
      repo_url: repoUrl.trim(),
      provider,
      sandbox_network: network,
      description: description.trim(),
    };
    if (branch.trim() !== "") input.default_branch = branch.trim();
    // A token left empty on an edit keeps the stored one, so it stays out of the body.
    if (token.trim() !== "") input.token = token.trim();
    save.mutate(input);
  };

  const errors = (path: string) => [...errorsFor(clientErrors, path), ...errorsFor(serverErrors, path)];
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !FIELDS.includes(k.split(".")[0]))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  const showSummary = formError && unplaced.length === 0 && Object.keys(serverErrors).length === 0;

  return (
    <Modal title={project ? `Edit ${project.name}` : "New project"} onClose={save.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} noValidate className="space-y-4">
        <TextField label="Name" required maxLength={NAME_MAX} value={name} onChange={setName} placeholder="web-app" hint="The name you pick the project by when you start a run." errors={errors("name")} />
        <TextField
          label="Repository URL"
          required
          mono
          value={repoUrl}
          onChange={setRepoUrl}
          placeholder="https://gitlab.com/group/project"
          hint="The https address of the repository, without a user name or password."
          errors={errors("repo_url")}
        />
        <div className="grid gap-4 sm:grid-cols-2">
          <SelectField
            label="Provider"
            value={provider}
            onChange={(v) => setProvider(isProvider(v) ? v : "")}
            options={PROVIDER_OPTIONS}
            hint="Detecting picks GitHub for github.com and GitLab for any other host."
            errors={errors("provider")}
          />
          <TextField label="Default branch" mono value={branch} onChange={setBranch} placeholder="main" hint="Runs start from this branch." errors={errors("default_branch")} />
        </div>
        <Field
          label="Access token"
          required={!project}
          hint={
            tokenSet
              ? "A token is set. Enter a new one to replace it."
              : "A personal access token that can push to the repository. It is not shown again once saved."
          }
          errors={errors("token")}
        >
          {(p) => (
            <input
              {...p}
              type="password"
              autoComplete="new-password"
              data-1p-ignore
              data-lpignore="true"
              spellCheck={false}
              value={token}
              onChange={(e) => setToken(e.target.value)}
              className={control(errors("token").length > 0, "font-mono")}
            />
          )}
        </Field>
        <CheckboxField
          checked={network}
          onChange={setNetwork}
          label="Allow network access in the sandbox"
          hint="Lets agents install dependencies while they work. Turn it on only for repositories you trust."
        />
        <TextAreaField label="Description" rows={3} value={description} onChange={setDescription} hint="What the repository is for, in a sentence or two." errors={errors("description")} />
        {unplaced.map((m, i) => (
          <InlineError key={i}>{m}</InlineError>
        ))}
        {showSummary ? <InlineError>{formError}</InlineError> : null}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={save.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={save.isPending}>
            {save.isPending ? "Saving…" : project ? "Save changes" : "Create project"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
