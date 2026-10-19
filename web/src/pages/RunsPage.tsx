import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router";
import {
  RUN_STATUSES,
  RUN_TRIGGERS,
  delegatedConversationId,
  isDelegatedRun,
  isTerminalStatus,
  issueRef,
  listRuns,
  type Run,
  type RunStatus,
  type RunTrigger,
} from "@/lib/api";
import { elapsedMs, formatCount, formatDateTime, formatDuration, formatMicros, humanize, shortId } from "@/lib/format";
import { requestNoun, requestRef } from "@/lib/mergeRequest";
import { shipRunRef } from "@/lib/plan";
import { useNow } from "@/lib/useNow";
import { useRefreshOnEvents } from "@/lib/useRefreshOnEvents";
import { Badge, RunStatusBadge } from "@/components/Badge";
import Button from "@/components/Button";
import { cardClass, PageHeader } from "@/components/Card";
import ConnectionIndicator from "@/components/ConnectionIndicator";
import { control } from "@/components/fields";
import PlanWaveLink from "@/components/project/PlanWaveLink";
import ProjectLink, { hasProject } from "@/components/project/ProjectLink";
import { EmptyState, Loading, QueryError } from "@/components/QueryState";

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/** The request a run updates, after its project's name: "!12" on GitLab, "#12" on GitHub, linking to the provider. */
function UpdatedRequest({ run }: { run: Run }) {
  const mr = run.merge_request;
  if (!run.merge_request_id || !mr || mr.id !== run.merge_request_id) return null;
  return (
    <a href={mr.url} target="_blank" rel="noreferrer" className="ml-1.5 font-mono text-xs underline" title={`Updates ${requestNoun(mr.provider).toLowerCase()} ${requestRef(mr)}`}>
      {requestRef(mr)}
    </a>
  );
}

/** How a run started: the trigger as a word, the issue an issue run came from, or a Delegated badge with a way back to the conversation. */
function TriggerCell({ run }: { run: Run }) {
  const issue = issueRef(run);
  if (issue) {
    return issue.url ? (
      <a href={issue.url} target="_blank" rel="noreferrer" className="underline" title="Open the issue on the provider">
        issue #{issue.number}
      </a>
    ) : (
      <>issue #{issue.number}</>
    );
  }
  if (shipRunRef(run)) return <PlanWaveLink run={run} />;
  if (!isDelegatedRun(run)) {
    return (
      <>
        {run.trigger}
        {run.trigger === "channel" && run.trigger_ref ? <span className="ml-1.5 font-mono text-xs text-zinc-500">{run.trigger_ref}</span> : null}
      </>
    );
  }
  const conversation = delegatedConversationId(run);
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <Badge tone="blue">Delegated</Badge>
      {conversation ? (
        <Link to={`/channels/${encodeURIComponent(conversation)}`} className="text-xs underline" title="Open the conversation this run was delegated from">
          Conversation
        </Link>
      ) : null}
    </span>
  );
}

function parseStatus(value: string | null): RunStatus | undefined {
  return RUN_STATUSES.find((s) => s === value);
}

function parseTrigger(value: string | null): RunTrigger | undefined {
  return RUN_TRIGGERS.find((t) => t === value);
}

export default function RunsPage() {
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const status = parseStatus(params.get("status"));
  const trigger = parseTrigger(params.get("trigger"));
  // The runs that named one merge request record: the project page's Merge requests table links here.
  const mergeRequestId = params.get("merge_request_id")?.trim() || undefined;

  const q = useQuery({
    queryKey: ["runs", status ?? "all", trigger ?? "all", mergeRequestId ?? "all"],
    queryFn: () => listRuns({ status, trigger, merge_request_id: mergeRequestId }),
    refetchOnWindowFocus: true,
  });

  const setFilter = (name: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(name, value);
    else next.delete(name);
    setParams(next, { replace: true });
  };
  // The request's own number, read from the rows that name it; the id is all the address carries.
  const filteredRequest = mergeRequestId ? q.data?.find((r) => r.merge_request?.id === mergeRequestId)?.merge_request : undefined;

  // Any run event may change a row (status, steps, cost), so every event refreshes the list.
  const connection = useRefreshOnEvents(["runs"]);

  const hasActive = !!q.data?.some((r) => !isTerminalStatus(r.status));
  const now = useNow(hasActive);

  return (
    <div>
      <PageHeader title="Runs">
        <ConnectionIndicator state={connection} />
        <label className="flex items-center gap-2 text-sm">
          <span className="text-zinc-500">Status</span>
          <select value={status ?? ""} onChange={(e) => setFilter("status", e.target.value)} className={control(false, "!w-auto")}>
            <option value="">All</option>
            {RUN_STATUSES.map((s) => (
              <option key={s} value={s}>
                {capitalize(humanize(s))}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-2 text-sm">
          <span className="text-zinc-500">Trigger</span>
          <select value={trigger ?? ""} onChange={(e) => setFilter("trigger", e.target.value)} className={control(false, "!w-auto")}>
            <option value="">All</option>
            {RUN_TRIGGERS.map((t) => (
              <option key={t} value={t}>
                {capitalize(t)}
              </option>
            ))}
          </select>
        </label>
        {mergeRequestId ? (
          <span className="flex items-center gap-2 text-sm">
            <span className="text-zinc-500">Merge request</span>
            <span className="font-mono">{filteredRequest ? requestRef(filteredRequest) : shortId(mergeRequestId)}</span>
            <Button size="sm" variant="ghost" onClick={() => setFilter("merge_request_id", "")} aria-label="Clear merge request filter">
              Clear
            </Button>
          </span>
        ) : null}
      </PageHeader>

      {q.isLoading ? <Loading label="Loading runs…" /> : null}
      {q.error ? <QueryError what="runs" error={q.error} onRetry={() => void q.refetch()} /> : null}
      {q.data && q.data.length === 0 ? (
        <EmptyState>
          {trigger || mergeRequestId
            ? "No runs match."
            : status
              ? `No ${humanize(status)} runs.`
              : "No runs yet. Start one from an agent."}
        </EmptyState>
      ) : null}
      {q.data && q.data.length > 0 ? (
        <div className={`${cardClass} overflow-x-auto`}>
          <table className="w-full min-w-[68rem] text-left text-sm">
            <thead className="border-b border-zinc-200 text-xs uppercase tracking-wide text-zinc-500 dark:border-zinc-800">
              <tr>
                <th className="px-4 py-2.5 font-medium">Run</th>
                <th className="px-4 py-2.5 font-medium">Agent</th>
                <th className="px-4 py-2.5 font-medium">Model</th>
                <th className="px-4 py-2.5 font-medium">Project</th>
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
                    {hasProject(r) ? (
                      <>
                        <ProjectLink run={r} />
                        <UpdatedRequest run={r} />
                      </>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="inline-flex flex-wrap items-center gap-1.5">
                      <RunStatusBadge status={r.status} />
                      {r.toolchain ? <Badge title="Toolchain of the project checkout">{r.toolchain}</Badge> : null}
                    </span>
                  </td>
                  <td className="px-4 py-2.5">
                    <TriggerCell run={r} />
                  </td>
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
