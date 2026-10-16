import type { InputDraft, InputField } from "@/lib/runInput";
import { CheckboxField, Field, NumberField, TextField, control } from "./fields";

type Props = {
  fields: InputField[];
  draft: InputDraft;
  onChange: (name: string, value: string | boolean) => void;
  errorsOf: (name: string) => string[];
  /** Used when the schema names no fields: the whole input as JSON. */
  whole: string;
  onWholeChange: (value: string) => void;
  wholeErrors: string[];
};

/** One control per property of a workflow's input schema, or a JSON box when it has none. */
export default function InputFields({ fields, draft, onChange, errorsOf, whole, onWholeChange, wholeErrors }: Props) {
  if (fields.length === 0) {
    return (
      <Field label="Input" hint="The workflow declares no input fields. Give the input as a JSON object." errors={wholeErrors}>
        {(p) => <textarea {...p} rows={6} value={whole} spellCheck={false} onChange={(e) => onWholeChange(e.target.value)} className={control(wholeErrors.length > 0, "font-mono text-xs")} />}
      </Field>
    );
  }
  return (
    <>
      {fields.map((f) => {
        const common = { label: f.name, required: f.required, hint: f.description, errors: errorsOf(f.name) };
        const value = draft[f.name];
        if (f.type === "boolean") {
          return <CheckboxField key={f.name} label={f.name} hint={f.description} checked={value === true} onChange={(v) => onChange(f.name, v)} />;
        }
        if (f.type === "number" || f.type === "integer") {
          return <NumberField key={f.name} {...common} value={typeof value === "string" ? value : ""} onChange={(v) => onChange(f.name, v)} />;
        }
        if (f.type === "json") {
          return (
            <Field key={f.name} {...common} hint={f.description ?? "JSON"}>
              {(p) => <textarea {...p} rows={4} value={typeof value === "string" ? value : ""} spellCheck={false} onChange={(e) => onChange(f.name, e.target.value)} className={control(common.errors.length > 0, "font-mono text-xs")} />}
            </Field>
          );
        }
        return <TextField key={f.name} {...common} value={typeof value === "string" ? value : ""} onChange={(v) => onChange(f.name, v)} />;
      })}
    </>
  );
}
