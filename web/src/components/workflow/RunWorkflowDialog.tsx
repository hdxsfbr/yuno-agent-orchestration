import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { ApiError, createRun, type WorkflowDefinition } from "@/lib/api";
import { errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { buildInput, emptyDraft, inputFields, parseWholeInput, type InputDraft } from "@/lib/runInput";
import { projectsKey, useProjects } from "@/lib/useProjects";
import Button from "../Button";
import InputFields from "../InputFields";
import { Modal } from "../Modal";
import { InlineError, Loading, QueryError, errorText } from "../QueryState";
import { SelectField } from "../fields";

type Props = {
  workflow: { id: string; name: string; definition: WorkflowDefinition };
  onClose: () => void;
};

/** Collects the run input (a form built from the workflow's input schema), starts the run and opens it. */
export default function RunWorkflowDialog({ workflow, onClose }: Props) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const fields = useMemo(() => inputFields(workflow.definition.input_schema), [workflow.definition.input_schema]);
  const [draft, setDraft] = useState<InputDraft>(() => emptyDraft(fields));
  const [whole, setWhole] = useState("{}");
  const [clientErrors, setClientErrors] = useState<Record<string, string>>({});
  const [serverErrors, setServerErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<string | null>(null);

  // A workflow that opts in works on a project, which the run names when it starts.
  const needsProject = !!workflow.definition.project;
  const projects = useProjects(needsProject);
  const [projectChoice, setProjectChoice] = useState("");
  const [projectError, setProjectError] = useState<string | null>(null);
  // A project deleted since it was chosen no longer counts as chosen.
  const projectId = projects.data?.some((p) => p.id === projectChoice) ? projectChoice : "";
  const noProjects = needsProject && !!projects.data && projects.data.length === 0;

  const start = useMutation({
    mutationFn: ({ input, project }: { input: Record<string, unknown>; project: string }) =>
      createRun({ workflow_id: workflow.id, input, ...(needsProject ? { project_id: project } : {}) }),
    onSuccess: (run) => {
      void qc.invalidateQueries({ queryKey: ["runs"] });
      navigate(`/runs/${run.id}`);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 422) {
        setServerErrors(parseFieldErrors(err.details));
        setFormError(err.message);
        // A refused project may be one that was just deleted: show the current list.
        if (needsProject) void qc.invalidateQueries({ queryKey: projectsKey });
      } else {
        setFormError(errorText(err));
      }
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (start.isPending) return;
    setServerErrors({});
    setFormError(null);
    const missingProject = needsProject && projectId === "";
    setProjectError(missingProject ? "Choose a project" : null);
    let input: Record<string, unknown> | undefined;
    if (fields.length === 0) {
      const parsed = parseWholeInput(whole);
      if (parsed.error || !parsed.input) {
        setClientErrors({ "": parsed.error ?? "" });
      } else {
        setClientErrors({});
        input = parsed.input;
      }
    } else {
      const built = buildInput(fields, draft);
      setClientErrors(built.errors);
      if (Object.keys(built.errors).length === 0) input = built.input;
    }
    if (input && !missingProject) start.mutate({ input, project: projectId });
  };

  const set = (name: string, value: string | boolean) => {
    setDraft((d) => ({ ...d, [name]: value }));
    setClientErrors((c) => {
      if (!(name in c)) return c;
      const rest = { ...c };
      delete rest[name];
      return rest;
    });
  };

  const errorsOf = (name: string) => [...(clientErrors[name] ? [clientErrors[name]] : []), ...errorsFor(serverErrors, name)];
  const known = new Set(fields.map((f) => f.name));
  if (needsProject) known.add("project_id");
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !known.has(k.split(".")[0]))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));
  const projectErrors = [...(projectError ? [projectError] : []), ...errorsFor(serverErrors, "project_id")];

  return (
    <Modal title={`Run ${workflow.name}`} onClose={start.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} className="space-y-4">
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
              onChange={(v) => {
                setProjectChoice(v);
                setProjectError(null);
              }}
              options={[{ value: "", label: "Choose a project" }, ...projects.data.map((p) => ({ value: p.id, label: p.name }))]}
              hint="The agents work on a checkout of this project's repository."
              errors={projectErrors}
            />
          ) : (
            <Loading label="Loading projects…" />
          )
        ) : null}

        <InputFields
          fields={fields}
          draft={draft}
          onChange={set}
          errorsOf={errorsOf}
          whole={whole}
          onWholeChange={setWhole}
          wholeErrors={clientErrors[""] ? [clientErrors[""]] : []}
        />

        {unplaced.length > 0 ? (
          <div className="space-y-1">
            {unplaced.map((m, i) => (
              <InlineError key={i}>{m}</InlineError>
            ))}
          </div>
        ) : null}
        {formError && unplaced.length === 0 && Object.keys(serverErrors).length === 0 ? <InlineError>{formError}</InlineError> : null}

        <div className="flex justify-end gap-2">
          <Button onClick={onClose} disabled={start.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={start.isPending || (needsProject && !projects.data?.length)}>
            {start.isPending ? "Starting…" : "Start run"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
