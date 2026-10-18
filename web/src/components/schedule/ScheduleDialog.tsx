import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { ApiError, createSchedule, listWorkflows, updateSchedule, type Schedule, type ScheduleInput, type Workflow } from "@/lib/api";
import { errorsFor, type FieldErrors } from "@/lib/agentForm";
import { draftFromInput, emptyDraft, inputFields } from "@/lib/runInput";
import { requestRef } from "@/lib/mergeRequest";
import { browserTimeZone, buildSchedule, scheduleErrors, timeZones, toLocalInput, type ScheduleFormState } from "@/lib/schedule";
import { projectsKey, useProjects } from "@/lib/useProjects";
import { isShippedTemplate } from "@/lib/workflow";
import Button from "../Button";
import InputFields from "../InputFields";
import { Modal } from "../Modal";
import MergeRequestSelect from "../project/MergeRequestSelect";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";
import { CheckboxField, Field, SelectField, TextField, control, toOptions } from "../fields";

type FormProps = { workflows: Workflow[]; schedule?: Schedule; onClose: () => void };

function initialState(workflows: Workflow[], schedule?: Schedule): ScheduleFormState {
  const wf = workflows.find((w) => w.id === schedule?.workflow_id);
  const fields = inputFields(wf?.definition.input_schema);
  return {
    workflowId: schedule?.workflow_id ?? "",
    projectId: schedule?.project_id ?? "",
    mergeRequestId: schedule?.merge_request_id ?? "",
    kind: schedule?.kind ?? "cron",
    cronExpr: schedule?.cron_expr ?? "",
    tz: schedule?.tz || browserTimeZone(),
    runAt: toLocalInput(schedule?.run_at),
    enabled: schedule?.enabled ?? true,
    draft: schedule && fields.length > 0 ? draftFromInput(fields, schedule.input) : emptyDraft(fields),
    whole: schedule && fields.length === 0 && Object.keys(schedule.input ?? {}).length > 0 ? JSON.stringify(schedule.input, null, 2) : "{}",
  };
}

function ScheduleForm({ workflows, schedule, onClose }: FormProps) {
  const qc = useQueryClient();
  const [form, setForm] = useState<ScheduleFormState>(() => initialState(workflows, schedule));
  const [clientErrors, setClientErrors] = useState<FieldErrors>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  // A shipped template names its agents instead of pointing at them, so it cannot be scheduled.
  const choices = workflows.filter((w) => !isShippedTemplate(w) || w.id === schedule?.workflow_id);
  const workflow = workflows.find((w) => w.id === form.workflowId);
  const fields = useMemo(() => inputFields(workflow?.definition.input_schema), [workflow]);
  const zones = useMemo(() => toOptions(timeZones(form.tz, browserTimeZone())), [form.tz]);
  const set = (patch: Partial<ScheduleFormState>) => setForm((f) => ({ ...f, ...patch }));

  // A workflow that opts in works on a project, which the schedule names for its runs.
  const needsProject = !!workflow?.definition.project;
  const projects = useProjects(needsProject);
  // A project deleted since it was chosen no longer counts as chosen.
  const projectId = projects.data?.some((p) => p.id === form.projectId) ? form.projectId : "";
  const noProjects = needsProject && !!projects.data && projects.data.length === 0;

  const save = useMutation({
    mutationFn: (input: ScheduleInput) => (schedule ? updateSchedule(schedule.id, input) : createSchedule(input)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["schedules"] });
      onClose();
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(scheduleErrors(err.details));
        setFormError(err.message);
        // A refused project may be one that was just deleted: show the current list.
        if (needsProject) {
          void qc.invalidateQueries({ queryKey: projectsKey });
          // A refused request may be one that was just merged or closed.
          void qc.invalidateQueries({ queryKey: ["merge-requests"] });
        }
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
    const built = buildSchedule({ ...form, projectId }, fields, needsProject, schedule?.merge_request_id ?? "");
    setClientErrors(built.errors);
    if (built.value) save.mutate(built.value);
  };

  const errors = (path: string) => [...errorsFor(clientErrors, path), ...errorsFor(serverErrors, path)];
  const mergeRequestShown = needsProject && projectId !== "";
  const known = new Set(["workflow_id", "project_id", "kind", "cron_expr", "tz", "run_at", "enabled", "input", ...(mergeRequestShown ? ["merge_request_id"] : [])]);
  // The request a saved schedule names stays on offer while its project is the chosen one.
  const savedProject = projects.data?.find((p) => p.id === schedule?.project_id);
  const current =
    schedule?.merge_request_id && schedule.project_id === projectId
      ? {
          id: schedule.merge_request_id,
          label: savedProject && schedule.merge_request_number != null ? requestRef({ provider: savedProject.provider, number: schedule.merge_request_number }) : "Merge request",
        }
      : undefined;
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !known.has(k.split(".")[0]))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  const showSummary = formError && unplaced.length === 0 && Object.keys(serverErrors).length === 0;

  return (
    <form onSubmit={submit} noValidate className="space-y-4">
      {choices.length === 0 ? (
        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          There are no workflows to schedule yet.{" "}
          <Link to="/workflows" className="underline">
            Create a workflow
          </Link>{" "}
          first.
        </p>
      ) : (
        <SelectField
          label="Workflow"
          required
          value={form.workflowId}
          errors={errors("workflow_id")}
          onChange={(id) => {
            const next = workflows.find((w) => w.id === id);
            set({ workflowId: id, projectId: "", mergeRequestId: "", draft: emptyDraft(inputFields(next?.definition.input_schema)), whole: "{}" });
          }}
          options={[
            { value: "", label: "Choose a workflow" },
            ...(schedule && !workflow ? [{ value: schedule.workflow_id, label: schedule.workflow_name ? `${schedule.workflow_name} (deleted)` : "Deleted workflow" }] : []),
            ...choices.map((w) => ({ value: w.id, label: w.name })),
          ]}
        />
      )}

      {needsProject ? (
        noProjects ? (
          <p className="text-sm">
            This workflow works on a project repository. Create one on the{" "}
            <Link to="/projects" className="underline">
              Projects page
            </Link>{" "}
            first.
          </p>
        ) : projects.error ? (
          <QueryError what="the projects" error={projects.error} onRetry={() => void projects.refetch()} />
        ) : projects.data ? (
          <SelectField
            label="Project"
            required
            value={projectId}
            onChange={(v) => set({ projectId: v, mergeRequestId: "" })}
            options={[{ value: "", label: "Choose a project" }, ...projects.data.map((p) => ({ value: p.id, label: p.name }))]}
            hint="Each run works on a checkout of this project's repository."
            errors={errors("project_id")}
          />
        ) : (
          <Loading label="Loading projects…" />
        )
      ) : null}

      {mergeRequestShown ? (
        <MergeRequestSelect
          projectId={projectId}
          value={form.mergeRequestId}
          onChange={(mergeRequestId) => set({ mergeRequestId })}
          errors={errors("merge_request_id")}
          current={current}
        />
      ) : null}

      <SelectField
        label="Runs"
        value={form.kind}
        errors={errors("kind")}
        onChange={(kind) => set({ kind: kind === "delay" ? "delay" : "cron" })}
        options={[
          { value: "cron", label: "On a repeating schedule" },
          { value: "delay", label: "Once, at a set time" },
        ]}
      />

      {form.kind === "cron" ? (
        <>
          <TextField
            label="Cron expression"
            required
            mono
            value={form.cronExpr}
            onChange={(v) => set({ cronExpr: v })}
            placeholder="0 9 * * 1-5"
            hint="Minute, hour, day of month, month, day of week. For example, 0 9 * * 1-5 runs on weekdays at 09:00."
            errors={errors("cron_expr")}
          />
          <SelectField label="Time zone" value={form.tz} onChange={(tz) => set({ tz })} options={zones} errors={errors("tz")} />
        </>
      ) : (
        <Field label="Run at" required hint={`In your time zone (${browserTimeZone()}).`} errors={errors("run_at")}>
          {(p) => <input {...p} type="datetime-local" value={form.runAt} onChange={(e) => set({ runAt: e.target.value })} className={control(errors("run_at").length > 0)} />}
        </Field>
      )}

      <CheckboxField checked={form.enabled} onChange={(enabled) => set({ enabled })} label="Enabled" hint="A disabled schedule keeps its settings but does not run." />

      {workflow || (schedule && fields.length === 0) ? (
        <fieldset className="space-y-4 rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
          <legend className="px-1 text-sm font-medium">Input</legend>
          <InputFields
            fields={fields}
            draft={form.draft}
            onChange={(name, value) => set({ draft: { ...form.draft, [name]: value } })}
            errorsOf={(name) => errors(`input.${name}`)}
            whole={form.whole}
            onWholeChange={(whole) => set({ whole })}
            wholeErrors={errors("input")}
          />
        </fieldset>
      ) : null}

      {unplaced.length > 0 ? (
        <div className="space-y-1">
          {unplaced.map((m, i) => (
            <InlineError key={i}>{m}</InlineError>
          ))}
        </div>
      ) : null}
      {showSummary ? <InlineError>{formError}</InlineError> : null}

      <div className="flex justify-end gap-2">
        <Button onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={save.isPending || choices.length === 0 || (needsProject && !projects.data?.length)}>
          {save.isPending ? "Saving…" : schedule ? "Save changes" : "Create schedule"}
        </Button>
      </div>
    </form>
  );
}

/** Create or edit a schedule. The form opens once the workflows are known, so a saved input can be shown field by field. */
export default function ScheduleDialog({ schedule, onClose }: { schedule?: Schedule; onClose: () => void }) {
  const q = useQuery({ queryKey: ["workflows"], queryFn: listWorkflows });
  return (
    <Modal title={schedule ? "Edit schedule" : "New schedule"} onClose={onClose}>
      {q.isLoading ? <Loading label="Loading workflows…" /> : null}
      {q.error ? <QueryError what="workflows" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data ? <ScheduleForm workflows={q.data} schedule={schedule} onClose={onClose} /> : null}
    </Modal>
  );
}
