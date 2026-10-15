import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { ApiError, createRun, type WorkflowDefinition } from "@/lib/api";
import { errorsFor, parseFieldErrors, type FieldErrors } from "@/lib/agentForm";
import { buildInput, emptyDraft, inputFields, parseWholeInput, type InputDraft } from "@/lib/runInput";
import Button from "../Button";
import { CheckboxField, Field, NumberField, TextField, control } from "../fields";
import { Modal } from "../Modal";
import { InlineError, errorText } from "../QueryState";

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

  const start = useMutation({
    mutationFn: (input: Record<string, unknown>) => createRun({ workflow_id: workflow.id, input }),
    onSuccess: (run) => {
      void qc.invalidateQueries({ queryKey: ["runs"] });
      navigate(`/runs/${run.id}`);
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
    if (start.isPending) return;
    setServerErrors({});
    setFormError(null);
    if (fields.length === 0) {
      const parsed = parseWholeInput(whole);
      if (parsed.error || !parsed.input) {
        setClientErrors({ "": parsed.error ?? "" });
        return;
      }
      setClientErrors({});
      start.mutate(parsed.input);
      return;
    }
    const built = buildInput(fields, draft);
    setClientErrors(built.errors);
    if (Object.keys(built.errors).length === 0) start.mutate(built.input);
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
  const unplaced = Object.entries(serverErrors)
    .filter(([k]) => !known.has(k.split(".")[0]))
    .flatMap(([k, m]) => m.map((x) => (k ? `${k}: ${x}` : x)));

  return (
    <Modal title={`Run ${workflow.name}`} onClose={start.isPending ? () => undefined : onClose}>
      <form onSubmit={submit} className="space-y-4">
        {fields.length === 0 ? (
          <Field label="Input" hint="The workflow declares no input fields. Give the run's input as a JSON object." errors={clientErrors[""] ? [clientErrors[""]] : []}>
            {(p) => <textarea {...p} rows={6} value={whole} spellCheck={false} onChange={(e) => setWhole(e.target.value)} className={control(!!clientErrors[""], "font-mono text-xs")} />}
          </Field>
        ) : (
          fields.map((f) => {
            const common = { label: f.name, required: f.required, hint: f.description, errors: errorsOf(f.name) };
            const value = draft[f.name];
            if (f.type === "boolean") {
              return <CheckboxField key={f.name} label={f.name} hint={f.description} checked={value === true} onChange={(v) => set(f.name, v)} />;
            }
            if (f.type === "number" || f.type === "integer") {
              return <NumberField key={f.name} {...common} value={typeof value === "string" ? value : ""} onChange={(v) => set(f.name, v)} />;
            }
            if (f.type === "json") {
              return (
                <Field key={f.name} {...common} hint={f.description ?? "JSON"}>
                  {(p) => <textarea {...p} rows={4} value={typeof value === "string" ? value : ""} spellCheck={false} onChange={(e) => set(f.name, e.target.value)} className={control(common.errors.length > 0, "font-mono text-xs")} />}
                </Field>
              );
            }
            return <TextField key={f.name} {...common} value={typeof value === "string" ? value : ""} onChange={(v) => set(f.name, v)} />;
          })
        )}

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
          <Button type="submit" variant="primary" disabled={start.isPending}>
            {start.isPending ? "Starting…" : "Start run"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
