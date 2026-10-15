import type { ReactNode } from "react";

export const cardClass = "rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900";

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return <div className={`${cardClass} p-4 ${className}`}>{children}</div>;
}

/** Label above a value, the pattern used by the status cards. */
export function Stat({ label, children, title }: { label: string; children: ReactNode; title?: string }) {
  return (
    <div className={`${cardClass} p-4`} title={title}>
      <div className="text-xs uppercase tracking-wide text-zinc-500">{label}</div>
      <div className="mt-1 text-lg font-medium">{children}</div>
    </div>
  );
}

export function PageHeader({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
      <h1 className="text-xl font-semibold">{title}</h1>
      {children ? <div className="flex flex-wrap items-center gap-2">{children}</div> : null}
    </div>
  );
}
