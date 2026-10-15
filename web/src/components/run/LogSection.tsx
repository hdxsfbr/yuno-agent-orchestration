import { useEffect, useRef } from "react";
import { formatDateTime } from "@/lib/format";
import type { LogLine } from "@/lib/trace";
import { MAX_LOG_LINES } from "@/lib/trace";

const levelClass: Record<string, string> = {
  error: "text-red-600 dark:text-red-400",
  warn: "text-amber-600 dark:text-amber-400",
  warning: "text-amber-600 dark:text-amber-400",
};

/** Log lines in arrival order; stays scrolled to the newest line unless the reader scrolled up. */
export default function LogSection({ logs }: { logs: LogLine[] }) {
  const box = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);

  useEffect(() => {
    const el = box.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [logs.length]);

  return (
    <div>
      <div
        ref={box}
        onScroll={(e) => {
          const el = e.currentTarget;
          pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        className="max-h-96 overflow-auto rounded-md border border-zinc-200 bg-zinc-50 p-2 font-mono text-xs dark:border-zinc-800 dark:bg-zinc-950"
      >
        <ul>
          {logs.map((l, i) => (
            <li key={l.id ?? i} className="flex gap-3 py-0.5">
              {l.at ? <span className="shrink-0 text-zinc-500">{formatDateTime(l.at)}</span> : null}
              <span className={`w-11 shrink-0 uppercase ${levelClass[l.level] ?? "text-zinc-500"}`}>{l.level}</span>
              <span className="whitespace-pre-wrap break-words">{l.message}</span>
            </li>
          ))}
        </ul>
      </div>
      {logs.length >= MAX_LOG_LINES ? <p className="mt-1 text-xs text-zinc-500">Showing the latest {MAX_LOG_LINES} lines.</p> : null}
    </div>
  );
}
