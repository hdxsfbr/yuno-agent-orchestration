import { useEffect, useRef, useState } from "react";
import { CONDITION_OPS, type Condition, type ConditionOp } from "@/lib/api";
import { OP_LABEL, formatList, formatValue, isCombined, parseList, parseValue } from "@/lib/condition";
import { errorsFor, type FieldErrors } from "@/lib/agentForm";
import { FieldErrors as ErrorList, SelectField, TextField, type Option } from "../fields";
import JsonEditor, { type BlockerFn } from "./JsonEditor";

type Mode = "always" | "single" | "advanced";

const MODES: Option[] = [
  { value: "always", label: "Always follow this edge" },
  { value: "single", label: "Follow when a value matches" },
  { value: "advanced", label: "Advanced (JSON)" },
];

const OPS: Option[] = CONDITION_OPS.map((op) => ({ value: op, label: OP_LABEL[op] }));

const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/**
 * A value edited as text. The text is parsed on every keystroke, but the box is only
 * rewritten from the value when the value stops matching what the text parses to, so typing
 * "3.05" is not cut short when "3.0" momentarily reads as the number 3.
 */
function ValueField({
  label,
  hint,
  value,
  parse,
  format,
  onChange,
  placeholder,
  errors,
}: {
  label: string;
  hint: string;
  value: unknown;
  parse: (text: string) => unknown;
  format: (value: unknown) => string;
  onChange: (value: unknown) => void;
  placeholder?: string;
  errors: string[];
}) {
  const [text, setText] = useState(() => format(value));
  const latest = useRef(text);
  latest.current = text;
  useEffect(() => {
    if (!same(parse(latest.current), value)) setText(format(value));
  }, [value, parse, format]);
  return (
    <TextField
      label={label}
      hint={hint}
      mono
      value={text}
      placeholder={placeholder}
      errors={errors}
      onChange={(next) => {
        setText(next);
        onChange(parse(next));
      }}
    />
  );
}

type Props = {
  value: Condition | undefined;
  onChange: (c: Condition | undefined) => void;
  /** Errors for this edge, keyed by the rest of the path ("when.path"). */
  errors: FieldErrors;
  blockerKey: string;
  onBlocker: BlockerFn;
};

/** The condition on an edge: none, a single {path, op, value} check, or raw JSON for all/any. */
export default function ConditionEditor({ value, onChange, errors, blockerKey, onBlocker }: Props) {
  const [mode, setMode] = useState<Mode>(() => (!value ? "always" : isCombined(value) ? "advanced" : "single"));

  const setModeTo = (next: Mode) => {
    setMode(next);
    if (next === "always") onChange(undefined);
    else if (next === "single" && (!value || isCombined(value))) onChange({ path: "", op: "eq", value: "" });
  };

  const op: ConditionOp = value?.op ?? "eq";
  const setOp = (next: ConditionOp) => {
    if (!value) return;
    const patch: Condition = { ...value, op: next };
    if (next === "exists") delete patch.value;
    else if (next === "in") patch.value = Array.isArray(value.value) ? value.value : value.value === undefined || value.value === "" ? [] : [value.value];
    else patch.value = Array.isArray(value.value) ? (value.value[0] ?? "") : (value.value ?? "");
    onChange(patch);
  };

  const pathErrors = errorsFor(errors, "when.path");
  if (mode === "single" && value && value.path && !value.path.startsWith("/")) pathErrors.push("Start the path with /, for example /reviewer/verdict");

  return (
    <fieldset className="space-y-3">
      <legend className="mb-1 text-sm font-medium">Condition</legend>
      <SelectField label="When to follow" value={mode} onChange={(v) => setModeTo(v as Mode)} options={MODES} errors={mode === "always" ? errorsFor(errors, "when") : []} />

      {mode === "single" && value ? (
        <>
          <ErrorList errors={errors.when ?? []} />
          <TextField
            label="Path"
            mono
            value={value.path ?? ""}
            placeholder="/reviewer/verdict"
            hint="Where to look in the run state: / then the node id, then the field."
            errors={pathErrors}
            onChange={(path) => onChange({ ...value, path })}
          />
          <SelectField label="Check" value={op} onChange={(v) => setOp(v as ConditionOp)} options={OPS} errors={errorsFor(errors, "when.op")} />
          {op === "exists" ? null : op === "in" ? (
            <ValueField
              label="Values"
              hint="Separate values with commas."
              value={value.value}
              parse={parseList}
              format={formatList}
              placeholder="approved, merged"
              errors={errorsFor(errors, "when.value")}
              onChange={(v) => onChange({ ...value, value: v })}
            />
          ) : (
            <ValueField
              label="Value"
              hint="Numbers, true, false and null are read as such; anything else is text. Use quotes to force text."
              value={value.value}
              parse={parseValue}
              format={formatValue}
              placeholder="changes_requested"
              errors={errorsFor(errors, "when.value")}
              onChange={(v) => onChange({ ...value, value: v })}
            />
          )}
        </>
      ) : null}

      {mode === "advanced" ? (
        <>
          {value && isCombined(value) ? (
            <p role="note" className="rounded-md border border-amber-300 bg-amber-50 p-2.5 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
              This condition combines several checks with all or any, which the form cannot show. Edit it as JSON.
            </p>
          ) : null}
          <JsonEditor
            label="Condition JSON"
            hint='Use {"path", "op", "value"} or combine checks with {"all": [...]} and {"any": [...]}.'
            rows={10}
            value={value}
            onChange={(v) => onChange(v as Condition | undefined)}
            blockerKey={blockerKey}
            onBlocker={onBlocker}
            errors={errorsFor(errors, "when")}
            placeholder={'{"all": [{"path": "/reviewer/verdict", "op": "eq", "value": "approved"}]}'}
          />
        </>
      ) : null}
    </fieldset>
  );
}
