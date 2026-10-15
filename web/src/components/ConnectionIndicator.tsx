import type { Connection } from "@/lib/useEventStream";

const view: Record<Connection, { label: string; dot: string }> = {
  open: { label: "Live", dot: "bg-emerald-500" },
  connecting: { label: "Connecting…", dot: "bg-zinc-400" },
  reconnecting: { label: "Reconnecting…", dot: "bg-amber-500 animate-pulse" },
};

export default function ConnectionIndicator({ state }: { state: Connection }) {
  const v = view[state];
  return (
    <span role="status" className="inline-flex items-center gap-1.5 text-xs text-zinc-500" title="Connection to the live event stream">
      <span aria-hidden className={`size-2 rounded-full ${v.dot}`} />
      {v.label}
    </span>
  );
}
