import { useCallback, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router";
import { RUN_STATUSES, isTerminalStatus, listRuns, type RunStatus } from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros, humanize, shortId } from "@/lib/format";
import { useEventStream } from "@/lib/useEventStream";
import { useNow } from "@/lib/useNow";
import { RunStatusBadge } from "@/components/Badge";
import { cardClass, PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { control } from "@/components/fields";
import { EmptyState, Loading, QueryError } from "@/components/QueryState";

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** How long to gather events before refreshing the list. */
const REFRESH_DELAY_MS = 500;

function parseStatus(value: string | null): RunStatus | undefined {
  return RUN_STATUSES.find((s) => s === value);
}

export default function RunsPage() {
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const status = parseStatus(params.get("status"));

  const q = useQuery({
    queryKey: ["runs", status ?? "all"],
    queryFn: () => listRuns({ status }),
    refetchOnWindowFocus: true,
  });

  // Any run event may change a row (status, steps, cost), so refresh the list, at most
  // twice a second however busy the stream is.
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const onEvent = useCallback(() => {
    if (timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = undefined;
      void qc.invalidateQueries({ queryKey: ["runs"] });
    }, REFRESH_DELAY_MS);
  }, [qc]);
  useEffect(() => () => clearTimeout(timer.current), []);
  const connection = useEventStream("/api/events", onEvent);

  const hasActive = !!q.data?.some((r) => !isTerminalStatus(r.status));
  const now = useNow(hasActive);

  return (
    <div>
      <PageHeader title="Runs">
        <ConnectionIndicator state={connection} />
        <label className="flex items-center gap-2 text-sm">
          <span className="text-zinc-500">Status</span>
          <select
            value={status ?? ""}
            onChange={(e) => {
              const next = new URLSearchParams(params);
              if (e.target.value) next.set("status", e.target.value);
              else next.delete("status");
              setParams(next, { replace: true });
            }}
            className={control(false, "!w-auto")}
          >
            <option value="">All</option>
            {RUN_STATUSES.map((s) => (
              <option key={s} value={s}>
                {capitalize(humanize(s))}
              </option>
            ))}
          </select>
        </label>
      </PageHeader>

      {q.isLoading ? <Loading label="Loading runs…" /> : null}
      {q.error ? <QueryError what="runs" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? (
        <EmptyState>{status ? `No ${humanize(status)} runs.` : "No runs yet. Start one from an agent."}</EmptyState>
      ) : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[60rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Run</th>
                <th className="px-4 py-2.5 font-medium">Agent</th>
                <th className="px-4 py-2.5 font-medium">Model</th>
                <th className="px-4 py-2.5 font-medium">Status</th>
                <th className="px-4 py-2.5 font-medium">Trigger</th>
                <th className="px-4 py-2.5 font-medium">Created</th>
                <th className="px-4 py-2.5 text-right font-medium">Steps</th>
                <th className="px-4 py-2.5 text-right font-medium">Cost</th>
                <th className="px-4 py-2.5 text-right font-medium">Duration</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {q.data.map((r) => (
                <tr
                  key={r.id}
                  className="cursor-pointer hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
                  onClick={(e) => {
                    // The id is a real link; let it navigate on its own.
                    if ((e.target as HTMLElement).closest("a")) return;
                    void navigate(`/runs/${r.id}`);
                  }}
                >
                  <td className="px-4 py-2.5">
                    <Link to={`/runs/${r.id}`} className="font-mono hover:underline" title={r.id}>
                      {shortId(r.id)}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">{r.agent_names?.length ? r.agent_names.join(", ") : "—"}</td>
                  <td className="px-4 py-2.5 font-mono text-xs">{r.models?.length ? r.models.join(", ") : "—"}</td>
                  <td className="px-4 py-2.5">
                    <RunStatusBadge status={r.status} />
                  </td>
                  <td className="px-4 py-2.5">{r.trigger}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-zinc-600 dark:text-zinc-400">{formatDateTime(r.created_at)}</td>
                  <td className="px-4 py-2.5 text-right tabular-nums">{formatCount(r.step_count)}</td>
                  <td className="px-4 py-2.5 text-right tabular-nums">{formatMicros(r.cost_usd_micros)}</td>
                  <td className="px-4 py-2.5 text-right tabular-nums">{formatDuration(elapsedMs(r.started_at, r.finished_at, now))}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}
