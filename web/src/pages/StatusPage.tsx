import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";

type Status = {
  version: string;
  uptime_s: number;
  db_ok: boolean;
  workers: number;
  sse_clients: number;
  runs: Record<string, number>;
};

export default function StatusPage() {
  const q = useQuery({
    queryKey: ["status"],
    queryFn: () => api<Status>("/api/status"),
    refetchInterval: 5000,
  });

  if (q.isLoading) return <p className="text-sm text-zinc-500">Loading…</p>;
  if (q.error) return <p className="text-sm text-red-600">Could not reach the API: {String(q.error)}</p>;
  const s = q.data!;

  const cards: [string, string | number][] = [
    ["Version", s.version],
    ["Uptime", `${s.uptime_s}s`],
    ["Database", s.db_ok ? "connected" : "unavailable"],
    ["Engine workers", s.workers],
    ["Live viewers", s.sse_clients],
    ["Queued runs", s.runs.queued ?? 0],
    ["Running", s.runs.running ?? 0],
    ["Waiting approval", s.runs.waiting_approval ?? 0],
  ];

  return (
    <div>
      <h1 className="mb-4 text-xl font-semibold">Status</h1>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        {cards.map(([label, value]) => (
          <div key={label} className="rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-900">
            <div className="text-xs uppercase tracking-wide text-zinc-500">{label}</div>
            <div className="mt-1 text-lg font-medium">{value}</div>
          </div>
        ))}
      </div>
    </div>
  );
}
