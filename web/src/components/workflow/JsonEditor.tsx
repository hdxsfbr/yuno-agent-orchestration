import { useEffect, useRef, useState } from "react";
import { Field, control } from "../fields";

export type BlockerFn = (key: string, message: string | null) => void;

const stringify = (v: unknown) => (v === undefined ? "" : JSON.stringify(v, null, 2));
const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

type Props = {
  label: string;
  hint?: string;
  /** The parsed value; undefined when the box is empty. */
  value: unknown;
  /** Called with the parsed value, or undefined when the box is emptied. Not called while the text is not valid JSON. */
  onChange: (value: unknown) => void;
  /** Reports "this editor holds text that cannot be saved" so the page can hold back Save. */
  blockerKey: string;
  onBlocker: BlockerFn;
  /** Messages from the server for this field. */
  errors?: string[];
  rows?: number;
  placeholder?: string;
};

/** A textarea for a JSON value that shows parse errors as you type and never emits broken JSON. */
export default function JsonEditor({ label, hint, value, onChange, blockerKey, onBlocker, errors = [], rows = 8, placeholder }: Props) {
  const [text, setText] = useState(() => stringify(value));
  const [parseError, setParseError] = useState<string | null>(null);
  // What the box currently stands for, so a value that comes back from the page after our
  // own onChange is not mistaken for an outside change and re-formatted under the cursor.
  const shown = useRef(value);

  useEffect(() => {
    if (parseError === null && !same(value, shown.current)) {
      shown.current = value;
      setText(stringify(value));
    }
  }, [value, parseError]);

  useEffect(() => {
    onBlocker(blockerKey, parseError ? `${label} is not valid JSON` : null);
  }, [onBlocker, blockerKey, label, parseError]);
  useEffect(() => () => onBlocker(blockerKey, null), [onBlocker, blockerKey]);

  const edit = (next: string) => {
    setText(next);
    if (next.trim() === "") {
      setParseError(null);
      shown.current = undefined;
      onChange(undefined);
      return;
    }
    try {
      const parsed: unknown = JSON.parse(next);
      setParseError(null);
      shown.current = parsed;
      onChange(parsed);
    } catch (e) {
      setParseError(e instanceof Error ? e.message : String(e));
    }
  };

  const all = [...(parseError ? [`Not valid JSON: ${parseError}`] : []), ...errors];
  return (
    <Field label={label} hint={hint} errors={all}>
      {(p) => (
        <textarea
          {...p}
          rows={rows}
          value={text}
          spellCheck={false}
          placeholder={placeholder}
          onChange={(e) => edit(e.target.value)}
          className={control(all.length > 0, "font-mono text-xs")}
        />
      )}
    </Field>
  );
}
