import { useEffect, useState } from "react";
import type { Limits } from "@/lib/api";
import { LIMIT_FIELDS, errorsFor, type FieldErrors } from "@/lib/agentForm";
import { NumberField } from "../fields";
import type { BlockerFn } from "./JsonEditor";

const INTEGER = /^\d+$/;
const DECIMAL = /^\d+(\.\d+)?$/;

type Spec = (typeof LIMIT_FIELDS)[number];

const asText = (n: number | undefined) => (n ? String(n) : "");

function LimitField({
  spec,
  value,
  errors,
  onChange,
  onBlocker,
}: {
  spec: Spec;
  value: number | undefined;
  errors: string[];
  onChange: (n: number | undefined) => void;
  onBlocker: BlockerFn;
}) {
  const [text, setText] = useState(() => asText(value));
  const trimmed = text.trim();
  const problem = trimmed === "" || (spec.integer ? INTEGER : DECIMAL).test(trimmed) ? null : spec.integer ? `${spec.label} must be a whole number` : `${spec.label} must be a number`;

  const key = `limits.${spec.key}`;
  useEffect(() => {
    onBlocker(key, problem);
  }, [onBlocker, key, problem]);
  useEffect(() => () => onBlocker(key, null), [onBlocker, key]);

  return (
    <NumberField
      label={spec.label}
      value={text}
      placeholder={spec.fallback}
      errors={[...(problem ? [problem] : []), ...errors]}
      onChange={(v) => {
        setText(v);
        const t = v.trim();
        if (t === "") onChange(undefined);
        else if ((spec.integer ? INTEGER : DECIMAL).test(t)) onChange(Number(t) || undefined);
      }}
    />
  );
}

type Props = {
  limits: Limits | undefined;
  /** Server errors keyed by full path ("limits.max_steps"). */
  errors: FieldErrors;
  onChange: (limits: Limits) => void;
  onBlocker: BlockerFn;
};

/** The seven limits a run of the workflow is held to; empty fields use the platform defaults. */
export default function LimitsPanel({ limits, errors, onChange, onBlocker }: Props) {
  const current = limits ?? {};
  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {LIMIT_FIELDS.map((spec) => (
        <LimitField
          key={spec.key}
          spec={spec}
          value={current[spec.key]}
          errors={errorsFor(errors, `limits.${spec.key}`)}
          onBlocker={onBlocker}
          onChange={(n) => {
            const next: Limits = { ...current };
            if (n === undefined) delete next[spec.key];
            else next[spec.key] = n;
            onChange(next);
          }}
        />
      ))}
    </div>
  );
}
