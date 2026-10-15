import { useId, useState, type KeyboardEvent } from "react";
import Button from "./Button";
import { control, FieldErrors } from "./fields";

type Props = {
  label: string;
  hint?: string;
  values: string[];
  onChange: (values: string[]) => void;
  placeholder?: string;
  /** Returns an error message for a value that must not be added. */
  validate?: (value: string) => string | null;
  /** Names offered as completions. */
  suggestions?: string[];
  /** Errors from the server or the form validation. */
  errors?: string[];
  mono?: boolean;
};

/** A list of strings edited as removable tags. Enter, comma or the Add button adds one. */
export default function TagInput({ label, hint, values, onChange, placeholder, validate, suggestions, errors = [], mono }: Props) {
  const id = useId();
  const [draft, setDraft] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);

  const commit = (raw: string): boolean => {
    const v = raw.trim();
    if (v === "") {
      setDraft("");
      setLocalError(null);
      return true;
    }
    if (values.includes(v)) {
      setLocalError(`"${v}" is already in the list`);
      return false;
    }
    const problem = validate?.(v) ?? null;
    if (problem) {
      setLocalError(problem);
      return false;
    }
    onChange([...values, v]);
    setDraft("");
    setLocalError(null);
    return true;
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      commit(draft);
    } else if (e.key === "Backspace" && draft === "" && values.length > 0) {
      onChange(values.slice(0, -1));
    }
  };

  const all = [...errors, ...(localError ? [localError] : [])];
  const listId = suggestions ? `${id}-list` : undefined;

  return (
    <div>
      <label htmlFor={id} className="mb-1 block text-sm font-medium">
        {label}
      </label>
      {values.length > 0 ? (
        <ul className="mb-2 flex flex-wrap gap-1.5">
          {values.map((v) => (
            <li key={v} className={`inline-flex items-center gap-1 rounded-full bg-zinc-100 py-0.5 pl-2.5 pr-1 text-xs dark:bg-zinc-800 ${mono ? "font-mono" : ""}`}>
              {v}
              <button
                type="button"
                aria-label={`Remove ${v}`}
                onClick={() => onChange(values.filter((x) => x !== v))}
                className="rounded-full px-1.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 dark:hover:bg-zinc-700 dark:hover:text-zinc-100"
              >
                ×
              </button>
            </li>
          ))}
        </ul>
      ) : null}
      <div className="flex gap-2">
        <input
          id={id}
          type="text"
          value={draft}
          list={listId}
          placeholder={placeholder}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={onKeyDown}
          onBlur={() => {
            if (draft.trim() !== "") commit(draft);
          }}
          aria-invalid={all.length > 0 || undefined}
          className={control(all.length > 0, mono ? "font-mono" : "")}
        />
        <Button onClick={() => commit(draft)} disabled={draft.trim() === ""}>
          Add
        </Button>
      </div>
      {suggestions ? (
        <datalist id={listId}>
          {suggestions.filter((s) => !values.includes(s)).map((s) => (
            <option key={s} value={s} />
          ))}
        </datalist>
      ) : null}
      {hint ? <p className="mt-1 text-xs text-zinc-500">{hint}</p> : null}
      <FieldErrors errors={all} />
    </div>
  );
}
