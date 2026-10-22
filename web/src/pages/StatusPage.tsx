import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { getOpenClawStatus, getStatus } from "@/lib/api";
import { useToken } from "@/lib/auth";
import { formatDateTime, formatDuration } from "@/lib/format";
import { providerLabel, repoDisplay } from "@/lib/project";
import { useChannels } from "@/lib/useChannels";
import { useProjects } from "@/lib/useProjects";
import { useSchedules } from "@/lib/useSchedules";
import { Badge } from "@/components/Badge";
import { Card, PageHeader, Stat, cardClass } from "@/components/Card";
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
    ["Paused", s.runs.paused ?? 0],
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

const MODE_LABEL: Record<string, string> = { polling: "polling", webhook: "webhook" };

/** The projects runs can work on, with a way to the page that manages them. */
function ProjectsPanel() {
  const q = useProjects();
  return (
    <section aria-labelledby="projects-heading">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h2 id="projects-heading" className="text-base font-semibold">
          Projects
        </h2>
        {q.data && q.data.length > 0 ? (
          <Link to="/projects" className="text-sm underline">
            Manage projects
          </Link>
        ) : null}
      </div>
      {q.isLoading ? <Loading /> : null}
      {q.error ? <QueryError what="the projects" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? (
        <Card>
          <p className="text-sm text-zinc-500">
            None yet. Create one on the{" "}
            <Link to="/projects" className="text-zinc-900 underline dark:text-zinc-100">
              Projects page
            </Link>
            .
          </p>
        </Card>
      ) : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[40rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Repository</th>
                <th className="px-4 py-2.5 font-medium">Provider</th>
                <th className="px-4 py-2.5 font-medium">Branch</th>
                <th className="px-4 py-2.5 font-medium">Sandbox network</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((p) => (
                <tr key={p.id}>
                  <td className="px-4 py-2.5 font-medium">{p.name}</td>
                  <td className="break-all px-4 py-2.5 font-mono text-xs" title={p.repo_url}>
                    {repoDisplay(p.repo_url)}
                  </td>
                  <td className="px-4 py-2.5">{providerLabel(p.provider)}</td>
                  <td className="px-4 py-2.5 font-mono text-xs">{p.default_branch}</td>
                  <td className="px-4 py-2.5">
                    <Badge tone={p.sandbox_network ? "amber" : "zinc"} title={p.sandbox_network ? "Agents can install dependencies in the sandbox" : "The sandbox has no network access"}>
                      {p.sandbox_network ? "Network on" : "Network off"}
                    </Badge>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}

function OpenClawPanel() {
  const q = useQuery({ queryKey: ["openclaw-status"], queryFn: getOpenClawStatus, refetchInterval: 5000 });
  const channels = useChannels();
  const telegram = channels.data?.telegram;

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
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Telegram</dt>
              <dd className="mt-1">
                {telegram ? (
                  <Badge tone={telegram.enabled ? "green" : "zinc"}>
                    {telegram.enabled ? `Enabled${MODE_LABEL[telegram.mode] ? ` (${MODE_LABEL[telegram.mode]})` : ""}` : "Disabled"}
                  </Badge>
                ) : (
                  <span className="text-zinc-500">{channels.error ? "Unavailable" : "—"}</span>
                )}
              </dd>
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

function SchedulerPanel() {
  const status = useQuery({ queryKey: ["status"], queryFn: getStatus, refetchInterval: 5000 });
  const schedules = useSchedules();
  const active = schedules.data?.filter((s) => s.enabled) ?? [];
  // The earliest upcoming run among the schedules that are switched on.
  const upcoming = active.map((s) => s.next_run_at).filter((t): t is string => !!t).sort((a, b) => Date.parse(a) - Date.parse(b))[0];
  const nextDue = status.data?.scheduler?.next_due_at ?? upcoming;
  const tick = status.data?.scheduler?.tick_seconds;

  return (
    <section aria-labelledby="scheduler-heading">
      <h2 id="scheduler-heading" className="mb-3 text-base font-semibold">
        Scheduler
      </h2>
      {schedules.isLoading ? <Loading /> : null}
      {schedules.error ? <QueryError what="the schedules" error={schedules.error} onRetry={() => void schedules.refetch()} /> : null}
      {schedules.data ? (
        <Card>
          <dl className="grid gap-x-6 gap-y-4 text-sm sm:grid-cols-3">
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Active schedules</dt>
              <dd className="mt-1 font-medium">{active.length}</dd>
            </div>
            <div>
              <dt className="text-xs uppercase tracking-wide text-zinc-500">Next due</dt>
              <dd className="mt-1 font-medium">{nextDue ? formatDateTime(nextDue) : "Nothing scheduled"}</dd>
            </div>
            {tick ? (
              <div>
                <dt className="text-xs uppercase tracking-wide text-zinc-500">Checks for due schedules</dt>
                <dd className="mt-1 font-medium">Every {formatDuration(tick * 1000)}</dd>
              </div>
            ) : null}
          </dl>
        </Card>
      ) : null}
    </section>
  );
}

export default function StatusPage() {
  const token = useToken();
  return (
    <div className="space-y-6">
      <PageHeader title="Status">
        <Badge tone={token ? "green" : "zinc"}>{token ? "API token in use" : "No API token"}</Badge>
      </PageHeader>
      <PlatformCards />
      <OpenClawPanel />
      <ProjectsPanel />
      <SchedulerPanel />
    </div>
  );
}
