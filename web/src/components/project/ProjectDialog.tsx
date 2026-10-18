import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, createProject, listWorkflows, updateProject, type Project, type ProjectInput, type ProjectProvider } from "@/lib/api";
import { addError, errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { capText, parseCap } from "@/lib/project";
import { projectsKey } from "@/lib/useProjects";
import Button from "../Button";
import { Modal } from "../Modal";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";
import { CheckboxField, Field, FieldErrors as FieldErrorList, NumberField, SelectField, TextAreaField, TextField, control } from "../fields";

const FIELDS = ["name", "repo_url", "provider", "default_branch", "token", "description", "instructions", "max_cost_usd_per_day", "max_cost_usd_per_month"];
/** The intake settings the form places next to their fields; any other `intake` error is listed under the form. */
const INTAKE_FIELDS = ["intake.enabled", "intake.label", "intake.workflow_id"];
/** The label the intake starts with. */
const DEFAULT_INTAKE_LABEL = "yuno";
const INTAKE_LABEL_MAX = 50;
const NAME_MAX = 64;
const DESCRIPTION_MAX = 1024;
const INSTRUCTIONS_MAX = 8192;

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
  const [instructions, setInstructions] = useState(project?.instructions ?? "");
  const [dayCap, setDayCap] = useState(capText(project?.max_cost_usd_per_day));
  const [monthCap, setMonthCap] = useState(capText(project?.max_cost_usd_per_month));
  const [intakeEnabled, setIntakeEnabled] = useState(project?.intake?.enabled ?? false);
  const [intakeLabel, setIntakeLabel] = useState(project?.intake?.label || DEFAULT_INTAKE_LABEL);
  const [intakeWorkflow, setIntakeWorkflow] = useState(project?.intake?.workflow_id ?? "");
  // The settings are sent only once the section was touched: an update without them keeps the stored ones.
  const [intakeTouched, setIntakeTouched] = useState(false);
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  const tokenSet = !!project?.token_set;

  // The workflows an issue can run: the operator's own, never a shipped template.
  const workflows = useQuery({ queryKey: ["workflows"], queryFn: listWorkflows });
  const operatorWorkflows = (workflows.data ?? []).filter((w) => !w.template_key);
  // A workflow deleted since it was chosen no longer counts as chosen; until the list is known the stored choice stands.
  const workflowId = !workflows.data || operatorWorkflows.some((w) => w.id === intakeWorkflow) ? intakeWorkflow : "";

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
    const standing = instructions.trim();
    // The server counts bytes; this first guard counts characters and the server's 422 covers the rest.
    if (standing.length > INSTRUCTIONS_MAX) found.instructions = [`Instructions must be at most ${INSTRUCTIONS_MAX} characters`];
    const day = parseCap(dayCap);
    if (!day.ok) addError(found, "max_cost_usd_per_day", day.message);
    const month = parseCap(monthCap);
    if (!month.ok) addError(found, "max_cost_usd_per_month", month.message);
    if (intakeTouched) {
      const label = intakeLabel.trim();
      if (label === "" || label.length > INTAKE_LABEL_MAX || /[\s,]/.test(label)) addError(found, "intake.label", `1 to ${INTAKE_LABEL_MAX} characters without spaces or commas`);
      if (intakeEnabled && workflowId === "") addError(found, "intake.workflow_id", "Choose a workflow");
    }
    setClientErrors(found);
    if (Object.keys(found).length > 0 || !day.ok || !month.ok) return;

    const input: ProjectInput = {
      name: n,
      repo_url: repoUrl.trim(),
      provider,
      sandbox_network: network,
      description: description.trim(),
      instructions: standing,
      // An empty cap is sent as null: an update replaces the settings, so it clears a cap that was set.
      max_cost_usd_per_day: day.value,
      max_cost_usd_per_month: month.value,
    };
    if (intakeTouched) input.intake = { enabled: intakeEnabled, label: intakeLabel.trim(), ...(workflowId ? { workflow_id: workflowId } : {}) };
    if (branch.trim() !== "") input.default_branch = branch.trim();
    // A token left empty on an edit keeps the stored one, so it stays out of the body.
    if (token.trim() !== "") input.token = token.trim();
    save.mutate(input);
  };

  const errors = (path: string) => [...errorsFor(clientErrors, path), ...errorsFor(serverErrors, path)];
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !FIELDS.includes(k.split(".")[0]) && !INTAKE_FIELDS.includes(k))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  // With no cap in the form, the server refuses to enable the intake.
  const dayCapNow = parseCap(dayCap);
  const monthCapNow = parseCap(monthCap);
  const noBudget = dayCapNow.ok && dayCapNow.value === null && monthCapNow.ok && monthCapNow.value === null;
  const touchIntake = <T,>(set: (v: T) => void) => (v: T) => {
    setIntakeTouched(true);
    set(v);
  };
  const shownLabel = intakeLabel.trim() || DEFAULT_INTAKE_LABEL;
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
        <TextAreaField
          label="Instructions"
          rows={6}
          value={instructions}
          onChange={setInstructions}
          hint="Standing instructions for every run on this project: conventions, how to build and test, what to leave alone."
          errors={errors("instructions")}
        />
        <fieldset className="space-y-2">
          <legend className="mb-1 text-sm font-medium">Budget</legend>
          <div className="grid gap-4 sm:grid-cols-2">
            <NumberField label="Per day (USD)" value={dayCap} onChange={setDayCap} placeholder="No cap" errors={errors("max_cost_usd_per_day")} />
            <NumberField label="Per month (USD)" value={monthCap} onChange={setMonthCap} placeholder="No cap" errors={errors("max_cost_usd_per_month")} />
          </div>
          <p className="text-xs text-zinc-500">Leave empty for no cap. New runs are refused once the cap is reached; running runs finish.</p>
        </fieldset>
        <fieldset className="space-y-3">
          <legend className="mb-1 text-sm font-medium">Issue intake</legend>
          <CheckboxField role="switch" checked={intakeEnabled} onChange={touchIntake(setIntakeEnabled)} label="Enabled" errors={errors("intake.enabled")} />
          {intakeEnabled && noBudget ? <p className="text-xs text-zinc-500">Set a daily or monthly budget before enabling intake.</p> : null}
          <TextField
            label="Label"
            mono
            value={intakeLabel}
            onChange={touchIntake(setIntakeLabel)}
            placeholder={DEFAULT_INTAKE_LABEL}
            hint={`Issues carrying this label become runs. The platform moves it to ${shownLabel}:running, ${shownLabel}:done or ${shownLabel}:failed as the work goes.`}
            errors={errors("intake.label")}
          />
          {workflows.error ? (
            <>
              <QueryError what="the workflows" error={workflows.error} onRetry={() => void workflows.refetch()} />
              <FieldErrorList errors={errors("intake.workflow_id")} />
            </>
          ) : workflows.data ? (
            <SelectField
              label="Workflow"
              required={intakeEnabled}
              value={workflowId}
              onChange={touchIntake(setIntakeWorkflow)}
              options={[{ value: "", label: "Choose a workflow" }, ...operatorWorkflows.map((w) => ({ value: w.id, label: w.name }))]}
              hint="Each issue starts a run of this workflow, with the issue as its task."
              errors={errors("intake.workflow_id")}
            />
          ) : (
            <>
              <Loading label="Loading workflows…" />
              <FieldErrorList errors={errors("intake.workflow_id")} />
            </>
          )}
        </fieldset>
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
