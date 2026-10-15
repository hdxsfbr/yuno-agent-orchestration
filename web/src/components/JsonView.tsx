import { formatCount } from "@/lib/format";

/** The text to show for a value: parsed JSON re-indented, other strings verbatim. */
export function prettyValue(value: unknown): { text: string; kind: "json" | "text" } {
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) {
      try {
        return { text: JSON.stringify(JSON.parse(trimmed), null, 2), kind: "json" };
      } catch {
        /* plain text that happens to start with a bracket */
      }
    }
    return { text: value, kind: "text" };
  }
  return { text: JSON.stringify(value, null, 2) ?? String(value), kind: "json" };
}

function summarize(value: unknown, text: string): string {
  if (Array.isArray(value)) return `${value.length} item${value.length === 1 ? "" : "s"}`;
  if (value !== null && typeof value === "object") {
    const n = Object.keys(value).length;
    return `${n} key${n === 1 ? "" : "s"}`;
  }
  return `${formatCount(text.length)} chars`;
}

/** Collapsible pretty-printed JSON or text. Nothing is rendered for a missing value. */
export default function JsonView({ label, value, defaultOpen = false }: { label: string; value: unknown; defaultOpen?: boolean }) {
  if (value === undefined || value === null || value === "") return null;
  const { text, kind } = prettyValue(value);
  return (
    <details open={defaultOpen} className="group rounded-md border border-zinc-200 bg-zinc-50 text-xs dark:border-zinc-800 dark:bg-zinc-950">
      <summary className="cursor-pointer select-none px-2.5 py-1.5 text-zinc-600 dark:text-zinc-400">
        <span className="font-medium text-zinc-900 dark:text-zinc-100">{label}</span>
        <span className="ml-2 text-zinc-500">{summarize(value, text)}</span>
      </summary>
      <pre className={`max-h-80 overflow-auto border-t border-zinc-200 p-2.5 dark:border-zinc-800 ${kind === "text" ? "whitespace-pre-wrap break-words font-sans" : "font-mono"}`}>
        {text}
      </pre>
    </details>
  );
}
