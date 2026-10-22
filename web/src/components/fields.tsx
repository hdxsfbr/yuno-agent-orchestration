import { Fragment, useId, type ReactNode } from "react";

export const controlClass =
  "w-full rounded-md border bg-white px-3 py-1.5 text-sm shadow-sm placeholder:text-zinc-400 focus:outline-none focus:ring-1 disabled:opacity-60 dark:bg-zinc-900 dark:placeholder:text-zinc-600";
const controlOk = "border-zinc-300 focus:border-zinc-500 focus:ring-zinc-500 dark:border-zinc-700 dark:focus:border-zinc-400 dark:focus:ring-zinc-400";
const controlBad = "border-red-500 focus:border-red-500 focus:ring-red-500 dark:border-red-500";

/** Class string for an input, select or textarea; `invalid` switches to the error border. */
export function control(invalid?: boolean, extra = ""): string {
  return `${controlClass} ${invalid ? controlBad : controlOk} ${extra}`;
}

type ControlProps = { id: string; "aria-invalid"?: true; "aria-describedby"?: string };

type FieldProps = {
  label: string;
  hint?: string;
  errors?: string[];
  required?: boolean;
  className?: string;
  children: (props: ControlProps) => ReactNode;
};

/** Label, control, hint and inline errors. The control gets its id and aria wiring. */
export function Field({ label, hint, errors = [], required, className = "", children }: FieldProps) {
  const id = useId();
  const describedBy = [hint ? `${id}-hint` : "", errors.length ? `${id}-err` : ""].filter(Boolean).join(" ") || undefined;
  return (
    <div className={className}>
      <label htmlFor={id} className="mb-1 block text-sm font-medium">
        {label}
        {required ? <span className="ml-0.5 text-red-600" aria-hidden>*</span> : null}
      </label>
      {children({ id, ...(errors.length ? { "aria-invalid": true as const } : {}), "aria-describedby": describedBy })}
      {hint ? (
        <p id={`${id}-hint`} className="mt-1 text-xs text-zinc-500">
          {hint}
        </p>
      ) : null}
      <FieldErrors id={`${id}-err`} errors={errors} />
    </div>
  );
}

export function FieldErrors({ errors, id }: { errors: string[]; id?: string }) {
  if (errors.length === 0) return null;
  return (
    <div id={id} role="alert" className="mt-1 space-y-0.5 text-xs text-red-600 dark:text-red-400">
      {errors.map((e, i) => (
        <p key={i}>{e}</p>
      ))}
    </div>
  );
}

type Common = { label: string; hint?: string; errors?: string[]; required?: boolean; className?: string };

export function TextField({
  value,
  onChange,
  placeholder,
  maxLength,
  mono,
  disabled,
  ...c
}: Common & { value: string; onChange: (v: string) => void; placeholder?: string; maxLength?: number; mono?: boolean; disabled?: boolean }) {
  return (
    <Field {...c}>
      {(p) => (
        <input
          {...p}
          type="text"
          value={value}
          maxLength={maxLength}
          placeholder={placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
          className={control(!!c.errors?.length, mono ? "font-mono" : "")}
        />
      )}
    </Field>
  );
}

/** Numeric text input: the value stays a string so "" means "not set". */
export function NumberField({
  value,
  onChange,
  placeholder,
  ...c
}: Common & { value: string; onChange: (v: string) => void; placeholder?: string }) {
  return (
    <Field {...c}>
      {(p) => (
        <input
          {...p}
          type="text"
          inputMode="decimal"
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
          className={control(!!c.errors?.length)}
        />
      )}
    </Field>
  );
}

export function TextAreaField({
  value,
  onChange,
  rows = 4,
  placeholder,
  ...c
}: Common & { value: string; onChange: (v: string) => void; rows?: number; placeholder?: string }) {
  return (
    <Field {...c}>
      {(p) => (
        <textarea
          {...p}
          rows={rows}
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
          className={control(!!c.errors?.length)}
        />
      )}
    </Field>
  );
}

/** `group` puts the option under an optgroup of that label; consecutive options share one. */
export type Option = { value: string; label: string; group?: string };

function optionGroups(options: readonly Option[]): { group?: string; options: Option[] }[] {
  const out: { group?: string; options: Option[] }[] = [];
  for (const o of options) {
    const last = out[out.length - 1];
    if (last && last.group === o.group) last.options.push(o);
    else out.push({ group: o.group, options: [o] });
  }
  return out;
}

export function SelectField({
  value,
  onChange,
  options,
  ...c
}: Common & { value: string; onChange: (v: string) => void; options: readonly Option[] }) {
  const renderOptions = (list: Option[]) =>
    list.map((o) => (
      <option key={o.value} value={o.value}>
        {o.label}
      </option>
    ));
  return (
    <Field {...c}>
      {(p) => (
        <select {...p} value={value} onChange={(e) => onChange(e.target.value)} className={control(!!c.errors?.length)}>
          {optionGroups(options).map((g, i) =>
            g.group ? (
              <optgroup key={`${i}-${g.group}`} label={g.group}>
                {renderOptions(g.options)}
              </optgroup>
            ) : (
              <Fragment key={`${i}-`}>{renderOptions(g.options)}</Fragment>
            ),
          )}
        </select>
      )}
    </Field>
  );
}

export function CheckboxField({
  checked,
  onChange,
  label,
  hint,
  errors = [],
  role,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  label: string;
  hint?: string;
  errors?: string[];
  /** "switch" for a setting that is on or off; a plain checkbox otherwise. */
  role?: "switch";
}) {
  const id = useId();
  return (
    <div>
      <div className="flex items-start gap-2">
        <input
          id={id}
          type="checkbox"
          role={role}
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          aria-invalid={errors.length ? true : undefined}
          aria-describedby={errors.length ? `${id}-err` : undefined}
          className="mt-0.5 size-4 rounded border-zinc-300 accent-zinc-900 dark:accent-zinc-100"
        />
        <label htmlFor={id} className="text-sm">
          <span className="font-medium">{label}</span>
          {hint ? <span className="block text-xs text-zinc-500">{hint}</span> : null}
        </label>
      </div>
      <FieldErrors id={`${id}-err`} errors={errors} />
    </div>
  );
}

/** Options for a select built from plain string values. */
export const toOptions = (values: readonly string[]): Option[] => values.map((v) => ({ value: v, label: v }));
