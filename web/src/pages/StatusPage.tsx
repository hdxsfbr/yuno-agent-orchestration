import { useQuery } from "@tanstack/react-query";
import { getOpenClawStatus, getStatus } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { Badge } from "@/components/Badge";
import { Card, PageHeader, Stat } from "@/components/Card";
import { Loading, QueryError } from "@/components/QueryState";

function PlatformCards() {
  const q = useQuery({ queryKey: ["status"], queryFn: getStatus, refetchInterval: 5000 });

  if (q.isLoading) return <Loading />;
  if (q.error) return <QueryError what="the platform status" error={q.error} onRetry={() => void q.refetch()} />;
  const s = q.data!;

  const cards: [string, string | number][] = [
    ["Version", s.version],
    ["Uptime", formatDuration(s.uptime_s * 1000)],
    ["Database", s.db_ok ? "connected" : "unavailable"],
    ["Engine workers", s.workers],
    ["Live viewers", s.sse_clients],
    ["Queued runs", s.runs.queued ?? 0],
    ["Running", s.runs.running ?? 0],
    ["Waiting approval", s.runs.waiting_approval ?? 0],
  ];

  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
      {cards.map(([label, value]) => (
        <Stat key={label} label={label}>
          {value}
        </Stat>
      ))}
    </div>
  );
}

function OpenClawPanel() {
  const q = useQuery({ queryKey: ["openclaw-status"], queryFn: getOpenClawStatus, refetchInterval: 5000 });

  return (
    <section aria-labelledby="openclaw-heading">
      <h2 id="openclaw-heading" className="mb-3 text-base font-semibold">
        OpenClaw
      </h2>
      {q.isLoading ? <Loading /> : null}
      {q.error ? <QueryError what="the OpenClaw status" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data ? (
        <Card>
          <dl className="grid gap-x-6 gap-y-4 text-sm sm:grid-cols-3">
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Gateway</dt>
              <dd className="mt-1">
                <Badge tone={q.data.connected ? "green" : "red"}>{q.data.connected ? "Connected" : "Disconnected"}</Badge>
              </dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Version</dt>
              <dd className="mt-1 font-medium">{q.data.version || "—"}</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Agents</dt>
              <dd className="mt-1 font-medium">{q.data.agents ?? "—"}</dd>
            </div>
            <div className="sm:col-span-3">
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Scopes</dt>
              <dd className="mt-1.5 flex flex-wrap gap-1.5">
                {q.data.scopes && q.data.scopes.length > 0 ? (
                  q.data.scopes.map((scope) => (
                    <span key={scope} className="rounded-full bg-zinc-100 px-2.5 py-0.5 font-mono text-xs dark:bg-zinc-800">
                      {scope}
                    </span>
                  ))
                ) : (
                  <span className="text-zinc-500">None granted</span>
                )}
              </dd>
            </div>
          </dl>
        </Card>
      ) : null}
    </section>
  );
}

export default function StatusPage() {
  return (
    <div className="space-y-6">
      <PageHeader title="Status" />
      <PlatformCards />
      <OpenClawPanel />
    </div>
  );
}
