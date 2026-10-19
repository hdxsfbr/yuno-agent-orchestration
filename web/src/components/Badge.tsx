import type { ReactNode } from "react";
import { humanize } from "@/lib/format";

export type Tone = "zinc" | "green" | "red" | "amber" | "blue" | "orange";

const tones: Record<Tone, string> = {
  zinc: "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300",
  green: "bg-emerald-100 text-emerald-800 dark:bg-emerald-950 dark:text-emerald-300",
  red: "bg-red-100 text-red-800 dark:bg-red-950 dark:text-red-300",
  amber: "bg-amber-100 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
  blue: "bg-blue-100 text-blue-800 dark:bg-blue-950 dark:text-blue-300",
  orange: "bg-orange-100 text-orange-800 dark:bg-orange-950 dark:text-orange-300",
};

export function Badge({ tone = "zinc", pulse = false, children, title }: { tone?: Tone; pulse?: boolean; children: ReactNode; title?: string }) {
  return (
    <span className={`inline-flex items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium ${tones[tone]}`} title={title}>
      {pulse ? <span aria-hidden className="size-1.5 animate-pulse rounded-full bg-current" /> : null}
      {children}
    </span>
  );
}

type Kind = "run" | "node" | "tool" | "approval" | "branch";

const statusTones: Record<Kind, Record<string, Tone>> = {
  run: {
    queued: "zinc",
    running: "blue",
    waiting_approval: "amber",
    succeeded: "green",
    failed: "red",
    cancelled: "zinc",
    timed_out: "orange",
  },
  node: { pending: "zinc", running: "blue", waiting_approval: "amber", succeeded: "green", failed: "red", skipped: "zinc" },
  tool: {
    executed: "green",
    awaiting_approval: "amber",
    approved: "green",
    rejected: "red",
    blocked: "red",
    expired: "orange",
    failed: "red",
  },
  approval: { pending: "amber", approved: "green", rejected: "red", expired: "orange" },
  branch: { running: "blue", waiting_approval: "amber", succeeded: "green", failed: "red", cancelled: "zinc" },
};

/** Words that replace the plain status for a kind of object: a branch parked on a question is waiting for the operator's answer. */
const statusLabels: Partial<Record<Kind, Record<string, string>>> = {
  branch: { waiting_approval: "Waiting for an answer" },
};

/** Status pill: the colour comes from the kind of object and its status. */
export function StatusBadge({ kind, status }: { kind: Kind; status: string }) {
  const tone = statusTones[kind][status] ?? "zinc";
  return (
    <Badge tone={tone} pulse={status === "running"}>
      {statusLabels[kind]?.[status] ?? humanize(status)}
    </Badge>
  );
}

export const RunStatusBadge = ({ status }: { status: string }) => <StatusBadge kind="run" status={status} />;
